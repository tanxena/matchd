package snapshot

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/wal"
)

// State is the engine's plain-data state (what a snapshot holds).
type State = engine.State

// Match is one trade as the fill log records it: both sides of a match, as
// reported to the two parties (decision 014).
type Match struct {
	MatchID     uint64
	Seq         engine.Seq // the taker's command
	Time        engine.Timestamp
	Instrument  engine.InstrumentID
	Price       book.Price
	Qty         book.Qty
	TakerSide   book.Side
	TakerOrder  book.OrderID
	TakerClient book.ClientID
	TakerClOrd  book.ClientOrderID
	TakerLeaves book.Qty
	MakerOrder  book.OrderID
	MakerClient book.ClientID
	MakerClOrd  book.ClientOrderID
	MakerLeaves book.Qty
}

// MatchOf combines the taker's and maker's fill events of one match.
func MatchOf(taker, maker engine.Event) Match {
	return Match{
		MatchID: taker.MatchID, Seq: taker.Seq, Time: taker.Time, Instrument: taker.Instrument,
		Price: taker.Price, Qty: taker.Qty, TakerSide: taker.Side,
		TakerOrder: taker.OrderID, TakerClient: taker.Client, TakerClOrd: taker.ClOrdID, TakerLeaves: taker.Leaves,
		MakerOrder: maker.OrderID, MakerClient: maker.Client, MakerClOrd: maker.ClOrdID, MakerLeaves: maker.Leaves,
	}
}

// Fill log record payload (the record's seq is the match id, its time the
// taker command's time):
//
//	cmd_seq u64, instrument u32, price i64, qty i64, taker_side u8,
//	taker_order u64, taker_client u32, taker_clordid u64, taker_leaves i64,
//	maker_order u64, maker_client u32, maker_clordid u64, maker_leaves i64   = 85 bytes
const matchBytes = 85

func appendMatch(b []byte, m *Match) []byte {
	le := binary.LittleEndian
	b = le.AppendUint64(b, uint64(m.Seq))
	b = le.AppendUint32(b, uint32(m.Instrument))
	b = le.AppendUint64(b, uint64(m.Price))
	b = le.AppendUint64(b, uint64(m.Qty))
	b = append(b, byte(m.TakerSide))
	b = le.AppendUint64(b, uint64(m.TakerOrder))
	b = le.AppendUint32(b, uint32(m.TakerClient))
	b = le.AppendUint64(b, uint64(m.TakerClOrd))
	b = le.AppendUint64(b, uint64(m.TakerLeaves))
	b = le.AppendUint64(b, uint64(m.MakerOrder))
	b = le.AppendUint32(b, uint32(m.MakerClient))
	b = le.AppendUint64(b, uint64(m.MakerClOrd))
	return le.AppendUint64(b, uint64(m.MakerLeaves))
}

func decodeMatch(r wal.Record) (Match, error) {
	if len(r.Payload) != matchBytes {
		return Match{}, fmt.Errorf("snapshot: fill record %d is %d bytes, want %d", r.Seq, len(r.Payload), matchBytes)
	}
	rd := reader{b: r.Payload}
	m := Match{MatchID: r.Seq, Time: engine.Timestamp(r.Time)}
	m.Seq = engine.Seq(rd.u64())
	m.Instrument = engine.InstrumentID(rd.u32())
	m.Price = book.Price(rd.u64())
	m.Qty = book.Qty(rd.u64())
	m.TakerSide = book.Side(rd.u8())
	m.TakerOrder = book.OrderID(rd.u64())
	m.TakerClient = book.ClientID(rd.u32())
	m.TakerClOrd = book.ClientOrderID(rd.u64())
	m.TakerLeaves = book.Qty(rd.u64())
	m.MakerOrder = book.OrderID(rd.u64())
	m.MakerClient = book.ClientID(rd.u32())
	m.MakerClOrd = book.ClientOrderID(rd.u64())
	m.MakerLeaves = book.Qty(rd.u64())
	return m, rd.err
}

func fillDir(dataDir string) string { return filepath.Join(dataDir, "fills") }

