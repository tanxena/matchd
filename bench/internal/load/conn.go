package load

import (
	"fmt"
	"sync"

	"github.com/tanxena/matchd/bench/internal/result"
	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/clock"
	"github.com/tanxena/matchd/internal/metrics"
	"github.com/tanxena/matchd/internal/proto"
)

// pending is a command sent and not yet answered.
type pending struct {
	kind     Kind
	clOrd    uint64 // NewOrder: its client order id
	orderID  uint64 // Cancel: the order it cancels
	intended int64  // open loop: when the schedule said to send it (clock.Now)
	sent     int64  // when it was written
	measured bool   // sent inside the measurement window
}

// conn is one logged-in connection: a writer (closed loop) or the shared
// pacer (open loop) sends, and one reader goroutine takes the answers.
//
// The engine answers one connection's commands in the order they were
// sent (one sequencer, one engine goroutine, one writer per session), so
// the first answer the reader sees for a command is always for the oldest
// unanswered one: pend is a FIFO, and every answer is checked against its
// head.
type conn struct {
	r   *runner
	idx int
	c   *client.Conn

	mu        sync.Mutex // guards everything below: the sender and the reader both use it
	gen       *Generator
	live      *Live
	pend      []pending // FIFO from head
	head      int
	nextClOrd uint64
	sent      result.Counts
	answers   result.Answers
	completed uint64 // first answers received inside the window
	writes    uint64
	fullNS    int64 // closed loop: time spent waiting for a free slot, in the window
	err       error // first protocol error; ends the run

	e2e     *metrics.LogHistogram // recorded by the reader only
	sendLag *metrics.LogHistogram // recorded by the sender only (open loop)

	// tokens (closed loop) holds one value per free slot in the window:
	// the writer takes one per command, the reader gives it back when the
	// command's first answer arrives.
	tokens chan struct{}

	buf []byte // the sender's encode buffer
}

func newConn(r *runner, idx int, c *client.Conn) *conn {
	cfg := r.cfg
	k := &conn{
		r: r, idx: idx, c: c,
		gen:     NewGenerator(cfg.mix, cfg.Seed, idx, cfg.LiveCap, r.depth),
		live:    NewLive(cfg.LiveCap, r.depth),
		pend:    make([]pending, 0, 1024),
		e2e:     metrics.NewLogHistogram(),
		sendLag: metrics.NewLogHistogram(),
		answers: result.Answers{Rejects: map[string]uint64{}},
		buf:     make([]byte, 0, 64<<10),
	}
	if cfg.Rate == 0 {
		k.tokens = make(chan struct{}, cfg.Window)
		for range cfg.Window {
			k.tokens <- struct{}{}
		}
	}
	return k
}

// appendBatch generates n commands into k.buf and queues them as pending.
// intended[i] (open loop) is command i's scheduled time; nil for closed
// loop. Must hold k.mu.
func (k *conn) appendBatch(n int, intended []int64, measuring bool) {
	k.buf = k.buf[:0]
	for i := range n {
		o, noTarget := k.gen.Next(k.live)
		p := pending{kind: o.Kind, measured: measuring}
		if intended != nil {
			p.intended = intended[i]
		}
		switch o.Kind {
		case Cancel, CapCancel:
			p.orderID = o.OrderID
			k.buf = proto.AppendCancel(k.buf, &proto.Cancel{Instrument: k.r.cfg.Instrument, OrderID: o.OrderID})
		default:
			k.nextClOrd++
			p.clOrd = k.nextClOrd
			m := proto.NewOrder{Instrument: k.r.cfg.Instrument, ClOrdID: p.clOrd, Side: proto.SideSell,
				Type: proto.TypeLimit, TIF: proto.TIFGTC, Price: o.Price, Qty: o.Qty}
			if o.Buy {
				m.Side = proto.SideBuy
			}
			if o.Kind == Crossing {
				m.TIF = proto.TIFIOC
			}
			k.buf = proto.AppendNewOrder(k.buf, &m)
		}
		if measuring {
			switch o.Kind {
			case Passive:
				k.sent.Passive++
			case Crossing:
				k.sent.Crossing++
			case Cancel:
				k.sent.Cancel++
			case CapCancel:
				k.sent.CapCancel++
			}
			if noTarget {
				k.sent.NoTarget++
			}
		}
		k.pend = append(k.pend, p)
	}
}

// stamp sets the send time of the last n pending commands. Must hold k.mu.
func (k *conn) stamp(n int, now int64) {
	for i := len(k.pend) - n; i < len(k.pend); i++ {
		k.pend[i].sent = now
	}
	if k.r.measuring() {
		k.writes++
	}
}

// writeClosed is the closed-loop writer: keep cfg.Window commands in
// flight, sending every free slot at once in one write.
func (k *conn) writeClosed() {
	for {
		t0 := clock.Now()
		select {
		case <-k.tokens:
		case <-k.r.stop:
			return
		}
		waited := clock.Now() - t0
		n := 1
	more:
		for n < k.r.cfg.Window {
			select {
			case <-k.tokens:
				n++
			default:
				break more
			}
		}
		measuring := k.r.measuring()
		k.mu.Lock()
		if measuring {
			k.fullNS += waited
		}
		k.appendBatch(n, nil, measuring)
		k.stamp(n, clock.Now())
		k.mu.Unlock()
		if err := k.c.WriteFrames(k.buf); err != nil {
			k.fail(fmt.Errorf("conn %d: write: %w", k.idx, err))
			return
		}
	}
}

