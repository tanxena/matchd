// Package client is a minimal Go client for the engine's wire protocol. It is
// used by engctl, the end-to-end tests, and (milestone 9) the load generator.
package client

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tanxena/matchd/internal/proto"
)

// Msg is one decoded server message. Type says which field is set.
type Msg struct {
	Type      proto.MsgType
	LoginAck  proto.LoginAck
	Ack       proto.Ack
	Reject    proto.Reject
	Fill      proto.Fill
	CancelAck proto.CancelAck

	// Market data.
	BookDelta     proto.BookDelta
	Trade         proto.Trade
	Snapshot      proto.Snapshot
	SnapshotLevel proto.SnapshotLevel
}

// Conn is a logged-in connection. Reading (Next) must happen on one
// goroutine; the send methods may be called from any goroutine.
type Conn struct {
	conn     net.Conn
	r        *proto.Reader
	ClientID uint32

	wmu  sync.Mutex // guards wbuf and serialises frames written to conn
	wbuf []byte
}

// ErrLoginRejected wraps the reason a Login was refused.
var ErrLoginRejected = errors.New("login rejected")

// Dial connects to addr and logs in as clientID, waiting up to timeout for
// the LoginAck. clientID 0 connects without logging in, which is enough for
// market data (Subscribe, Resend) but not for orders.
func Dial(addr string, clientID uint32, timeout time.Duration) (*Conn, error) {
	nc, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	if tc, ok := nc.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	c := &Conn{conn: nc, r: proto.NewReader(nc), ClientID: clientID}
	if clientID == 0 {
		return c, nil
	}
	if err := c.send(func(b []byte) []byte { return proto.AppendLogin(b, &proto.Login{ClientID: clientID}) }); err != nil {
		_ = nc.Close()
		return nil, err
	}
	_ = nc.SetReadDeadline(time.Now().Add(timeout))
	m, err := c.Next()
	_ = nc.SetReadDeadline(time.Time{})
	switch {
	case err != nil:
		_ = nc.Close()
		return nil, fmt.Errorf("waiting for LoginAck: %w", err)
	case m.Type == proto.TypeReject:
		_ = nc.Close()
		return nil, fmt.Errorf("%w: %v", ErrLoginRejected, m.Reject.Reason)
	case m.Type != proto.TypeLoginAck:
		_ = nc.Close()
		return nil, fmt.Errorf("expected LoginAck, got %v", m.Type)
	}
	return c, nil
}

func (c *Conn) send(enc func([]byte) []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.wbuf = enc(c.wbuf[:0])
	_, err := c.conn.Write(c.wbuf)
	return err
}

// NewOrder sends a NewOrder.
func (c *Conn) NewOrder(m proto.NewOrder) error {
	return c.send(func(b []byte) []byte { return proto.AppendNewOrder(b, &m) })
}

// Cancel sends a Cancel.
func (c *Conn) Cancel(m proto.Cancel) error {
	return c.send(func(b []byte) []byte { return proto.AppendCancel(b, &m) })
}

// Subscribe sends a Subscribe.
func (c *Conn) Subscribe(m proto.Subscribe) error {
	return c.send(func(b []byte) []byte { return proto.AppendSubscribe(b, &m) })
}

// Resend sends a Resend.
func (c *Conn) Resend(m proto.Resend) error {
	return c.send(func(b []byte) []byte { return proto.AppendResend(b, &m) })
}

// Heartbeat sends a Heartbeat.
func (c *Conn) Heartbeat() error {
	return c.send(proto.AppendHeartbeat)
}

