// Package sequencer gives every command its global sequence number and
// timestamp before the engine sees it.
//
// Milestone 3 version: in-memory only. Milestone 4 adds the write-ahead log,
// the fsync policies and ack gating; the contract below stays the same.
//
// The sequencer is where time enters the system. It reads the clock once per
// command and writes the result onto the command; the engine only copies it.
// That way replaying the log reproduces the same timestamps (PRD 5.4).
package sequencer

import (
	"time"

	"github.com/tanxena/matchd/internal/engine"
)

// Clock returns the current time in nanoseconds since the Unix epoch.
type Clock func() engine.Timestamp

// WallClock reads the system clock.
func WallClock() engine.Timestamp { return engine.Timestamp(time.Now().UnixNano()) }

// Sequencer stamps commands. Not safe for concurrent use: exactly one
// goroutine (the one calling Run) owns it.
type Sequencer struct {
	clock   Clock
	lastSeq engine.Seq
}

// New returns a sequencer whose first command gets Seq 1.
func New(clock Clock) *Sequencer {
	return &Sequencer{clock: clock}
}

// Stamp assigns the next Seq and the current time to c.
func (s *Sequencer) Stamp(c *engine.Command) {
	s.lastSeq++
	c.Seq = s.lastSeq
	c.Time = s.clock()
}

// Run is the sequencer goroutine's body: it stamps every command from in, in
// arrival order, and passes it to out. The order commands arrive on in IS the
// global order; every gateway reader sends into the same channel. When in is
// closed, Run closes out.
func (s *Sequencer) Run(in <-chan engine.Command, out chan<- engine.Command) {
	defer close(out)
	for c := range in {
		s.Stamp(&c)
		out <- c
	}
}
