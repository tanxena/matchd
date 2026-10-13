// Package wal is the write-ahead log: an append-only, segmented file of
// sequenced records, each protected by a CRC32C (decision 011).
//
// On-disk layout:
//
//	<dir>/<first seq, 20 digits>.log      one file per segment
//
//	segment = header record*
//	header  = "MTCHWAL" version(1 byte)                    8 bytes
//	record  = length u32 | crc u32 | seq u64 | time i64 | payload
//	          length = 16 + len(payload): the bytes after the crc field
//	          crc    = CRC32C (Castagnoli) of those same bytes
//
// All integers little-endian. The payload is opaque here; the sequencer
// stores an encoded engine.Command in it.
package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tanxena/matchd/internal/clock"
	"github.com/tanxena/matchd/internal/fsutil"
	"github.com/tanxena/matchd/internal/metrics"
)

const (
	segmentMagic  = "MTCHWAL"
	formatVersion = 1
	headerSize    = 8
	recordHeader  = 8  // length + crc
	recordFixed   = 16 // seq + time, counted in length
	// MaxPayload bounds one record's payload, so a corrupt length field
	// can't make the scanner skip megabytes. The largest command is well
	// under 300 bytes. Keeping this small also keeps small the window in
	// which a corrupted length near the end of the log could pass for a
	// torn tail (see parseRecord).
	MaxPayload = 4 << 10
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt means the log is damaged somewhere other than a torn tail.
// Recovery must not continue automatically: a human decides.
var ErrCorrupt = errors.New("wal: corrupt log")

// File is what the log needs from an open segment file. *os.File satisfies
// it. Tests substitute wrappers that count, delay or fail Sync.
type File interface {
	io.Writer
	Sync() error
	Close() error
}

// Options configures a Log.
type Options struct {
	// SegmentSize: start a new segment once the current one reaches this
	// many bytes. 0 = 64 MiB.
	SegmentSize int64
	// OpenFile opens (creating if needed) a segment for appending. nil =
	// os.OpenFile with O_CREATE|O_WRONLY|O_APPEND.
	OpenFile func(path string) (File, error)
	// AllowGaps relaxes sequence continuity from "each record is the
	// previous seq + 1" to "strictly increasing". The command WAL never sets
	// it; the fill log, keyed by match id, does (decision 014).
	AllowGaps bool
	// ReadOnly opens the log only to read it: a torn tail is reported in
	// RecoveryInfo but not truncated, nothing is created, and the returned
	// Log must not be appended to. replay-check uses it.
	ReadOnly bool
	// Log receives recovery messages (torn tails). nil = discard.
	Log *slog.Logger
	// FsyncLatency, if set, records every fsync's duration (decision 017).
	FsyncLatency *metrics.Histogram
}

// Record is one log entry as seen by a replay callback. Payload aliases an
// internal buffer and is only valid during the callback.
type Record struct {
	Seq     uint64
	Time    int64
	Payload []byte
}

// RecoveryInfo describes what Open found.
type RecoveryInfo struct {
	Segments  int    // segment files present after recovery
	Records   int    // records replayed
	FirstSeq  uint64 // first record's seq (0 if the log is empty)
	LastSeq   uint64 // last record's seq (0 if the log is empty)
	TornBytes int64  // bytes truncated from the tail of the last segment
}

// Log is an open write-ahead log.
//
// Concurrency: exactly one goroutine appends (Append and Flush: the
// sequencer). Sync may run concurrently on another goroutine (the group
// commit syncer). LastAppended and LastSynced may be read from anywhere.
type Log struct {
	dir  string
	opts Options
	log  *slog.Logger

	// mu guards cur, curPath and curSize. Flush holds it while writing (a
	// short write syscall); Sync holds it only to read cur, never while
	// fsyncing, so a slow fsync never blocks an append.
	mu      sync.Mutex
	cur     File
	curPath string
	curSize int64

	// syncMu serialises Sync with segment rolls, so a segment file is
	// never closed while a Sync on it is in progress.
	syncMu sync.Mutex

	appended atomic.Uint64 // last seq whose bytes have been written to the OS
	synced   atomic.Uint64 // last seq covered by a successful fsync
	written  atomic.Uint64 // bytes written to segments since Open (metrics)

	pending      []byte // records appended but not yet flushed (appender only)
	pendingLast  uint64 // seq of the last record in pending
	pendingFirst uint64 // seq of the first record in pending (0 if empty)
	closed       bool
}

// Open scans every segment in dir in order, calling replay (if non-nil) for
// each record, verifies checksums and sequence continuity, truncates a torn
// tail from the last segment, and returns a Log positioned to append after
// the last record.
//
// A torn tail is a damaged record at the end of the last segment, followed
// only by end of file or zero bytes: the shape an interrupted write leaves.
// Damage anywhere else (a bad record followed by more data, any bad record
// in an earlier segment, a sequence gap, a bad segment header) returns an
// error wrapping ErrCorrupt.
func Open(dir string, opts Options, replay func(Record) error) (*Log, RecoveryInfo, error) {
	if opts.SegmentSize <= 0 {
		opts.SegmentSize = 64 << 20
	}
	if opts.OpenFile == nil {
		opts.OpenFile = openAppend
	}
	l := &Log{dir: dir, opts: opts, log: opts.Log}
	if l.log == nil {
		l.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.ReadOnly {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return l, RecoveryInfo{}, nil // nothing logged yet
		}
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, RecoveryInfo{}, err
	}
	segs, err := listSegments(dir)
	if err != nil {
		return nil, RecoveryInfo{}, err
	}

	var info RecoveryInfo
	var last uint64
	var lastSize int64 // size of the last remaining segment after recovery
	for i, seg := range segs {
		isLast := i == len(segs)-1
		res, err := scanSegment(seg.path, seg.first, last, isLast, opts.AllowGaps, replay)
		if err != nil {
			return nil, info, err
		}
		if res.records > 0 && info.FirstSeq == 0 {
			info.FirstSeq = seg.first
		}
		info.Records += res.records
		if res.records > 0 {
			last = res.lastSeq
		}
		lastSize = res.size
		if isLast && res.tornAt >= 0 && opts.ReadOnly {
			info.TornBytes = res.size - res.tornAt
		} else if isLast && res.tornAt >= 0 {
			info.TornBytes = res.size - res.tornAt
			l.log.Warn("wal: truncating torn tail", "segment", seg.path,
				"offset", res.tornAt, "bytes", info.TornBytes, "last_good_seq", last)
			if res.tornAt < headerSize {
				// Not even a complete header: the segment never held a
				// record. Remove it; the previous segment (if any) becomes
				// the one to append to.
				if err := os.Remove(seg.path); err != nil {
					return nil, info, err
				}
				segs = segs[:i]
				if len(segs) > 0 {
					st, err := os.Stat(segs[len(segs)-1].path)
					if err != nil {
						return nil, info, err
					}
					lastSize = st.Size()
				}
				break
			}
			if err := os.Truncate(seg.path, res.tornAt); err != nil {
				return nil, info, err
			}
			lastSize = res.tornAt
		}
	}
	if len(segs) > 0 {
		l.curPath, l.curSize = segs[len(segs)-1].path, lastSize
	}
	info.Segments = len(segs)
	info.LastSeq = last
	l.appended.Store(last)
	l.synced.Store(last) // everything that survived on disk counts as durable

	if l.curPath != "" && !opts.ReadOnly {
		f, err := opts.OpenFile(l.curPath)
		if err != nil {
			return nil, info, err
		}
		l.cur = f
	}
	return l, info, nil
}

func openAppend(path string) (File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

type segment struct {
	path  string
	first uint64
}

func segmentName(first uint64) string { return fmt.Sprintf("%020d.log", first) }

func listSegments(dir string) ([]segment, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var segs []segment
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".log") {
			continue
		}
		first, err := strconv.ParseUint(strings.TrimSuffix(name, ".log"), 10, 64)
		if err != nil || len(name) != 24 {
			return nil, fmt.Errorf("%w: unexpected file %q in WAL directory", ErrCorrupt, name)
		}
		segs = append(segs, segment{path: filepath.Join(dir, name), first: first})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].first < segs[j].first })
	return segs, nil
}

