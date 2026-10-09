// Package engine is the single-threaded core: it consumes a totally ordered
// stream of commands, applies each to the right order book, and emits the
// resulting events.
//
// The engine is deterministic. Its output depends only on the sequence of
// commands it is given: it never reads the wall clock, never uses randomness,
// and never iterates a Go map in a way that affects output. Timestamps and
// sequence numbers arrive on the commands (from the sequencer, milestone 4)
// and are copied onto events. Replaying the same commands into a fresh engine
// therefore reproduces the same books and the same events, which is what
// recovery (milestone 5) relies on.
package engine

import (
	"fmt"

	"github.com/tanxena/matchd/internal/book"
)

// InstrumentID identifies an instrument (and its book) inside the engine.
// IDs are assigned in registration order starting at 1; 0 is never valid.
type InstrumentID uint32

// Seq is the global sequence number of a command. The sequencer assigns it;
// the engine requires each command's Seq to be exactly one more than the
// previous one.
type Seq uint64

// Timestamp is nanoseconds since the Unix epoch, assigned by the sequencer and
// stored with the command. The engine only copies it onto events.
type Timestamp int64

// CommandKind selects what a Command does.
type CommandKind uint8

const (
	CmdNewOrder      CommandKind = 1
	CmdCancel        CommandKind = 2
	CmdAddInstrument CommandKind = 3
)

func (k CommandKind) String() string {
	switch k {
	case CmdNewOrder:
		return "NewOrder"
	case CmdCancel:
		return "Cancel"
	case CmdAddInstrument:
		return "AddInstrument"
	}
	return fmt.Sprintf("cmd(%d)", uint8(k))
}

// Command is one input to the engine. It is a flat struct rather than an
// interface with one type per command so that it can be passed by value
// through a channel and stored in a reusable buffer without allocating.
// Fields not used by Kind are left zero.
type Command struct {
	Seq        Seq
	Time       Timestamp
	Kind       CommandKind
	Instrument InstrumentID  // NewOrder, Cancel
	Client     book.ClientID // who sent it; set by the gateway from the session

	// NewOrder
	ClOrdID book.ClientOrderID
	Side    book.Side
	Type    book.OrderType
	TIF     book.TIF
	Price   book.Price
	Qty     book.Qty

	// Cancel
	OrderID book.OrderID

	// AddInstrument
	Spec book.Instrument
}

// EventKind selects what an Event reports.
type EventKind uint8

const (
	// EvAck: a new order was accepted and given OrderID. Qty, Side, Price
	// echo the order. Always the first event for an accepted order.
	EvAck EventKind = 1
	// EvReject: the command was refused with Reason. The book is unchanged.
	EvReject EventKind = 2
	// EvFill: one party's side of a match. Each match produces two EvFill
	// events with the same MatchID: the taker's first, then the maker's.
	EvFill EventKind = 3
	// EvCancelled: an order's remaining quantity (Qty) left the book
	// without trading: by a cancel request, or because a market order's
	// remainder expired. CancelReason says which.
	EvCancelled EventKind = 4
	// EvBookDelta: the level at (Instrument, Side, Price) now holds Qty in
	// total; Qty 0 means the level is gone. Market data is built from these.
	EvBookDelta EventKind = 5
	// EvInstrumentAdded: an AddInstrument command succeeded; Instrument is
	// the new instrument's id.
	EvInstrumentAdded EventKind = 6
)

func (k EventKind) String() string {
	switch k {
	case EvAck:
		return "Ack"
	case EvReject:
		return "Reject"
	case EvFill:
		return "Fill"
	case EvCancelled:
		return "Cancelled"
	case EvBookDelta:
		return "BookDelta"
	case EvInstrumentAdded:
		return "InstrumentAdded"
	}
	return fmt.Sprintf("event(%d)", uint8(k))
}

// Role says which side of a match a fill event describes.
type Role uint8

const (
	RoleTaker Role = 1 // the incoming order
	RoleMaker Role = 2 // the resting order
)

// CancelReason says why an EvCancelled happened.
type CancelReason uint8

const (
	CancelRequested CancelReason = 1 // the owner sent a cancel
	CancelExpired   CancelReason = 2 // a market order's unfilled remainder
)

// Event is one output of the engine. Like Command it is a flat, pointer-free
// struct, so a slice of Events is a single allocation the GC never scans and
// events are copied, not shared. Fields not used by Kind are zero.
//
// Every event carries the Seq and Time of the command that caused it, which
// is how the publisher (milestone 4) holds events back until that command is
// durable.
type Event struct {
	Seq        Seq
	Time       Timestamp
	Kind       EventKind
	Instrument InstrumentID

	// Order identity, for routing to the owning client.
	Client  book.ClientID
	ClOrdID book.ClientOrderID
	OrderID book.OrderID

	Side  book.Side
	Price book.Price
	// Qty depends on Kind: Ack = order qty; Fill = matched qty;
	// Cancelled = qty removed; BookDelta = level's new total.
	Qty book.Qty
	// Leaves is the party's open quantity after a fill: for the taker, what
	// is still to be matched; for the maker, what is still resting.
	Leaves  book.Qty
	MatchID uint64 // Fill: shared by the taker and maker events of one match
	Role    Role   // Fill

	Reason       book.RejectReason // Reject
	CancelReason CancelReason      // Cancelled
}
