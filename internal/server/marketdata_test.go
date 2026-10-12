package server

import (
	"errors"
	"math/rand/v2"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/proto"
)

// subscriber runs a Feed on its own connection until stopped.
type subscriber struct {
	feed *client.Feed
	conn *client.Conn
	mu   sync.Mutex // guards feed between the reader goroutine and the test
	done chan error // the reader's exit reason
}

func subscribe(t *testing.T, s *testServer, id uint32, inst uint32, drop func(client.Msg) bool) *subscriber {
	t.Helper()
	conn := dial(t, s, id)
	sub := &subscriber{conn: conn, done: make(chan error, 1)}
	sub.mu.Lock()
	f, err := client.NewFeed(conn, inst)
	if err != nil {
		t.Fatal(err)
	}
	f.Drop = drop
	sub.feed = f
	sub.mu.Unlock()
	go func() { // reader: applies every message until the connection closes
		for {
			m, err := conn.Next()
			if err != nil {
				sub.done <- err
				return
			}
			sub.mu.Lock()
			err = sub.feed.Apply(m)
			sub.mu.Unlock()
			if err != nil {
				sub.done <- err
				return
			}
		}
	}()
	return sub
}

func (s *subscriber) snapshot() (md uint64, live bool, bids, asks []client.Level, st client.FeedStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bids, asks = s.feed.Book()
	return s.feed.MDSeq(), s.feed.Live(), bids, asks, s.feed.Stats
}

// waitFor waits until the subscriber is live at md_seq >= md.
func (s *subscriber) waitFor(t *testing.T, md uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, live, _, _, st := s.snapshot()
		if live && got >= md {
			return
		}
		select {
		case err := <-s.done:
			t.Fatalf("subscriber's reader stopped at md_seq %d (want %d): %v; stats %+v", got, md, err, st)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscriber stuck at md_seq %d (live %v), want %d; stats %+v", got, live, md, st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// engineLevels is the engine's own book for instrument id, as levels.
func engineLevels(e *engine.Engine, id engine.InstrumentID) (bids, asks []client.Level) {
	st := e.State().Instruments[id-1]
	conv := func(orders []book.RestingOrder) []client.Level {
		var out []client.Level
		for _, r := range orders {
			if n := len(out); n > 0 && out[n-1].Price == int64(r.Price) {
				out[n-1].Qty += int64(r.Qty)
			} else {
				out = append(out, client.Level{Price: int64(r.Price), Qty: int64(r.Qty)})
			}
		}
		return out
	}
	return conv(st.Bids), conv(st.Asks)
}

// load runs two traders sending n orders each (rests, crosses, cancels)
// and waits for every answer.
func load(t *testing.T, s *testServer, n int, seed uint64, onHalf func()) {
	t.Helper()
	var wg sync.WaitGroup // the traders
	half := make(chan struct{})
	var once sync.Once
	for c := range uint32(2) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := client.Dial(s.Addr(), 100+c, wait)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			rng := rand.New(rand.NewPCG(seed, uint64(c)))
			acked := []uint64{}
			for i := range n {
				if i == n/2 {
					once.Do(func() { close(half) })
				}
				if len(acked) > 0 && rng.IntN(5) == 0 {
					_ = conn.Cancel(proto.Cancel{Instrument: 1, OrderID: acked[rng.IntN(len(acked))]})
				} else {
					side := proto.SideBuy
					if rng.IntN(2) == 0 {
						side = proto.SideSell
					}
					_ = conn.NewOrder(limit(1, uint64(seed*100000)+uint64(i), side, int64(95+rng.IntN(11)), int64(1+rng.IntN(5))))
				}
				// Wait for this command's answer, so the book evolves steadily.
				_ = conn.SetReadDeadline(time.Now().Add(wait))
				for {
					m, err := conn.Next()
					if err != nil {
						t.Errorf("trader %d: %v", c, err)
						return
					}
					if m.Type == proto.TypeAck {
						acked = append(acked, m.Ack.OrderID)
						break
					}
					if m.Type == proto.TypeReject || (m.Type == proto.TypeCancelAck && m.CancelAck.Reason == proto.CancelRequested) {
						break
					}
				}
			}
		}()
	}
	go func() { <-half; onHalf() }()
	wg.Wait()
}

