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
)

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

	// out is the bounded outbound queue: the publisher offers engine events
	// here without blocking (Deliver), the writer drains it. Its capacity is
	// the slow-consumer threshold.
	out chan engine.Event
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
		out:    make(chan engine.Event, g.cfg.OutQueue),
		ctrl:   make(chan control, 16),
		done:   make(chan struct{}),
	}
}

// Deliver implements publisher.Subscriber: a non-blocking offer.
func (s *session) Deliver(ev engine.Event) bool {
	select {
	case s.out <- ev:
		return true
	default:
		return false
	}
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
	s.close(fmt.Sprintf("slow consumer: outbound queue full (%d events)", cap(s.out)))
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
	r := proto.NewReader(s.conn)
	var (
		login  proto.Login
		order  proto.NewOrder
		cancel proto.Cancel
		sub    proto.Subscribe
		resend proto.Resend
	)
	for {
		if s.g.cfg.IdleTimeout > 0 {
			_ = s.conn.SetReadDeadline(time.Now().Add(s.g.cfg.IdleTimeout))
		}
		typ, payload, err := r.Next()
		if err != nil {
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
	buf := make([]byte, 0, 128) // one encoded frame at a time, reused
	for {
		select {
		case ev := <-s.out:
			buf = appendEvent(buf[:0], &ev)
		case c := <-s.ctrl:
			buf = appendControl(buf[:0], &c)
			if c.closeAfter {
				_, _ = bw.Write(buf)
				_ = s.flush(bw)
				s.close(c.reason)
				return
			}
		case <-s.done:
			return
		}
		if len(buf) > 0 {
			if _, err := bw.Write(buf); err != nil {
				s.close("write error: " + err.Error())
				return
			}
		}
		if len(s.out) == 0 && len(s.ctrl) == 0 {
			if err := s.flush(bw); err != nil {
				s.close("write error: " + err.Error())
				return
			}
		}
	}
}

func (s *session) flush(bw *bufio.Writer) error {
	if s.g.cfg.WriteTimeout > 0 {
		_ = s.conn.SetWriteDeadline(time.Now().Add(s.g.cfg.WriteTimeout))
	}
	return bw.Flush()
}

// appendEvent encodes an event as its wire message: the engine's events for
// this client, and market data (deltas, trades, and the publisher's
// snapshot and resend answers). InstrumentAdded is never queued to a
// session; it encodes to nothing.
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
	case engine.EvSnapshotBegin:
		return proto.AppendSnapshot(dst, &proto.Snapshot{Instrument: uint32(ev.Instrument), MDSeq: ev.MDSeq, Levels: uint32(ev.Qty)})
	case engine.EvSnapshotLevel:
		return proto.AppendSnapshotLevel(dst, &proto.SnapshotLevel{
			Instrument: uint32(ev.Instrument), Side: uint8(ev.Side), Price: int64(ev.Price), Qty: int64(ev.Qty),
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
