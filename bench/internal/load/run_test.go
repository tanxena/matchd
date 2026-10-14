package load

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/gateway"
	"github.com/tanxena/matchd/internal/proto"
	"github.com/tanxena/matchd/internal/sequencer"
	"github.com/tanxena/matchd/internal/server"
)

func startEngine(t *testing.T) *server.Server {
	t.Helper()
	gw := gateway.DefaultConfig()
	gw.Addr = "127.0.0.1:0"
	policy, _ := sequencer.ParsePolicy("never")
	s, err := server.Start(server.Config{
		DataDir: t.TempDir(), Gateway: gw, Fsync: policy, HTTPAddr: "127.0.0.1:0",
		Instruments: []book.Instrument{{Symbol: "BENCH", TickSize: 1, LotSize: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A short closed-loop and open-loop run of every mix against a real
// engine: everything sent is answered, every number the report uses is
// filled in, and the engine agrees on how many commands it executed.
func TestRunAgainstEngine(t *testing.T) {
	if runtime.GOOS != "windows" {
		// Run reads the engine's, its own and other processes' CPU time
		// through bench/internal/sysinfo, which only Windows implements.
		t.Skip("the benchmark harness is Windows-only (CPU accounting through bench/internal/sysinfo)")
	}
	for _, tc := range []struct {
		mix  string
		rate int
	}{{"resting", 0}, {"crossing", 0}, {"cancel-heavy", 0}, {"resting", 5000}, {"cancel-heavy", 5000}} {
		mode := "closed"
		if tc.rate > 0 {
			mode = "open"
		}
		t.Run(tc.mix+"-"+mode, func(t *testing.T) {
			s := startEngine(t)
			res, err := Run(Config{Addr: s.Addr(), HTTPAddr: s.HTTPAddr(), Mix: tc.mix, Rate: tc.rate, Conns: 2, Window: 16,
				Warmup: 300 * time.Millisecond, Duration: 700 * time.Millisecond, Seed: 1, Depth: 500})
			if err != nil {
				t.Fatalf("%v; errors %v", err, res.Errors)
			}
			if len(res.Errors) > 0 {
				t.Fatalf("errors: %v", res.Errors)
			}
			t.Logf("%s %s: %.0f/s, sent %+v, answers %+v, e2e p50 %d ns p99 %d ns, core p50 %d ns (n=%d), book %d -> %d",
				tc.mix, mode, res.ThroughputPerSec, res.Sent, res.Answers, res.E2E.P50NS, res.E2E.P99NS, res.Core.P50NS, res.Core.Count,
				res.BookOrdersStart, res.BookOrdersEnd)
			sent := res.Sent.Total()
			if sent == 0 || res.Completed == 0 || res.E2E.Count != sent {
				t.Fatalf("sent %d, completed %d, e2e samples %d (want one per command sent in the window)", sent, res.Completed, res.E2E.Count)
			}
			// The engine's window encloses ours by a scrape's duration.
			if c := res.Engine.Commands; c < float64(res.Completed)*0.9 || c > float64(res.Completed)*1.5+100 {
				t.Errorf("engine executed %.0f commands in its window, loadgen saw %d answered", c, res.Completed)
			}
			if res.Core.Count == 0 || res.Core.P50NS <= 0 || res.Core.MaxNS < res.Core.P999NS || res.Core.P999NS < res.Core.P50NS {
				t.Errorf("core latency %+v", res.Core)
			}
			if res.Engine.ClockSource == "" || res.Engine.ClockReadCostNS <= 0 {
				t.Errorf("engine clock not reported: %+v", res.Engine)
			}
			if res.Sent.CapCancel != 0 {
				t.Errorf("%d cap cancels", res.Sent.CapCancel)
			}
			if tc.mix != "resting" || mode == "closed" {
				if res.Answers.Fills == 0 {
					t.Error("no fills")
				}
			}
			if mode == "open" {
				if res.SendLag == nil || res.SendLag.Count != sent {
					t.Errorf("send lag %+v for %d sent", res.SendLag, sent)
				}
				if r := res.ThroughputPerSec; r < 4000 || r > 6000 {
					t.Errorf("open loop at 5000/s answered %.0f/s", r)
				}
			} else if res.Writes == 0 || res.Writes > sent {
				t.Errorf("%d writes for %d commands", res.Writes, sent)
			}
			if res.CPU.Loadgen <= 0 || res.CPU.Logical == 0 {
				t.Errorf("cpu %+v", res.CPU)
			}
			if res.MeasuredNS < int64(600*time.Millisecond) || res.WarmupNS < int64(250*time.Millisecond) {
				t.Errorf("window %v, warmup %v", time.Duration(res.MeasuredNS), time.Duration(res.WarmupNS))
			}
		})
	}
}

// An answer that isn't for the oldest pending command ends the run: the
// latency of every later command would otherwise be attributed wrongly.
func TestAnswerOutOfOrderIsAnError(t *testing.T) {
	r := &runner{cfg: Config{Rate: 1000}, stop: make(chan struct{})} // open loop: no slot tokens to return
	r.cfg.Defaults()
	r.cfg.mix = Mixes["resting"]
	k := newConn(r, 0, nil)
	k.pend = append(k.pend, pending{kind: Passive, clOrd: 1}, pending{kind: Passive, clOrd: 2})
	if err := k.handle(&client.Msg{Type: proto.TypeAck, Ack: proto.Ack{ClOrdID: 2, OrderID: 9}}, 0); err == nil ||
		!strings.Contains(err.Error(), "oldest pending") {
		t.Fatalf("out-of-order Ack: %v", err)
	}
	if err := k.handle(&client.Msg{Type: proto.TypeAck, Ack: proto.Ack{ClOrdID: 1, OrderID: 9}}, 0); err != nil {
		t.Fatal(err)
	}
	if k.live.Len() != 1 {
		t.Fatal("acked passive order not live")
	}
	// A cancel's answer must be for the cancel at the head.
	k.pend = append(k.pend, pending{kind: Cancel, orderID: 9})
	if err := k.handle(&client.Msg{Type: proto.TypeCancelAck, CancelAck: proto.CancelAck{OrderID: 8, Reason: proto.CancelRequested}}, 0); err == nil {
		t.Fatal("CancelAck for the wrong order accepted")
	}
	// An IOC remainder is not a first answer, and a maker fill with nothing
	// left removes the order from the live set.
	if err := k.handle(&client.Msg{Type: proto.TypeCancelAck, CancelAck: proto.CancelAck{OrderID: 5, Reason: proto.CancelIOCUnfilled}}, 0); err != nil {
		t.Fatal(err)
	}
	if err := k.handle(&client.Msg{Type: proto.TypeFill, Fill: proto.Fill{OrderID: 9, Role: proto.RoleMaker, Leaves: 0}}, 0); err != nil || k.live.Len() != 0 {
		t.Fatalf("maker fill: %v, live %d", err, k.live.Len())
	}
	if err := k.handle(&client.Msg{Type: proto.TypeAck, Ack: proto.Ack{ClOrdID: 3}}, 0); err == nil {
		t.Fatal("Ack with a cancel at the head accepted")
	}
}

// Open loop measures from the scheduled send time (no coordinated
// omission): a command scheduled at 100 but written late at 900 and
// answered at 1000 took 900, not 100. Closed loop measures from the write.
func TestLatencyIsMeasuredFromTheSchedule(t *testing.T) {
	for _, tc := range []struct {
		rate int
		want int64
	}{{1000, 900}, {0, 100}} {
		r := &runner{cfg: Config{Rate: tc.rate, Window: 1}, stop: make(chan struct{})}
		r.cfg.Defaults()
		r.cfg.mix = Mixes["resting"]
		k := newConn(r, 0, nil)
		if k.tokens != nil {
			<-k.tokens // the slot the command occupies
		}
		k.pend = append(k.pend, pending{kind: Passive, clOrd: 1, intended: 100, sent: 900, measured: true})
		if err := k.handle(&client.Msg{Type: proto.TypeAck, Ack: proto.Ack{ClOrdID: 1, OrderID: 1}}, 1000); err != nil {
			t.Fatal(err)
		}
		s := k.e2e.Snapshot()
		if s.Count() != 1 || s.SumNS != tc.want {
			t.Errorf("rate %d: recorded %d ns (count %d), want %d", tc.rate, s.SumNS, s.Count(), tc.want)
		}
	}
}
