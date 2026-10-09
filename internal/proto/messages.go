package proto

import (
	"encoding/binary"
	"fmt"
)

// Field enums. Their numeric values are part of the wire contract. They
// deliberately equal the engine's own values (book.Side etc.), and a gateway
// test asserts that; but the wire owns them, so a change to the engine's
// enums can't silently change the protocol.
const (
	SideBuy  uint8 = 1
	SideSell uint8 = 2

	TypeLimit  uint8 = 1
	TypeMarket uint8 = 2

	TIFGTC uint8 = 1
	TIFDay uint8 = 2

	RoleTaker uint8 = 1
	RoleMaker uint8 = 2

	CancelRequested uint8 = 1
	CancelExpired   uint8 = 2
)

// Reason is a reject reason on the wire. 1–99 are the engine's reasons;
// 100 and up are raised by the gateway before a command is sequenced.
type Reason uint8

const (
	ReasonInvalidSide            Reason = 1
	ReasonInvalidType            Reason = 2
	ReasonInvalidQty             Reason = 3
	ReasonInvalidPrice           Reason = 4
	ReasonInvalidTIF             Reason = 5
	ReasonDuplicateClientOrderID Reason = 6
	ReasonUnknownOrder           Reason = 7
	ReasonUnknownInstrument      Reason = 8
	ReasonInvalidInstrument      Reason = 9
	ReasonDuplicateSymbol        Reason = 10

	ReasonNotLoggedIn     Reason = 100 // an order or cancel before Login
	ReasonAlreadyLoggedIn Reason = 101 // a second Login on one connection
	ReasonClientInUse     Reason = 102 // another connection is logged in as this client
	ReasonInvalidClientID Reason = 103 // client id 0
	ReasonUnsupported     Reason = 104 // a valid message this server doesn't handle yet
)

func (r Reason) String() string {
	switch r {
	case ReasonInvalidSide:
		return "InvalidSide"
	case ReasonInvalidType:
		return "InvalidType"
	case ReasonInvalidQty:
		return "InvalidQty"
	case ReasonInvalidPrice:
		return "InvalidPrice"
	case ReasonInvalidTIF:
		return "InvalidTIF"
	case ReasonDuplicateClientOrderID:
		return "DuplicateClientOrderID"
	case ReasonUnknownOrder:
		return "UnknownOrder"
	case ReasonUnknownInstrument:
		return "UnknownInstrument"
	case ReasonInvalidInstrument:
		return "InvalidInstrument"
	case ReasonDuplicateSymbol:
		return "DuplicateSymbol"
	case ReasonNotLoggedIn:
		return "NotLoggedIn"
	case ReasonAlreadyLoggedIn:
		return "AlreadyLoggedIn"
	case ReasonClientInUse:
		return "ClientInUse"
	case ReasonInvalidClientID:
		return "InvalidClientID"
	case ReasonUnsupported:
		return "Unsupported"
	}
	return fmt.Sprintf("reason(%d)", uint8(r))
}

// Payload sizes. Each message type has exactly one size; Decode rejects any
// other length.
const (
	SizeLogin     = 4
	SizeLoginAck  = 4
	SizeNewOrder  = 31
	SizeCancel    = 12
	SizeSubscribe = 4
	SizeHeartbeat = 0
	SizeAck       = 53
	SizeReject    = 37
	SizeFill      = 70
	SizeCancelAck = 54
)

// dec reads little-endian fields from a payload whose length has already
// been checked. It is a value type used on the stack, so decoding doesn't
// allocate.
type dec struct {
	b   []byte
	off int
}

func (d *dec) u8() uint8   { v := d.b[d.off]; d.off++; return v }
func (d *dec) u32() uint32 { v := binary.LittleEndian.Uint32(d.b[d.off:]); d.off += 4; return v }
func (d *dec) u64() uint64 { v := binary.LittleEndian.Uint64(d.b[d.off:]); d.off += 8; return v }
func (d *dec) i64() int64  { return int64(d.u64()) }

func checkSize(t MsgType, payload []byte, want int) error {
	if len(payload) != want {
		return fmt.Errorf("%w: %v payload is %d bytes, want %d", ErrPayloadSize, t, len(payload), want)
	}
	return nil
}

var le = binary.LittleEndian

// ---------------------------------------------------------------------------
// Inbound

// Login identifies the client for the rest of the connection.
//
//	0 client_id u32
type Login struct {
	ClientID uint32
}

func AppendLogin(dst []byte, m *Login) []byte {
	dst = appendHeader(dst, TypeLogin, SizeLogin)
	return le.AppendUint32(dst, m.ClientID)
}

func (m *Login) Decode(p []byte) error {
	if err := checkSize(TypeLogin, p, SizeLogin); err != nil {
		return err
	}
	d := dec{b: p}
	m.ClientID = d.u32()
	return nil
}

// NewOrder submits an order. Price is ignored for market orders and must be 0.
//
//	0  instrument      u32
//	4  client_order_id u64
//	12 side            u8
//	13 type            u8
//	14 tif             u8
//	15 price           i64 (ticks)
//	23 qty             i64 (lots)
type NewOrder struct {
	Instrument uint32
	ClOrdID    uint64
	Side       uint8
	Type       uint8
	TIF        uint8
	Price      int64
	Qty        int64
}

