package client

import (
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/proto"
)

// The Feed's state machine without a server: messages are handed to Apply
// directly, and whatever the Feed sends (Subscribe, Resend) is read from the
// other end of an in-memory pipe.

type pipeFeed struct {
	f    *Feed
	peer *proto.Reader // reads what the Feed sends
}

func newPipeFeed(t *testing.T) *pipeFeed {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	conn := &Conn{conn: a, r: proto.NewReader(a)}
	pf := &pipeFeed{peer: proto.NewReader(b)}
	got := make(chan *Feed, 1) // NewFeed blocks writing Subscribe until we read it
	go func() {
		f, err := NewFeed(conn, 7)
		if err != nil {
			t.Error(err)
		}
		got <- f
	}()
	pf.expect(t, proto.TypeSubscribe)
	pf.f = <-got
	return pf
}

// expect reads the next request the Feed sent and returns its payload.
func (p *pipeFeed) expect(t *testing.T, want proto.MsgType) []byte {
	t.Helper()
	type res struct {
		typ proto.MsgType
		p   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		typ, payload, err := p.peer.Next()
		ch <- res{typ, append([]byte(nil), payload...), err}
	}()
	select {
	case r := <-ch:
		if r.err != nil || r.typ != want {
			t.Fatalf("feed sent %v (%v), want %v", r.typ, r.err, want)
		}
		return r.p
	case <-time.After(2 * time.Second):
		t.Fatalf("feed sent nothing, want %v", want)
	}
	return nil
}

// apply applies m; if the Feed responds with a request, the pipe must be
// drained concurrently (net.Pipe is unbuffered), so callers that expect a
// request use applyExpect instead.
func (p *pipeFeed) apply(t *testing.T, m Msg) {
	t.Helper()
	if err := p.f.Apply(m); err != nil {
		t.Fatal(err)
	}
}

func (p *pipeFeed) applyExpect(t *testing.T, m Msg, want proto.MsgType) []byte {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- p.f.Apply(m) }()
	payload := p.expect(t, want)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	return payload
}

func snap(md uint64, levels uint32) Msg {
	return Msg{Type: proto.TypeSnapshot, Snapshot: proto.Snapshot{Instrument: 7, MDSeq: md, Levels: levels}}
}

func lvl(side uint8, px, qty int64) Msg {
	return Msg{Type: proto.TypeSnapshotLevel, SnapshotLevel: proto.SnapshotLevel{Instrument: 7, Side: side, Price: px, Qty: qty}}
}

func dlt(md uint64, side uint8, px, qty int64) Msg {
	return Msg{Type: proto.TypeBookDelta, BookDelta: proto.BookDelta{Instrument: 7, MDSeq: md, Side: side, Price: px, Qty: qty}}
}

func TestFeedSnapshotThenDeltas(t *testing.T) {
	p := newPipeFeed(t)
	p.apply(t, dlt(3, proto.SideBuy, 1, 1)) // before the snapshot: ignored
	p.apply(t, snap(10, 2))
	if p.f.Live() {
		t.Fatal("live before its snapshot levels arrived")
	}
	p.apply(t, lvl(proto.SideBuy, 100, 5))
	p.apply(t, lvl(proto.SideSell, 101, 3))
	if !p.f.Live() || p.f.MDSeq() != 10 {
		t.Fatalf("live=%v md=%d", p.f.Live(), p.f.MDSeq())
	}
	p.apply(t, dlt(11, proto.SideBuy, 100, 7))  // update
	p.apply(t, dlt(12, proto.SideSell, 101, 0)) // remove
	p.apply(t, dlt(12, proto.SideSell, 101, 9)) // duplicate: ignored
	p.apply(t, Msg{Type: proto.TypeTrade, Trade: proto.Trade{Instrument: 7, MDSeq: 13, Price: 100, Qty: 2}})
	p.apply(t, dlt(14, proto.SideSell, 102, 4))
	p.apply(t, dlt(1, proto.SideBuy, 1, 1)) // a long-past md_seq: duplicate
	bids, asks := p.f.Book()
	if !reflect.DeepEqual(bids, []Level{{100, 7}}) || !reflect.DeepEqual(asks, []Level{{102, 4}}) || p.f.MDSeq() != 14 {
		t.Fatalf("book %v %v at %d", bids, asks, p.f.MDSeq())
	}
	if st := p.f.Stats; st.Duplicates != 2 || st.Trades != 1 || st.Applied != 4 {
		t.Fatalf("stats %+v", st)
	}
}

