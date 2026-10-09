package main

import "testing"

func TestInstrumentFlag(t *testing.T) {
	var f instrumentFlags
	for _, v := range []string{"XYZ:1:1", "ABC:5:100"} {
		if err := f.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	want := instrumentFlags{{Symbol: "XYZ", TickSize: 1, LotSize: 1}, {Symbol: "ABC", TickSize: 5, LotSize: 100}}
	if len(f) != 2 || f[0] != want[0] || f[1] != want[1] {
		t.Fatalf("got %+v", f)
	}
	for _, bad := range []string{"XYZ", "XYZ:1", "XYZ:a:1", "XYZ:1:b", ":1:1", "XYZ:0:1", "XYZ:1:-5"} {
		var g instrumentFlags
		if err := g.Set(bad); err == nil {
			t.Errorf("Set(%q) accepted", bad)
		}
	}
}