// Acceptance 4: a subscriber that joins mid-stream receives a snapshot,
// detects induced gaps, resends, and converges to the same book as a
// subscriber that was there from the start, and as the engine.
func TestMarketDataConvergence(t *testing.T) {
	s := start(t, nil)
	fromStart := subscribe(t, s, 1, 1, nil)

	dropped := map[uint64]bool{} // md_seq values already dropped once (resends must get through)
	var lateJoiner *subscriber
	load(t, s, 1500, 1, func() {
		lateJoiner = subscribe(t, s, 2, 1, func(m client.Msg) bool {
			md := m.BookDelta.MDSeq + m.Trade.MDSeq // Drop sees only deltas and trades; one of these is set
			if md%37 == 0 && !dropped[md] {
				dropped[md] = true // lose this frame once, as a network would
				return true
			}
			return false
		})
	})

	// A third subscriber joining now gets a snapshot at the final md_seq.
	probe := subscribe(t, s, 3, 1, nil)
	probe.waitFor(t, 1)
	final, _, _, _, _ := probe.snapshot()
	fromStart.waitFor(t, final)
	lateJoiner.waitFor(t, final)

	mdA, _, bidsA, asksA, stA := fromStart.snapshot()
	mdB, _, bidsB, asksB, stB := lateJoiner.snapshot()
	_, _, bidsC, asksC, _ := probe.snapshot()
	if mdA != final || mdB != final {
		t.Fatalf("md_seq: from-start %d, late joiner %d, probe %d", mdA, mdB, final)
	}
	if !reflect.DeepEqual(bidsA, bidsB) || !reflect.DeepEqual(asksA, asksB) {
		t.Fatalf("late joiner's book differs from the from-start subscriber's:\n A bids %v asks %v\n B bids %v asks %v", bidsA, asksA, bidsB, asksB)
	}
	if !reflect.DeepEqual(bidsA, bidsC) || !reflect.DeepEqual(asksA, asksC) {
		t.Fatal("a fresh snapshot differs from the incrementally built book")
	}
	if stB.Gaps == 0 || stB.Resends == 0 || stB.Snapshots != 1 {
		t.Fatalf("late joiner stats %+v: the test didn't exercise gap recovery", stB)
	}
	if stA.Gaps != 0 || stA.Snapshots != 1 {
		t.Fatalf("from-start subscriber stats %+v: it should never have had a gap", stA)
	}
	_ = s.Close()
	bidsE, asksE := engineLevels(s.Engine(), 1)
	if !reflect.DeepEqual(bidsA, bidsE) || !reflect.DeepEqual(asksA, asksE) {
		t.Fatalf("subscribers' book differs from the engine's:\n md   bids %v asks %v\n eng  bids %v asks %v", bidsA, asksA, bidsE, asksE)
	}
	if st := s.Engine().State().Instruments[0].MDSeq; st != final {
		t.Fatalf("engine md_seq %d, subscribers %d", st, final)
	}
	t.Logf("final md_seq %d; late joiner: %d gaps, %d resends, %d duplicates ignored, %d applied; levels %d bids / %d asks",
		final, stB.Gaps, stB.Resends, stB.Duplicates, stB.Applied, len(bidsA), len(asksA))
}

// A gap older than the ring: the resend is refused, the Feed subscribes
// again, and still converges.
func TestResendBeyondRingResnapshots(t *testing.T) {
	s := start(t, func(c *Config) { c.MDRing = 16 })
	fromStart := subscribe(t, s, 1, 1, nil)
	var lateJoiner *subscriber
	dropping := 0
	load(t, s, 600, 2, func() {
		lateJoiner = subscribe(t, s, 2, 1, func(m client.Msg) bool {
			// Lose 40 consecutive messages once: more than the ring holds.
			if dropping < 40 && (m.Type == proto.TypeBookDelta || m.Type == proto.TypeTrade) {
				md := m.BookDelta.MDSeq + m.Trade.MDSeq
				if md > 0 {
					dropping++
					return true
				}
			}
			return false
		})
	})
	probe := subscribe(t, s, 3, 1, nil)
	probe.waitFor(t, 1)
	final, _, _, _, _ := probe.snapshot()
	fromStart.waitFor(t, final)
	lateJoiner.waitFor(t, final)
	_, _, bidsA, asksA, _ := fromStart.snapshot()
	_, _, bidsB, asksB, stB := lateJoiner.snapshot()
	if !reflect.DeepEqual(bidsA, bidsB) || !reflect.DeepEqual(asksA, asksB) {
		t.Fatal("re-snapshotted subscriber's book differs from the from-start subscriber's")
	}
	if stB.Resnapshots == 0 || s.Publisher().Stats().ResendMisses == 0 {
		t.Fatalf("stats %+v, publisher %+v: the gap didn't exceed the ring", stB, s.Publisher().Stats())
	}
	t.Logf("late joiner %+v", stB)
}

