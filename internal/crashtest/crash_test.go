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
			if r == 6 {
				o.Type, o.TIF, o.Price = proto.TypeMarket, 0, 0
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
func startEngine(t *testing.T, dir, policy string, segBytes int) (*exec.Cmd, string, *strings.Builder) {
	t.Helper()
	cmd := exec.Command(engineBin, "-data", dir, "-listen", "127.0.0.1:0", "-fsync", policy,
		"-snapshot-every", "200", "-snapshot-interval", "20ms", "-idle-timeout", "0",
		"-wal-segment-bytes", fmt.Sprint(segBytes),
		"-instrument", "XYZ:1:1", "-instrument", "ABC:1:1")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	logs := &strings.Builder{}
	var logMu sync.Mutex // guards logs (written by the scanner goroutine)
	addrCh := make(chan string, 1)
	go func() { // scanner: forwards the address, keeps the log
		sc := bufio.NewScanner(stderr)
		sent := false
		for sc.Scan() {
			logMu.Lock()
			logs.WriteString(sc.Text() + "\n")
			logMu.Unlock()
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
	dir := t.TempDir()
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
	}

	for run := range runs {
		policy := policies[run%len(policies)]
		// Odd runs: 64 KiB WAL segments, so kills land during segment rolls
		// and pruning. Even runs: 64 MiB, so nothing is pruned and every
		// acknowledged command can be checked against the WAL directly.
		segBytes := 64 << 20
		if run%2 == 1 {
			segBytes = 64 << 10
		}
		cmd, addr, logs := startEngine(t, dir, policy, segBytes)

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
		prevWALLast = uint64(eng.LastSeq()) // the next run must continue from here
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
