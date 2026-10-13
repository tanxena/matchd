package gateway

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/proto"
	"github.com/tanxena/matchd/internal/publisher"
)

// maxPendingSnapshots bounds the snapshots a session holds but hasn't
// streamed yet. Only a client that subscribes over and over without reading
// gets near it, and it is then a slow consumer.
const maxPendingSnapshots = 4

// control is a reply the gateway itself generates (not an engine event):
// LoginAck, Heartbeat, or a gateway-level Reject. The reader goroutine
// produces these and hands them to the writer, so only the writer touches
// the socket.
type control struct {
	typ    proto.MsgType // TypeLoginAck, TypeHeartbeat or TypeReject
	client uint32        // LoginAck
	reject proto.Reject  // Reject
	// closeAfter: write this frame, flush, then close the session with
	// reason. Used for fatal session errors, so the client learns why.
	closeAfter bool
	reason     string
}

type session struct {
	g      *Gateway
	conn   net.Conn
	remote string

	// client is 0 until Login succeeds. Written only by the reader
	// goroutine; read by close (any goroutine) under closeMu.
	client book.ClientID

	// out is the outbound queue: the publisher offers engine events here
	// without blocking (Deliver), the writer drains it. A client that stays
	// over its soft limit longer than WriteTimeout, or reaches its hard
	// limit, is a slow consumer (decision 022).
	out *outQueue
	// slowWhy is why Deliver last refused an event, for SlowConsumer's log
	// line. Both run on the publisher goroutine, one right after the other.
	slowWhy string
	// snaps holds market-data snapshots waiting to be streamed, in the order
	// of their EvSnapshot markers in out (decision 016).
	snaps chan *publisher.Snapshot
	// ctrl carries gateway replies from the reader to the writer.
	ctrl chan control
	// done is closed exactly once, by close. Every blocking operation in
	// both goroutines also selects on it, so closing it releases them.
	done chan struct{}

	closeMu   sync.Mutex // guards closed, reason and client during close
	closed    bool
	closeOnce sync.Once
}

func newSession(g *Gateway, conn net.Conn) *session {
	return &session{
		g:      g,
		conn:   conn,
		remote: conn.RemoteAddr().String(),
		out:    newOutQueue(g.cfg.OutQueue, g.cfg.OutQueue+publisher.MaxPending, &g.peakQueued),
		snaps:  make(chan *publisher.Snapshot, maxPendingSnapshots),
		ctrl:   make(chan control, 16),
		done:   make(chan struct{}),
	}
}

// Deliver implements publisher.Subscriber: a non-blocking offer, refused
// only if the client is a slow consumer (decision 022).
func (s *session) Deliver(ev engine.Event) bool {
	ok, why := s.out.push(ev, s.g.cfg.WriteTimeout, time.Now)
	if !ok {
		s.slowWhy = why
	}
	return ok
}

// DeliverSnapshot implements publisher.Subscriber without blocking: the
// snapshot goes on snaps, and an EvSnapshot marker on out holds its place
// among the events. The writer streams it on reaching the marker. (If the
// marker doesn't fit, the snapshot is left on snaps, but the publisher then
// disconnects the session as a slow consumer, so it is never streamed.)
func (s *session) DeliverSnapshot(snap *publisher.Snapshot) bool {
	select {
	case s.snaps <- snap:
	default:
		s.slowWhy = fmt.Sprintf("%d market-data snapshots already waiting to be sent", maxPendingSnapshots)
		return false
	}
	return s.Deliver(engine.Event{Kind: engine.EvSnapshot, Instrument: snap.Instrument, MDSeq: snap.MDSeq})
}

// Closed implements publisher.Subscriber.
func (s *session) Closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// SlowConsumer implements publisher.Subscriber.
func (s *session) SlowConsumer() {
	s.close("slow consumer: " + s.slowWhy)
}

