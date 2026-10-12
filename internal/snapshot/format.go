// Package snapshot writes and reads engine snapshots, keeps the manifest,
// records the fill log, orchestrates recovery, and implements replay-check
// (decisions 013 and 014).
//
// Data directory layout:
//
//	<data>/wal/*.log                command WAL (internal/wal)
//	<data>/fills/*.log              fill log: one record per released match
//	<data>/snapshots/<seq>.snap     snapshot files
//	<data>/manifest.json            which snapshots are valid, oldest first
package snapshot

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
)

// Snapshot file layout (all integers little-endian):
//
//	header        "MTCHSNP" version(1)                     8 bytes
//	seq           u64   last command included
//	last_order_id u64
//	last_match_id u64
//	n_instruments u32
//	per instrument, in id order:
//	  symbol_len u8, symbol, tick_size i64, lot_size i64
//	  n_bids u32, bids...    in priority order
//	  n_asks u32, asks...    in priority order
//	  order = id u64, client u32, clordid u64, price i64, qty i64   (36 bytes)
//	crc           u32   CRC32C of every byte before it
const (
	snapMagic   = "MTCHSNP"
	snapVersion = 1
	orderBytes  = 36
)

// ErrCorruptSnapshot means a snapshot file fails its checks.
var ErrCorruptSnapshot = errors.New("snapshot: corrupt")

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Encode serialises st.
func Encode(st engine.State) []byte {
	le := binary.LittleEndian
	n := 8 + 28
	for _, is := range st.Instruments {
		n += 1 + len(is.Spec.Symbol) + 16 + 8 + orderBytes*(len(is.Bids)+len(is.Asks))
	}
	b := make([]byte, 0, n+4)
	b = append(b, snapMagic...)
	b = append(b, snapVersion)
	b = le.AppendUint64(b, uint64(st.Seq))
	b = le.AppendUint64(b, uint64(st.LastOrderID))
	b = le.AppendUint64(b, st.LastMatchID)
	b = le.AppendUint32(b, uint32(len(st.Instruments)))
	for _, is := range st.Instruments {
		b = append(b, byte(len(is.Spec.Symbol)))
		b = append(b, is.Spec.Symbol...)
		b = le.AppendUint64(b, uint64(is.Spec.TickSize))
		b = le.AppendUint64(b, uint64(is.Spec.LotSize))
		for _, side := range [2][]book.RestingOrder{is.Bids, is.Asks} {
			b = le.AppendUint32(b, uint32(len(side)))
			for _, r := range side {
				b = le.AppendUint64(b, uint64(r.ID))
				b = le.AppendUint32(b, uint32(r.Client))
				b = le.AppendUint64(b, uint64(r.ClOrdID))
				b = le.AppendUint64(b, uint64(r.Price))
				b = le.AppendUint64(b, uint64(r.Qty))
			}
		}
	}
	return le.AppendUint32(b, crc32.Checksum(b, castagnoli))
}

// Decode parses a snapshot file. Any inconsistency (bad magic or version,
// bad checksum, truncation, trailing bytes) returns ErrCorruptSnapshot.
func Decode(b []byte) (engine.State, error) {
	var st engine.State
	bad := func(format string, args ...any) (engine.State, error) {
		return engine.State{}, fmt.Errorf("%w: %s", ErrCorruptSnapshot, fmt.Sprintf(format, args...))
	}
	if len(b) < 8+28+4 {
		return bad("only %d bytes", len(b))
	}
	if string(b[:7]) != snapMagic || b[7] != snapVersion {
		return bad("bad header % x", b[:8])
	}
	body, sum := b[:len(b)-4], binary.LittleEndian.Uint32(b[len(b)-4:])
	if got := crc32.Checksum(body, castagnoli); got != sum {
		return bad("checksum mismatch (stored %08x, computed %08x)", sum, got)
	}
	r := reader{b: body, off: 8}
	st.Seq = engine.Seq(r.u64())
	st.LastOrderID = book.OrderID(r.u64())
	st.LastMatchID = r.u64()
	ninst := r.u32()
	for i := uint32(0); i < ninst && r.err == nil; i++ {
		var is engine.InstrumentState
		is.Spec.Symbol = string(r.bytes(int(r.u8())))
		is.Spec.TickSize = int64(r.u64())
		is.Spec.LotSize = int64(r.u64())
		for si, side := range [2]book.Side{book.Buy, book.Sell} {
			n := int(r.u32())
			if n*orderBytes > len(body)-r.off {
				return bad("instrument %d claims %d orders, only %d bytes left", i+1, n, len(body)-r.off)
			}
			var orders []book.RestingOrder // nil when empty, as engine.State produces
			if n > 0 {
				orders = make([]book.RestingOrder, 0, n)
			}
			for range n {
				orders = append(orders, book.RestingOrder{
					ID: book.OrderID(r.u64()), Client: book.ClientID(r.u32()), ClOrdID: book.ClientOrderID(r.u64()),
					Side: side, Price: book.Price(r.u64()), Qty: book.Qty(r.u64()),
				})
			}
			if si == 0 {
				is.Bids = orders
			} else {
				is.Asks = orders
			}
		}
		st.Instruments = append(st.Instruments, is)
	}
	if r.err != nil {
		return bad("%v", r.err)
	}
	if r.off != len(body) {
		return bad("%d trailing bytes", len(body)-r.off)
	}
	return st, nil
}

// reader is a bounds-checked little-endian cursor; the first overrun sets
// err and every later read returns zero.
type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil || n > len(r.b)-r.off {
		if r.err == nil {
			r.err = fmt.Errorf("truncated at offset %d (wanted %d bytes)", r.off, n)
		}
		return nil
	}
	p := r.b[r.off : r.off+n]
	r.off += n
	return p
}

func (r *reader) u8() uint8 {
	if p := r.take(1); p != nil {
		return p[0]
	}
	return 0
}

func (r *reader) u32() uint32 {
	if p := r.take(4); p != nil {
		return binary.LittleEndian.Uint32(p)
	}
	return 0
}

func (r *reader) u64() uint64 {
	if p := r.take(8); p != nil {
		return binary.LittleEndian.Uint64(p)
	}
	return 0
}

func (r *reader) bytes(n int) []byte { return r.take(n) }
