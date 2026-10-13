package server

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/gateway"
	"github.com/tanxena/matchd/internal/metrics"
	"github.com/tanxena/matchd/internal/proto"
	"github.com/tanxena/matchd/internal/publisher"
)

// Milestone 7: the HTTP side (decision 018), metrics (017), graceful
// shutdown (019) and acceptance 5.

func withHTTP(c *Config) { c.HTTPAddr = "127.0.0.1:0" }

// slowConsumers counts slow-consumer disconnects however they were caught:
// by the publisher (queue over its limits) or by a session's writer (a
// socket write past WriteTimeout). Decision 022.
func slowConsumers(s *testServer) uint64 {
	return s.Publisher().Stats().SlowConsumers + s.Gateway().Stats().SlowWrites
}

func httpDo(t *testing.T, method, url, body string, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v in %q", method, url, err, data)
		}
	}
	return resp.StatusCode
}

func instruments(t *testing.T, s *testServer) []publisher.InstrumentInfo {
	t.Helper()
	var list []publisher.InstrumentInfo
	if code := httpDo(t, "GET", "http://"+s.HTTPAddr()+"/instruments", "", &list); code != http.StatusOK {
		t.Fatalf("GET /instruments: %d", code)
	}
	return list
}

func TestInstrumentEndpoints(t *testing.T) {
	dir := t.TempDir()
	s := start(t, func(c *Config) { withHTTP(c); c.DataDir = dir })
	base := "http://" + s.HTTPAddr()

	var health map[string]string
	if code := httpDo(t, "GET", base+"/healthz", "", &health); code != 200 || health["status"] != "ok" {
		t.Fatalf("healthz: %d %v", code, health)
	}
	want := []publisher.InstrumentInfo{
		{ID: 1, Symbol: "XYZ", TickSize: 1, LotSize: 1},
		{ID: 2, Symbol: "ABC", TickSize: 1, LotSize: 1},
	}
	if got := instruments(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("instruments %+v", got)
	}

	var added publisher.InstrumentInfo
	if code := httpDo(t, "POST", base+"/instruments", `{"symbol":"DEF","tick_size":5,"lot_size":100}`, &added); code != http.StatusCreated ||
		added != (publisher.InstrumentInfo{ID: 3, Symbol: "DEF", TickSize: 5, LotSize: 100}) {
		t.Fatalf("POST DEF: %d %+v", code, added)
	}
	seqAfterAdd := s.durable.Load()
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"symbol":"DEF","tick_size":1,"lot_size":1}`, http.StatusConflict},                                // duplicate: sequenced, rejected
		{`{"symbol":"GHI","tick_size":0,"lot_size":1}`, http.StatusBadRequest},                              // invalid: refused before the log
		{`{"symbol":"` + strings.Repeat("A", 300) + `","tick_size":1,"lot_size":1}`, http.StatusBadRequest}, // would have crashed the WAL encoder
		{`{"symbol":"JKL","tick_size":1,"lot_size":1,"extra":1}`, http.StatusBadRequest},                    // unknown field
		{`not json`, http.StatusBadRequest},
	} {
		var e map[string]string
		if code := httpDo(t, "POST", base+"/instruments", c.body, &e); code != c.code || e["error"] == "" {
			t.Errorf("POST %.40s: %d %v, want %d", c.body, code, e, c.code)
		}
	}
	if got := s.durable.Load(); got != seqAfterAdd+1 { // only the duplicate reached the log
		t.Errorf("seq went from %d to %d; invalid requests must not be logged", seqAfterAdd, got)
	}
	if code := httpDo(t, "DELETE", base+"/instruments", "", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: %d", code)
	}

	// Trade on the new instrument; the listing shows its md_seq and orders.
	cl := dial(t, s, 1)
	for i := range 3 {
		if err := cl.NewOrder(limit(3, uint64(i+1), proto.SideBuy, int64(100+5*i), 100)); err != nil {
			t.Fatal(err)
		}
	}
	recv(t, cl, 3)
	got := instruments(t, s)
	if len(got) != 3 || got[2] != (publisher.InstrumentInfo{ID: 3, Symbol: "DEF", TickSize: 5, LotSize: 100, MDSeq: 3, Orders: 3}) {
		t.Fatalf("after trading: %+v", got)
	}

	// The admin's reserved client id can't be used to log in.
	nc, r := rawConn(t, s)
	if _, err := nc.Write(proto.AppendLogin(nil, &proto.Login{ClientID: uint32(gateway.ReservedClientID)})); err != nil {
		t.Fatal(err)
	}
	expectReject(t, r, proto.ReasonInvalidClientID)

	// After a restart the instrument is still there, with the same id: it
	// was a logged command, replayed (or in the shutdown snapshot).
	_ = s.Close()
	s2 := start(t, func(c *Config) { withHTTP(c); c.DataDir = dir })
	if got2 := instruments(t, s2); !reflect.DeepEqual(got2, got) {
		t.Fatalf("after restart: %+v, want %+v", got2, got)
	}
}

// scrape fetches /metrics, checks every line is well-formed text format,
// and returns sample values keyed by "name{labels}".
func scrape(t *testing.T, s *testServer) map[string]float64 {
	t.Helper()
	resp, err := http.Get("http://" + s.HTTPAddr() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != metrics.ContentType {
		t.Fatalf("content type %q", ct)
	}
	sample := regexp.MustCompile(`^([a-z_]+(\{[^}]*\})?) (\S+)$`)
	typed := map[string]bool{}
	out := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			typed[strings.Fields(rest)[0]] = true
			continue
		}
		if strings.HasPrefix(line, "# HELP ") {
			continue
		}
		m := sample.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("malformed metrics line %q", line)
		}
		family := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.SplitN(m[1], "{", 2)[0], "_bucket"), "_sum"), "_count")
		if !typed[family] && !typed[strings.SplitN(m[1], "{", 2)[0]] {
			t.Fatalf("sample %q before its # TYPE", line)
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			t.Fatalf("value in %q: %v", line, err)
		}
		out[m[1]] = v
	}
	return out
}

func TestMetricsEndpoint(t *testing.T) {
	s := start(t, func(c *Config) { withHTTP(c); c.SnapshotEvery = 3 })
	a, b := dial(t, s, 1), dial(t, s, 2)
	_ = a.NewOrder(limit(1, 1, proto.SideSell, 100, 5))
	recv(t, a, 1)
	_ = b.NewOrder(limit(1, 1, proto.SideBuy, 100, 2)) // matches: 2 fills, 1 trade
	recv(t, b, 2)
	_ = b.NewOrder(limit(1, 2, proto.SideBuy, 90, 1))
	ack := recv(t, b, 1)[0]
	_ = b.Cancel(proto.Cancel{Instrument: 1, OrderID: ack.Ack.OrderID})
	recv(t, b, 1)
	_ = b.NewOrder(limit(9, 3, proto.SideBuy, 90, 1)) // unknown instrument: rejected
	recv(t, b, 1)
	_ = b.Cancel(proto.Cancel{Instrument: 1, OrderID: 999}) // unknown order: rejected
	recv(t, b, 1)

	waitForLog(t, s, "snapshot written") // SnapshotEvery 3: written in the background
	m := scrape(t, s)
	for series, want := range map[string]float64{
		`matchd_commands_total{type="new_order",outcome="accepted"}`:      3,
		`matchd_commands_total{type="new_order",outcome="rejected"}`:      1,
		`matchd_commands_total{type="cancel",outcome="accepted"}`:         1,
		`matchd_commands_total{type="cancel",outcome="rejected"}`:         1,
		`matchd_commands_total{type="add_instrument",outcome="accepted"}`: 0, // XYZ and ABC were added at startup, before the publisher ran
		`matchd_fills_total`:                                     2,
		`matchd_trades_total`:                                    1,
		`matchd_connections`:                                     2,
		`matchd_connections_accepted_total`:                      2,
		`matchd_slow_consumer_disconnects_total`:                 0,
		`matchd_durable_seq`:                                     8, // 2 instruments + 6 commands
		`matchd_matching_latency_seconds_count`:                  6,
		`matchd_clock_info{source="` + s.clockInfo.Source + `"}`: 1,
	} {
		if got, ok := m[series]; !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", series, got, ok, want)
		}
	}
	if m[`matchd_wal_fsync_seconds_count`] < 1 || m[`matchd_wal_written_bytes_total`] <= 0 {
		t.Errorf("WAL metrics: fsyncs %v, bytes %v", m[`matchd_wal_fsync_seconds_count`], m[`matchd_wal_written_bytes_total`])
	}
	if m[`matchd_matching_latency_seconds_bucket{le="+Inf"}`] != 6 || m[`matchd_matching_latency_seconds_sum`] <= 0 {
		t.Errorf("latency histogram: %v / %v", m[`matchd_matching_latency_seconds_bucket{le="+Inf"}`], m[`matchd_matching_latency_seconds_sum`])
	}
	// The first bucket bound is the decided 0.25 us, in seconds.
	if _, ok := m[`matchd_matching_latency_seconds_bucket{le="2.5e-07"}`]; !ok {
		t.Error("no le=\"2.5e-07\" bucket")
	}
	if age := m[`matchd_snapshot_age_seconds`]; !(age >= 0 && age < 60) { // SnapshotEvery 3: one was taken
		t.Errorf("snapshot age %v", age)
	}

	// GET /debug/latency (decision 021): the same 6 spans, precise to 3.1%.
	var lat latencyResponse
	if code := httpDo(t, "GET", "http://"+s.HTTPAddr()+"/debug/latency", "", &lat); code != http.StatusOK {
		t.Fatalf("GET /debug/latency: %d", code)
	}
	if lat.Matching.SubBits != 5 || lat.Matching.Count() != 6 {
		t.Errorf("/debug/latency: sub_bits %d, count %d; want 5 and 6", lat.Matching.SubBits, lat.Matching.Count())
	}
	// Recorded from the same reading as the /metrics histogram: same sum.
	if got, want := float64(lat.Matching.SumNS)/1e9, m[`matchd_matching_latency_seconds_sum`]; math.Abs(got-want) > 1e-12 {
		t.Errorf("/debug/latency sum %vs, /metrics sum %vs", got, want)
	}
}

// Metrics count only what has been released, i.e. durable.
func TestMetricsCountOnlyDurableCommands(t *testing.T) {
	g := &gate{}
	s := start(t, func(c *Config) { withHTTP(c); withDebugLog(c); c.Fsync = policy(t, "group:1ms"); c.WAL = gatedWAL(g) })
	t.Cleanup(g.open)
	cl := dial(t, s, 1)
	g.hold()
	_ = cl.NewOrder(limit(1, 1, proto.SideBuy, 100, 1))
	accepted := `matchd_commands_total{type="new_order",outcome="accepted"}`
	deadline := time.Now().Add(wait)
	for scrape(t, s)[`matchd_events_held`] == 0 { // the engine has executed it; its events wait for the fsync
		if time.Now().After(deadline) {
			t.Fatal("the order's events never reached the publisher")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if m := scrape(t, s); m[accepted] != 0 {
		t.Fatalf("before fsync: accepted %v", m[accepted])
	}
	if strings.Contains(s.logs.String(), `msg=command`) {
		t.Fatal("the per-command debug line was logged before the command was durable")
	}
	g.open()
	recv(t, cl, 1)
	if m := scrape(t, s); m[accepted] != 1 {
		t.Fatalf("after fsync: accepted %v", m[accepted])
	}
	waitForLog(t, s, "msg=command seq=3 cmd=NewOrder result=Ack client=1") // PRD 5.8: one debug line per command
}

func TestHealthzAfterDurabilityFailure(t *testing.T) {
	g := &gate{}
	s := start(t, func(c *Config) { withHTTP(c); c.WAL = gatedWAL(g) })
	cl := dial(t, s, 1)
	g.fail.Store(true)
	_ = cl.NewOrder(limit(1, 1, proto.SideBuy, 100, 1))
	<-s.Failed()
	var h map[string]string
	if code := httpDo(t, "GET", "http://"+s.HTTPAddr()+"/healthz", "", &h); code != http.StatusServiceUnavailable || h["status"] != "failed" {
		t.Fatalf("healthz after failure: %d %v", code, h)
	}
	var e map[string]string
	if code := httpDo(t, "POST", "http://"+s.HTTPAddr()+"/instruments", `{"symbol":"NEW","tick_size":1,"lot_size":1}`, &e); code != http.StatusServiceUnavailable {
		t.Fatalf("POST after failure: %d %v", code, e)
	}
}

// Graceful shutdown (decision 019): every command accepted before Close is
// answered, the shutdown snapshot covers all of them, and the next start
// replays nothing.
func TestGracefulShutdownDrainsAndSnapshots(t *testing.T) {
	dir := t.TempDir()
	s := start(t, func(c *Config) { c.DataDir = dir; c.SnapshotEvery = 1 << 30 })
	cl := dial(t, s, 1)
	acks := make(chan int)
	go func() { // read until the server closes the connection
		n := 0
		_ = cl.SetReadDeadline(time.Now().Add(20 * time.Second))
		for {
			m, err := cl.Next()
			if err != nil {
				acks <- n
				return
			}
			if m.Type == proto.TypeAck {
				n++
			}
		}
	}()
	for i := range 2000 { // pipelined: Close catches many still in flight
		if err := cl.NewOrder(limit(1, uint64(i+1), proto.SideBuy, int64(1+i%50), 1)); err != nil {
			break
		}
		if i == 1000 {
			go func() { _ = s.Close() }()
		}
	}
	n := <-acks
	_ = s.Close()

	s2 := start(t, func(c *Config) { c.DataDir = dir; c.SnapshotEvery = 1 << 30 })
	_ = s2.Close()
	resting := len(s2.Engine().State().Instruments[0].Bids)
	if resting != n {
		t.Fatalf("client received %d acks, the recovered book has %d of its orders", n, resting)
	}
	if n < 500 {
		t.Fatalf("only %d acks before shutdown: the test didn't load the pipeline", n)
	}
	if s2.recovered.Replayed != 0 || s2.recovered.SnapshotSeq != uint64(s2.Engine().LastSeq()) {
		t.Fatalf("restart replayed %d WAL records from snapshot %d (last seq %d); the shutdown snapshot should cover everything",
			s2.recovered.Replayed, s2.recovered.SnapshotSeq, s2.Engine().LastSeq())
	}
	t.Logf("%d of 2000 orders accepted before shutdown, all answered; restart replayed 0 records", n)
}

// A client that isn't reading can't hold shutdown hostage: after
// ShutdownTimeout its connection is closed anyway.
func TestShutdownTimeoutCutsAStuckClient(t *testing.T) {
	s := start(t, func(c *Config) {
		c.ShutdownTimeout = 300 * time.Millisecond
		c.Gateway.SocketWriteBuffer = 4 << 10
	})
	nc, _ := rawConn(t, s)
	_ = nc.(*net.TCPConn).SetReadBuffer(4 << 10)
	var frames []byte
	frames = proto.AppendLogin(frames, &proto.Login{ClientID: 5})
	for i := range 2000 {
		frames = proto.AppendNewOrder(frames, &proto.NewOrder{Instrument: 1, ClOrdID: uint64(i + 1), Side: proto.SideBuy,
			Type: proto.TypeLimit, TIF: proto.TIFGTC, Price: 10, Qty: 1})
	}
	if _, err := nc.Write(frames); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(wait)
	for s.durable.Load() < 2+2000 { // every order sequenced; their acks can't all be sent
		if time.Now().After(deadline) {
			t.Fatalf("orders not sequenced: durable seq %d", s.durable.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	_ = s.Close()
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Close took %v with a 300 ms shutdown timeout", took)
	}
	waitForLog(t, s, "shutdown timeout: closing connections")
}

// The WAL is made durable before the shutdown snapshot is written, so a
// crash in the middle of writing it loses nothing. The hook copies the data
// directory at that moment, which is what such a crash would leave behind.
func TestCrashDuringShutdownSnapshotLosesNothing(t *testing.T) {
	dir, crashDir := t.TempDir(), t.TempDir()
	s := start(t, func(c *Config) {
		c.DataDir = dir
		c.SnapshotEvery = 1 << 30
		c.Fsync = policy(t, "group:5ms") // the case where the engine runs ahead of the disk
		c.beforeShutdownSnapshot = func() { copyDir(t, dir, crashDir) }
	})
	trade(t, s, 300, 1)
	_ = s.Close()
	want := s.Engine().State()

	crashed := start(t, func(c *Config) { c.DataDir = crashDir })
	_ = crashed.Close()
	if !reflect.DeepEqual(crashed.Engine().State(), want) {
		t.Fatal("state recovered from a crash during the shutdown snapshot differs")
	}
	if crashed.recovered.Replayed == 0 {
		t.Fatal("the crash copy should have had to replay the WAL")
	}
	clean := start(t, func(c *Config) { c.DataDir = dir })
	_ = clean.Close()
	if !reflect.DeepEqual(clean.Engine().State(), want) || clean.recovered.Replayed != 0 {
		t.Fatalf("clean restart: replayed %d, state equal %v", clean.recovered.Replayed, reflect.DeepEqual(clean.Engine().State(), want))
	}
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Acceptance 5: a subscriber that stops reading is disconnected, and the
// engine's latency histogram does not move. The same load runs twice, once
// with only healthy readers and once with a market-data subscriber that
// never reads. Two things are checked (review F-008):
//   - exactly: the engine never found its output channel full during the
//     slow phase (Engine.OutputBlocked), which is how a slow consumer would
//     reach the engine at all;
//   - statistically: p50, p99 and p99.9 from the 3.1%-precise latency
//     histogram (not the 2-2.5x-wide /metrics buckets) stay within stated
//     ratios of the baseline's (1.4x, 1.8x, 2.5x; measured 0.90-1.06x under
//     -race), on 6,000 commands per phase.
func TestSlowConsumerDoesNotMoveEngineLatency(t *testing.T) {
	s := start(t, func(c *Config) {
		c.Gateway.OutQueue = 256
		c.Gateway.SocketWriteBuffer = 4 << 10
		c.Gateway.WriteTimeout = 300 * time.Millisecond // how long it may stay behind (decision 022)
	})
	run := func() (metrics.LogSnapshot, uint64) {
		before, blocked := s.fine.Snapshot(), s.Engine().OutputBlocked()
		load(t, s, 3000, 7, func() {})
		return s.fine.Snapshot().Sub(before), s.Engine().OutputBlocked() - blocked
	}
	baseline, baseBlocked := run()

	nc, err := net.DialTimeout("tcp", s.Addr(), wait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nc.Close() }()
	_ = nc.(*net.TCPConn).SetReadBuffer(4 << 10)
	if _, err := nc.Write(proto.AppendSubscribe(nil, &proto.Subscribe{Instrument: 1})); err != nil {
		t.Fatal(err)
	}
	// ...and never reads.
	slow, slowBlocked := run()
	if slowConsumers(s) == 0 {
		t.Fatalf("the stalled subscriber was never disconnected: %+v %+v", s.Publisher().Stats(), s.Gateway().Stats())
	}
	if baseBlocked != 0 || slowBlocked != 0 {
		t.Errorf("the engine waited on a full output channel %d times in the baseline and %d with the stalled subscriber; want 0",
			baseBlocked, slowBlocked)
	}
	for _, q := range []struct {
		q     float64
		ratio float64
	}{{0.5, 1.4}, {0.99, 1.8}, {0.999, 2.5}} {
		b, w := baseline.QuantileNS(q.q), slow.QuantileNS(q.q)
		t.Logf("p%v: baseline %v, stalled subscriber %v (%.2fx; limit %.1fx)", q.q*100, time.Duration(b), time.Duration(w), float64(w)/float64(b), q.ratio)
		// The ratio, plus 200 ns of slack for the sub-microsecond p50, where
		// a single cache miss is a large fraction.
		if float64(w) > q.ratio*float64(b)+200 {
			t.Errorf("p%v moved from %v to %v, more than %.1fx", q.q*100, time.Duration(b), time.Duration(w), q.ratio)
		}
	}
	t.Logf("%d and %d commands", baseline.Count(), slow.Count())
}

// A rejected AddInstrument leaves no spec behind in the publisher, and
// concurrent adds each get their own id and all show up.
func TestAdminRejectForgetsSpec(t *testing.T) {
	s := start(t, withHTTP)
	if _, err := s.AddInstrument(book.Instrument{Symbol: "XYZ", TickSize: 1, LotSize: 1}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if info, err := s.AddInstrument(book.Instrument{Symbol: "NEW", TickSize: 2, LotSize: 3}); err != nil || info.ID != 3 {
		t.Fatalf("add NEW: %+v %v", info, err)
	}
	got := instruments(t, s)
	if len(got) != 3 || got[2].Symbol != "NEW" || got[2].TickSize != 2 {
		t.Fatalf("instruments %+v", got)
	}
	var wg sync.WaitGroup // concurrent adds get distinct ids and all show up
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.AddInstrument(book.Instrument{Symbol: "C" + strconv.Itoa(i), TickSize: 1, LotSize: 1}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := instruments(t, s); len(got) != 11 {
		t.Fatalf("%d instruments after concurrent adds", len(got))
	}
}

// The case the lingering close exists for (decision 019): the client's
// answers sit unread in its receive buffer while it is still sending
// orders the stopped reader will never take. A plain close of a socket with
// unread input sends a reset, and the reset makes the client's stack throw
// away the answers it had already received.
func TestShutdownDoesNotResetAClientStillSending(t *testing.T) {
	dir := t.TempDir()
	s := start(t, func(c *Config) { c.DataDir = dir })
	nc, _ := rawConn(t, s)
	order := func(i int) []byte {
		return proto.AppendNewOrder(nil, &proto.NewOrder{Instrument: 1, ClOrdID: uint64(i + 1), Side: proto.SideBuy,
			Type: proto.TypeLimit, TIF: proto.TIFGTC, Price: int64(1 + i%30), Qty: 1})
	}
	frames := proto.AppendLogin(nil, &proto.Login{ClientID: 9})
	for i := range 300 {
		frames = append(frames, order(i)...)
	}
	if _, err := nc.Write(frames); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(wait)
	for s.durable.Load() < 2+300 { // all 300 executed; their acks wait, unread, at the client
		if time.Now().After(deadline) {
			t.Fatal("orders not executed")
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan struct{})
	go func() { _ = s.Close(); close(closed) }()
	for i := 300; i < 600; i++ { // keep sending while the server stops reading
		if _, err := nc.Write(order(i)); err != nil {
			break
		}
	}
	<-closed

	// Only now read: everything the server sent must still be there.
	r := proto.NewReader(nc)
	_ = nc.SetReadDeadline(time.Now().Add(wait))
	acks := 0
	for {
		typ, _, err := r.Next()
		if err != nil {
			break // EOF; a reset here would mean answers were destroyed
		}
		if typ == proto.TypeAck {
			acks++
		}
	}
	s2 := start(t, func(c *Config) { c.DataDir = dir })
	_ = s2.Close()
	if n := len(s2.Engine().State().Instruments[0].Bids); acks != n {
		t.Fatalf("client could read %d acks; %d of its orders were executed", acks, n)
	}
}
