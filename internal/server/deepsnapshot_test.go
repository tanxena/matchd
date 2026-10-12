package server

import (
	"net"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/proto"
)

// seedDeep rests n buy levels (prices 1..n) and n sell levels (prices
// 100001..100000+n) on instrument 1, one lot each, sending in batches small
// enough for the trader's own queue.
func seedDeep(t *testing.T, s *testServer, n int) {
	t.Helper()
	trader := dial(t, s, 50)
	const batch = 32
	sent := 0
	for i := range 2 * n {
		side, px := proto.SideBuy, int64(1+i/2)
		if i%2 == 1 {
			side, px = proto.SideSell, int64(100001+i/2)
		}
		if err := trader.NewOrder(limit(1, uint64(i+1), side, px, 1)); err != nil {
			t.Fatal(err)
		}
		if sent++; sent == batch || i == 2*n-1 {
			for _, m := range recv(t, trader, sent) {
				if m.Type != proto.TypeAck {
					t.Fatalf("seeding: got %v", m)
				}
			}
			sent = 0
		}
	}
}

// A book far deeper than the session queue (decision 016): the writer
// streams the snapshot straight from the publisher's copy, so a subscriber
// that reads gets every level, then live data, and nobody is disconnected.
func TestDeepSnapshotBeyondQueue(t *testing.T) {
	const levels = 2000 // per side: 4,000 levels against a 64-event queue
	s := start(t, func(c *Config) { c.Gateway.OutQueue = 64 })
	seedDeep(t, s, levels)

	sub := subscribe(t, s, 0, 1, nil) // market data needs no login
	sub.waitFor(t, 2*levels)
	_, _, bids, asks, st := sub.snapshot()
	if len(bids) != levels || len(asks) != levels || bids[0].Price != levels || asks[0].Price != 100001 ||
		bids[levels-1].Price != 1 || asks[levels-1].Price != 100000+levels {
		t.Fatalf("snapshot: %d bids (best %v), %d asks (best %v)", len(bids), bids[0], len(asks), asks[0])
	}
	if st.Snapshots != 1 || st.Gaps != 0 {
		t.Fatalf("feed stats %+v", st)
	}

	// Live data continues after the snapshot: a sell crossing the best bid
	// prints a trade (md 4001) and removes the level (md 4002).
	trader := dial(t, s, 51)
	if err := trader.NewOrder(limit(1, 1, proto.SideSell, levels, 1)); err != nil {
		t.Fatal(err)
	}
	sub.waitFor(t, 2*levels+2)
	_, _, bids, _, st = sub.snapshot()
	if len(bids) != levels-1 || bids[0].Price != levels-1 || st.Trades != 1 {
		t.Fatalf("after the trade: %d bids, best %v; stats %+v", len(bids), bids[0], st)
	}
	if ps := s.Publisher().Stats(); ps.SlowConsumers != 0 {
		t.Fatalf("publisher stats %+v", ps)
	}
}

// A client that subscribes to a deep book and never reads can't take the
// snapshot: a socket write blocks past WriteTimeout and the session is
// closed. Trading carries on.
func TestDeepSnapshotUnreadDisconnects(t *testing.T) {
	s := start(t, func(c *Config) {
		c.Gateway.OutQueue = 64
		c.Gateway.SocketWriteBuffer = 4 << 10
		c.Gateway.WriteTimeout = 300 * time.Millisecond
	})
	seedDeep(t, s, 4000) // 8,000 levels, about 216 KB of frames

	nc, err := net.DialTimeout("tcp", s.Addr(), wait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nc.Close() }()
	_ = nc.(*net.TCPConn).SetReadBuffer(4 << 10)
	if _, err := nc.Write(proto.AppendSubscribe(nil, &proto.Subscribe{Instrument: 1})); err != nil {
		t.Fatal(err)
	}
	// ...and never reads.
	waitForLog(t, s, "write error during snapshot")

	trader := dial(t, s, 51)
	if err := trader.NewOrder(limit(1, 1, proto.SideBuy, 50000, 1)); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, trader, 1)[0]; m.Type != proto.TypeAck {
		t.Fatalf("trader got %v", m)
	}
}
