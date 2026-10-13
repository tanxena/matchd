package server

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/proto"
	"github.com/tanxena/matchd/internal/wal"
)

// stallFile delays one fsync, once armed: a disk that stalls.
type stallFile struct {
	wal.File
	stall *atomic.Int64 // nanoseconds to sleep in the next Sync; 0 = none
}

func (f stallFile) Sync() error {
	if d := f.stall.Swap(0); d > 0 {
		time.Sleep(time.Duration(d))
	}
	return f.File.Sync()
}

// Decision 022: a durability stall under load holds every answer back,
// then releases them at once. Clients that keep reading take the burst,
// far above their queue's soft limit, without being disconnected as slow
// consumers; the stall shows in /metrics.
func TestDurabilityStallBurstDoesNotDisconnect(t *testing.T) {
	const soft = 32
	var stall atomic.Int64
	s := start(t, func(c *Config) {
		withHTTP(c)
		c.Fsync = policy(t, "group:1ms")
		c.Gateway.OutQueue = soft
		c.Gateway.WriteTimeout = 2 * time.Second
		c.WAL = wal.Options{OpenFile: func(path string) (wal.File, error) {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			return stallFile{File: f, stall: &stall}, err
		}}
	})

	// A market-data subscriber that reads everything: every resting order
	// sends it a delta, so a stall's backlog lands in its queue too.
	sub, err := client.Dial(s.Addr(), 0, wait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if err := sub.Subscribe(proto.Subscribe{Instrument: 1}); err != nil {
		t.Fatal(err)
	}
	var subMsgs atomic.Int64
	go func() {
		for {
			if _, err := sub.Next(); err != nil {
				return
			}
			subMsgs.Add(1)
		}
	}()

	// Traders pipeline resting orders as fast as they can while reading
	// every answer.
	const traders, perTrader = 3, 6000
	var wg sync.WaitGroup
	var acks atomic.Int64
	halfway := make(chan struct{})
	var once sync.Once
	for c := range uint32(traders) {
		conn := dial(t, s, 10+c)
		wg.Add(2)
		go func() { // reader
			defer wg.Done()
			for n := 0; n < perTrader; {
				m, err := conn.Next()
				if err != nil {
					t.Errorf("trader %d: %v after %d answers", c, err, n)
					_ = conn.Close() // unblock the writer
					return
				}
				if m.Type == proto.TypeAck {
					n++
					acks.Add(1)
				}
			}
		}()
		go func() { // writer
			defer wg.Done()
			defer once.Do(func() { close(halfway) }) // even if it fails early
			for i := range perTrader {
				if i == perTrader/3 {
					once.Do(func() { close(halfway) })
				}
				px := int64(10 + i%40)
				if err := conn.NewOrder(limit(1, uint64(i+1), proto.SideBuy, px, 1)); err != nil {
					t.Errorf("trader %d send: %v", c, err)
					return
				}
			}
		}()
	}
	<-halfway
	stall.Store(int64(50 * time.Millisecond)) // the next fsync takes 50 ms longer
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatalf("load didn't finish: %d of %d acks; stats %+v %+v", acks.Load(), traders*perTrader, s.Publisher().Stats(), s.Gateway().Stats())
	}

	if got := acks.Load(); got != traders*perTrader {
		t.Fatalf("%d acks, want %d", got, traders*perTrader)
	}
	gs, ps := s.Gateway().Stats(), s.Publisher().Stats()
	t.Logf("peak queued events in one session: %d (soft limit %d); subscriber got %d messages", gs.PeakQueued, soft, subMsgs.Load())
	if n := slowConsumers(s); n != 0 {
		t.Fatalf("%d slow-consumer disconnects during a 50 ms stall: %+v %+v", n, ps, gs)
	}
	if gs.PeakQueued <= soft {
		t.Fatalf("the burst never went above the soft limit (peak %d): the test didn't test anything", gs.PeakQueued)
	}
	m := scrape(t, s)
	if m["matchd_wal_fsync_stalls_total"] < 1 {
		t.Errorf("matchd_wal_fsync_stalls_total = %v, want >= 1", m["matchd_wal_fsync_stalls_total"])
	}
	if m["matchd_slow_consumer_disconnects_total"] != 0 {
		t.Errorf("matchd_slow_consumer_disconnects_total = %v", m["matchd_slow_consumer_disconnects_total"])
	}
	waitForLog(t, s, "durability stall: slow fsync")
}

// A client that falls behind and stays behind is still a slow consumer:
// over the soft limit for longer than WriteTimeout. Here it reads, but
// too slowly ever to catch up.
func TestPersistentlySlowReaderIsDisconnected(t *testing.T) {
	s := start(t, func(c *Config) {
		c.Gateway.OutQueue = 32
		c.Gateway.WriteTimeout = 300 * time.Millisecond
		c.Gateway.SocketWriteBuffer = 4 << 10
	})
	slow, err := client.Dial(s.Addr(), 0, wait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slow.Close() }()
	if err := slow.Subscribe(proto.Subscribe{Instrument: 1}); err != nil {
		t.Fatal(err)
	}
	go func() { // one message every 20 ms: far slower than the market data
		for {
			if _, err := slow.Next(); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	trader := dial(t, s, 2)
	deadline := time.Now().Add(20 * time.Second)
	for i := 0; slowConsumers(s) == 0; i++ {
		if time.Now().After(deadline) {
			t.Fatalf("persistently slow reader never disconnected: %+v %+v", s.Publisher().Stats(), s.Gateway().Stats())
		}
		_ = trader.NewOrder(limit(1, uint64(i+1), proto.SideBuy, int64(10+i%50), 1))
		recv(t, trader, 1)
	}
	waitForLog(t, s, `reason="slow consumer: `)
}