func AppendNewOrder(dst []byte, m *NewOrder) []byte {
	dst = appendHeader(dst, TypeNewOrder, SizeNewOrder)
	dst = le.AppendUint32(dst, m.Instrument)
	dst = le.AppendUint64(dst, m.ClOrdID)
	dst = append(dst, m.Side, m.Type, m.TIF)
	dst = le.AppendUint64(dst, uint64(m.Price))
	return le.AppendUint64(dst, uint64(m.Qty))
}

func (m *NewOrder) Decode(p []byte) error {
	if err := checkSize(TypeNewOrder, p, SizeNewOrder); err != nil {
		return err
	}
	d := dec{b: p}
	m.Instrument = d.u32()
	m.ClOrdID = d.u64()
	m.Side, m.Type, m.TIF = d.u8(), d.u8(), d.u8()
	m.Price = d.i64()
	m.Qty = d.i64()
	return nil
}

// Cancel asks to cancel a resting order.
//
//	0 instrument u32
//	4 order_id   u64
type Cancel struct {
	Instrument uint32
	OrderID    uint64
}

func AppendCancel(dst []byte, m *Cancel) []byte {
	dst = appendHeader(dst, TypeCancel, SizeCancel)
	dst = le.AppendUint32(dst, m.Instrument)
	return le.AppendUint64(dst, m.OrderID)
}

func (m *Cancel) Decode(p []byte) error {
	if err := checkSize(TypeCancel, p, SizeCancel); err != nil {
		return err
	}
	d := dec{b: p}
	m.Instrument = d.u32()
	m.OrderID = d.u64()
	return nil
}

// Subscribe asks for an instrument's market data (served from milestone 6).
//
//	0 instrument u32
type Subscribe struct {
	Instrument uint32
}

func AppendSubscribe(dst []byte, m *Subscribe) []byte {
	dst = appendHeader(dst, TypeSubscribe, SizeSubscribe)
	return le.AppendUint32(dst, m.Instrument)
}

func (m *Subscribe) Decode(p []byte) error {
	if err := checkSize(TypeSubscribe, p, SizeSubscribe); err != nil {
		return err
	}
	d := dec{b: p}
	m.Instrument = d.u32()
	return nil
}

// AppendHeartbeat appends a Heartbeat frame (no payload). Valid in both
// directions.
func AppendHeartbeat(dst []byte) []byte {
	return appendHeader(dst, TypeHeartbeat, SizeHeartbeat)
}

// DecodeHeartbeat checks a Heartbeat payload (which must be empty).
func DecodeHeartbeat(p []byte) error { return checkSize(TypeHeartbeat, p, SizeHeartbeat) }

// ---------------------------------------------------------------------------
// Outbound

// LoginAck confirms a Login.
//
//	0 client_id u32
type LoginAck struct {
	ClientID uint32
}

func AppendLoginAck(dst []byte, m *LoginAck) []byte {
	dst = appendHeader(dst, TypeLoginAck, SizeLoginAck)
	return le.AppendUint32(dst, m.ClientID)
}

func (m *LoginAck) Decode(p []byte) error {
	if err := checkSize(TypeLoginAck, p, SizeLoginAck); err != nil {
		return err
	}
	d := dec{b: p}
	m.ClientID = d.u32()
	return nil
}

// Ack confirms a new order and gives its engine-assigned order id.
//
//	0  seq             u64
//	8  time            i64 (ns since Unix epoch, from the sequencer)
//	16 instrument      u32
//	20 client_order_id u64
//	28 order_id        u64
//	36 side            u8
//	37 price           i64
//	45 qty             i64
type Ack struct {
	Seq        uint64
	Time       int64
	Instrument uint32
	ClOrdID    uint64
	OrderID    uint64
	Side       uint8
	Price      int64
	Qty        int64
}

func AppendAck(dst []byte, m *Ack) []byte {
	dst = appendHeader(dst, TypeAck, SizeAck)
	dst = le.AppendUint64(dst, m.Seq)
	dst = le.AppendUint64(dst, uint64(m.Time))
	dst = le.AppendUint32(dst, m.Instrument)
	dst = le.AppendUint64(dst, m.ClOrdID)
	dst = le.AppendUint64(dst, m.OrderID)
	dst = append(dst, m.Side)
	dst = le.AppendUint64(dst, uint64(m.Price))
	return le.AppendUint64(dst, uint64(m.Qty))
}

func (m *Ack) Decode(p []byte) error {
	if err := checkSize(TypeAck, p, SizeAck); err != nil {
		return err
	}
	d := dec{b: p}
	m.Seq = d.u64()
	m.Time = d.i64()
	m.Instrument = d.u32()
	m.ClOrdID = d.u64()
	m.OrderID = d.u64()
	m.Side = d.u8()
	m.Price = d.i64()
	m.Qty = d.i64()
	return nil
}

