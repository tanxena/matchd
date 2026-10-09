package proto

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/tanxena/matchd/internal/testutil"
)

// PRD 5.5: encode/decode with no allocation on the hot path for NewOrder,
// Ack and Fill. Checked two ways, as in internal/engine (decision 007):
// testing.AllocsPerRun, and the strict per-stack count, which also catches
// allocations too rare for AllocsPerRun's integer division to show.

var (
	sampleNewOrder = NewOrder{Instrument: 1, ClOrdID: 42, Side: SideBuy, Type: TypeLimit, TIF: TIFGTC, Price: 1000, Qty: 5}
	sampleAck      = Ack{Seq: 9, Time: 1_700_000_000_000_000_000, Instrument: 1, ClOrdID: 42, OrderID: 77, Side: SideBuy, Price: 1000, Qty: 5}
	sampleFill     = Fill{Seq: 9, Time: 1_700_000_000_000_000_000, Instrument: 1, ClOrdID: 42, OrderID: 77, MatchID: 3,
		Side: SideBuy, Role: RoleTaker, Price: 1000, Qty: 2, Leaves: 3}
)

// loopReader serves the same bytes forever, so a Reader can be measured in
// steady state without the source running dry.
type loopReader struct {
	data []byte
	off  int
}

func (l *loopReader) Read(p []byte) (int, error) {
	n := copy(p, l.data[l.off:])
	l.off = (l.off + n) % len(l.data)
	return n, nil
}

var _ io.Reader = (*loopReader)(nil)

func TestZeroAllocCodec(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("coverage instrumentation allocates")
	}
	buf := make([]byte, 0, 256)
	var no NewOrder
	var ack Ack
	var fill Fill
	noBytes := AppendNewOrder(nil, &sampleNewOrder)[HeaderSize:]
	ackBytes := AppendAck(nil, &sampleAck)[HeaderSize:]
	fillBytes := AppendFill(nil, &sampleFill)[HeaderSize:]

	stream := AppendNewOrder(nil, &sampleNewOrder)
	stream = AppendAck(stream, &sampleAck)
	stream = AppendFill(stream, &sampleFill)
	rd := NewReader(&loopReader{data: bytes.Repeat(stream, 100)})

	ops := []struct {
		name string
		fn   func()
	}{
		{"encode NewOrder", func() { buf = AppendNewOrder(buf[:0], &sampleNewOrder) }},
		{"encode Ack", func() { buf = AppendAck(buf[:0], &sampleAck) }},
		{"encode Fill", func() { buf = AppendFill(buf[:0], &sampleFill) }},
		{"decode NewOrder", func() { _ = no.Decode(noBytes) }},
		{"decode Ack", func() { _ = ack.Decode(ackBytes) }},
		{"decode Fill", func() { _ = fill.Decode(fillBytes) }},
		{"read frame", func() {
			if _, _, err := rd.Next(); err != nil {
				panic(err)
			}
		}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for range 1000 {
				op.fn()
			}
			if avg := testing.AllocsPerRun(10_000, op.fn); avg != 0 {
				t.Errorf("AllocsPerRun = %v, want 0", avg)
			}
			if sites := testutil.OwnAllocations(100_000, op.fn); len(sites) > 0 {
				t.Errorf("allocations:\n%s", strings.Join(sites, "\n"))
			}
		})
	}
}

func BenchmarkCodec(b *testing.B) {
	buf := make([]byte, 0, 256)
	var no NewOrder
	var ack Ack
	var fill Fill
	noBytes := AppendNewOrder(nil, &sampleNewOrder)[HeaderSize:]
	ackBytes := AppendAck(nil, &sampleAck)[HeaderSize:]
	fillBytes := AppendFill(nil, &sampleFill)[HeaderSize:]
	bench := func(name string, fn func()) {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				fn()
			}
		})
	}
	bench("encode NewOrder", func() { buf = AppendNewOrder(buf[:0], &sampleNewOrder) })
	bench("encode Ack", func() { buf = AppendAck(buf[:0], &sampleAck) })
	bench("encode Fill", func() { buf = AppendFill(buf[:0], &sampleFill) })
	bench("decode NewOrder", func() { _ = no.Decode(noBytes) })
	bench("decode Ack", func() { _ = ack.Decode(ackBytes) })
	bench("decode Fill", func() { _ = fill.Decode(fillBytes) })
}
