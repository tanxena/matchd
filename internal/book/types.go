// Package book implements a single-instrument limit order book with
// price-time priority matching.
//
// A Book is not safe for concurrent use. By design it is owned by exactly one
// goroutine (the engine, from milestone 2 onward), so it contains no locks.
//
// All prices and quantities are integers: prices are counts of ticks and
// quantities are counts of lots. There are no floats anywhere in this package.
package book

import (
	"errors"
	"fmt"
)

// Price is a price expressed as a whole number of ticks. A valid limit price
// is strictly positive.
type Price int64

// Qty is a quantity expressed as a whole number of lots. A valid order
// quantity is strictly positive.
type Qty int64

// OrderID identifies an accepted order. It is assigned by the caller (the
// engine), never by the book, and must be unique for the life of the book.
type OrderID uint64

// ClientID identifies the client that owns an order.
type ClientID uint32

// ClientOrderID is the client's own identifier for an order. It must be unique
// per client among that client's live (resting) orders.
type ClientOrderID uint64

// Side is the side of the book an order belongs to. The zero value is invalid
// so that an uninitialised order is rejected rather than silently treated as a
// buy.
type Side uint8

const (
	Buy  Side = 1
	Sell Side = 2
)

// Opposite returns the side an order on s trades against.
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

func (s Side) String() string {
	switch s {
	case Buy:
		return "buy"
	case Sell:
		return "sell"
	}
	return fmt.Sprintf("side(%d)", uint8(s))
}

// OrderType selects how an order interacts with the book.
type OrderType uint8

const (
	// Limit trades at its price or better; any remainder rests.
	Limit OrderType = 1
	// Market trades at any price until filled or the opposite side is empty;
	// any remainder is cancelled (it never rests).
	Market OrderType = 2
)