func TestSubscribeUnknownInstrument(t *testing.T) {
	s := start(t, nil)
	c := dial(t, s, 1)
	_ = c.Subscribe(proto.Subscribe{Instrument: 99})
	if m := recv(t, c, 1)[0]; m.Type != proto.TypeReject || m.Reject.Reason != proto.ReasonUnknownInstrument {
		t.Fatalf("got %v", m)
	}
}

// After a restart, a new subscriber sees the recovered book, and md_seq
// continues where it left off.
func TestMarketDataAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s1 := start(t, func(c *Config) { c.DataDir = dir; c.SnapshotEvery = 100 })
	load(t, s1, 300, 3, func() {})
	_ = s1.Close()
	wantMD := s1.Engine().State().Instruments[0].MDSeq
	bidsE, asksE := engineLevels(s1.Engine(), 1)

	s2 := start(t, func(c *Config) { c.DataDir = dir; c.SnapshotEvery = 100 })
	sub := subscribe(t, s2, 1, 1, nil)
	sub.waitFor(t, wantMD)
	md, _, bids, asks, _ := sub.snapshot()
	if md != wantMD || !reflect.DeepEqual(bids, bidsE) || !reflect.DeepEqual(asks, asksE) {
		t.Fatalf("after restart: md_seq %d (want %d), book differs: %v %v vs %v %v", md, wantMD, bids, asks, bidsE, asksE)
	}
	// The next market-data event continues the numbering.
	tr := dial(t, s2, 50)
	_ = tr.NewOrder(limit(1, 1, proto.SideBuy, 50, 1))
	recv(t, tr, 1)
	sub.waitFor(t, wantMD+1)
}

// Market data is held back until its command is durable, like everything else.
func TestMarketDataWaitsForFsync(t *testing.T) {
	g := &gate{}
	s := start(t, func(c *Config) { c.Fsync = policy(t, "group:1ms"); c.WAL = gatedWAL(g) })
	sub := subscribe(t, s, 1, 1, nil)
	sub.waitFor(t, 0)
	tr := dial(t, s, 2)
	g.hold()
	t.Cleanup(g.open)
	_ = tr.NewOrder(limit(1, 1, proto.SideBuy, 50, 3))
	time.Sleep(300 * time.Millisecond)
	if md, _, _, _, _ := sub.snapshot(); md != 0 {
		t.Fatalf("market data md_seq %d reached a subscriber before its fsync", md)
	}
	g.open()
	sub.waitFor(t, 1)
}

// A market-data subscriber that stops reading is disconnected; trading goes on.
func TestSlowMarketDataSubscriber(t *testing.T) {
	s := start(t, func(c *Config) {
		c.Gateway.OutQueue = 64
		c.Gateway.SocketWriteBuffer = 4 << 10
		c.Gateway.WriteTimeout = 0
	})
	nc, err := net.DialTimeout("tcp", s.Addr(), wait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nc.Close() }()
	_ = nc.(*net.TCPConn).SetReadBuffer(4 << 10)
	if _, err := nc.Write(proto.AppendSubscribe(nil, &proto.Subscribe{Instrument: 1})); err != nil {
		t.Fatal(err)
	}
	// ...and never reads. Meanwhile a trader generates plenty of market data.
	deadline := time.Now().Add(20 * time.Second)
	trader := dial(t, s, 2)
	for i := 0; s.Publisher().Stats().SlowConsumers == 0; i++ {
		if time.Now().After(deadline) {
			t.Fatalf("slow subscriber never disconnected: %+v", s.Publisher().Stats())
		}
		_ = trader.NewOrder(limit(1, uint64(i+1), proto.SideBuy, int64(10+i%50), 1))
		if m := recv(t, trader, 1)[0]; m.Type != proto.TypeAck {
			t.Fatalf("trader got %v", m)
		}
	}
	waitForLog(t, s, "slow market-data consumer disconnected")
	_ = nc.SetDeadline(time.Now().Add(wait))
	buf := make([]byte, 64<<10)
	for {
		if _, err := nc.Read(buf); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatal("server did not close the slow subscriber")
			}
			break
		}
	}
}
