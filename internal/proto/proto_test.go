package proto

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
	"testing/iotest"
)

// codec ties one message type to its encoder and a fresh decoder, so every
// test below can loop over all types.
type codec struct {
	typ    MsgType
	size   int
	random func(r *rand.Rand) any         // a message with random fields
	encode func(dst []byte, m any) []byte // AppendX
	decode func(p []byte) (any, error)    // new value, Decode
}

func u8(r *rand.Rand) uint8   { return uint8(r.Uint32()) }
func u32(r *rand.Rand) uint32 { return r.Uint32() }
func u64(r *rand.Rand) uint64 { return r.Uint64() }
func i64(r *rand.Rand) int64  { return int64(r.Uint64()) }

var codecs = []codec{
	{TypeLogin, SizeLogin,
		func(r *rand.Rand) any { return Login{ClientID: u32(r)} },
		func(dst []byte, m any) []byte { v := m.(Login); return AppendLogin(dst, &v) },
		func(p []byte) (any, error) { var m Login; err := m.Decode(p); return m, err }},
	{TypeNewOrder, SizeNewOrder,
		func(r *rand.Rand) any {
			return NewOrder{Instrument: u32(r), ClOrdID: u64(r), Side: u8(r), Type: u8(r), TIF: u8(r), Price: i64(r), Qty: i64(r)}
		},
		func(dst []byte, m any) []byte { v := m.(NewOrder); return AppendNewOrder(dst, &v) },
		func(p []byte) (any, error) { var m NewOrder; err := m.Decode(p); return m, err }},
	{TypeCancel, SizeCancel,
		func(r *rand.Rand) any { return Cancel{Instrument: u32(r), OrderID: u64(r)} },
		func(dst []byte, m any) []byte { v := m.(Cancel); return AppendCancel(dst, &v) },
		func(p []byte) (any, error) { var m Cancel; err := m.Decode(p); return m, err }},
	{TypeSubscribe, SizeSubscribe,
		func(r *rand.Rand) any { return Subscribe{Instrument: u32(r)} },
		func(dst []byte, m any) []byte { v := m.(Subscribe); return AppendSubscribe(dst, &v) },
		func(p []byte) (any, error) { var m Subscribe; err := m.Decode(p); return m, err }},
	{TypeHeartbeat, SizeHeartbeat,
		func(r *rand.Rand) any { return struct{}{} },
		func(dst []byte, _ any) []byte { return AppendHeartbeat(dst) },
		func(p []byte) (any, error) { return struct{}{}, DecodeHeartbeat(p) }},
	{TypeLoginAck, SizeLoginAck,
		func(r *rand.Rand) any { return LoginAck{ClientID: u32(r)} },
		func(dst []byte, m any) []byte { v := m.(LoginAck); return AppendLoginAck(dst, &v) },
		func(p []byte) (any, error) { var m LoginAck; err := m.Decode(p); return m, err }},
	{TypeAck, SizeAck,
		func(r *rand.Rand) any {
			return Ack{Seq: u64(r), Time: i64(r), Instrument: u32(r), ClOrdID: u64(r), OrderID: u64(r), Side: u8(r), Price: i64(r), Qty: i64(r)}
		},
		func(dst []byte, m any) []byte { v := m.(Ack); return AppendAck(dst, &v) },
		func(p []byte) (any, error) { var m Ack; err := m.Decode(p); return m, err }},
	{TypeReject, SizeReject,
		func(r *rand.Rand) any {
			return Reject{Seq: u64(r), Time: i64(r), Instrument: u32(r), ClOrdID: u64(r), OrderID: u64(r), Reason: Reason(u8(r))}
		},
		func(dst []byte, m any) []byte { v := m.(Reject); return AppendReject(dst, &v) },
		func(p []byte) (any, error) { var m Reject; err := m.Decode(p); return m, err }},
	{TypeFill, SizeFill,
		func(r *rand.Rand) any {
			return Fill{Seq: u64(r), Time: i64(r), Instrument: u32(r), ClOrdID: u64(r), OrderID: u64(r), MatchID: u64(r),
				Side: u8(r), Role: u8(r), Price: i64(r), Qty: i64(r), Leaves: i64(r)}
		},
		func(dst []byte, m any) []byte { v := m.(Fill); return AppendFill(dst, &v) },
		func(p []byte) (any, error) { var m Fill; err := m.Decode(p); return m, err }},
	{TypeCancelAck, SizeCancelAck,
		func(r *rand.Rand) any {
			return CancelAck{Seq: u64(r), Time: i64(r), Instrument: u32(r), ClOrdID: u64(r), OrderID: u64(r),
				Side: u8(r), Reason: u8(r), Price: i64(r), Qty: i64(r)}
		},
		func(dst []byte, m any) []byte { v := m.(CancelAck); return AppendCancelAck(dst, &v) },
		func(p []byte) (any, error) { var m CancelAck; err := m.Decode(p); return m, err }},
}

func codecFor(t MsgType) *codec {
	for i := range codecs {
		if codecs[i].typ == t {
			return &codecs[i]
		}
	}
	return nil
}

