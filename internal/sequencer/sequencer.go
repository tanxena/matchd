// Package sequencer gives every command its global sequence number and
// timestamp, writes it to the write-ahead log, and applies the fsync policy
// (decision 012).
//
// The sequencer is where time enters the system. It reads the clock once per
// command and writes the result into the log with the command; the engine
// only copies it. Replaying the log therefore reproduces the same timestamps
// (PRD 5.4).
package sequencer

import (
	"fmt"
	"strings"
	"time"

	"github.com/tanxena/matchd/internal/clock"
	"github.com/tanxena/matchd/internal/durable"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/wal"
)

// Clock returns the current time in nanoseconds since the Unix epoch.
type Clock func() engine.Timestamp

// WallClock reads the wall clock at internal/clock's resolution: on
// Windows, time.Now only advances every timer tick (about 0.5 ms), which
// would give a whole burst of commands the same timestamp (decision 017).
func WallClock() engine.Timestamp { return engine.Timestamp(clock.WallNanos()) }

// Mode is an fsync policy.
type Mode uint8

const (
	// Always: a batch of commands is written and fsynced before any of them
	// reaches the engine.
	Always Mode = iota + 1
	// Group: commands reach the engine as soon as they are written; a
	// syncer fsyncs at most every Window; the publisher holds their events
	// until the fsync covers them.
	Group
	// Never: written but never fsynced. Tests only: a power failure can
	// lose any amount of acknowledged work.
	Never
)

// Policy is a parsed -fsync setting.
type Policy struct {
	Mode Mode
	// Window is the group-commit interval: the longest a written command
	// waits for its fsync (Group only).
	Window time.Duration
	// MaxBatch caps how many commands the sequencer takes in one batch, and
	// (Group) how many may be written but unsynced before the syncer is
	// told to fsync at once instead of waiting out the window.
	MaxBatch int
}

// Defaults chosen with the user (decision 012).
const (
	DefaultWindow   = time.Millisecond
	DefaultMaxBatch = 4096
)

// ParsePolicy parses "always", "never", "group" or "group:<duration>".
func ParsePolicy(s string) (Policy, error) {
	p := Policy{Window: DefaultWindow, MaxBatch: DefaultMaxBatch}
	switch {
	case s == "always":
		p.Mode = Always
	case s == "never":
		p.Mode = Never
	case s == "group":
		p.Mode = Group
	case strings.HasPrefix(s, "group:"):
		d, err := time.ParseDuration(strings.TrimPrefix(s, "group:"))
		if err != nil || d <= 0 {
			return p, fmt.Errorf("fsync policy %q: want group:<positive duration>, e.g. group:1ms", s)
		}
		p.Mode, p.Window = Group, d
	default:
		return p, fmt.Errorf("fsync policy %q: want always, group:<duration> or never", s)
	}
	return p, nil
}

func (p Policy) String() string {
	switch p.Mode {
	case Always:
		return "always"
	case Group:
		return "group:" + p.Window.String()
	case Never:
		return "never"
	}
	return "invalid"
}

// Sequencer stamps commands, logs them, and enforces the fsync policy. It
// is owned by exactly one goroutine (the one calling Run, or startup code
// calling LogNow before Run).
type Sequencer struct {
	clock   Clock
	log     *wal.Log
	policy  Policy
	durable *durable.Seq

	lastSeq engine.Seq
	enc     []byte           // one encoded command, reused
	batch   []engine.Command // reused across batches
}

// New returns a sequencer that continues after the log's last record.
func New(clock Clock, log *wal.Log, policy Policy, d *durable.Seq) *Sequencer {
	if policy.MaxBatch <= 0 {
		policy.MaxBatch = DefaultMaxBatch
	}
	return &Sequencer{
		clock: clock, log: log, policy: policy, durable: d,
		lastSeq: engine.Seq(log.LastAppended()),
		batch:   make([]engine.Command, 0, policy.MaxBatch),
	}
}

// Stamp assigns the next Seq and the current time to c.
func (s *Sequencer) Stamp(c *engine.Command) {
	s.lastSeq++
	c.Seq = s.lastSeq
	c.Time = s.clock()
}

func (s *Sequencer) append(c *engine.Command) {
	s.enc = engine.AppendCommand(s.enc[:0], c)
	s.log.Append(uint64(c.Seq), int64(c.Time), s.enc)
}

// LogNow stamps c and makes it durable before returning, whatever the
// policy. Startup uses it for configured instruments, before Run starts.
func (s *Sequencer) LogNow(c *engine.Command) error {
	s.Stamp(c)
	s.append(c)
	if err := s.log.Flush(); err != nil {
		return err
	}
	n, err := s.log.Sync()
	if err != nil {
		return err
	}
	s.durable.Advance(n)
	return nil
}