func TestFeedGapResendsAndRecovers(t *testing.T) {
	p := newPipeFeed(t)
	p.apply(t, snap(10, 0))
	p.apply(t, dlt(11, proto.SideBuy, 100, 1))
	// 12 is lost; 13 reveals the gap: the Feed asks for 12 onward.
	payload := p.applyExpect(t, dlt(13, proto.SideBuy, 100, 3), proto.TypeResend)
	var r proto.Resend
	if r.Decode(payload) != nil || r.FromMDSeq != 12 || r.Instrument != 7 {
		t.Fatalf("resend %+v", r)
	}
	p.apply(t, dlt(14, proto.SideBuy, 100, 4)) // still recovering: ignored, no second Resend
	if p.f.Live() || p.f.Stats.Resends != 1 {
		t.Fatalf("live=%v resends=%d", p.f.Live(), p.f.Stats.Resends)
	}
	// The resend: 12, 13, 14 in order.
	p.apply(t, dlt(12, proto.SideBuy, 100, 2))
	p.apply(t, dlt(13, proto.SideBuy, 100, 3))
	p.apply(t, dlt(14, proto.SideBuy, 100, 4))
	if bids, _ := p.f.Book(); !p.f.Live() || p.f.MDSeq() != 14 || !reflect.DeepEqual(bids, []Level{{100, 4}}) {
		t.Fatalf("after recovery: live=%v md=%d bids=%v", p.f.Live(), p.f.MDSeq(), bids)
	}
}

func TestFeedResendUnavailableResubscribes(t *testing.T) {
	p := newPipeFeed(t)
	p.apply(t, snap(10, 1))
	p.apply(t, lvl(proto.SideBuy, 100, 5))
	p.applyExpect(t, dlt(500, proto.SideBuy, 100, 3), proto.TypeResend)
	p.applyExpect(t, Msg{Type: proto.TypeReject, Reject: proto.Reject{Instrument: 7, Reason: proto.ReasonResendUnavailable}}, proto.TypeSubscribe)
	p.apply(t, dlt(501, proto.SideBuy, 100, 3)) // waiting for the new snapshot: ignored
	p.apply(t, snap(501, 1))
	p.apply(t, lvl(proto.SideBuy, 99, 8))
	if bids, _ := p.f.Book(); !p.f.Live() || p.f.MDSeq() != 501 || !reflect.DeepEqual(bids, []Level{{99, 8}}) {
		t.Fatalf("after re-snapshot: live=%v md=%d bids=%v", p.f.Live(), p.f.MDSeq(), bids)
	}
	if p.f.Stats.Resnapshots != 1 || p.f.Stats.Snapshots != 2 {
		t.Fatalf("stats %+v", p.f.Stats)
	}
}

func TestFeedIgnoresOtherInstruments(t *testing.T) {
	p := newPipeFeed(t)
	p.apply(t, snap(10, 0))
	other := dlt(11, proto.SideBuy, 1, 1)
	other.BookDelta.Instrument = 8
	p.apply(t, other)
	if p.f.MDSeq() != 10 || p.f.Stats.Applied != 0 {
		t.Fatal("applied another instrument's delta")
	}
}

// Review F-005: a Resend refused by the server's rate limit is asked again
// on the first message after RetryDelay, not abandoned.
func TestFeedRetriesAThrottledResend(t *testing.T) {
	p := newPipeFeed(t)
	p.apply(t, snap(10, 0))
	p.applyExpect(t, dlt(12, proto.SideBuy, 100, 5), proto.TypeResend) // 11 is missing
	p.apply(t, Msg{Type: proto.TypeReject, Reject: proto.Reject{Instrument: 7, Reason: proto.ReasonTooManyRequests}})
	p.apply(t, dlt(13, proto.SideBuy, 100, 6)) // too soon: no request (net.Pipe would block the Feed if it sent one)
	time.Sleep(RetryDelay + 20*time.Millisecond)
	payload := p.applyExpect(t, dlt(14, proto.SideBuy, 100, 7), proto.TypeResend) // asks again, from 11
	var r proto.Resend
	if r.Decode(payload) != nil || r.FromMDSeq != 11 {
		t.Fatalf("retried resend %+v", r)
	}
	if p.f.Stats.Throttled != 1 || p.f.Stats.Resends != 2 {
		t.Fatalf("stats %+v", p.f.Stats)
	}
}