// Reject refuses a command. Seq is 0 for rejects raised by the gateway before
// the command was sequenced (reasons 100 and up).
//
//	0  seq             u64
//	8  time            i64
//	16 instrument      u32
//	20 client_order_id u64 (NewOrder) or 0
//	28 order_id        u64 (Cancel) or 0
//	36 reason          u8
type Reject struct {
	Seq        uint64
	Time       int64
	Instrument uint32
	ClOrdID    uint64
	OrderID    uint64
	Reason     Reason
}

func AppendReject(dst []byte, m *Reject) []byte {
	dst = appendHeader(dst, TypeReject, SizeReject)
	dst = le.AppendUint64(dst, m.Seq)
	dst = le.AppendUint64(dst, uint64(m.Time))
	dst = le.AppendUint32(dst, m.Instrument)
	dst = le.AppendUint64(dst, m.ClOrdID)
	dst = le.AppendUint64(dst, m.OrderID)
	return append(dst, uint8(m.Reason))
}

func (m *Reject) Decode(p []byte) error {
	if err := checkSize(TypeReject, p, SizeReject); err != nil {
		return err
	}
	d := dec{b: p}
	m.Seq = d.u64()
	m.Time = d.i64()
	m.Instrument = d.u32()
	m.ClOrdID = d.u64()
	m.OrderID = d.u64()
	m.Reason = Reason(d.u8())
	return nil
}

// Fill reports one side of a match to its owner.
//
//	0  seq             u64
//	8  time            i64
//	16 instrument      u32
//	20 client_order_id u64
//	28 order_id        u64
//	36 match_id        u64 (same on the taker's and maker's fill)
//	44 side            u8  (the recipient's side)
//	45 role            u8  (taker or maker)
//	46 price           i64
//	54 qty             i64
//	62 leaves          i64 (recipient's open qty after this fill)
type Fill struct {
	Seq        uint64
	Time       int64
	Instrument uint32
	ClOrdID    uint64
	OrderID    uint64
	MatchID    uint64
	Side       uint8
	Role       uint8
	Price      int64
	Qty        int64
	Leaves     int64
}

func AppendFill(dst []byte, m *Fill) []byte {
	dst = appendHeader(dst, TypeFill, SizeFill)
	dst = le.AppendUint64(dst, m.Seq)
	dst = le.AppendUint64(dst, uint64(m.Time))
	dst = le.AppendUint32(dst, m.Instrument)
	dst = le.AppendUint64(dst, m.ClOrdID)
	dst = le.AppendUint64(dst, m.OrderID)
	dst = le.AppendUint64(dst, m.MatchID)
	dst = append(dst, m.Side, m.Role)
	dst = le.AppendUint64(dst, uint64(m.Price))
	dst = le.AppendUint64(dst, uint64(m.Qty))
	return le.AppendUint64(dst, uint64(m.Leaves))
}

func (m *Fill) Decode(p []byte) error {
	if err := checkSize(TypeFill, p, SizeFill); err != nil {
		return err
	}
	d := dec{b: p}
	m.Seq = d.u64()
	m.Time = d.i64()
	m.Instrument = d.u32()
	m.ClOrdID = d.u64()
	m.OrderID = d.u64()
	m.MatchID = d.u64()
	m.Side, m.Role = d.u8(), d.u8()
	m.Price = d.i64()
	m.Qty = d.i64()
	m.Leaves = d.i64()
	return nil
}

// CancelAck reports that an order's remaining quantity left the book without
// trading: because the owner cancelled it, or because a market order's
// remainder expired.
//
//	0  seq             u64
//	8  time            i64
//	16 instrument      u32
//	20 client_order_id u64
//	28 order_id        u64
//	36 side            u8
//	37 reason          u8 (CancelRequested or CancelExpired)
//	38 price           i64 (0 for an expired market order)
//	46 qty             i64 (quantity removed)
type CancelAck struct {
	Seq        uint64
	Time       int64
	Instrument uint32
	ClOrdID    uint64
	OrderID    uint64
	Side       uint8
	Reason     uint8
	Price      int64
	Qty        int64
}

func AppendCancelAck(dst []byte, m *CancelAck) []byte {
	dst = appendHeader(dst, TypeCancelAck, SizeCancelAck)
	dst = le.AppendUint64(dst, m.Seq)
	dst = le.AppendUint64(dst, uint64(m.Time))
	dst = le.AppendUint32(dst, m.Instrument)
	dst = le.AppendUint64(dst, m.ClOrdID)
	dst = le.AppendUint64(dst, m.OrderID)
	dst = append(dst, m.Side, m.Reason)
	dst = le.AppendUint64(dst, uint64(m.Price))
	return le.AppendUint64(dst, uint64(m.Qty))
}

func (m *CancelAck) Decode(p []byte) error {
	if err := checkSize(TypeCancelAck, p, SizeCancelAck); err != nil {
		return err
	}
	d := dec{b: p}
	m.Seq = d.u64()
	m.Time = d.i64()
	m.Instrument = d.u32()
	m.ClOrdID = d.u64()
	m.OrderID = d.u64()
	m.Side, m.Reason = d.u8(), d.u8()
	m.Price = d.i64()
	m.Qty = d.i64()
	return nil
}