type scanResult struct {
	records int
	lastSeq uint64
	size    int64 // file size
	tornAt  int64 // offset of a torn tail to truncate, or -1
}

// scanSegment reads one segment file and scans it. prev is the seq of the
// record before this segment (0 at the start of the log).
func scanSegment(path string, first, prev uint64, isLast, gaps bool, replay func(Record) error) (scanResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return scanResult{tornAt: -1}, err
	}
	return scanBytes(filepath.Base(path), data, first, prev, isLast, gaps, replay)
}

// scanBytes is the whole segment-parsing logic, on bytes in memory, so it
// can be fuzzed without touching the file system.
func scanBytes(name string, data []byte, first, prev uint64, isLast, gaps bool, replay func(Record) error) (scanResult, error) {
	res := scanResult{tornAt: -1, size: int64(len(data))}
	corrupt := func(off int, format string, args ...any) (scanResult, error) {
		return res, fmt.Errorf("%w: %s at offset %d: %s", ErrCorrupt, name, off, fmt.Sprintf(format, args...))
	}

	// Header.
	if len(data) < headerSize {
		if isLast && allZero(data) || isLast && bytes.HasPrefix([]byte(segmentMagic), data) {
			res.tornAt = 0 // the crash hit while writing the header
			return res, nil
		}
		return corrupt(0, "segment shorter than its header")
	}
	if string(data[:7]) != segmentMagic {
		return corrupt(0, "bad magic %q", data[:7])
	}
	if data[7] != formatVersion {
		return corrupt(7, "unsupported format version %d", data[7])
	}

	off := headerSize
	expect := prev + 1
	if prev == 0 {
		expect = first // the first segment of the log starts wherever it starts
	}
	for off < len(data) {
		rec, n, problem := parseRecord(data[off:])
		if problem != "" {
			// A damaged record. It's a torn tail only if it's in the last
			// segment and nothing but zeros follows the damage.
			if isLast && tailIsTorn(data[off:], n) {
				res.tornAt = int64(off)
				return res, nil
			}
			return corrupt(off, "%s", problem)
		}
		if res.records == 0 && rec.Seq != first {
			return corrupt(off, "first record has seq %d, segment name says %d", rec.Seq, first)
		}
		if gaps {
			if rec.Seq < expect {
				return corrupt(off, "seq %d after %d: not increasing", rec.Seq, expect-1)
			}
		} else if rec.Seq != expect {
			return corrupt(off, "seq %d, expected %d (gap or repeat)", rec.Seq, expect)
		}
		if replay != nil {
			if err := replay(rec); err != nil {
				return res, fmt.Errorf("wal: replaying seq %d: %w", rec.Seq, err)
			}
		}
		res.records++
		res.lastSeq = rec.Seq
		expect = rec.Seq + 1
		off += n
	}
	return res, nil
}

