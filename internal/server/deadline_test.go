package server

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/proto"
)

// Review F-001: a session idle for longer than WriteTimeout, then sent a
// burst bigger than the writer's 32 KiB buffer, must not be cut off. The
// buffer flushes itself to the socket when full; if only explicit flushes
// set the write deadline, that implicit flush runs under a deadline set
// long ago, already expired, and fails at once as a "slow consumer".
//
// GOMAXPROCS 1 makes the burst land in the queue before the writer runs:
// the publisher releases the whole fill burst in one go after group
// commit, so the writer then writes more than 32 KiB before its first
// explicit flush.
func TestIdleThenBurstIsNotASlowConsumer(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	const makerOrders = 600
	s := start(t, func(c *Config) {
		c.Fsync = policy(t, "group:150ms")
		c.Gateway.WriteTimeout = 300 * time.Millisecond
		c.Gateway.IdleTimeout = 0
	})
	maker, taker := dial(t, s, 1), dial(t, s, 2)
	for i := range makerOrders {
		if err := maker.NewOrder(limit(1, uint64(i+1), proto.SideSell, 100, 1)); err != nil {
			t.Fatal(err)
		}
	}
	recv(t, maker, makerOrders)                                                // the acks
	if err := taker.NewOrder(limit(1, 1, proto.SideBuy, 100, 1)); err != nil { // one fill: both sessions write once
		t.Fatal(err)
	}
	recv(t, taker, 2)
	recv(t, maker, 1)

	time.Sleep(700 * time.Millisecond) // both sessions idle well past WriteTimeout

	// A market buy for the rest: 599 matches, each a fill to both sides.
	m := proto.NewOrder{Instrument: 1, ClOrdID: 2, Side: proto.SideBuy, Type: proto.TypeMarket, Qty: makerOrders - 1}
	if err := taker.NewOrder(m); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	got := make([]int, 2)
	for i, c := range []struct {
		n int
	}{{makerOrders - 1}, {1 + makerOrders - 1}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := maker
			if i == 1 {
				conn = taker
			}
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			for got[i] < c.n {
				if _, err := conn.Next(); err != nil {
					return
				}
				got[i]++
			}
		}()
	}
	wg.Wait()
	if got[0] != makerOrders-1 || got[1] != makerOrders || slowConsumers(s) != 0 {
		t.Fatalf("maker got %d of %d fills, taker %d of %d messages; slow consumers %d (%+v)",
			got[0], makerOrders-1, got[1], makerOrders, slowConsumers(s), s.Gateway().Stats())
	}
}
