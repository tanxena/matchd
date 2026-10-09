package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func payloadFor(seq uint64) []byte {
	// Varying lengths, deterministic content.
	return bytes.Repeat([]byte{byte(seq)}, int(seq%37))
}

// writeLog appends records first..last in batches of batch and closes the log.
func writeLog(t *testing.T, dir string, opts Options, first, last uint64, batch int) {
	t.Helper()
	l, _, err := Open(dir, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	for seq := first; seq <= last; seq++ {
		l.Append(seq, int64(seq)*1000, payloadFor(seq))
		if int(seq-first+1)%batch == 0 || seq == last {
			if err := l.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := l.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

// readAll reopens dir and returns the seqs replayed and the recovery info,
// checking every record's content.
func readAll(t *testing.T, dir string, opts Options) ([]uint64, RecoveryInfo, error) {
	t.Helper()
	var seqs []uint64
	l, info, err := Open(dir, opts, func(r Record) error {
		if r.Time != int64(r.Seq)*1000 || !bytes.Equal(r.Payload, payloadFor(r.Seq)) {
			return fmt.Errorf("record %d has wrong content", r.Seq)
		}
		seqs = append(seqs, r.Seq)
		return nil
	})
	if err == nil {
		_ = l.Close()
	}
	return seqs, info, err
}

func segPaths(t *testing.T, dir string) []string {
	t.Helper()
	segs, err := listSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	var p []string
	for _, s := range segs {
		p = append(p, s.path)
	}
	return p
}

func TestRoundTripAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	opts := Options{SegmentSize: 300} // a few records per segment
	writeLog(t, dir, opts, 1, 200, 7)
	seqs, info, err := readAll(t, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 200 || seqs[0] != 1 || seqs[199] != 200 || info.LastSeq != 200 || info.FirstSeq != 1 || info.Records != 200 {
		t.Fatalf("replayed %d records, info %+v", len(seqs), info)
	}
	if info.Segments < 10 {
		t.Fatalf("only %d segments; the test isn't exercising rolling", info.Segments)
	}
	// Append after reopening continues the same log.
	writeLog(t, dir, opts, 201, 260, 5)
	seqs, info, err = readAll(t, dir, opts)
	if err != nil || len(seqs) != 260 || info.LastSeq != 260 {
		t.Fatalf("after reopen+append: %d records, %+v, %v", len(seqs), info, err)
	}
	// Every segment is named after its first record.
	for _, p := range segPaths(t, dir) {
		data, _ := os.ReadFile(p)
		if len(data) <= headerSize {
			t.Fatalf("empty segment %s", p)
		}
		rec, _, problem := parseRecord(data[headerSize:])
		if problem != "" || filepath.Base(p) != segmentName(rec.Seq) {
			t.Fatalf("segment %s starts with seq %d (%s)", p, rec.Seq, problem)
		}
	}
}

func TestEmptyLog(t *testing.T) {
	dir := t.TempDir()
	l, info, err := Open(dir, Options{}, nil)
	if err != nil || info.LastSeq != 0 || info.Segments != 0 {
		t.Fatalf("%+v %v", info, err)
	}
	if n, err := l.Sync(); err != nil || n != 0 {
		t.Fatalf("sync on empty log: %d %v", n, err)
	}
	_ = l.Close()
}

// Cut the final record at every possible length: each must recover as a torn
// tail back to the previous record, and the log must accept appends after.
func TestTornTailEveryOffset(t *testing.T) {
	src := t.TempDir()
	writeLog(t, src, Options{}, 1, 5, 5)
	path := segPaths(t, src)[0]
	full, _ := os.ReadFile(path)
	// Find where record 5 starts.
	off := headerSize
	for i := 0; i < 4; i++ {
		_, n, _ := parseRecord(full[off:])
		off += n
	}
	for cut := off + 1; cut < len(full); cut++ {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(path)), full[:cut], 0o644); err != nil {
			t.Fatal(err)
		}
		seqs, info, err := readAll(t, dir, Options{})
		if err != nil || len(seqs) != 4 || info.LastSeq != 4 || info.TornBytes != int64(cut-off) {
			t.Fatalf("cut at %d: %d records, %+v, %v", cut, len(seqs), info, err)
		}
		writeLog(t, dir, Options{}, 5, 6, 1) // seq 5 again, then 6
		if seqs, _, err := readAll(t, dir, Options{}); err != nil || len(seqs) != 6 {
			t.Fatalf("cut at %d: after re-append %d records, %v", cut, len(seqs), err)
		}
	}
}

func TestTornTailShapes(t *testing.T) {
	type shape struct {
		name   string
		mangle func(data []byte, lastRecOff int) []byte
		want   int // records surviving
	}
	shapes := []shape{
		{"zero-filled tail", func(d []byte, _ int) []byte { return append(d, make([]byte, 4096)...) }, 5},
		{"last record checksum bad", func(d []byte, last int) []byte { d[len(d)-1] ^= 0xff; return d }, 4},
		{"last record checksum bad then zeros", func(d []byte, last int) []byte {
			d[len(d)-1] ^= 0xff
			return append(d, make([]byte, 100)...)
		}, 4},
		{"partial record header", func(d []byte, _ int) []byte { return append(d, 0x20, 0, 0) }, 5},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			dir := t.TempDir()
			writeLog(t, dir, Options{}, 1, 5, 5)
			path := segPaths(t, dir)[0]
			data, _ := os.ReadFile(path)
			last := lastRecordOffset(data)
			if err := os.WriteFile(path, sh.mangle(data, last), 0o644); err != nil {
				t.Fatal(err)
			}
			seqs, info, err := readAll(t, dir, Options{})
			if err != nil || len(seqs) != sh.want || info.TornBytes == 0 {
				t.Fatalf("%d records, %+v, %v", len(seqs), info, err)
			}
		})
	}
}