// Every message survives encode -> frame reader -> decode, many frames back
// to back on one stream, read one byte at a time (worst-case fragmentation).
func TestRoundTripAllTypes(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var stream []byte
	var want []any
	var types []MsgType
	for range 200 {
		for _, c := range codecs {
			m := c.random(rng)
			before := len(stream)
			stream = c.encode(stream, m)
			if got := len(stream) - before; got != HeaderSize+c.size {
				t.Fatalf("%v frame is %d bytes, want %d", c.typ, got, HeaderSize+c.size)
			}
			want = append(want, m)
			types = append(types, c.typ)
		}
	}
	r := NewReader(iotest.OneByteReader(bytes.NewReader(stream)))
	for i := range want {
		typ, payload, err := r.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if typ != types[i] {
			t.Fatalf("frame %d: type %v, want %v", i, typ, types[i])
		}
		got, err := codecFor(typ).decode(payload)
		if err != nil {
			t.Fatalf("frame %d (%v): %v", i, typ, err)
		}
		if got != want[i] {
			t.Fatalf("frame %d (%v):\n got  %+v\n want %+v", i, typ, got, want[i])
		}
	}
	if _, _, err := r.Next(); err != io.EOF {
		t.Fatalf("after last frame: %v, want io.EOF", err)
	}
}

// Golden bytes pin the documented layout. If someone reorders fields in both
// the encoder and decoder, the round trip still passes; this doesn't.
func TestNewOrderGoldenBytes(t *testing.T) {
	m := NewOrder{Instrument: 0x04030201, ClOrdID: 0x0c0b0a0908070605, Side: 1, Type: 2, TIF: 1,
		Price: 0x161514131211100f, Qty: 0x1e1d1c1b1a191817}
	want := []byte{
		33, 0, 0, 0, // length = 2 + 31
		Version, byte(TypeNewOrder),
		0x01, 0x02, 0x03, 0x04, // instrument
		0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, // client_order_id
		1, 2, 1, // side, type, tif
		0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, // price
		0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, // qty
	}
	if got := AppendNewOrder(nil, &m); !bytes.Equal(got, want) {
		t.Fatalf("encoding:\n got  % x\n want % x", got, want)
	}
}

func TestDecodeRejectsWrongSize(t *testing.T) {
	for _, c := range codecs {
		for _, n := range []int{c.size - 1, c.size + 1} {
			if n < 0 {
				continue
			}
			if _, err := c.decode(make([]byte, n)); !errors.Is(err, ErrPayloadSize) {
				t.Errorf("%v with %d-byte payload: err %v, want ErrPayloadSize", c.typ, n, err)
			}
		}
	}
}

func TestReaderErrors(t *testing.T) {
	good := AppendLogin(nil, &Login{ClientID: 7})
	cases := []struct {
		name   string
		stream []byte
		want   error
	}{
		{"empty stream", nil, io.EOF},
		{"truncated header", good[:3], io.ErrUnexpectedEOF},
		{"truncated payload", good[:len(good)-1], io.ErrUnexpectedEOF},
		{"length below 2", []byte{1, 0, 0, 0, Version, 1}, ErrFrameLength},
		{"length too large", []byte{0xff, 0xff, 0, 0, Version, 1}, ErrFrameLength},
		{"wrong version", []byte{6, 0, 0, 0, Version + 1, byte(TypeLogin), 7, 0, 0, 0}, ErrVersion},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := NewReader(bytes.NewReader(c.stream)).Next()
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// The frame boundary check must use the declared length, so a payload of
// exactly MaxPayload is accepted and one byte more is not.
func TestReaderMaxPayloadBoundary(t *testing.T) {
	frame := func(n int) []byte {
		b := appendHeader(nil, TypeHeartbeat, n)
		return append(b, make([]byte, n)...)
	}
	if _, p, err := NewReader(bytes.NewReader(frame(MaxPayload))).Next(); err != nil || len(p) != MaxPayload {
		t.Fatalf("MaxPayload frame: len %d err %v", len(p), err)
	}
	if _, _, err := NewReader(bytes.NewReader(frame(MaxPayload + 1))).Next(); !errors.Is(err, ErrFrameLength) {
		t.Fatalf("MaxPayload+1 frame: err %v, want ErrFrameLength", err)
	}
}

// FuzzReader: arbitrary bytes must never panic the reader or a decoder, and
// any frame that decodes must re-encode to exactly the bytes it came from
// (the encoding is canonical, so there is one byte form per message).
func FuzzReader(f *testing.F) {
	rng := rand.New(rand.NewPCG(3, 4))
	for _, c := range codecs {
		f.Add(c.encode(nil, c.random(rng)))
	}
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, Version, 0x02})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(bytes.NewReader(data))
		consumed := 0
		for {
			typ, payload, err := r.Next()
			if err != nil {
				return
			}
			frameLen := HeaderSize + len(payload)
			raw := data[consumed : consumed+frameLen]
			consumed += frameLen
			c := codecFor(typ)
			if c == nil {
				continue // unknown type: the gateway closes the connection
			}
			m, err := c.decode(payload)
			if err != nil {
				continue
			}
			if re := c.encode(nil, m); !bytes.Equal(re, raw) {
				t.Fatalf("%v: decoded then re-encoded differently:\n in  % x\n out % x", typ, raw, re)
			}
		}
	})
}
