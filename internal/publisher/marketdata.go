package publisher

import (
	"cmp"
	"slices"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
)

// Market data (milestone 6, decision 016).
//
// For each instrument the publisher keeps:
//   - a price-level view of the book (mdBook.bids/asks), built only from
//     released BookDelta events, so it is exactly the book as of mdSeq and
//     never shows anything not yet durable;
//   - a ring of the last RingSize market-data events, to serve Resend;
//   - the sessions subscribed to it.
//
// All of this belongs to the publisher goroutine. Subscribe and Resend
// requests from gateway goroutines arrive on p.requests and are handled
// between events, so a snapshot at md_seq S is always followed by exactly
// the events from S+1 on: no gap, no overlap.

// DefaultRingSize is the per-instrument resend window (chosen with the
// user, decision 016).
const DefaultRingSize = 1 << 16

type mdBook struct {
	mdSeq uint64 // md_seq of the last event applied
	bids  map[book.Price]book.Qty
	asks  map[book.Price]book.Qty

	ring      []engine.Event // ring[md_seq % len(ring)]; allocated on first use
	ringFirst uint64         // first md_seq stored since this process started (0 = none yet)

	subs []Subscriber
}

func newMDBook(mdSeq uint64) *mdBook {
	return &mdBook{mdSeq: mdSeq, bids: map[book.Price]book.Qty{}, asks: map[book.Price]book.Qty{}}
}

// oldestAvailable is the oldest md_seq the ring can still resend.
func (mb *mdBook) oldestAvailable(size int) uint64 {
	if mb.ringFirst == 0 {
		return mb.mdSeq + 1 // nothing stored yet
	}
	oldest := mb.ringFirst
	if mb.mdSeq >= uint64(size) && mb.mdSeq-uint64(size)+1 > oldest {
		oldest = mb.mdSeq - uint64(size) + 1
	}
	return oldest
}

// InitMarketData seeds an instrument's view at startup from the recovered
// engine: its md_seq and its levels as price -> total qty. Call before Run.
func (p *Publisher) InitMarketData(id engine.InstrumentID, mdSeq uint64, bids, asks map[book.Price]book.Qty) {
	mb := newMDBook(mdSeq)
	for px, q := range bids {
		mb.bids[px] = q
	}
	for px, q := range asks {
		mb.asks[px] = q
	}
	p.md[id] = mb
}

type requestKind uint8

const (
	reqSubscribe requestKind = iota + 1
	reqResend
)

type request struct {
	kind       requestKind
	instrument engine.InstrumentID
	from       uint64 // reqResend
	sub        Subscriber
}

// Subscribe asks for instrument's market data on s: a snapshot, then every
// delta and trade after it. Subscribing again sends a fresh snapshot.
// Safe from any goroutine.
func (p *Publisher) Subscribe(id engine.InstrumentID, s Subscriber) {
	p.request(request{kind: reqSubscribe, instrument: id, sub: s})
}

// Resend asks for instrument's market-data events from md_seq from on,
// out of the ring buffer. Safe from any goroutine.
func (p *Publisher) Resend(id engine.InstrumentID, from uint64, s Subscriber) {
	p.request(request{kind: reqResend, instrument: id, from: from, sub: s})
}

func (p *Publisher) request(r request) {
	select {
	case p.requests <- r:
	case <-p.stopped: // the publisher has shut down; nothing will answer
	}
}

func (p *Publisher) handle(r request) {
	mb := p.md[r.instrument]
	if mb == nil {
		p.offer(r.sub, engine.Event{Kind: engine.EvReject, Instrument: r.instrument, Reason: book.RejectUnknownInstrument})
		return
	}
	switch r.kind {
	case reqSubscribe:
		if !p.sendSnapshot(mb, r.instrument, r.sub) {
			return
		}
		if !slices.Contains(mb.subs, r.sub) {
			mb.subs = append(mb.subs, r.sub)
		}
		p.subscriptions.Add(1)
	case reqResend:
		p.resends.Add(1)
		if r.from > mb.mdSeq {
			return // nothing newer than what the client asked from exists yet
		}
		if r.from < mb.oldestAvailable(p.ringSize) {
			p.resendMisses.Add(1)
			p.offer(r.sub, engine.Event{Kind: engine.EvResendUnavailable, Instrument: r.instrument, MDSeq: r.from})
			return
		}
		for seq := r.from; seq <= mb.mdSeq; seq++ {
			if !p.offer(r.sub, mb.ring[seq%uint64(p.ringSize)]) {
				return
			}
		}
	}
}

