package durable

import (
	"errors"
	"sync"
	"testing"
)

func TestAdvanceIsMonotonicAndNotifies(t *testing.T) {
	d := New(5)
	d.Advance(3) // lower: ignored, no ping
	select {
	case <-d.Notify():
		t.Fatal("ping for a non-advance")
	default:
	}
	d.Advance(8)
	d.Advance(9) // second ping coalesces with the first
	if d.Load() != 9 {
		t.Fatalf("seq %d", d.Load())
	}
	<-d.Notify()
	select {
	case <-d.Notify():
		t.Fatal("pings should coalesce to one")
	default:
	}
}

func TestFailStopsAdvancing(t *testing.T) {
	d := New(0)
	d.Advance(2)
	boom := errors.New("fsync: input/output error")
	d.Fail(boom)
	d.Fail(errors.New("second failure")) // ignored
	d.Advance(10)
	if d.Load() != 2 {
		t.Fatalf("advanced to %d after Fail", d.Load())
	}
	select {
	case <-d.Failed():
	default:
		t.Fatal("Failed not closed")
	}
	if !errors.Is(d.Err(), boom) {
		t.Fatalf("Err = %v", d.Err())
	}
}

// Many advancers at once (run with -race): the result is the maximum.
func TestConcurrentAdvance(t *testing.T) {
	d := New(0)
	var wg sync.WaitGroup // the advancing goroutines
	for g := range uint64(8) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := uint64(1); i <= 1000; i++ {
				d.Advance(i*8 + g)
			}
		}()
	}
	wg.Wait()
	if d.Load() != 8007 {
		t.Fatalf("seq %d, want 8007", d.Load())
	}
}
