package crashtest

import (
	"strings"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/proto"
)

// The real binary under a real signal (decision 019): SIGTERM (Ctrl-Break
// on Windows) shuts the engine down gracefully, it exits 0 after writing a
// shutdown snapshot, and the next start replays no WAL at all.
func TestSignalShutsDownGracefully(t *testing.T) {
	dir := t.TempDir()
	cmd, addr, logs := startEngine(t, dir, "always", 1<<20)
	cl, err := client.Dial(addr, 1, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 300 {
		_ = cl.NewOrder(proto.NewOrder{Instrument: 1, ClOrdID: uint64(i + 1), Side: proto.SideBuy, Type: proto.TypeLimit,
			TIF: proto.TIFGTC, Price: int64(1 + i%40), Qty: 1})
	}
	got := 0
	_ = cl.SetReadDeadline(time.Now().Add(10 * time.Second))
	for got < 300 {
		m, err := cl.Next()
		if err != nil {
			t.Fatalf("after %d acks: %v", got, err)
		}
		if m.Type == proto.TypeAck {
			got++
		}
	}

	if err := interrupt(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Skipf("can't deliver an interrupt from here (no shared console?): %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("engine exited with %v after the signal:\n%s", err, logs.String())
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("engine still running 15 s after the signal:\n%s", logs.String())
	}
	if _, err := cl.Next(); err == nil {
		t.Fatal("the client's connection is still open after shutdown")
	}
	out := logs.String()
	for _, want := range []string{`msg="shutting down"`, `msg="server stopped"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %s:\n%s", want, out)
		}
	}

	cmd2, _, logs2 := startEngine(t, dir, "always", 1<<20)
	_ = cmd2.Process.Kill()
	_ = cmd2.Wait()
	if !strings.Contains(logs2.String(), "wal_replayed=0") {
		t.Fatalf("restart after a graceful shutdown replayed WAL records:\n%s", logs2.String())
	}
}
