package crashtest

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/proto"
	"github.com/tanxena/matchd/internal/snapshot"
	"github.com/tanxena/matchd/internal/wal"
)

var (
	crashFull = flag.Bool("crash", false, "run the full crash test (50 consecutive kill-and-recover runs)")
	crashRuns = flag.Int("crashruns", 0, "number of kill-and-recover runs (overrides -crash)")
)

var engineBin string // built once by TestMain

func TestMain(m *testing.M) {
	flag.Parse()
	dir, err := os.MkdirTemp("", "crashtest-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	engineBin = filepath.Join(dir, "engine")
	if os.PathSeparator == '\\' {
		engineBin += ".exe"
	}
	build := exec.Command("go", "build", "-o", engineBin, "github.com/tanxena/matchd/cmd/engine")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building the engine binary:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// What clients send and receive, kept for checking after the kill.

// answer is a sequenced reply a client received: proof the command was
// acknowledged (Ack, engine Reject, or CancelAck for a requested cancel).
type answer struct {
	seq     uint64
	kind    engine.CommandKind
	client  uint32
	clOrd   uint64 // NewOrder answers
	orderID uint64 // Cancel answers
}

type receivedFill struct {
	client uint32
	fill   proto.Fill
}

type record struct {
	mu      sync.Mutex // guards everything below; the client's reader and writer both use it
	answers []answer
	fills   []receivedFill
	live    map[uint32][]uint64 // client -> acked order ids (cancel candidates)
}

// runClient trades until the connection breaks (the kill).
func runClient(addr string, id uint32, clOrdBase uint64, rec *record, rng *rand.Rand) {
	c, err := client.Dial(addr, id, 5*time.Second)
	if err != nil {
		return // the engine may already be dead
	}
	defer func() { _ = c.Close() }()
	done := make(chan struct{}) // closed when the reader sees the connection end
	go func() {                 // reader
		defer close(done)
		for {
			m, err := c.Next()
			if err != nil {
				return
			}
			rec.mu.Lock()
			switch m.Type {
			case proto.TypeAck:
				rec.answers = append(rec.answers, answer{seq: m.Ack.Seq, kind: engine.CmdNewOrder, client: id, clOrd: m.Ack.ClOrdID})
				rec.live[id] = append(rec.live[id], m.Ack.OrderID)
			case proto.TypeReject:
				r := m.Reject
				if r.Seq == 0 {
					break // gateway reject: not sequenced
				}
				a := answer{seq: r.Seq, client: id}
				if r.ClOrdID != 0 {
					a.kind, a.clOrd = engine.CmdNewOrder, r.ClOrdID
				} else {
					a.kind, a.orderID = engine.CmdCancel, r.OrderID
				}
				rec.answers = append(rec.answers, a)
			case proto.TypeCancelAck:
				if m.CancelAck.Reason == proto.CancelRequested {
					rec.answers = append(rec.answers, answer{seq: m.CancelAck.Seq, kind: engine.CmdCancel, client: id, orderID: m.CancelAck.OrderID})
				}
			case proto.TypeFill:
				rec.fills = append(rec.fills, receivedFill{client: id, fill: m.Fill})
			}
			rec.mu.Unlock()
		}
	}()
	for n := uint64(1); ; n++ { // writer
		select {
		case <-done:
			return
		default:
		}
		var err error
		if r := rng.IntN(10); r < 7 {
			side := proto.SideBuy
			if rng.IntN(2) == 0 {
				side = proto.SideSell
			}
			o := proto.NewOrder{Instrument: uint32(1 + rng.IntN(2)), ClOrdID: clOrdBase + n, Side: side, Type: proto.TypeLimit,
				TIF: proto.TIFGTC, Price: int64(100 + rng.IntN(9) - 4), Qty: int64(1 + rng.IntN(5))}
			switch r {
			case 6:
				o.Type, o.TIF, o.Price = proto.TypeMarket, 0, 0
			case 5: // the rest cancelled on arrival (milestone 8)
				o.TIF = proto.TIFIOC
			case 4: // all or nothing: often rejected
				o.TIF, o.Qty = proto.TIFFOK, int64(1+rng.IntN(12))
			}
			err = c.NewOrder(o)
		} else {
			rec.mu.Lock()
			ids := rec.live[id]
			var oid uint64
			if len(ids) > 0 {
				oid = ids[rng.IntN(len(ids))]
			}
			rec.mu.Unlock()
			if oid == 0 {
				continue
			}
			err = c.Cancel(proto.Cancel{Instrument: uint32(1 + rng.IntN(2)), OrderID: oid}) // sometimes the wrong instrument
		}
		if err != nil {
			<-done
			return
		}
		if n%25 == 0 {
			time.Sleep(time.Millisecond) // keep volumes moderate
		}
	}
}

// ---------------------------------------------------------------------------

var listenRe = regexp.MustCompile(`msg="gateway listening" addr=(\S+)`)

// startEngine runs the engine binary on dir and returns it once it accepts
// connections, with its address and a buffer collecting its log.
// engineLog is the engine's stderr as collected so far. The scanner
// goroutine appends to it while the test reads it, so both lock.
type engineLog struct {
	mu sync.Mutex // guards b
	b  strings.Builder
}

func (l *engineLog) add(line string) {
	l.mu.Lock()
	l.b.WriteString(line + "\n")
	l.mu.Unlock()
}

func (l *engineLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func startEngine(t *testing.T, dir, archive, policy string, segBytes int) (*exec.Cmd, string, *engineLog) {
	t.Helper()
	cmd := exec.Command(engineBin, "-data", dir, "-archive", archive, "-listen", "127.0.0.1:0", "-http", "", "-fsync", policy,
		"-snapshot-every", "200", "-snapshot-interval", "20ms", "-idle-timeout", "0",
		"-wal-segment-bytes", fmt.Sprint(segBytes),
		"-instrument", "XYZ:1:1", "-instrument", "ABC:1:1")
	prepareInterrupt(cmd)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	logs := &engineLog{}
	addrCh := make(chan string, 1)
	go func() { // scanner: forwards the address, keeps the log
		sc := bufio.NewScanner(stderr)
		sent := false
		for sc.Scan() {
			logs.add(sc.Text())
			if m := listenRe.FindStringSubmatch(sc.Text()); m != nil && !sent {
				addrCh <- m[1]
				sent = true
			}
		}
		if !sent {
			close(addrCh)
		}
	}()
	select {
	case addr, ok := <-addrCh:
		if !ok {
			_ = cmd.Wait()
			t.Fatalf("engine exited during startup:\n%s", logs.String())
		}
		return cmd, addr, logs
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("engine didn't start:\n%s", logs.String())
	}
	return nil, "", nil
}

func TestCrashRecovery(t *testing.T) {
	runs := 5
	if *crashFull {
		runs = 50
	}
	if *crashRuns > 0 {
		runs = *crashRuns
	}
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	// Segments pruned behind snapshots are moved here, not deleted (review
	// F-007), so after the last run every acknowledged command and every
	// received fill, from every run, can be checked against a replay of
	// the whole history from seq 1. Same volume as dir: they are renamed.
	archive := filepath.Join(base, "archive")
	policies := []string{"always", "group:1ms", "never"}
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1)) // a different crash pattern each time...
	seed := rng.Uint64()
	t.Logf("crash pattern seed %d", seed) // ...but logged, to reproduce a failure
	rng = rand.New(rand.NewPCG(seed, 2))

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	seenFills := map[[2]uint64]bool{} // (match id, role) received by any client, all runs
	var prevWALLast uint64
	var totals struct {
		answers, fills, checkedInWAL, matchesCompared, snapshotsCompared, tornTails int
		walPruned                                                                   bool
		injected                                                                    int // torn tails written into the WAL by the harness
	}
	var allAnswers []answer     // every run's, for the whole-history check
	var allFills []receivedFill // likewise
	injectedLast := 0           // bytes of torn tail injected after the previous run (0: none)
	tornRe := regexp.MustCompile(`msg=recovered .*torn_bytes=(\d+)`)

	for run := range runs {
		policy := policies[run%len(policies)]
		// Odd runs: 64 KiB WAL segments, so kills land during segment rolls
		// and pruning. Even runs: 64 MiB, so nothing is pruned and every
		// acknowledged command can be checked against the WAL directly.
		segBytes := 64 << 20
		if run%2 == 1 {
			segBytes = 64 << 10
		}
		cmd, addr, logs := startEngine(t, dir, archive, policy, segBytes)
		// A torn tail the harness injected after the previous run must have
		// been found and cut off by the real binary's recovery.
		if injectedLast > 0 {
			m := tornRe.FindStringSubmatch(logs.String())
			if m == nil || m[1] == "0" {
				_ = cmd.Process.Kill()
				t.Fatalf("run %d: the engine didn't report the %d-byte torn tail injected after the previous run:\n%s", run, injectedLast, logs.String())
			}
			totals.tornTails++
			injectedLast = 0
		}

		rec := &record{live: map[uint32][]uint64{}}
		var wg sync.WaitGroup // the client goroutines
		for c := range uint32(3) {
			wg.Add(1)
			crng := rand.New(rand.NewPCG(seed, uint64(run*10)+uint64(c)))
			go func() { defer wg.Done(); runClient(addr, c+1, uint64(run+1)*1_000_000, rec, crng) }()
		}
		time.Sleep(time.Duration(80+rng.IntN(320)) * time.Millisecond)
		if err := cmd.Process.Kill(); err != nil { // SIGKILL / TerminateProcess
			t.Fatal(err)
		}
		_ = cmd.Wait()
		wg.Wait()

		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("run %d (fsync=%s): %s\nengine log:\n%s", run, policy, fmt.Sprintf(format, args...), logs.String())
		}

		// 1. replay-check: WAL vs newest snapshot vs fill log.
		res, err := snapshot.ReplayCheck(dir)
		if err != nil {
			fail("replay-check: %v", err)
		}
		if res.SnapshotCompared {
			totals.snapshotsCompared++
		}

		// 2. Every acknowledged command is in the WAL, at the seq it was
		//    acknowledged with, with the content the client sent.
		walCmds := map[uint64]engine.Command{}
		_, info, err := wal.Open(filepath.Join(dir, "wal"), wal.Options{ReadOnly: true}, func(r wal.Record) error {
			var c engine.Command
			if err := engine.DecodeCommand(r.Payload, &c); err != nil {
				return err
			}
			walCmds[r.Seq] = c
			return nil
		})
		if err != nil {
			fail("reading WAL: %v", err)
		}
		for _, a := range rec.answers {
			if a.seq > info.LastSeq {
				fail("command acknowledged with seq %d but the WAL ends at %d: an ack escaped before durability", a.seq, info.LastSeq)
			}
			if a.seq <= prevWALLast {
				fail("seq %d reused: it was already in the WAL before this run (which ended at %d)", a.seq, prevWALLast)
			}
			c, ok := walCmds[a.seq]
			if !ok {
				if a.seq >= info.FirstSeq {
					fail("acknowledged seq %d missing from the WAL", a.seq)
				}
				continue // pruned: covered by the snapshots replay-check verified
			}
			mismatch := c.Kind != a.kind || uint32(c.Client) != a.client ||
				(a.kind == engine.CmdNewOrder && uint64(c.ClOrdID) != a.clOrd) ||
				(a.kind == engine.CmdCancel && uint64(c.OrderID) != a.orderID)
			if mismatch {
				fail("WAL record %d is %+v, but the client was answered for %+v", a.seq, c, a)
			}
			totals.checkedInWAL++
		}

		// 3. No fill delivered twice, and every fill a client received is the
		//    one the replay produces.
		for _, rf := range rec.fills {
			f := rf.fill
			key := [2]uint64{f.MatchID, uint64(f.Role)}
			if seenFills[key] {
				fail("fill for match %d (role %d) delivered twice", f.MatchID, f.Role)
			}
			seenFills[key] = true
			m, ok := res.Matches[f.MatchID]
			if !ok {
				switch {
				case f.Seq > res.WALLast:
					fail("client received match %d for seq %d, but the WAL ends at %d: a fill escaped before durability", f.MatchID, f.Seq, res.WALLast)
				case f.Seq > res.BaseSeq:
					fail("client received match %d for seq %d, which replaying the WAL does not produce", f.MatchID, f.Seq)
				}
				continue // at or before replay-check's base: covered by the snapshots it verified
			}
			orderID, clOrd, leaves := m.TakerOrder, m.TakerClOrd, m.TakerLeaves
			if f.Role == proto.RoleMaker {
				orderID, clOrd, leaves = m.MakerOrder, m.MakerClOrd, m.MakerLeaves
			}
			if f.Price != int64(m.Price) || f.Qty != int64(m.Qty) || f.OrderID != uint64(orderID) ||
				f.ClOrdID != uint64(clOrd) || f.Leaves != int64(leaves) || f.Seq != uint64(m.Seq) {
				fail("client %d's fill %+v differs from the replay's match %+v", rf.client, f, m)
			}
			totals.matchesCompared++
		}

		// 4. Recovery as the engine does it (latest snapshot + WAL tail)
		//    equals replay-check's replay of the WAL from its base.
		eng, wl, recovered, err := snapshot.Recover(dir, wal.Options{Log: quiet}, quiet)
		if err != nil {
			fail("recover: %v", err)
		}
		if recovered.WAL.TornBytes > 0 {
			totals.tornTails++
		}
		if info.FirstSeq > 1 {
			totals.walPruned = true
		}
		_ = wl.Close()
		if !reflect.DeepEqual(eng.State(), res.End) {
			fail("state recovered from snapshot + WAL differs from replaying the WAL")
		}

		totals.answers += len(rec.answers)
		totals.fills += len(rec.fills)
		allAnswers = append(allAnswers, rec.answers...)
		allFills = append(allFills, rec.fills...)
		prevWALLast = uint64(eng.LastSeq()) // the next run must continue from here

		// 5. Every fifth run, leave a torn tail for the next start: the
		//    first bytes of a record that never finished (shorter than a
		//    record header). Kills alone almost never tear a record, since
		//    a completed write is in the page cache (decision 015).
		if run%5 == 4 && run+1 < runs {
			injectedLast = injectTornTail(t, filepath.Join(dir, "wal"))
			totals.injected++
		}
	}

	// 6. The whole history, from seq 1: the live WAL plus every archived
	//    segment. Every acknowledged command of every run is in it, with
	//    the content the client sent, and every fill any client received is
	//    exactly the match a replay from an empty engine produces.
	cmds, matches := replayHistory(t, archive, dir)
	for _, a := range allAnswers {
		c, ok := cmds[a.seq]
		if !ok {
			t.Fatalf("acknowledged seq %d is missing from the full history", a.seq)
		}
		if c.Kind != a.kind || uint32(c.Client) != a.client ||
			(a.kind == engine.CmdNewOrder && uint64(c.ClOrdID) != a.clOrd) ||
			(a.kind == engine.CmdCancel && uint64(c.OrderID) != a.orderID) {
			t.Fatalf("history record %d is %+v, but the client was answered for %+v", a.seq, c, a)
		}
	}
	for _, rf := range allFills {
		m, ok := matches[rf.fill.MatchID]
		if !ok {
			t.Fatalf("client %d received match %d, which replaying the full history does not produce", rf.client, rf.fill.MatchID)
		}
		if err := sameFill(rf.fill, m); err != nil {
			t.Fatalf("client %d: %v", rf.client, err)
		}
	}
	t.Logf("whole history: %d commands from seq 1; all %d acknowledged commands and all %d received fills checked against it",
		len(cmds), len(allAnswers), len(allFills))
	if totals.injected > 0 && totals.tornTails < totals.injected {
		t.Fatalf("%d torn tails injected, %d recovered", totals.injected, totals.tornTails)
	}
	t.Logf("%d kill-and-recover runs: %d acknowledged commands (%d checked against the WAL, the rest pruned behind snapshots), "+
		"%d fills received (%d compared with the replay), newest snapshot rebuilt from the WAL in %d runs, "+
		"torn WAL tails recovered in %d runs, WAL pruned: %v, final seq %d",
		runs, totals.answers, totals.checkedInWAL, totals.fills, totals.matchesCompared, totals.snapshotsCompared,
		totals.tornTails, totals.walPruned, prevWALLast)
	if totals.answers == 0 || totals.fills == 0 {
		t.Fatal("no traffic: the harness didn't exercise anything")
	}
}

