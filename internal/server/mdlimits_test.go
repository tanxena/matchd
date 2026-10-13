package server

import (
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/proto"
	"github.com/tanxena/matchd/internal/publisher"
)

// Review F-005: market-data requests need no login and are answered on the
// publisher goroutine, so their cost is bounded. A Resend more than
// publisher.MaxResend events back is answered ResendUnavailable (the
// client re-snapshots); within it, it is served.
func TestResendIsBounded(t *testing.T) {
	s := start(t, nil)
	trader := dial(t, s, 1)
	const orders = publisher.MaxResend + 500 // one BookDelta each
	for i := range orders {
		_ = trader.NewOrder(limit(1, uint64(i+1), proto.SideBuy, int64(1+i), 1))
	}
	recv(t, trader, orders)

	sub, err := client.Dial(s.Addr(), 0, wait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if err := sub.Subscribe(proto.Subscribe{Instrument: 1}); err != nil {
		t.Fatal(err)
	}
	snap := next(t, sub)
	if snap.Type != proto.TypeSnapshot {
		t.Fatalf("got %v, want a snapshot", snap)
	}
	for range snap.Snapshot.Levels {
		next(t, sub)
	}
	last := snap.Snapshot.MDSeq

	// Too far back: refused, nothing streamed.
	if err := sub.Resend(proto.Resend{Instrument: 1, FromMDSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if m := next(t, sub); m.Type != proto.TypeReject || m.Reject.Reason != proto.ReasonResendUnavailable {
		t.Fatalf("Resend of %d events: got %v, want ResendUnavailable", last, m)
	}
	// Within the bound: every event from there is sent.
	from := last - 99
	if err := sub.Resend(proto.Resend{Instrument: 1, FromMDSeq: from}); err != nil {
		t.Fatal(err)
	}
	for want := from; want <= last; want++ {
		if m := next(t, sub); m.Type != proto.TypeBookDelta || m.BookDelta.MDSeq != want {
			t.Fatalf("resend: got %v, want BookDelta md=%d", m, want)
		}
	}
}

// ...and one connection can't make the publisher work flat out: beyond a
// burst, its requests are refused by the gateway with TooManyRequests.
func TestMarketDataRequestsAreRateLimited(t *testing.T) {
	s := start(t, nil)
	sub, err := client.Dial(s.Addr(), 0, wait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	// One request at a time, each answer read before the next request (so
	// the separate limit of 4 snapshots waiting to be sent is never hit).
	const n = 100
	snapshots, limited := 0, 0
	for range n {
		if err := sub.Subscribe(proto.Subscribe{Instrument: 1}); err != nil {
			t.Fatal(err)
		}
		m := next(t, sub)
		switch {
		case m.Type == proto.TypeSnapshot:
			snapshots++
		case m.Type == proto.TypeReject && m.Reject.Reason == proto.ReasonTooManyRequests:
			limited++
		default:
			t.Fatalf("unexpected %v", m)
		}
	}
	t.Logf("%d subscribes at once: %d served, %d refused", n, snapshots, limited)
	if limited == 0 || snapshots > 40 {
		t.Fatalf("%d of %d back-to-back subscribes served, %d refused: no rate limit", snapshots, n, limited)
	}
	if st := s.Publisher().Stats(); st.Subscriptions != uint64(snapshots) {
		t.Fatalf("publisher served %d subscriptions, the client saw %d snapshots", st.Subscriptions, snapshots)
	}
}

func next(t *testing.T, c *client.Conn) client.Msg {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(wait))
	m, err := c.Next()
	if err != nil {
		t.Fatal(err)
	}
	return m
}