// A segment that crashed while its header was being written is removed, and
// appends continue in the previous segment.
func TestTornSegmentHeader(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, Options{}, 1, 3, 3)
	if err := os.WriteFile(filepath.Join(dir, segmentName(4)), []byte("MTC"), 0o644); err != nil {
		t.Fatal(err)
	}
	seqs, info, err := readAll(t, dir, Options{})
	if err != nil || len(seqs) != 3 || info.Segments != 1 {
		t.Fatalf("%d records, %+v, %v", len(seqs), info, err)
	}
	writeLog(t, dir, Options{}, 4, 6, 3)
	if seqs, info, err := readAll(t, dir, Options{}); err != nil || len(seqs) != 6 || info.Segments != 1 {
		t.Fatalf("after append: %d records, %+v, %v", len(seqs), info, err)
	}
}

func lastRecordOffset(data []byte) int {
	off, last := headerSize, headerSize
	for off < len(data) {
		_, n, problem := parseRecord(data[off:])
		if problem != "" {
			break
		}
		last = off
		off += n
	}
	return last
}

func TestCorruptionIsFatal(t *testing.T) {
	type cs struct {
		name   string
		mangle func(t *testing.T, dir string)
	}
	firstSeg := func(t *testing.T, dir string) string { return segPaths(t, dir)[0] }
	lastSeg := func(t *testing.T, dir string) string { p := segPaths(t, dir); return p[len(p)-1] }
	modify := func(t *testing.T, path string, f func([]byte) []byte) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, f(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []cs{
		{"flipped byte mid-log in last segment", func(t *testing.T, dir string) {
			modify(t, lastSeg(t, dir), func(d []byte) []byte { d[headerSize+30] ^= 0x01; return d })
		}},
		{"bad record at end of an earlier segment", func(t *testing.T, dir string) {
			modify(t, firstSeg(t, dir), func(d []byte) []byte { d[len(d)-1] ^= 0x01; return d })
		}},
		{"torn-looking record in an earlier segment", func(t *testing.T, dir string) {
			modify(t, firstSeg(t, dir), func(d []byte) []byte { return d[:len(d)-3] })
		}},
		{"garbage length followed by data", func(t *testing.T, dir string) {
			modify(t, lastSeg(t, dir), func(d []byte) []byte {
				binary.LittleEndian.PutUint32(d[headerSize:], 0xfffffff0)
				return d
			})
		}},
		{"bad checksum followed by non-zero bytes", func(t *testing.T, dir string) {
			modify(t, lastSeg(t, dir), func(d []byte) []byte {
				d[len(d)-1] ^= 0x01
				return append(d, 0x55, 0x55)
			})
		}},
		{"missing middle segment (seq gap)", func(t *testing.T, dir string) {
			p := segPaths(t, dir)
			if err := os.Remove(p[1]); err != nil {
				t.Fatal(err)
			}
		}},
		{"bad magic", func(t *testing.T, dir string) {
			modify(t, firstSeg(t, dir), func(d []byte) []byte { d[0] = 'X'; return d })
		}},
		{"unsupported version", func(t *testing.T, dir string) {
			modify(t, firstSeg(t, dir), func(d []byte) []byte { d[7] = 99; return d })
		}},
		{"segment name disagrees with first record", func(t *testing.T, dir string) {
			p := segPaths(t, dir)
			if err := os.Rename(p[0], filepath.Join(dir, segmentName(0))); err != nil {
				t.Fatal(err)
			}
		}},
		{"stray file in directory", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "notes.log"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeLog(t, dir, Options{SegmentSize: 400}, 1, 60, 4)
			if len(segPaths(t, dir)) < 3 {
				t.Fatal("need at least 3 segments")
			}
			c.mangle(t, dir)
			_, _, err := readAll(t, dir, Options{SegmentSize: 400})
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("err = %v, want ErrCorrupt", err)
			}
			t.Logf("rejected as expected: %v", err)
		})
	}
}

// faultFile wraps a real file so tests can count, block and fail Sync.
type faultFile struct {
	File
	syncs   *atomic.Int64
	failing *atomic.Bool
}

func (f faultFile) Sync() error {
	f.syncs.Add(1)
	if f.failing.Load() {
		return errors.New("injected fsync failure")
	}
	return f.File.Sync()
}

