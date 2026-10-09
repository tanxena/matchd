package engine_test

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
)

func randomCommand(r *rand.Rand) engine.Command {
	switch r.IntN(3) {
	case 0:
		return engine.Command{Kind: engine.CmdNewOrder, Instrument: engine.InstrumentID(r.Uint32()),
			Client: book.ClientID(r.Uint32()), ClOrdID: book.ClientOrderID(r.Uint64()),
			Side: book.Side(r.Uint32()), Type: book.OrderType(r.Uint32()), TIF: book.TIF(r.Uint32()),
			Price: book.Price(r.Uint64()), Qty: book.Qty(r.Uint64())}
	case 1:
		return engine.Command{Kind: engine.CmdCancel, Instrument: engine.InstrumentID(r.Uint32()),
			Client: book.ClientID(r.Uint32()), OrderID: book.OrderID(r.Uint64())}
	}
	sym := make([]byte, r.IntN(256))
	for i := range sym {
		sym[i] = byte('A' + r.IntN(26))
	}
	return engine.Command{Kind: engine.CmdAddInstrument, Client: book.ClientID(r.Uint32()),
		Spec: book.Instrument{Symbol: string(sym), TickSize: int64(r.Uint64()), LotSize: int64(r.Uint64())}}
}

func TestCommandRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for range 10_000 {
		c := randomCommand(r)
		p := engine.AppendCommand(nil, &c)
		got := engine.Command{Seq: 77, Time: 88} // Decode must leave these alone
		if err := engine.DecodeCommand(p, &got); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		c.Seq, c.Time = 77, 88
		if got != c {
			t.Fatalf("round trip:\n got  %+v\n want %+v", got, c)
		}
	}
}

// Golden bytes: the WAL format is persistent, so its layout is pinned.
func TestCommandGoldenBytes(t *testing.T) {
	c := engine.Command{Kind: engine.CmdNewOrder, Instrument: 0x04030201, Client: 0x08070605,
		ClOrdID: 0x100f0e0d0c0b0a09, Side: 1, Type: 2, TIF: 1, Price: 0x1c1b1a1918171615, Qty: 0x24232221201f1e1d}
	want := []byte{
		1,                      // kind NewOrder
		0x01, 0x02, 0x03, 0x04, // instrument
		0x05, 0x06, 0x07, 0x08, // client
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, // clordid
		1, 2, 1, // side, type, tif
		0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, // price
		0x1d, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, // qty
	}
	if got := engine.AppendCommand(nil, &c); !bytes.Equal(got, want) {
		t.Fatalf("encoding:\n got  % x\n want % x", got, want)
	}
	cancel := engine.Command{Kind: engine.CmdCancel, Instrument: 1, Client: 2, OrderID: 3}
	if got := engine.AppendCommand(nil, &cancel); !bytes.Equal(got, []byte{2, 1, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("cancel encoding % x", got)
	}
}

func TestDecodeCommandRejectsBadInput(t *testing.T) {
	ok := engine.Command{Kind: engine.CmdNewOrder, Instrument: 1, Qty: 1}
	good := engine.AppendCommand(nil, &ok)
	add := engine.Command{Kind: engine.CmdAddInstrument, Spec: book.Instrument{Symbol: "XYZ", TickSize: 1, LotSize: 1}}
	goodAdd := engine.AppendCommand(nil, &add)
	bad := map[string][]byte{
		"empty":                  nil,
		"unknown kind":           {9, 0, 0},
		"short NewOrder":         good[:len(good)-1],
		"long NewOrder":          append(append([]byte{}, good...), 0),
		"short Cancel":           {2, 1, 0, 0},
		"symbol length too long": append(append([]byte{}, goodAdd[:21]...), 10, 'X'),
		"AddInstrument fragment": goodAdd[:10],
	}
	for name, p := range bad {
		var c engine.Command
		if err := engine.DecodeCommand(p, &c); !errors.Is(err, engine.ErrBadCommand) {
			t.Errorf("%s: err = %v, want ErrBadCommand", name, err)
		}
	}
}

func FuzzDecodeCommand(f *testing.F) {
	r := rand.New(rand.NewPCG(7, 8))
	for range 6 {
		c := randomCommand(r)
		f.Add(engine.AppendCommand(nil, &c))
	}
	f.Fuzz(func(t *testing.T, p []byte) {
		var c engine.Command
		if engine.DecodeCommand(p, &c) != nil {
			return
		}
		if re := engine.AppendCommand(nil, &c); !bytes.Equal(re, p) {
			t.Fatalf("decode then encode changed the bytes:\n in  % x\n out % x", p, re)
		}
	})
}

func TestCommandEncodingAllocs(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("coverage instrumentation allocates")
	}
	c := engine.Command{Kind: engine.CmdNewOrder, Instrument: 1, Client: 2, ClOrdID: 3, Side: 1, Type: 1, TIF: 1, Price: 100, Qty: 5}
	buf := make([]byte, 0, 64)
	var out engine.Command
	if n := testing.AllocsPerRun(10_000, func() {
		buf = engine.AppendCommand(buf[:0], &c)
		_ = engine.DecodeCommand(buf, &out)
	}); n != 0 {
		t.Fatalf("NewOrder encode+decode: %v allocs", n)
	}
}
