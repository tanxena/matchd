package book

import (
	"strings"
	"testing"
)

func TestInstrumentValidateSymbol(t *testing.T) {
	ok := []string{"XYZ", "BRK.B", "es_z6", "A-1", strings.Repeat("A", MaxSymbolLen)}
	bad := []string{"", strings.Repeat("A", MaxSymbolLen+1), "has space", "quo\"te", "é", "a/b"}
	for _, s := range ok {
		if err := (Instrument{Symbol: s, TickSize: 1, LotSize: 1}).Validate(); err != nil {
			t.Errorf("%q rejected: %v", s, err)
		}
	}
	for _, s := range bad {
		if (Instrument{Symbol: s, TickSize: 1, LotSize: 1}).Validate() == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