// close tears the session down once, from any goroutine: it records why,
// releases both goroutines (done), closes the socket (which also unblocks a
// writer stuck in Write and a reader stuck in Read), unregisters the client
// and logs.
func (s *session) close(reason string) {
	s.closeOnce.Do(func() {
		s.closeMu.Lock()
		s.closed = true
		client := s.client
		s.closeMu.Unlock()

		close(s.done)
		_ = s.conn.Close()
		if client != 0 {
			s.g.pub.Unregister(client, s)
		}
		s.g.forget(s)
		s.g.log.Info("connection closed", "remote", s.remote, "client", client, "reason", reason)
	})
}

// reply hands a control frame to the writer. It gives up if the session is
// closing.
func (s *session) reply(c control) bool {
	select {
	case s.ctrl <- c:
		return true
	case <-s.done:
		return false
	}
}

// fatal sends a gateway Reject, then has the writer close the session after
// flushing it.
func (s *session) fatal(r proto.Reason, rej proto.Reject, reason string) {
	rej.Reason = r
	s.reply(control{typ: proto.TypeReject, reject: rej, closeAfter: true, reason: reason})
}

// readLoop is the reader goroutine.
func (s *session) readLoop() {
	defer s.g.wg.Done()
	defer s.g.readers.Done()
	r := proto.NewReader(s.conn)
	var (
		login  proto.Login
		order  proto.NewOrder
		cancel proto.Cancel
		sub    proto.Subscribe
		resend proto.Resend
	)
	for {
		// A graceful shutdown stops readers but leaves the session to its
		// writer (StopReading). Checked again after setting the idle
		// deadline, which could otherwise replace StopReading's wake-up.
		if s.g.draining.Load() {
			return
		}
		if s.g.cfg.IdleTimeout > 0 {
			_ = s.conn.SetReadDeadline(time.Now().Add(s.g.cfg.IdleTimeout))
		}
		if s.g.draining.Load() {
			return
		}
		typ, payload, err := r.Next()
		if err != nil {
			if s.g.draining.Load() {
				return
			}
			s.close(readErrorReason(err))
			return
		}

		var decodeErr error
		switch typ {
		case proto.TypeLogin:
			if decodeErr = login.Decode(payload); decodeErr != nil {
				break
			}
			if !s.handleLogin(login.ClientID) {
				return // fatal reply queued; the writer closes the session
			}

		case proto.TypeNewOrder:
			if decodeErr = order.Decode(payload); decodeErr != nil {
				break
			}
			if s.client == 0 {
				s.fatal(proto.ReasonNotLoggedIn, proto.Reject{Instrument: order.Instrument, ClOrdID: order.ClOrdID}, "order before login")
				return
			}
			// Field values are passed through unchecked: the engine
			// validates them, so even a bad side becomes a logged,
			// sequenced, replayable Reject (decision 009).
			if !s.submit(engine.Command{
				Kind: engine.CmdNewOrder, Instrument: engine.InstrumentID(order.Instrument),
				Client: s.client, ClOrdID: book.ClientOrderID(order.ClOrdID),
				Side: book.Side(order.Side), Type: book.OrderType(order.Type), TIF: book.TIF(order.TIF),
				Price: book.Price(order.Price), Qty: book.Qty(order.Qty),
			}) {
				return
			}

		case proto.TypeCancel:
			if decodeErr = cancel.Decode(payload); decodeErr != nil {
				break
			}
			if s.client == 0 {
				s.fatal(proto.ReasonNotLoggedIn, proto.Reject{Instrument: cancel.Instrument, OrderID: cancel.OrderID}, "cancel before login")
				return
			}
			if !s.submit(engine.Command{
				Kind: engine.CmdCancel, Instrument: engine.InstrumentID(cancel.Instrument),
				Client: s.client, OrderID: book.OrderID(cancel.OrderID),
			}) {
				return
			}

		case proto.TypeSubscribe:
			if decodeErr = sub.Decode(payload); decodeErr != nil {
				break
			}
			// Market data is public: no login needed. The publisher answers
			// with a snapshot, then streams deltas and trades.
			s.g.pub.Subscribe(engine.InstrumentID(sub.Instrument), s)

		case proto.TypeResend:
			if decodeErr = resend.Decode(payload); decodeErr != nil {
				break
			}
			s.g.pub.Resend(engine.InstrumentID(resend.Instrument), resend.FromMDSeq, s)

		case proto.TypeHeartbeat:
			if decodeErr = proto.DecodeHeartbeat(payload); decodeErr != nil {
				break
			}
			if !s.reply(control{typ: proto.TypeHeartbeat}) {
				return
			}

		default:
			s.close(fmt.Sprintf("protocol error: unexpected message type %v", typ))
			return
		}
		if decodeErr != nil {
			s.close("protocol error: " + decodeErr.Error())
			return
		}
	}
}