// sendDue (open loop, called by the pacer) sends the commands whose
// scheduled times are in intended, in one write.
func (k *conn) sendDue(intended []int64) error {
	measuring := k.r.measuring()
	k.mu.Lock()
	k.appendBatch(len(intended), intended, measuring)
	now := clock.Now()
	k.stamp(len(intended), now)
	k.mu.Unlock()
	if measuring {
		for _, t := range intended {
			k.sendLag.Observe(now - t)
		}
	}
	return k.c.WriteFrames(k.buf)
}

func (k *conn) fail(err error) {
	k.mu.Lock()
	if k.err == nil {
		k.err = err
	}
	k.mu.Unlock()
	k.r.abort(err)
}

// read takes every message until the connection closes.
func (k *conn) read() {
	for {
		m, err := k.c.Next()
		now := clock.Now()
		if err != nil {
			if !k.r.stopping() {
				k.fail(fmt.Errorf("conn %d: read: %w", k.idx, err))
			}
			return
		}
		if err := k.handle(&m, now); err != nil {
			k.fail(fmt.Errorf("conn %d: %w", k.idx, err))
			return
		}
	}
}

// handle processes one message received at now.
func (k *conn) handle(m *client.Msg, now int64) error {
	measuring := k.r.measuring()
	k.mu.Lock()
	defer k.mu.Unlock()
	switch m.Type {
	case proto.TypeAck:
		p, err := k.answer(m.Ack.ClOrdID, 0, "Ack")
		if err != nil {
			return err
		}
		if p.kind == Passive {
			k.live.Add(m.Ack.OrderID)
		}
		k.done(p, now, measuring)
		if measuring {
			k.answers.Acks++
		}
	case proto.TypeReject:
		rj := m.Reject
		p, err := k.answer(rj.ClOrdID, rj.OrderID, "Reject")
		if err != nil {
			return err
		}
		k.done(p, now, measuring)
		if measuring {
			k.answers.Rejects[rj.Reason.String()]++
		}
	case proto.TypeCancelAck:
		ca := m.CancelAck
		if ca.Reason != proto.CancelRequested { // an IOC remainder: not a first answer
			if measuring && ca.Reason == proto.CancelIOCUnfilled {
				k.answers.IOCUnfilled++
			}
			return nil
		}
		p, err := k.answer(0, ca.OrderID, "CancelAck")
		if err != nil {
			return err
		}
		k.done(p, now, measuring)
		if measuring {
			k.answers.CancelAcks++
		}
	case proto.TypeFill:
		if m.Fill.Role == proto.RoleMaker && m.Fill.Leaves == 0 {
			k.live.Remove(m.Fill.OrderID)
		}
		if measuring {
			k.answers.Fills++
		}
	case proto.TypeHeartbeat:
	default:
		return fmt.Errorf("unexpected %v", m.Type)
	}
	return nil
}

// answer pops the oldest pending command, which must be the one this
// answer is for: the NewOrder with client order id clOrd, or (clOrd 0) the
// Cancel of orderID. Must hold k.mu. (No closure for the match: the
// reader runs once per message, and a closure over the message would put
// it on the heap each time.)
func (k *conn) answer(clOrd, orderID uint64, what string) (pending, error) {
	if k.head == len(k.pend) {
		return pending{}, fmt.Errorf("%s (clordid %d, order %d) with nothing pending", what, clOrd, orderID)
	}
	p := k.pend[k.head]
	if ok := clOrd != 0 && p.clOrd == clOrd || clOrd == 0 && p.clOrd == 0 && p.orderID == orderID; !ok {
		return pending{}, fmt.Errorf("%s (clordid %d, order %d), but the oldest pending command is %+v", what, clOrd, orderID, p)
	}
	k.head++
	if k.head == len(k.pend) { // empty: reuse from the start
		k.pend, k.head = k.pend[:0], 0
	} else if k.head >= 4096 && k.head*2 >= len(k.pend) { // mostly consumed: slide down
		n := copy(k.pend, k.pend[k.head:])
		k.pend, k.head = k.pend[:n], 0
	}
	return p, nil
}

// done records a command's first answer. Must hold k.mu.
func (k *conn) done(p pending, now int64, measuring bool) {
	if p.measured {
		from := p.sent
		if k.r.cfg.Rate > 0 {
			from = p.intended // open loop: a late send counts against latency
		}
		k.e2e.Observe(now - from)
	}
	if measuring {
		k.completed++
	}
	if k.tokens != nil {
		k.tokens <- struct{}{} // never blocks: one slot per command in flight
	}
}

// inFlight is the number of unanswered commands.
func (k *conn) inFlight() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.pend) - k.head
}