func TestSyncFailureDoesNotAdvance(t *testing.T) {
	var syncs atomic.Int64
	var failing atomic.Bool
	opts := Options{OpenFile: func(p string) (File, error) {
		f, err := openAppend(p)
		return faultFile{File: f, syncs: &syncs, failing: &failing}, err
	}}
	l, _, err := Open(t.TempDir(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	l.Append(1, 0, nil)
	_ = l.Flush()
	if n, err := l.Sync(); err != nil || n != 1 {
		t.Fatalf("first sync: %d %v", n, err)
	}
	l.Append(2, 0, nil)
	_ = l.Flush()
	failing.Store(true)
	if n, err := l.Sync(); err == nil || n != 1 || l.LastSynced() != 1 {
		t.Fatalf("failed sync: n=%d err=%v synced=%d; must stay at 1", n, err, l.LastSynced())
	}
	if l.LastAppended() != 2 {
		t.Fatalf("appended = %d", l.LastAppended())
	}
}

// One goroutine appends while another syncs, as in group commit. Run with
// -race. Afterwards everything is synced and reads back intact.
func TestConcurrentAppendAndSync(t *testing.T) {
	dir := t.TempDir()
	opts := Options{SegmentSize: 2048}
	l, _, err := Open(dir, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{}) // closed when the appender is done
	var wg sync.WaitGroup       // the syncer goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		var prev uint64
		for {
			n, err := l.Sync()
			if err != nil {
				t.Error(err)
				return
			}
			if n < prev {
				t.Errorf("LastSynced went backwards: %d after %d", n, prev)
			}
			prev = n
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	for seq := uint64(1); seq <= 3000; seq++ {
		l.Append(seq, int64(seq)*1000, payloadFor(seq))
		if seq%10 == 0 {
			if err := l.Flush(); err != nil {
				t.Fatal(err)
			}
			if s := l.LastSynced(); s > l.LastAppended() {
				t.Fatalf("synced %d beyond appended %d", s, l.LastAppended())
			}
		}
	}
	close(stop)
	wg.Wait()
	if _, err := l.Sync(); err != nil || l.LastSynced() != 3000 {
		t.Fatalf("final sync: %d %v", l.LastSynced(), err)
	}
	_ = l.Close()
	if seqs, _, err := readAll(t, dir, opts); err != nil || len(seqs) != 3000 {
		t.Fatalf("%d records, %v", len(seqs), err)
	}
}

func TestAppendRejectsSeqGap(t *testing.T) {
	l, _, err := Open(t.TempDir(), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	l.Append(1, 0, nil)
	defer func() {
		if recover() == nil {
			t.Fatal("append with a seq gap did not panic")
		}
	}()
	l.Append(3, 0, nil)
}

// FuzzScan: arbitrary bytes as the last segment must never panic the
// scanner. If it reports a torn tail, the bytes before the tear must scan
// cleanly with the same records: recovery's truncation has to produce a
// valid log. (In-memory, so it runs millions of inputs; TestTorn* and
// TestCorruption* cover the file handling.)
func FuzzScan(f *testing.F) {
	seed := t0Segment()
	f.Add(seed)
	f.Add(seed[:len(seed)-5])
	f.Add(append(append([]byte{}, seed...), 0, 0, 0, 0))
	f.Add([]byte("MTCH"))
	f.Fuzz(func(t *testing.T, data []byte) {
		res, err := scanBytes("fuzz.log", data, 1, 0, true, nil)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("non-corruption error from pure scan: %v", err)
			}
			return
		}
		if res.tornAt < 0 {
			return
		}
		if res.tornAt < headerSize {
			return // recovery removes the whole file
		}
		again, err := scanBytes("fuzz.log", data[:res.tornAt], 1, 0, true, nil)
		if err != nil || again.tornAt != -1 || again.records != res.records || again.lastSeq != res.lastSeq {
			t.Fatalf("after truncating at %d: %+v %v (before: %+v)", res.tornAt, again, err, res)
		}
	})
}

func t0Segment() []byte {
	dir, _ := os.MkdirTemp("", "walseed")
	defer func() { _ = os.RemoveAll(dir) }()
	l, _, _ := Open(dir, Options{}, nil)
	for seq := uint64(1); seq <= 3; seq++ {
		l.Append(seq, int64(seq), payloadFor(seq))
	}
	_ = l.Flush()
	_ = l.Close()
	data, _ := os.ReadFile(filepath.Join(dir, segmentName(1)))
	return data
}

// BenchmarkAppendSync: one small record written and fsynced per op, the
// cost of fsync=always with no batching.
func BenchmarkAppendSync(b *testing.B) {
	l, _, err := Open(b.TempDir(), Options{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	payload := make([]byte, 36) // a NewOrder command
	seq := uint64(0)
	for b.Loop() {
		seq++
		l.Append(seq, 0, payload)
		if err := l.Flush(); err != nil {
			b.Fatal(err)
		}
		if _, err := l.Sync(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAppendFlush: written to the OS but not fsynced (fsync=never).
func BenchmarkAppendFlush(b *testing.B) {
	l, _, err := Open(b.TempDir(), Options{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	payload := make([]byte, 36)
	seq := uint64(0)
	for b.Loop() {
		seq++
		l.Append(seq, 0, payload)
		if err := l.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}
