package sequencer

import (
	"testing"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
)

func TestRunStampsInOrder(t *testing.T) {
	var now engine.Timestamp = 1000
	s := New(func() engine.Timestamp { now += 5; return now })
	in := make(chan engine.Command)     // test -> sequencer goroutine
	out := make(chan engine.Command, 8) // sequencer goroutine -> test
	go s.Run(in, out)
	go func() { // producer: three commands, then close
		for i := range 3 {
			in <- engine.Command{Kind: engine.CmdCancel, OrderID: book.OrderID(100 + i)}
		}
		close(in)
	}()
	var got []engine.Command
	for c := range out { // ends when Run closes out
		got = append(got, c)
	}
	if len(got) != 3 {
		t.Fatalf("got %d commands", len(got))
	}
	for i, c := range got {
		if c.Seq != engine.Seq(i+1) || c.Time != engine.Timestamp(1005+5*i) || c.OrderID != book.OrderID(100+i) {
			t.Errorf("command %d: seq=%d time=%d order=%d", i, c.Seq, c.Time, c.OrderID)
		}
	}
}