// Snapshot is one instrument's market-data book as of MDSeq: bids best
// first, then asks best first. The publisher builds a fresh one for each
// subscription and never touches it again, so the session's writer can read
// it without locks while the publisher goes on changing its own view.
type Snapshot struct {
	Instrument engine.InstrumentID
	MDSeq      uint64
	Levels     []Level
}

// Level is one price level's total resting quantity.
type Level struct {
	Side  book.Side
	Price book.Price
	Qty   book.Qty
}

// sendSnapshot copies the instrument's levels as of mb.mdSeq, sorted, and
// hands them to the subscriber to stream.
func (p *Publisher) sendSnapshot(mb *mdBook, id engine.InstrumentID, s Subscriber) bool {
	levels := make([]Level, 0, len(mb.bids)+len(mb.asks))
	for px, q := range mb.bids {
		levels = append(levels, Level{book.Buy, px, q})
	}
	slices.SortFunc(levels, func(a, b Level) int { return cmp.Compare(b.Price, a.Price) }) // highest bid first
	nb := len(levels)
	for px, q := range mb.asks {
		levels = append(levels, Level{book.Sell, px, q})
	}
	slices.SortFunc(levels[nb:], func(a, b Level) int { return cmp.Compare(a.Price, b.Price) }) // lowest ask first

	if !s.DeliverSnapshot(&Snapshot{Instrument: id, MDSeq: mb.mdSeq, Levels: levels}) {
		p.slow(s, id, mb.mdSeq)
		return false
	}
	p.snapshotsSent.Add(1)
	return true
}

// marketData applies a released BookDelta or Trade to the instrument's view
// and ring, and fans it out to subscribers.
func (p *Publisher) marketData(ev *engine.Event) {
	mb := p.md[ev.Instrument]
	if mb == nil {
		mb = newMDBook(0)
		p.md[ev.Instrument] = mb
	}
	if ev.MDSeq != mb.mdSeq+1 {
		// The engine numbers md events gap-free and the publisher releases
		// them in order, so this is a bug upstream. Subscribers would see
		// the gap and resend; log it loudly rather than hide it.
		p.log.Error("market data out of sequence", "instrument", ev.Instrument, "md_seq", ev.MDSeq, "expected", mb.mdSeq+1)
	}
	if ev.Kind == engine.EvBookDelta {
		side := mb.bids
		if ev.Side == book.Sell {
			side = mb.asks
		}
		if ev.Qty == 0 {
			delete(side, ev.Price)
		} else {
			side[ev.Price] = ev.Qty
		}
	}
	mb.mdSeq = ev.MDSeq
	if mb.ring == nil {
		mb.ring = make([]engine.Event, p.ringSize)
	}
	mb.ring[ev.MDSeq%uint64(p.ringSize)] = *ev
	if mb.ringFirst == 0 {
		mb.ringFirst = ev.MDSeq
	}

	kept := mb.subs[:0]
	for _, s := range mb.subs {
		if s.Closed() {
			continue // the session went away; forget it
		}
		if p.offer(s, *ev) {
			kept = append(kept, s)
		}
	}
	clear(mb.subs[len(kept):]) // drop references to removed subscribers
	mb.subs = kept
}

// offer delivers ev to s without blocking. A full queue makes s a slow
// consumer: it is told to disconnect, and false is returned.
func (p *Publisher) offer(s Subscriber, ev engine.Event) bool {
	if s.Deliver(ev) {
		p.delivered.Add(1)
		return true
	}
	p.slow(s, ev.Instrument, ev.MDSeq)
	return false
}

// slow disconnects a market-data subscriber that couldn't take more.
func (p *Publisher) slow(s Subscriber, id engine.InstrumentID, mdSeq uint64) {
	p.slowConsumers.Add(1)
	p.log.Warn("slow market-data consumer disconnected", "instrument", id, "md_seq", mdSeq)
	s.SlowConsumer()
}