// handleLogin processes a Login. It returns false if the session is being
// closed.
func (s *session) handleLogin(id uint32) bool {
	switch {
	case s.client != 0:
		s.fatal(proto.ReasonAlreadyLoggedIn, proto.Reject{}, "second login on one connection")
		return false
	case id == 0:
		s.fatal(proto.ReasonInvalidClientID, proto.Reject{}, "login with client id 0")
		return false
	case book.ClientID(id) == ReservedClientID:
		s.fatal(proto.ReasonInvalidClientID, proto.Reject{}, "login with the admin endpoint's reserved client id")
		return false
	}
	// Mark the session logged in before registering, under closeMu, so that
	// if close runs concurrently it either sees client == 0 (and we notice
	// closed below) or sees the id and unregisters it.
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return false
	}
	if !s.g.pub.Register(book.ClientID(id), s) {
		s.closeMu.Unlock()
		s.fatal(proto.ReasonClientInUse, proto.Reject{}, fmt.Sprintf("client %d already connected", id))
		return false
	}
	s.client = book.ClientID(id)
	s.closeMu.Unlock()
	s.g.log.Info("login", "remote", s.remote, "client", id)
	return s.reply(control{typ: proto.TypeLoginAck, client: id})
}

// submit puts a command on the shared command channel. If the channel is
// full the reader blocks, so it stops reading this client's socket: TCP flow
// control then pushes back on the client (inbound backpressure).
func (s *session) submit(c engine.Command) bool {
	select {
	case s.g.cmds <- c:
		return true
	case <-s.done:
		return false
	}
}

func readErrorReason(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, io.EOF):
		return "client closed the connection"
	case errors.Is(err, net.ErrClosed):
		return "connection closed"
	case isConnReset(err):
		return "client reset the connection"
	case errors.As(err, &ne) && ne.Timeout():
		return "idle timeout"
	case errors.Is(err, proto.ErrFrameLength), errors.Is(err, proto.ErrVersion):
		return "protocol error: " + err.Error()
	}
	return "read error: " + err.Error()
}

// writeLoop is the writer goroutine. It encodes engine events and control
// replies into a buffered writer and flushes whenever both queues are empty,
// so a burst of events goes out in few syscalls while a lone event is sent at
// once.
func (s *session) writeLoop() {
	defer s.g.wg.Done()
	bw := bufio.NewWriterSize(s.conn, 32<<10)
	buf := make([]byte, 0, 128)           // one encoded frame at a time, reused
	batch := make([]engine.Event, 0, 256) // events taken from out, written in order
	next := 0                             // batch[next:] are still to write
	finishing := s.g.finishing            // closed by FinishWriters; nil once seen
	draining := false
	idle := func() bool { return next == len(batch) && s.out.len() == 0 && len(s.ctrl) == 0 }
	for {
		if draining && idle() {
			// Graceful shutdown: everything queued has been written.
			if err := s.flush(bw); err != nil {
				s.writeFailed(err, "")
				return
			}
			s.lingerClose()
			s.close("server shutting down")
			return
		}
		if next == len(batch) {
			batch, next = s.out.pop(batch[:0], cap(batch)), 0
		}
		// Control replies first, without waiting: a closeAfter or a
		// heartbeat answer isn't stuck behind a long run of events.
		var ctrl *control
		select {
		case c := <-s.ctrl:
			ctrl = &c
		case <-s.done:
			return
		default:
		}
		if ctrl == nil && next == len(batch) {
			// Nothing to write: wait for an event, a reply, shutdown.
			select {
			case <-finishing:
				draining, finishing = true, nil
				continue
			case <-s.out.ready:
				continue
			case c := <-s.ctrl:
				ctrl = &c
			case <-s.done:
				return
			}
		}
		switch {
		case ctrl != nil:
			buf = appendControl(buf[:0], ctrl)
			if ctrl.closeAfter {
				_, _ = bw.Write(buf)
				_ = s.flush(bw)
				s.close(ctrl.reason)
				return
			}
		default:
			ev := &batch[next]
			next++
			buf = appendEvent(buf[:0], ev)
			if ev.Kind == engine.EvSnapshot {
				if err := s.streamSnapshot(bw); err != nil {
					s.writeFailed(err, "during snapshot")
					return
				}
				buf = buf[:0]
			}
		}
		if len(buf) > 0 {
			if _, err := bw.Write(buf); err != nil {
				s.writeFailed(err, "")
				return
			}
		}
		if idle() {
			if err := s.flush(bw); err != nil {
				s.writeFailed(err, "")
				return
			}
		}
	}
}