// parseRecord decodes the record at the start of b. It returns the record,
// its total size, and a description of the problem if it is damaged.
//
// For a damaged record, n says where the damage ends, for tailIsTorn: the
// bytes after b[n:] must all be zero for the damage to count as a torn
// write.
//   - Fewer bytes than a record header, or a record that runs past the end
//     of the file: n = len(b). An interrupted write of the final record
//     looks exactly like this.
//   - A checksum mismatch: n = the record's size; any later bytes must be
//     zero.
//   - A length out of range: n = 0, so the whole remainder must be zero
//     (a zero-filled tail reads as length 0). A real torn write always
//     carries the correct length, because the length is written first; a
//     garbage length followed by data is corruption.
//
// Known blind spot: a length corrupted to a value that is in range but runs
// past the end of the file, within the last MaxPayload bytes, is
// indistinguishable from a torn final record and is truncated.
func parseRecord(b []byte) (rec Record, n int, problem string) {
	if len(b) < recordHeader+recordFixed {
		return rec, len(b), "incomplete record header"
	}
	length := binary.LittleEndian.Uint32(b[0:4])
	if length < recordFixed || length > recordFixed+MaxPayload {
		return rec, 0, fmt.Sprintf("record length %d out of range", length)
	}
	total := recordHeader + int(length)
	if total > len(b) {
		return rec, len(b), fmt.Sprintf("record of %d bytes runs past end of file", total)
	}
	body := b[recordHeader:total]
	if got, want := crc32.Checksum(body, castagnoli), binary.LittleEndian.Uint32(b[4:8]); got != want {
		return rec, total, fmt.Sprintf("checksum mismatch (stored %08x, computed %08x)", want, got)
	}
	rec.Seq = binary.LittleEndian.Uint64(body[0:8])
	rec.Time = int64(binary.LittleEndian.Uint64(body[8:16]))
	rec.Payload = body[16:]
	return rec, total, ""
}