// Run is the sequencer goroutine's body. It returns nil after in is closed
// and everything has been written (and, unless Never, fsynced), or the
// first write or fsync error. Either way it closes out. On error it has
// already marked durability failed, so no event for an unsynced command will
// ever be released.
func (s *Sequencer) Run(in <-chan engine.Command, out chan<- engine.Command) error {
	defer close(out)

	var syncer *groupSyncer
	if s.policy.Mode == Group {
		syncer = startGroupSyncer(s.log, s.durable, s.policy.Window)
		defer syncer.stop()
	}

	for {
		var c engine.Command
		var ok bool
		select {
		case c, ok = <-in:
		case <-s.durable.Failed():
			// The group syncer's fsync failed. Stop feeding the engine:
			// nothing it produces could ever be released.
			return s.durable.Err()
		}
		if !ok {
			return s.finish(syncer)
		}
		// Take whatever else is already waiting, up to MaxBatch, without
		// blocking: one write (and under Always one fsync) for all of it.
		s.batch = append(s.batch[:0], c)
		inClosed := false
	drain:
		for len(s.batch) < s.policy.MaxBatch {
			select {
			case c, ok := <-in:
				if !ok {
					inClosed = true
					break drain
				}
				s.batch = append(s.batch, c)
			default:
				break drain
			}
		}

		for i := range s.batch {
			s.Stamp(&s.batch[i])
			s.append(&s.batch[i])
		}
		if err := s.log.Flush(); err != nil {
			return s.fail(err)
		}
		switch s.policy.Mode {
		case Always:
			n, err := s.log.Sync()
			if err != nil {
				return s.fail(err) // the batch is never forwarded
			}
			s.durable.Advance(n)
		case Never:
			s.durable.Advance(s.log.LastAppended())
		case Group:
			syncer.markDirty()
			if s.log.LastAppended()-s.log.LastSynced() >= uint64(s.policy.MaxBatch) {
				syncer.kick() // don't let unsynced work (and held events) pile up
			}
		}
		for i := range s.batch {
			out <- s.batch[i]
		}
		if inClosed {
			return s.finish(syncer)
		}
	}
}

// finish makes everything written durable before Run returns (except under
// Never, which never fsyncs).
func (s *Sequencer) finish(syncer *groupSyncer) error {
	if syncer != nil {
		syncer.stop() // no concurrent Sync from here on
		if err := syncer.err(); err != nil {
			return err
		}
	}
	if s.policy.Mode == Never {
		return nil
	}
	n, err := s.log.Sync()
	if err != nil {
		return s.fail(err)
	}
	s.durable.Advance(n)
	return nil
}

func (s *Sequencer) fail(err error) error {
	s.durable.Fail(err)
	return err
}

// LastSeq returns the last Seq assigned.
func (s *Sequencer) LastSeq() engine.Seq { return s.lastSeq }

// groupSyncer is the group-commit fsync goroutine. After the sequencer
// writes a batch it calls markDirty; the syncer then waits out the window
// (or until kicked), fsyncs once for everything written so far, and
// advances the durable seq.
type groupSyncer struct {
	log     *wal.Log
	durable *durable.Seq
	window  time.Duration

	dirty   chan struct{} // capacity 1: "there is unsynced data"
	kickCh  chan struct{} // capacity 1: "sync now, don't wait for the window"
	stopCh  chan struct{} // closed by stop
	done    chan struct{} // closed when the goroutine has exited
	stopped bool          // only touched by the sequencer goroutine
	failure error         // set by the syncer goroutine before closing done
}

func startGroupSyncer(log *wal.Log, d *durable.Seq, window time.Duration) *groupSyncer {
	g := &groupSyncer{
		log: log, durable: d, window: window,
		dirty: make(chan struct{}, 1), kickCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}), done: make(chan struct{}),
	}
	go g.run()
	return g
}

func (g *groupSyncer) markDirty() {
	select {
	case g.dirty <- struct{}{}:
	default:
	}
}

func (g *groupSyncer) kick() {
	select {
	case g.kickCh <- struct{}{}:
	default:
	}
}

// stop ends the goroutine and waits for it. Safe to call twice.
func (g *groupSyncer) stop() {
	if g.stopped {
		return
	}
	g.stopped = true
	close(g.stopCh)
	<-g.done
}

// err returns the fsync error that stopped the syncer, if any. Only valid
// after stop.
func (g *groupSyncer) err() error { return g.failure }

func (g *groupSyncer) run() {
	defer close(g.done)
	timer := time.NewTimer(time.Hour) // reused for every window
	timer.Stop()
	for {
		select {
		case <-g.dirty:
		case <-g.kickCh:
		case <-g.stopCh:
			return
		}
		// Data is waiting. Let more arrive for up to one window, unless
		// told to hurry, then cover all of it with one fsync.
		timer.Reset(g.window)
		select {
		case <-timer.C:
		case <-g.kickCh:
			timer.Stop()
		case <-g.stopCh:
			timer.Stop()
			return // the sequencer does the final fsync itself
		}
		n, err := g.log.Sync()
		if err != nil {
			g.failure = err
			g.durable.Fail(err)
			return
		}
		g.durable.Advance(n)
	}
}