func (t OrderType) String() string {
	switch t {
	case Limit:
		return "limit"
	case Market:
		return "market"
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// TIF is a limit order's time in force.
type TIF uint8

const (
	// GTC (good till cancelled) rests until filled or cancelled.
	GTC TIF = 1
	// Day is accepted and treated exactly like GTC in v1: there is no session
	// end, so nothing expires it. See docs/decisions/003.
	Day TIF = 2
	// IOC (immediate or cancel) trades what it can on arrival; the rest is
	// cancelled instead of resting (decision 020).
	IOC TIF = 3
	// FOK (fill or kill) trades its whole quantity on arrival or not at all:
	// if the book can't fill it completely it is rejected, untouched.
	FOK TIF = 4
)

func (t TIF) String() string {
	switch t {
	case GTC:
		return "gtc"
	case Day:
		return "day"
	case IOC:
		return "ioc"
	case FOK:
		return "fok"
	}
	return fmt.Sprintf("tif(%d)", uint8(t))
}

// RejectReason says why a command was refused. RejectNone means accepted.
type RejectReason uint8

const (
	RejectNone RejectReason = iota
	RejectInvalidSide
	RejectInvalidType
	RejectInvalidQty
	RejectInvalidPrice
	RejectInvalidTIF
	RejectDuplicateClientOrderID
	RejectUnknownOrder

	// Engine-level reasons. A Book never returns these; they live here so the
	// engine, the wire protocol and the reference book share one enum.
	RejectUnknownInstrument
	RejectInvalidInstrument
	RejectDuplicateSymbol

	// RejectFOKNoLiquidity: a fill-or-kill order the book can't fill
	// completely. A book reason, numbered after the engine's so that every
	// existing value keeps its meaning in logs and on the wire.
	RejectFOKNoLiquidity
)

func (r RejectReason) String() string {
	switch r {
	case RejectNone:
		return "none"
	case RejectInvalidSide:
		return "InvalidSide"
	case RejectInvalidType:
		return "InvalidType"
	case RejectInvalidQty:
		return "InvalidQty"
	case RejectInvalidPrice:
		return "InvalidPrice"
	case RejectInvalidTIF:
		return "InvalidTIF"
	case RejectDuplicateClientOrderID:
		return "DuplicateClientOrderID"
	case RejectUnknownOrder:
		return "UnknownOrder"
	case RejectUnknownInstrument:
		return "UnknownInstrument"
	case RejectInvalidInstrument:
		return "InvalidInstrument"
	case RejectDuplicateSymbol:
		return "DuplicateSymbol"
	case RejectFOKNoLiquidity:
		return "FOKNoLiquidity"
	}
	return fmt.Sprintf("reject(%d)", uint8(r))
}

// Order is a new-order command as the book sees it.
type Order struct {
	ID      OrderID
	Client  ClientID
	ClOrdID ClientOrderID
	Side    Side
	Type    OrderType
	TIF     TIF   // limit only; a market order ignores it, except that IOC and FOK are refused
	Price   Price // limit only; must be 0 for market orders
	Qty     Qty
}

// Result describes what happened to a submitted order.
//
// For an accepted order, Filled + Rested + Expired == the order's Qty.
type Result struct {
	Reject  RejectReason // RejectNone if the order was accepted
	Filled  Qty          // quantity traded immediately
	Rested  Qty          // quantity now resting on the book (limit only)
	Expired Qty          // unfilled quantity cancelled on arrival (market, or limit IOC)
}

// CancelResult describes the outcome of a cancel. On success it also echoes
// the removed order's side, price and client order id, so the caller can emit
// a cancel acknowledgement and a book delta without a second lookup.
type CancelResult struct {
	Reject    RejectReason // RejectNone if the order was found and removed
	Side      Side
	Price     Price
	ClOrdID   ClientOrderID
	Cancelled Qty // remaining quantity that was removed from the book
}

// Fill is one match between an incoming (taker) order and a resting (maker)
// order. It is reported once; the engine fans it out to both parties.
type Fill struct {
	Price        Price // always the maker's resting price
	Qty          Qty
	TakerSide    Side // aggressor side
	Taker        OrderID
	TakerClient  ClientID
	Maker        OrderID
	MakerClient  ClientID
	MakerClOrdID ClientOrderID
	MakerLeft    Qty // maker's remaining quantity after this fill; 0 means it left the book
}

// RestingOrder is a read-only view of an order on the book, used for
// inspection and for comparing books in tests.
type RestingOrder struct {
	ID      OrderID
	Client  ClientID
	ClOrdID ClientOrderID
	Side    Side
	Price   Price
	Qty     Qty // remaining quantity
}

// Instrument describes the single instrument a book trades.
//
// Prices and quantities inside the book are already counts of ticks and lots,
// so TickSize and LotSize are not used by matching. They record what one tick
// and one lot are worth (in the instrument's smallest price and quantity
// units) so that edges such as the CLI can convert for display.
type Instrument struct {
	Symbol   string
	TickSize int64
	LotSize  int64
}

func symbolRune(r rune) bool {
	switch {
	case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	}
	return r == '.' || r == '_' || r == '-'
}

// MaxSymbolLen bounds a symbol's length. The WAL's command encoding allows
// 255 bytes; the tighter limit keeps symbols readable and leaves room.
const MaxSymbolLen = 32

// Validate reports whether the instrument definition is usable: a symbol of
// 1 to MaxSymbolLen letters, digits, '.', '_' or '-', and positive tick and
// lot sizes.
func (in Instrument) Validate() error {
	if in.Symbol == "" {
		return errors.New("instrument: empty symbol")
	}
	if len(in.Symbol) > MaxSymbolLen {
		return fmt.Errorf("instrument: symbol is %d bytes, the limit is %d", len(in.Symbol), MaxSymbolLen)
	}
	for _, r := range in.Symbol {
		if !symbolRune(r) {
			return fmt.Errorf("instrument: symbol %q: only letters, digits, '.', '_' and '-' are allowed", in.Symbol)
		}
	}
	if in.TickSize <= 0 {
		return fmt.Errorf("instrument %s: tick size must be positive, got %d", in.Symbol, in.TickSize)
	}
	if in.LotSize <= 0 {
		return fmt.Errorf("instrument %s: lot size must be positive, got %d", in.Symbol, in.LotSize)
	}
	return nil
}

// Validate checks the fields of a new order that do not depend on book state.
// Checks run in a fixed order (side, type, qty, price, tif) so that an order
// with several problems always gets the same reason; the reference book
// applies the same order, and the differential test compares reasons.
func (o *Order) Validate() RejectReason {
	if o.Side != Buy && o.Side != Sell {
		return RejectInvalidSide
	}
	if o.Type != Limit && o.Type != Market {
		return RejectInvalidType
	}
	if o.Qty <= 0 {
		return RejectInvalidQty
	}
	switch o.Type {
	case Limit:
		if o.Price <= 0 {
			return RejectInvalidPrice
		}
		if o.TIF != GTC && o.TIF != Day && o.TIF != IOC && o.TIF != FOK {
			return RejectInvalidTIF
		}
	case Market:
		if o.Price != 0 {
			return RejectInvalidPrice
		}
		// IOC and FOK are limit-only (decision 020): a market order already
		// behaves like IOC, and "market FOK" is out of scope. Any other TIF
		// on a market order is ignored, as before.
		if o.TIF == IOC || o.TIF == FOK {
			return RejectInvalidTIF
		}
	}
	return RejectNone
}
