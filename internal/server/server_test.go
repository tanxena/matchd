package server

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/gateway"
	"github.com/tanxena/matchd/internal/proto"
)

// End-to-end tests: real TCP over loopback, the real pipeline.

const wait = 5 * time.Second

// syncBuffer is a log sink safe for concurrent writers (session goroutines
// log independently).
type syncBuffer struct {
	mu sync.Mutex // guards b
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type testServer struct {
	*Server
	logs *syncBuffer
}

func start(t *testing.T, tweak func(*Config)) *testServer {
	t.Helper()
	logs := &syncBuffer{}
	gw := gateway.DefaultConfig()
	gw.Addr = "127.0.0.1:0"
	var now engine.Timestamp // fake clock; only the sequencer goroutine calls it
	cfg := Config{
		DataDir: t.TempDir(),
		Gateway: gw,
		Instruments: []book.Instrument{
			{Symbol: "XYZ", TickSize: 1, LotSize: 1},
			{Symbol: "ABC", TickSize: 1, LotSize: 1},
		},
		Clock: func() engine.Timestamp { now += 1000; return now },
		Log:   slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if tweak != nil {
		tweak(&cfg)
	}
	s, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{Server: s, logs: logs}
	t.Cleanup(func() {
		_ = s.Close()
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})
	return ts
}

func dial(t *testing.T, s *testServer, id uint32) *client.Conn {
	t.Helper()
	c, err := client.Dial(s.Addr(), id, wait)
	if err != nil {
		t.Fatalf("dial client %d: %v", id, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// recv reads n messages, failing the test if they don't arrive in time.
func recv(t *testing.T, c *client.Conn, n int) []client.Msg {
	t.Helper()
	out := make([]client.Msg, 0, n)
	_ = c.SetReadDeadline(time.Now().Add(wait))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	for len(out) < n {
		m, err := c.Next()
		if err != nil {
			t.Fatalf("client %d: after %d of %d messages: %v (got %v)", c.ClientID, len(out), n, err, out)
		}
		out = append(out, m)
	}
	return out
}

// quiet asserts nothing more arrives for a short while.
func quiet(t *testing.T, c *client.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	if m, err := c.Next(); err == nil {
		t.Fatalf("client %d: unexpected message %v", c.ClientID, m)
	}
}

func limit(inst uint32, clOrd uint64, side uint8, px, qty int64) proto.NewOrder {
	return proto.NewOrder{Instrument: inst, ClOrdID: clOrd, Side: side, Type: proto.TypeLimit, TIF: proto.TIFGTC, Price: px, Qty: qty}
}

func TestOrderAckFillOverTCP(t *testing.T) {
	s := start(t, nil)
	seller, buyer := dial(t, s, 1), dial(t, s, 2)

	if err := seller.NewOrder(limit(1, 501, proto.SideSell, 100, 5)); err != nil {
		t.Fatal(err)
	}
	ack := recv(t, seller, 1)[0]
	if ack.Type != proto.TypeAck || ack.Ack.OrderID != 1 || ack.Ack.ClOrdID != 501 || ack.Ack.Qty != 5 || ack.Ack.Seq != 3 {
		// seq 1 and 2 were the two instruments
		t.Fatalf("seller ack: %v", ack)
	}

	if err := buyer.NewOrder(limit(1, 701, proto.SideBuy, 101, 3)); err != nil {
		t.Fatal(err)
	}
	bm := recv(t, buyer, 2)
	if bm[0].Type != proto.TypeAck || bm[0].Ack.OrderID != 2 {
		t.Fatalf("buyer ack: %v", bm[0])
	}
	f := bm[1].Fill
	if bm[1].Type != proto.TypeFill || f.Role != proto.RoleTaker || f.Price != 100 || f.Qty != 3 || f.Leaves != 0 ||
		f.ClOrdID != 701 || f.Side != proto.SideBuy {
		t.Fatalf("buyer fill: %v", bm[1])
	}
	mf := recv(t, seller, 1)[0].Fill
	if mf.Role != proto.RoleMaker || mf.MatchID != f.MatchID || mf.Price != 100 || mf.Qty != 3 || mf.Leaves != 2 ||
		mf.ClOrdID != 501 || mf.Side != proto.SideSell || mf.Seq != f.Seq {
		t.Fatalf("seller fill: %+v vs taker %+v", mf, f)
	}
	quiet(t, seller)
	quiet(t, buyer)
}

func TestCancelAndEngineRejects(t *testing.T) {
	s := start(t, nil)
	a, b := dial(t, s, 1), dial(t, s, 2)
	_ = a.NewOrder(limit(2, 1, proto.SideBuy, 50, 4))
	id := recv(t, a, 1)[0].Ack.OrderID

	// Another client can't cancel it; the engine answers (seq > 0).
	_ = b.Cancel(proto.Cancel{Instrument: 2, OrderID: id})
	if r := recv(t, b, 1)[0]; r.Type != proto.TypeReject || r.Reject.Reason != proto.ReasonUnknownOrder || r.Reject.Seq == 0 {
		t.Fatalf("foreign cancel: %v", r)
	}
	// The owner can.
	_ = a.Cancel(proto.Cancel{Instrument: 2, OrderID: id})
	ca := recv(t, a, 1)[0]
	if ca.Type != proto.TypeCancelAck || ca.CancelAck.OrderID != id || ca.CancelAck.Qty != 4 ||
		ca.CancelAck.ClOrdID != 1 || ca.CancelAck.Reason != proto.CancelRequested || ca.CancelAck.Price != 50 {
		t.Fatalf("cancel ack: %v", ca)
	}
	// Field validation happens in the engine: sequenced rejects.
	bad := []struct {
		m    proto.NewOrder
		want proto.Reason
	}{
		{limit(1, 9, proto.SideBuy, 100, 0), proto.ReasonInvalidQty},
		{limit(1, 9, 7, 100, 1), proto.ReasonInvalidSide},
		{limit(99, 9, proto.SideBuy, 100, 1), proto.ReasonUnknownInstrument},
	}
	for _, c := range bad {
		_ = a.NewOrder(c.m)
		if r := recv(t, a, 1)[0]; r.Type != proto.TypeReject || r.Reject.Reason != c.want || r.Reject.Seq == 0 || r.Reject.ClOrdID != 9 {
			t.Fatalf("order %+v: got %v, want reject %v", c.m, r, c.want)
		}
	}
}

func TestMarketOrderExpiry(t *testing.T) {
	s := start(t, nil)
	mm, taker := dial(t, s, 1), dial(t, s, 2)
	_ = mm.NewOrder(limit(1, 1, proto.SideSell, 100, 2))
	recv(t, mm, 1)
	_ = taker.NewOrder(proto.NewOrder{Instrument: 1, ClOrdID: 5, Side: proto.SideBuy, Type: proto.TypeMarket, Qty: 7})
	msgs := recv(t, taker, 3) // Ack, Fill(2), CancelAck(expired 5)
	if msgs[0].Type != proto.TypeAck || msgs[1].Type != proto.TypeFill || msgs[1].Fill.Leaves != 5 ||
		msgs[2].Type != proto.TypeCancelAck || msgs[2].CancelAck.Reason != proto.CancelExpired || msgs[2].CancelAck.Qty != 5 {
		t.Fatalf("market taker got %v", msgs)
	}
}

// waitForLog waits for substr to appear in the server log. Sessions log their
// close reason just after closing the socket, so a client can observe the
// close before the line is written; reading the log immediately would race.
func waitForLog(t *testing.T, s *testServer, substr string) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !strings.Contains(s.logs.String(), substr) {
		if time.Now().After(deadline) {
			t.Fatalf("server never logged %q", substr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// rawConn is a connection that speaks frames by hand, for testing what a
// misbehaving client gets.
func rawConn(t *testing.T, s *testServer) (net.Conn, *proto.Reader) {
	t.Helper()
	c, err := net.DialTimeout("tcp", s.Addr(), wait)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(wait))
	return c, proto.NewReader(c)
}

// expectReject reads one frame that must be a gateway Reject (seq 0).
func expectReject(t *testing.T, r *proto.Reader, want proto.Reason) {
	t.Helper()
	typ, p, err := r.Next()
	if err != nil {
		t.Fatalf("want Reject %v, got error %v", want, err)
	}
	var rej proto.Reject
	if typ != proto.TypeReject || rej.Decode(p) != nil || rej.Reason != want || rej.Seq != 0 {
		t.Fatalf("want gateway Reject %v, got %v %+v", want, typ, rej)
	}
}

// expectClosed asserts the server closed the connection.
func expectClosed(t *testing.T, r *proto.Reader) {
	t.Helper()
	typ, _, err := r.Next()
	if err == nil {
		t.Fatalf("connection still open: got %v", typ)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("connection not closed by the server (read timed out)")
	}
}

func TestLoginRules(t *testing.T) {
	s := start(t, nil)

	t.Run("order before login", func(t *testing.T) {
		c, r := rawConn(t, s)
		_, _ = c.Write(proto.AppendNewOrder(nil, &proto.NewOrder{Instrument: 1, ClOrdID: 3, Side: 1, Type: 1, TIF: 1, Price: 1, Qty: 1}))
		expectReject(t, r, proto.ReasonNotLoggedIn)
		expectClosed(t, r)
	})
	t.Run("client id 0", func(t *testing.T) {
		c, r := rawConn(t, s)
		_, _ = c.Write(proto.AppendLogin(nil, &proto.Login{ClientID: 0}))
		expectReject(t, r, proto.ReasonInvalidClientID)
		expectClosed(t, r)
	})
	t.Run("second login on one connection", func(t *testing.T) {
		c, r := rawConn(t, s)
		_, _ = c.Write(proto.AppendLogin(nil, &proto.Login{ClientID: 40}))
		if typ, _, err := r.Next(); err != nil || typ != proto.TypeLoginAck {
			t.Fatalf("first login: %v %v", typ, err)
		}
		_, _ = c.Write(proto.AppendLogin(nil, &proto.Login{ClientID: 41}))
		expectReject(t, r, proto.ReasonAlreadyLoggedIn)
		expectClosed(t, r)
	})
	t.Run("client already connected, then reusable", func(t *testing.T) {
		first := dial(t, s, 50)
		_, err := client.Dial(s.Addr(), 50, wait)
		if !errors.Is(err, client.ErrLoginRejected) || !strings.Contains(err.Error(), "ClientInUse") {
			t.Fatalf("second session for client 50: %v", err)
		}
		_ = first.Close()
		// The server notices the close asynchronously; the id frees up then.
		deadline := time.Now().Add(wait)
		for {
			c, err := client.Dial(s.Addr(), 50, wait)
			if err == nil {
				_ = c.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("client 50 never became reusable: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func TestProtocolErrorsCloseOnlyThatConnection(t *testing.T) {
	s := start(t, nil)
	healthy := dial(t, s, 1)
	login := proto.AppendLogin(nil, &proto.Login{ClientID: 77})
	cases := map[string][]byte{
		"wrong version":              {6, 0, 0, 0, proto.Version + 1, byte(proto.TypeLogin), 1, 0, 0, 0},
		"unknown type":               {2, 0, 0, 0, proto.Version, 0x7e},
		"wrong payload size":         {5, 0, 0, 0, proto.Version, byte(proto.TypeLogin), 1, 0, 0},
		"length too large":           {0xff, 0xff, 0xff, 0x7f, proto.Version, byte(proto.TypeNewOrder)},
		"outbound type sent inbound": proto.AppendAck(nil, &proto.Ack{}),
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			c, r := rawConn(t, s)
			_, _ = c.Write(login)
			if typ, _, err := r.Next(); err != nil || typ != proto.TypeLoginAck {
				t.Fatalf("login: %v %v", typ, err)
			}
			_, _ = c.Write(frame)
			expectClosed(t, r)
		})
	}
	// Nobody else noticed.
	_ = healthy.Heartbeat()
	if m := recv(t, healthy, 1)[0]; m.Type != proto.TypeHeartbeat {
		t.Fatalf("healthy client got %v", m)
	}
	waitForLog(t, s, "protocol error")
}

func TestSubscribeUnsupportedAndHeartbeat(t *testing.T) {
	s := start(t, nil)
	c := dial(t, s, 1)
	_ = c.Subscribe(proto.Subscribe{Instrument: 1})
	if m := recv(t, c, 1)[0]; m.Type != proto.TypeReject || m.Reject.Reason != proto.ReasonUnsupported {
		t.Fatalf("subscribe: %v", m)
	}
	_ = c.Heartbeat() // the connection is still usable
	if m := recv(t, c, 1)[0]; m.Type != proto.TypeHeartbeat {
		t.Fatalf("heartbeat: %v", m)
	}
}

func TestIdleTimeout(t *testing.T) {
	s := start(t, func(c *Config) { c.Gateway.IdleTimeout = 200 * time.Millisecond })
	_, r := rawConn(t, s)
	expectClosed(t, r) // sent nothing at all
	waitForLog(t, s, "idle timeout")
}

// Acceptance 5 (the gateway part): a client that stops reading is
// disconnected, and nobody else is held up while that happens.
func TestSlowConsumerDisconnected(t *testing.T) {
	s := start(t, func(c *Config) {
		c.Gateway.OutQueue = 64
		c.Gateway.SocketWriteBuffer = 4 << 10
		c.Gateway.WriteTimeout = 0 // rely on the queue limit alone
		c.Gateway.IdleTimeout = 0
	})
	healthy := dial(t, s, 2)

	// The slow client: logs in, then floods orders that each produce a
	// Reject event, and never reads another byte.
	nc, err := net.DialTimeout("tcp", s.Addr(), wait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nc.Close() }()
	_ = nc.(*net.TCPConn).SetReadBuffer(4 << 10)
	if _, err := nc.Write(proto.AppendLogin(nil, &proto.Login{ClientID: 1})); err != nil {
		t.Fatal(err)
	}
	if typ, _, err := proto.NewReader(nc).Next(); err != nil || typ != proto.TypeLoginAck {
		t.Fatalf("slow client login: %v %v", typ, err)
	}
	floodDone := make(chan struct{}) // closed when the flood goroutine stops
	go func() {
		defer close(floodDone)
		var batch []byte
		for range 100 {
			batch = proto.AppendNewOrder(batch, &proto.NewOrder{Instrument: 1, ClOrdID: 1, Side: proto.SideBuy, Type: proto.TypeLimit, TIF: proto.TIFGTC, Price: 1, Qty: 0})
		}
		for range 20_000 { // up to 2M orders; stops at the first write error (disconnected)
			if _, err := nc.Write(batch); err != nil {
				return
			}
		}
	}()

	// Meanwhile the healthy client keeps trading; time each round trip.
	var worst time.Duration
	deadline := time.Now().Add(20 * time.Second)
	for i := 0; s.Publisher().Stats().SlowConsumers == 0; i++ {
		if time.Now().After(deadline) {
			t.Fatalf("slow client never disconnected; publisher stats %+v", s.Publisher().Stats())
		}
		t0 := time.Now()
		_ = healthy.NewOrder(limit(2, uint64(i+1), proto.SideBuy, 10, 1))
		if m := recv(t, healthy, 1)[0]; m.Type != proto.TypeAck {
			t.Fatalf("healthy client got %v", m)
		}
		worst = max(worst, time.Since(t0))
	}
	t.Logf("healthy client's worst round trip while the slow client filled its queue: %v", worst)

	// The slow client's connection is closed by the server. Reading what's
	// left ends in EOF, or in an error when the server closed with data
	// still unread (reset on Linux; "connection aborted" or "forcibly
	// closed" on Windows). The only outcome that means "not closed" is
	// timing out.
	_ = nc.SetDeadline(time.Now().Add(wait))
	if _, err := io.Copy(io.Discard, nc); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatal("server did not close the slow client's connection")
		}
	}
	<-floodDone
	waitForLog(t, s, "slow consumer")
	// And the healthy client is still fine afterwards.
	_ = healthy.Heartbeat()
	if m := recv(t, healthy, 1)[0]; m.Type != proto.TypeHeartbeat {
		t.Fatalf("healthy client after: %v", m)
	}
}

// Many clients at once. Every order gets exactly one answer; order ids are
// gap-free across all clients; every match is reported to exactly two
// parties. Run with -race.
func TestManyConcurrentClients(t *testing.T) {
	s := start(t, nil)
	const clients, orders = 8, 400
	type result struct {
		answers int
		acks    []uint64
		matches map[uint64]int
	}
	results := make([]result, clients)
	var wg sync.WaitGroup // one goroutine per client
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := dial(t, s, uint32(i+1))
			res := result{matches: map[uint64]int{}}
			// Reader goroutine: counts answers until it has one per order,
			// then waits for stray maker fills to settle.
			got := make(chan struct{}) // closed when every order is answered
			go func() {
				for {
					_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
					m, err := c.Next()
					if err != nil {
						if res.answers < orders {
							t.Errorf("client %d: %v after %d answers", i+1, err, res.answers)
						}
						close(got)
						return
					}
					switch m.Type {
					case proto.TypeAck:
						res.answers++
						res.acks = append(res.acks, m.Ack.OrderID)
					case proto.TypeReject:
						res.answers++
					case proto.TypeFill:
						res.matches[m.Fill.MatchID]++
					}
				}
			}()
			for j := range orders {
				side := proto.SideBuy
				if (i+j)%2 == 0 {
					side = proto.SideSell
				}
				px := int64(100 + (i*7+j*13)%9 - 4) // a band around 100: plenty of crossing
				if err := c.NewOrder(limit(1, uint64(j+1), side, px, int64(1+j%5))); err != nil {
					t.Errorf("client %d send: %v", i+1, err)
					return
				}
			}
			<-got
			results[i] = res
		}()
	}
	wg.Wait()

	total := 0
	seen := map[uint64]bool{}
	matches := map[uint64]int{}
	for i, r := range results {
		if r.answers != orders {
			t.Errorf("client %d: %d answers for %d orders", i+1, r.answers, orders)
		}
		total += len(r.acks)
		for _, id := range r.acks {
			if seen[id] {
				t.Fatalf("order id %d acked twice", id)
			}
			seen[id] = true
		}
		for m, n := range r.matches {
			matches[m] += n
		}
	}
	for id := uint64(1); id <= uint64(total); id++ {
		if !seen[id] {
			t.Fatalf("order ids not gap-free: %d missing of 1..%d", id, total)
		}
	}
	for m, n := range matches {
		if n != 2 {
			t.Fatalf("match %d reported to %d parties, want 2", m, n)
		}
	}
	if len(matches) == 0 {
		t.Fatal("no matches happened; the test isn't exercising fills")
	}
	t.Logf("%d clients x %d orders: %d acks, %d matches", clients, orders, total, len(matches))
}
