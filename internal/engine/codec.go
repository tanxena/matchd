package engine

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/tanxena/matchd/internal/book"
)

// Command encoding for the write-ahead log (decision 011). Seq and Time are
// not encoded: the WAL record header carries them, and replay copies them
// back onto the decoded command.
//
// Every command starts with its kind byte, then a fixed layout per kind,
// little-endian:
//
//	NewOrder (36 bytes total)
//	  0  kind       u8
//	  1  instrument u32
//	  5  client     u32
//	  9  clordid    u64
//	  17 side       u8
//	  18 type       u8
//	  19 tif        u8
//	  20 price      i64
//	  28 qty        i64
//
//	Cancel (17 bytes total)
//	  0  kind       u8
//	  1  instrument u32
//	  5  client     u32
//	  9  order_id   u64
//
//	AddInstrument (22 + len(symbol) bytes total)
//	  0  kind       u8
//	  1  client     u32
//	  5  tick_size  i64
//	  13 lot_size   i64
//	  21 symbol_len u8
//	  22 symbol     bytes
//
// This format is persistent: a WAL written today must replay tomorrow.
// Changing it needs a new WAL format version, not an edit (decision 011).

// ErrBadCommand means bytes from the log don't decode as a command. With a
// checksummed WAL that means a bug (or a format change), not disk damage.
var ErrBadCommand = errors.New("engine: undecodable command")

const (
	newOrderSize  = 36
	cancelSize    = 17
	addInstrFixed = 22
	maxSymbolLen  = 255
)

// AppendCommand appends c's encoding to dst.
func AppendCommand(dst []byte, c *Command) []byte {
	le := binary.LittleEndian
	switch c.Kind {
	case CmdNewOrder:
		dst = append(dst, byte(CmdNewOrder))
		dst = le.AppendUint32(dst, uint32(c.Instrument))
		dst = le.AppendUint32(dst, uint32(c.Client))
		dst = le.AppendUint64(dst, uint64(c.ClOrdID))
		dst = append(dst, byte(c.Side), byte(c.Type), byte(c.TIF))
		dst = le.AppendUint64(dst, uint64(c.Price))
		return le.AppendUint64(dst, uint64(c.Qty))
	case CmdCancel:
		dst = append(dst, byte(CmdCancel))
		dst = le.AppendUint32(dst, uint32(c.Instrument))
		dst = le.AppendUint32(dst, uint32(c.Client))
		return le.AppendUint64(dst, uint64(c.OrderID))
	case CmdAddInstrument:
		sym := c.Spec.Symbol
		if len(sym) > maxSymbolLen {
			// Reject at the source rather than writing an unreplayable record.
			panic(fmt.Sprintf("engine: symbol of %d bytes exceeds %d", len(sym), maxSymbolLen))
		}
		dst = append(dst, byte(CmdAddInstrument))
		dst = le.AppendUint32(dst, uint32(c.Client))
		dst = le.AppendUint64(dst, uint64(c.Spec.TickSize))
		dst = le.AppendUint64(dst, uint64(c.Spec.LotSize))
		dst = append(dst, byte(len(sym)))
		return append(dst, sym...)
	}
	panic(fmt.Sprintf("engine: cannot encode command kind %v", c.Kind))
}

// DecodeCommand decodes p into c, leaving c.Seq and c.Time untouched.
func DecodeCommand(p []byte, c *Command) error {
	if len(p) == 0 {
		return fmt.Errorf("%w: empty", ErrBadCommand)
	}
	le := binary.LittleEndian
	kind := CommandKind(p[0])
	seq, tm := c.Seq, c.Time
	switch kind {
	case CmdNewOrder:
		if len(p) != newOrderSize {
			return fmt.Errorf("%w: NewOrder is %d bytes, want %d", ErrBadCommand, len(p), newOrderSize)
		}
		*c = Command{
			Kind:       CmdNewOrder,
			Instrument: InstrumentID(le.Uint32(p[1:])),
			Client:     book.ClientID(le.Uint32(p[5:])),
			ClOrdID:    book.ClientOrderID(le.Uint64(p[9:])),
			Side:       book.Side(p[17]),
			Type:       book.OrderType(p[18]),
			TIF:        book.TIF(p[19]),
			Price:      book.Price(le.Uint64(p[20:])),
			Qty:        book.Qty(le.Uint64(p[28:])),
		}
	case CmdCancel:
		if len(p) != cancelSize {
			return fmt.Errorf("%w: Cancel is %d bytes, want %d", ErrBadCommand, len(p), cancelSize)
		}
		*c = Command{
			Kind:       CmdCancel,
			Instrument: InstrumentID(le.Uint32(p[1:])),
			Client:     book.ClientID(le.Uint32(p[5:])),
			OrderID:    book.OrderID(le.Uint64(p[9:])),
		}
	case CmdAddInstrument:
		if len(p) < addInstrFixed || len(p) != addInstrFixed+int(p[21]) {
			return fmt.Errorf("%w: AddInstrument length %d inconsistent", ErrBadCommand, len(p))
		}
		*c = Command{
			Kind:   CmdAddInstrument,
			Client: book.ClientID(le.Uint32(p[1:])),
			Spec: book.Instrument{
				TickSize: int64(le.Uint64(p[5:])),
				LotSize:  int64(le.Uint64(p[13:])),
				Symbol:   string(p[22:]),
			},
		}
	default:
		return fmt.Errorf("%w: unknown kind %d", ErrBadCommand, p[0])
	}
	c.Seq, c.Time = seq, tm
	return nil
}