// Next reads and decodes the next server message.
func (c *Conn) Next() (Msg, error) {
	typ, p, err := c.r.Next()
	if err != nil {
		return Msg{}, err
	}
	m := Msg{Type: typ}
	switch typ {
	case proto.TypeLoginAck:
		err = m.LoginAck.Decode(p)
	case proto.TypeAck:
		err = m.Ack.Decode(p)
	case proto.TypeReject:
		err = m.Reject.Decode(p)
	case proto.TypeFill:
		err = m.Fill.Decode(p)
	case proto.TypeCancelAck:
		err = m.CancelAck.Decode(p)
	case proto.TypeHeartbeat:
		err = proto.DecodeHeartbeat(p)
	case proto.TypeBookDelta:
		err = m.BookDelta.Decode(p)
	case proto.TypeTrade:
		err = m.Trade.Decode(p)
	case proto.TypeSnapshot:
		err = m.Snapshot.Decode(p)
	case proto.TypeSnapshotLevel:
		err = m.SnapshotLevel.Decode(p)
	default:
		err = fmt.Errorf("unexpected message type %v", typ)
	}
	return m, err
}

// SetReadDeadline bounds the next Next call.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// Close closes the connection.
func (c *Conn) Close() error { return c.conn.Close() }

// String renders a message on one line for engctl and test failures.
func (m Msg) String() string {
	switch m.Type {
	case proto.TypeLoginAck:
		return fmt.Sprintf("LoginAck client=%d", m.LoginAck.ClientID)
	case proto.TypeAck:
		a := m.Ack
		return fmt.Sprintf("Ack seq=%d order=%d clordid=%d inst=%d %s %d @ %d",
			a.Seq, a.OrderID, a.ClOrdID, a.Instrument, sideName(a.Side), a.Qty, a.Price)
	case proto.TypeReject:
		r := m.Reject
		return fmt.Sprintf("Reject seq=%d reason=%v clordid=%d order=%d inst=%d", r.Seq, r.Reason, r.ClOrdID, r.OrderID, r.Instrument)
	case proto.TypeFill:
		f := m.Fill
		role := "taker"
		if f.Role == proto.RoleMaker {
			role = "maker"
		}
		return fmt.Sprintf("Fill seq=%d match=%d order=%d clordid=%d %s %s %d @ %d leaves=%d",
			f.Seq, f.MatchID, f.OrderID, f.ClOrdID, role, sideName(f.Side), f.Qty, f.Price, f.Leaves)
	case proto.TypeCancelAck:
		c := m.CancelAck
		why := "cancelled"
		switch c.Reason {
		case proto.CancelExpired:
			why = "expired"
		case proto.CancelIOCUnfilled:
			why = "ioc-unfilled"
		}
		return fmt.Sprintf("CancelAck seq=%d order=%d clordid=%d %s %s qty=%d @ %d",
			c.Seq, c.OrderID, c.ClOrdID, why, sideName(c.Side), c.Qty, c.Price)
	case proto.TypeHeartbeat:
		return "Heartbeat"
	case proto.TypeBookDelta:
		d := m.BookDelta
		return fmt.Sprintf("BookDelta md=%d inst=%d %s %d -> qty %d", d.MDSeq, d.Instrument, sideName(d.Side), d.Price, d.Qty)
	case proto.TypeTrade:
		tr := m.Trade
		return fmt.Sprintf("Trade md=%d inst=%d match=%d %d @ %d (aggressor %s)", tr.MDSeq, tr.Instrument, tr.MatchID, tr.Qty, tr.Price, sideName(tr.Side))
	case proto.TypeSnapshot:
		return fmt.Sprintf("Snapshot inst=%d md=%d levels=%d", m.Snapshot.Instrument, m.Snapshot.MDSeq, m.Snapshot.Levels)
	case proto.TypeSnapshotLevel:
		l := m.SnapshotLevel
		return fmt.Sprintf("  level %s %d x %d", sideName(l.Side), l.Price, l.Qty)
	}
	return m.Type.String()
}

func sideName(s uint8) string {
	switch s {
	case proto.SideBuy:
		return "buy"
	case proto.SideSell:
		return "sell"
	}
	return fmt.Sprintf("side(%d)", s)
}