// writeFailed closes the session after a failed write. A write that hit
// the WriteTimeout deadline means the client didn't drain in time: a slow
// consumer, counted as one (decision 022). Anything else is a write error.
func (s *session) writeFailed(err error, during string) {
	if during != "" {
		during = " " + during
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		s.g.slowWrites.Add(1)
		s.close(fmt.Sprintf("slow consumer: socket write%s blocked for longer than WriteTimeout (%v)", during, s.g.cfg.WriteTimeout))
		return
	}
	s.close("write error" + during + ": " + err.Error())
}

// streamSnapshot writes the next pending snapshot: its header, then one
// SnapshotLevel frame per level, read straight from the publisher's
// immutable copy rather than queued as events. When the client reads
// slowly, the socket writes block (TCP backpressure), each one bounded by
// WriteTimeout: a client that can't take the snapshot is disconnected, and
// one that can isn't limited by the queue's size. Events published
// meanwhile wait on out; if out fills, the client is a slow consumer as
// usual.
func (s *session) streamSnapshot(bw *bufio.Writer) error {
	var snap *publisher.Snapshot
	select {
	case snap = <-s.snaps: // queued before its marker, so it is there
	case <-s.done:
		return net.ErrClosed
	}
	var frame [64]byte
	b := proto.AppendSnapshot(frame[:0], &proto.Snapshot{
		Instrument: uint32(snap.Instrument), MDSeq: snap.MDSeq, Levels: uint32(len(snap.Levels)),
	})
	if err := s.write(bw, b); err != nil {
		return err
	}
	for i := range snap.Levels {
		l := &snap.Levels[i]
		b = proto.AppendSnapshotLevel(frame[:0], &proto.SnapshotLevel{
			Instrument: uint32(snap.Instrument), Side: uint8(l.Side), Price: int64(l.Price), Qty: int64(l.Qty),
		})
		if err := s.write(bw, b); err != nil {
			return err
		}
	}
	return nil
}

// write buffers one frame, first flushing under the write deadline if the
// frame doesn't fit, so every socket write a long snapshot causes is
// bounded by WriteTimeout.
func (s *session) write(bw *bufio.Writer, frame []byte) error {
	if bw.Available() < len(frame) {
		if err := s.flush(bw); err != nil {
			return err
		}
	}
	_, err := bw.Write(frame)
	return err
}

// lingerTimeout bounds lingerClose's wait for the client to close.
const lingerTimeout = time.Second

// lingerClose ends a graceful shutdown's session without destroying the
// answers just sent. Closing a TCP socket that still has unread input (a
// client pipelining orders the stopped reader no longer takes) sends a
// reset instead of a FIN, and a reset makes the client's stack discard data
// it received but hadn't read yet: answers that arrived would be lost. So:
// half-close (FIN: "nothing more from us"), then read and discard what the
// client still sends until it closes or lingerTimeout passes (decision 019).
func (s *session) lingerClose() {
	tc, ok := s.conn.(*net.TCPConn)
	if !ok {
		return
	}
	if tc.CloseWrite() != nil {
		return
	}
	_ = tc.SetReadDeadline(time.Now().Add(lingerTimeout))
	_, _ = io.Copy(io.Discard, tc)
}

