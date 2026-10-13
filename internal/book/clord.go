package book

import "fmt"

// ClOrdIDs is the set of live (client, client order id) pairs across a
// group of books: one engine's instruments. PRD 5.1 makes a client order id
// unique per client among that client's live orders, on any instrument
// (review F-003), so every book in the group checks it before accepting an
// order, and adds and removes its own resting orders.
//
// It is touched only by whoever owns the books (the engine goroutine), like
// the books themselves. Like a book's own indexes, it is not part of a
// snapshot: books are rebuilt from their orders, and Share adds them back.
type ClOrdIDs struct {
	live index[clientOrderKey] // value always 1 (0 marks an empty slot)
}

// NewClOrdIDs returns an empty set.
func NewClOrdIDs() *ClOrdIDs { return &ClOrdIDs{live: newIndex[clientOrderKey](1024)} }

// Len is the number of live pairs.
func (c *ClOrdIDs) Len() int { return c.live.len() }

// Share makes b check client order ids against c, and keep c up to date,
// from now on. The orders b already holds are added; it fails if one of
// them is already in c (a duplicate across books, which a valid state never
// has).
func (b *Book) Share(c *ClOrdIDs) error {
	if b.shared != nil {
		return fmt.Errorf("book %s: already shares client order ids", b.instr.Symbol)
	}
	var mine []RestingOrder
	for _, s := range [2]Side{Buy, Sell} {
		mine = b.Orders(s, mine)
	}
	for _, r := range mine {
		if _, dup := c.live.get(clientOrderKey{r.Client, r.ClOrdID}); dup {
			return fmt.Errorf("book %s: client %d's client order id %d is live on another instrument", b.instr.Symbol, r.Client, r.ClOrdID)
		}
	}
	for _, r := range mine {
		c.live.put(clientOrderKey{r.Client, r.ClOrdID}, 1)
	}
	b.shared = c
	return nil
}