// FillLog records every match the publisher releases (decision 014). It is
// written by the publisher goroutine only. It is not fsynced: it is an
// audit trail for replay-check, not part of the durability contract, and a
// power failure may cut its tail (replay-check allows missing matches, not
// wrong ones).
type FillLog struct {
	log   *wal.Log
	buf   []byte
	taker engine.Event // the taker half of the match being released
	last  uint64       // last match id recorded
}

// OpenFillLog opens (creating if needed) the fill log in dataDir.
//
// The fill log is an audit trail, not part of the durability contract
// (review F-006), so:
//   - its segments are never fsynced, not even when one fills and a new one
//     starts: the publisher goroutine, which writes it, must never wait on
//     a disk;
//   - a fill log damaged beyond a torn tail (possible after a power
//     failure, since it is never fsynced) doesn't stop the server: it is
//     moved aside to fills.damaged-<unix nanoseconds>, a warning is
//     logged, and a fresh one is started. Replay-check treats the missing
//     matches as allowed.
func OpenFillLog(dataDir string, log *slog.Logger) (*FillLog, error) {
	return openFillLog(dataDir, log, wal.Options{})
}

// OpenFillLogArchived is OpenFillLog, but segments pruned behind the
// snapshots are moved into archiveDir instead of deleted (review F-007).
func OpenFillLogArchived(dataDir string, log *slog.Logger, archiveDir string) (*FillLog, error) {
	return openFillLog(dataDir, log, wal.Options{ArchiveDir: archiveDir})
}

// openFillLog is OpenFillLog with base options (tests inject file wrappers
// and small segments).
func openFillLog(dataDir string, log *slog.Logger, opts wal.Options) (*FillLog, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	opts.AllowGaps, opts.NoSyncOnRoll, opts.Log = true, true, log
	l, info, err := wal.Open(fillDir(dataDir), opts, nil)
	if errors.Is(err, wal.ErrCorrupt) {
		aside := fmt.Sprintf("%s.damaged-%d", fillDir(dataDir), time.Now().UnixNano())
		if rerr := os.Rename(fillDir(dataDir), aside); rerr != nil {
			return nil, fmt.Errorf("snapshot: fill log damaged (%v), and moving it aside failed: %w", err, rerr)
		}
		log.Warn("fill log damaged: moved aside, starting a new one; replay-check will see the older matches as missing",
			"err", err, "moved_to", aside)
		l, info, err = wal.Open(fillDir(dataDir), opts, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot: fill log: %w", err)
	}
	return &FillLog{log: l, last: info.LastSeq}, nil
}

// Released is called by the publisher for every event it releases. Fill
// events arrive as a taker fill followed immediately by its maker fill (the
// engine emits them adjacently and the publisher releases in order); the
// pair becomes one record.
func (f *FillLog) Released(ev *engine.Event) {
	if ev.Kind != engine.EvFill {
		return
	}
	if ev.Role == engine.RoleTaker {
		f.taker = *ev
		return
	}
	if f.taker.MatchID != ev.MatchID || ev.MatchID <= f.last {
		// Impossible unless the publisher reordered events; skip rather than
		// write a record the scanner would reject.
		return
	}
	m := MatchOf(f.taker, *ev)
	f.buf = appendMatch(f.buf[:0], &m)
	f.log.Append(m.MatchID, int64(m.Time), f.buf)
	f.last = m.MatchID
}

// Flush writes the records appended since the last Flush (one write).
func (f *FillLog) Flush() error { return f.log.Flush() }

// LastMatchID returns the last match id recorded.
func (f *FillLog) LastMatchID() uint64 { return f.last }

// RemoveBelow deletes fill-log segments that only hold matches with id <
// matchID (those covered by every retained snapshot).
func (f *FillLog) RemoveBelow(matchID uint64) (int, error) {
	return f.log.RemoveSegmentsBelow(matchID)
}

// Close flushes and closes the fill log.
func (f *FillLog) Close() error {
	err := f.log.Flush()
	if cerr := f.log.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadFillLog returns every recorded match, read-only.
func ReadFillLog(dataDir string) ([]Match, wal.RecoveryInfo, error) {
	var out []Match
	_, info, err := wal.Open(fillDir(dataDir), wal.Options{AllowGaps: true, ReadOnly: true}, func(r wal.Record) error {
		m, err := decodeMatch(r)
		if err != nil {
			return err
		}
		out = append(out, m)
		return nil
	})
	return out, info, err
}