// injectTornTail appends the first bytes of a record header to the newest
// WAL segment, as a write cut short by a crash would leave them, and
// returns how many.
func injectTornTail(t *testing.T, walDir string) int {
	t.Helper()
	segs, err := filepath.Glob(filepath.Join(walDir, "*.log"))
	if err != nil || len(segs) == 0 {
		t.Fatalf("no WAL segment to tear: %v", err)
	}
	f, err := os.OpenFile(segs[len(segs)-1], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	torn := []byte{0x30, 0x00, 0x00} // a length field, unfinished
	if _, err := f.Write(torn); err != nil {
		t.Fatal(err)
	}
	return len(torn)
}

// replayHistory copies the archived and live WAL segments into one
// directory, replays all of it from seq 1 into an empty engine, and returns
// every command by seq and every match the replay produced.
func replayHistory(t *testing.T, archive, dataDir string) (map[uint64]engine.Command, map[uint64]snapshot.Match) {
	t.Helper()
	full := t.TempDir()
	for _, src := range []string{filepath.Join(archive, "wal"), filepath.Join(dataDir, "wal")} {
		segs, _ := filepath.Glob(filepath.Join(src, "*.log"))
		for _, p := range segs {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(full, filepath.Base(p)), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	cmds := map[uint64]engine.Command{}
	matches := map[uint64]snapshot.Match{}
	eng := engine.New()
	var evs []engine.Event
	var taker engine.Event
	_, info, err := wal.Open(full, wal.Options{ReadOnly: true}, func(r wal.Record) error {
		var c engine.Command
		if err := engine.DecodeCommand(r.Payload, &c); err != nil {
			return err
		}
		c.Seq, c.Time = engine.Seq(r.Seq), engine.Timestamp(r.Time)
		cmds[r.Seq] = c
		evs = eng.Step(&c, evs[:0])
		for _, ev := range evs {
			if ev.Kind != engine.EvFill {
				continue
			}
			if ev.Role == engine.RoleTaker {
				taker = ev
				continue
			}
			matches[ev.MatchID] = snapshot.MatchOf(taker, ev)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("replaying the full history: %v", err)
	}
	if info.FirstSeq != 1 {
		t.Fatalf("the full history starts at seq %d, not 1: a pruned segment wasn't archived", info.FirstSeq)
	}
	return cmds, matches
}

// sameFill reports how a fill a client received differs from the match.
func sameFill(f proto.Fill, m snapshot.Match) error {
	orderID, clOrd, leaves := m.TakerOrder, m.TakerClOrd, m.TakerLeaves
	if f.Role == proto.RoleMaker {
		orderID, clOrd, leaves = m.MakerOrder, m.MakerClOrd, m.MakerLeaves
	}
	if f.Price != int64(m.Price) || f.Qty != int64(m.Qty) || f.OrderID != uint64(orderID) ||
		f.ClOrdID != uint64(clOrd) || f.Leaves != int64(leaves) || f.Seq != uint64(m.Seq) {
		return fmt.Errorf("fill %+v differs from the replay's match %+v", f, m)
	}
	return nil
}