// tailIsTorn: the damage starting at b[0] ends at b[n] (see parseRecord).
// It counts as a torn write only if everything after that is zero, or there
// is nothing after it: a later, separate append would have left non-zero
// data.
func tailIsTorn(b []byte, n int) bool {
	return allZero(b[n:])
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// Append adds a record to the in-memory batch. Nothing reaches the file
// until Flush. seq must be exactly one more than the previous record's.
func (l *Log) Append(seq uint64, time int64, payload []byte) {
	if len(payload) > MaxPayload {
		panic(fmt.Sprintf("wal: payload of %d bytes exceeds MaxPayload", len(payload)))
	}
	if last := l.lastAppendedOrPending(); last != 0 && (seq != last+1 && !l.opts.AllowGaps || seq <= last) {
		panic(fmt.Sprintf("wal: append seq %d after %d", seq, last))
	}
	start := len(l.pending)
	l.pending = binary.LittleEndian.AppendUint32(l.pending, uint32(recordFixed+len(payload)))
	l.pending = binary.LittleEndian.AppendUint32(l.pending, 0) // crc, filled in below
	l.pending = binary.LittleEndian.AppendUint64(l.pending, seq)
	l.pending = binary.LittleEndian.AppendUint64(l.pending, uint64(time))
	l.pending = append(l.pending, payload...)
	body := l.pending[start+recordHeader:]
	binary.LittleEndian.PutUint32(l.pending[start+4:], crc32.Checksum(body, castagnoli))
	if l.pendingFirst == 0 {
		l.pendingFirst = seq
	}
	l.pendingLast = seq
}

func (l *Log) lastAppendedOrPending() uint64 {
	if l.pendingLast != 0 {
		return l.pendingLast
	}
	return l.appended.Load()
}

// Flush writes the batch to the current segment with a single Write,
// starting a new segment first if the current one is full. It does not
// fsync. After it returns, LastAppended covers the batch.
func (l *Log) Flush() error {
	if len(l.pending) == 0 {
		return nil
	}
	if l.cur == nil || l.curSize >= l.opts.SegmentSize {
		if err := l.roll(l.pendingFirst); err != nil {
			return err
		}
	}
	l.mu.Lock()
	n, err := l.cur.Write(l.pending)
	l.curSize += int64(n)
	l.mu.Unlock()
	l.written.Add(uint64(n))
	if err != nil {
		return fmt.Errorf("wal: write: %w", err)
	}
	l.appended.Store(l.pendingLast)
	l.pending = l.pending[:0]
	l.pendingFirst, l.pendingLast = 0, 0
	return nil
}

// roll finishes the current segment (fsync and close, so everything in it is
// durable) and creates a new one whose first record will be first.
func (l *Log) roll(first uint64) error {
	l.syncMu.Lock() // wait for any in-flight Sync on the old file
	defer l.syncMu.Unlock()

	if l.cur != nil {
		if err := l.cur.Sync(); err != nil {
			return fmt.Errorf("wal: sync before roll: %w", err)
		}
		l.synced.Store(l.appended.Load())
		if err := l.cur.Close(); err != nil {
			return fmt.Errorf("wal: close segment: %w", err)
		}
	}
	path := filepath.Join(l.dir, segmentName(first))
	f, err := l.opts.OpenFile(path)
	if err != nil {
		return fmt.Errorf("wal: create segment: %w", err)
	}
	if _, err := f.Write(append([]byte(segmentMagic), formatVersion)); err != nil {
		return fmt.Errorf("wal: write segment header: %w", err)
	}
	// Make the new file's directory entry durable too (decision 011).
	if err := fsutil.SyncDir(l.dir); err != nil {
		return fmt.Errorf("wal: sync directory: %w", err)
	}
	l.mu.Lock()
	l.cur, l.curPath, l.curSize = f, path, headerSize
	l.mu.Unlock()
	return nil
}

// Sync fsyncs the current segment and advances LastSynced to whatever had
// been appended when it started. Safe to call concurrently with
// Append/Flush. It returns the new LastSynced.
func (l *Log) Sync() (uint64, error) {
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	l.mu.Lock()
	f := l.cur
	upto := l.appended.Load() // read under mu: these bytes are in f already
	l.mu.Unlock()
	if f == nil || upto <= l.synced.Load() {
		return l.synced.Load(), nil
	}
	start := clock.Now()
	err := f.Sync()
	l.opts.FsyncLatency.Observe(clock.Now() - start)
	if err != nil {
		return l.synced.Load(), fmt.Errorf("wal: fsync: %w", err)
	}
	l.synced.Store(upto)
	return upto, nil
}

// Written is the number of bytes written to segments since Open. Safe from
// any goroutine.
func (l *Log) Written() uint64 { return l.written.Load() }

// LastAppended is the seq of the last record written to the OS.
func (l *Log) LastAppended() uint64 { return l.appended.Load() }

// LastSynced is the seq of the last record known to be on stable storage.
func (l *Log) LastSynced() uint64 { return l.synced.Load() }

// Close flushes nothing and fsyncs nothing: call Flush and Sync first if
// you want them. It closes the current segment.
func (l *Log) Close() error {
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	if l.closed || l.cur == nil {
		l.closed = true
		return nil
	}
	l.closed = true
	return l.cur.Close()
}

// RemoveSegmentsBelow deletes every segment whose records all have seq < seq
// (PRD 5.4: a segment is deletable once a snapshot's seq exceeds its last
// seq). A segment's last seq is its successor's first seq - 1, so the last
// segment is never removed, and neither is the one being appended to. Safe
// to call while another goroutine appends. Returns how many were removed.
func (l *Log) RemoveSegmentsBelow(seq uint64) (int, error) {
	l.mu.Lock()
	current := l.curPath
	l.mu.Unlock()
	segs, err := listSegments(l.dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for i := 0; i+1 < len(segs); i++ {
		lastSeq := segs[i+1].first - 1
		if lastSeq >= seq || segs[i].path == current {
			break // segments are in order: nothing later qualifies either
		}
		if err := os.Remove(segs[i].path); err != nil {
			return removed, fmt.Errorf("wal: remove %s: %w", segs[i].path, err)
		}
		removed++
	}
	return removed, nil
}
