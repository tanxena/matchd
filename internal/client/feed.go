package client

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/tanxena/matchd/internal/proto"
)

// Feed maintains one instrument's order book from the market-data stream
// and recovers from gaps (PRD 5.6, decision 016):
//
//   - It subscribes, waits for the Snapshot and its levels, then applies
//     BookDelta and Trade messages in md_seq order.
//   - A message whose md_seq is past the next expected one is a gap: the
//     Feed sends Resend(next expected) and ignores later messages until the
//     missing one arrives (the resend replays everything from there on, in
//     order, before any newer live message).
//   - Messages at or below the current md_seq are duplicates and ignored.
//   - Reject(ResendUnavailable) means the gap is older than the server's
//     ring: the Feed subscribes again and starts over from a new snapshot.
//   - Reject(TooManyRequests) means the server's per-connection rate limit
//     refused the request (review F-005): the Feed asks again on the first
//     message after RetryDelay.
//
// A Feed is driven by its owner calling Apply with every message read from
// the connection; it is not safe for concurrent use.
type Feed struct {
	conn       *Conn
	instrument uint32

	state     feedState
	remaining uint32 // snapshot levels still to come
	mdSeq     uint64 // md_seq the book reflects
	bids      map[int64]int64
	asks      map[int64]int64

	// Drop, if set, is consulted for every BookDelta and Trade for this
	// instrument; returning true discards it, so tests can induce md_seq
	// gaps. Snapshots are never dropped: TCP doesn't lose a frame on its
	// own, and a gap in the sequenced stream is what the recovery path
	// handles.
	Drop func(m Msg) bool

	// After a TooManyRequests reject: don't ask again before retryAt; and
	// resubscribe is set if the refused request was a Subscribe.
	retryAt     time.Time
	resubscribe bool

	Stats FeedStats
}

// RetryDelay is how long a Feed waits after a TooManyRequests reject before
// asking again: a little over the server's refill time for one request.
const RetryDelay = 200 * time.Millisecond

// FeedStats counts what the Feed has done.
type FeedStats struct {
	Snapshots   int // snapshots applied
	Gaps        int // gaps detected
	Resends     int // Resend requests sent
	Resnapshots int // re-subscriptions after ResendUnavailable
	Throttled   int // requests refused with TooManyRequests (and retried)
	Duplicates  int // messages ignored as already applied
	Applied     int // deltas and trades applied
	Trades      int
}

type feedState uint8

const (
	awaitingSnapshot feedState = iota + 1
	loadingLevels
	live
	recovering // a gap was seen; waiting for the resend
)

// NewFeed subscribes to instrument on conn and returns the Feed tracking it.
func NewFeed(conn *Conn, instrument uint32) (*Feed, error) {
	f := &Feed{conn: conn, instrument: instrument}
	return f, f.subscribe()
}

func (f *Feed) subscribe() error {
	f.state = awaitingSnapshot
	return f.conn.Subscribe(proto.Subscribe{Instrument: f.instrument})
}

// Live reports whether the Feed has a complete, gap-free book.
func (f *Feed) Live() bool { return f.state == live }

// MDSeq returns the md_seq the book reflects.
func (f *Feed) MDSeq() uint64 { return f.mdSeq }