func (s *session) flush(bw *bufio.Writer) error {
	if s.g.cfg.WriteTimeout > 0 {
		_ = s.conn.SetWriteDeadline(time.Now().Add(s.g.cfg.WriteTimeout))
	}
	return bw.Flush()
}

// appendEvent encodes an event as its wire message: the engine's events for
// this client, and market data (deltas, trades, and the publisher's resend
// answer). It encodes nothing for an EvSnapshot marker, which the writer
// handles itself (streamSnapshot), or for InstrumentAdded, which is never
// queued to a session.
func appendEvent(dst []byte, ev *engine.Event) []byte {
	switch ev.Kind {
	case engine.EvAck:
		return proto.AppendAck(dst, &proto.Ack{
			Seq: uint64(ev.Seq), Time: int64(ev.Time), Instrument: uint32(ev.Instrument),
			ClOrdID: uint64(ev.ClOrdID), OrderID: uint64(ev.OrderID),
			Side: uint8(ev.Side), Price: int64(ev.Price), Qty: int64(ev.Qty),
		})
	case engine.EvReject:
		return proto.AppendReject(dst, &proto.Reject{
			Seq: uint64(ev.Seq), Time: int64(ev.Time), Instrument: uint32(ev.Instrument),
			ClOrdID: uint64(ev.ClOrdID), OrderID: uint64(ev.OrderID),
			Reason: proto.Reason(ev.Reason), // same numbering; see wire_test.go
		})
	case engine.EvFill:
		return proto.AppendFill(dst, &proto.Fill{
			Seq: uint64(ev.Seq), Time: int64(ev.Time), Instrument: uint32(ev.Instrument),
			ClOrdID: uint64(ev.ClOrdID), OrderID: uint64(ev.OrderID), MatchID: ev.MatchID,
			Side: uint8(ev.Side), Role: uint8(ev.Role),
			Price: int64(ev.Price), Qty: int64(ev.Qty), Leaves: int64(ev.Leaves),
		})
	case engine.EvBookDelta:
		return proto.AppendBookDelta(dst, &proto.BookDelta{
			Seq: uint64(ev.Seq), Time: int64(ev.Time), Instrument: uint32(ev.Instrument), MDSeq: ev.MDSeq,
			Side: uint8(ev.Side), Price: int64(ev.Price), Qty: int64(ev.Qty),
		})
	case engine.EvTrade:
		return proto.AppendTrade(dst, &proto.Trade{
			Seq: uint64(ev.Seq), Time: int64(ev.Time), Instrument: uint32(ev.Instrument), MDSeq: ev.MDSeq,
			MatchID: ev.MatchID, Side: uint8(ev.Side), Price: int64(ev.Price), Qty: int64(ev.Qty),
		})
	case engine.EvResendUnavailable:
		return proto.AppendReject(dst, &proto.Reject{Instrument: uint32(ev.Instrument), Reason: proto.ReasonResendUnavailable})
	case engine.EvCancelled:
		return proto.AppendCancelAck(dst, &proto.CancelAck{
			Seq: uint64(ev.Seq), Time: int64(ev.Time), Instrument: uint32(ev.Instrument),
			ClOrdID: uint64(ev.ClOrdID), OrderID: uint64(ev.OrderID),
			Side: uint8(ev.Side), Reason: uint8(ev.CancelReason),
			Price: int64(ev.Price), Qty: int64(ev.Qty),
		})
	}
	return dst
}

func appendControl(dst []byte, c *control) []byte {
	switch c.typ {
	case proto.TypeLoginAck:
		return proto.AppendLoginAck(dst, &proto.LoginAck{ClientID: c.client})
	case proto.TypeHeartbeat:
		return proto.AppendHeartbeat(dst)
	case proto.TypeReject:
		return proto.AppendReject(dst, &c.reject)
	}
	return dst
}