// Apply processes one message from the connection. Messages that aren't
// market data for this Feed's instrument are ignored.
func (f *Feed) Apply(m Msg) error {
	switch m.Type {
	case proto.TypeSnapshot:
		if m.Snapshot.Instrument != f.instrument || f.state != awaitingSnapshot {
			return nil // a snapshot we didn't ask for (e.g. from an earlier subscribe)
		}
		f.bids, f.asks = map[int64]int64{}, map[int64]int64{}
		f.mdSeq, f.remaining = m.Snapshot.MDSeq, m.Snapshot.Levels
		f.state = loadingLevels
		if f.remaining == 0 {
			f.state = live
		}
		f.Stats.Snapshots++
	case proto.TypeSnapshotLevel:
		if m.SnapshotLevel.Instrument != f.instrument || f.state != loadingLevels {
			return nil
		}
		l := m.SnapshotLevel
		f.side(l.Side)[l.Price] = l.Qty
		f.remaining--
		if f.remaining == 0 {
			f.state = live
		}
	case proto.TypeBookDelta:
		if m.BookDelta.Instrument == f.instrument {
			return f.sequenced(m, m.BookDelta.MDSeq)
		}
	case proto.TypeTrade:
		if m.Trade.Instrument == f.instrument {
			return f.sequenced(m, m.Trade.MDSeq)
		}
	case proto.TypeReject:
		if m.Reject.Instrument != f.instrument {
			return nil
		}
		switch m.Reject.Reason {
		case proto.ReasonResendUnavailable:
			f.Stats.Resnapshots++
			return f.subscribe()
		case proto.ReasonTooManyRequests:
			f.Stats.Throttled++
			f.retryAt = time.Now().Add(RetryDelay)
			switch f.state {
			case recovering: // the Resend was refused: ask again, later
				f.state = live
			case awaitingSnapshot: // the Subscribe was refused
				f.resubscribe = true
			}
		}
	}
	return nil
}

// sequenced handles a delta or trade carrying md_seq.
func (f *Feed) sequenced(m Msg, md uint64) error {
	if f.resubscribe && !time.Now().Before(f.retryAt) {
		f.resubscribe = false
		return f.subscribe()
	}
	if f.state == awaitingSnapshot || f.state == loadingLevels {
		return nil // the snapshot will already include it, or isn't here yet
	}
	if f.Drop != nil && f.Drop(m) {
		return nil
	}
	switch {
	case md <= f.mdSeq:
		f.Stats.Duplicates++
		return nil
	case md > f.mdSeq+1:
		if f.state == live && !time.Now().Before(f.retryAt) { // first sign of this gap: ask once
			f.Stats.Gaps++
			f.Stats.Resends++
			f.state = recovering
			return f.conn.Resend(proto.Resend{Instrument: f.instrument, FromMDSeq: f.mdSeq + 1})
		}
		return nil // still recovering: the resend will bring it in order
	}
	// md == f.mdSeq+1: the next one.
	if m.Type == proto.TypeBookDelta {
		d := m.BookDelta
		if d.Qty == 0 {
			delete(f.side(d.Side), d.Price)
		} else {
			f.side(d.Side)[d.Price] = d.Qty
		}
	} else {
		f.Stats.Trades++
	}
	f.mdSeq = md
	f.Stats.Applied++
	f.state = live
	return nil
}

func (f *Feed) side(s uint8) map[int64]int64 {
	if s == proto.SideBuy {
		return f.bids
	}
	return f.asks
}

// Level is one price level of the book.
type Level struct {
	Price, Qty int64
}

// Book returns the bids (best first) and asks (best first).
func (f *Feed) Book() (bids, asks []Level) {
	for px, q := range f.bids {
		bids = append(bids, Level{px, q})
	}
	for px, q := range f.asks {
		asks = append(asks, Level{px, q})
	}
	slices.SortFunc(bids, func(a, b Level) int { return cmp.Compare(b.Price, a.Price) })
	slices.SortFunc(asks, func(a, b Level) int { return cmp.Compare(a.Price, b.Price) })
	return bids, asks
}

// String renders the top n levels per side, for engctl.
func (f *Feed) String(n int) string {
	bids, asks := f.Book()
	s := fmt.Sprintf("instrument %d @ md_seq %d\n%12s %10s | %-10s %s\n", f.instrument, f.mdSeq, "bid qty", "bid", "ask", "ask qty")
	for i := range n {
		var l, r string
		if i < len(bids) {
			l = fmt.Sprintf("%12d %10d", bids[i].Qty, bids[i].Price)
		} else {
			l = fmt.Sprintf("%12s %10s", "", "")
		}
		if i < len(asks) {
			r = fmt.Sprintf("%-10d %d", asks[i].Price, asks[i].Qty)
		}
		if i >= len(bids) && i >= len(asks) {
			break
		}
		s += l + " | " + r + "\n"
	}
	return s
}
