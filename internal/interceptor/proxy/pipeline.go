// Package proxy is Notary's reverse proxy in front of Mem0. Its forwarding
// handler lives in proxy.go; this file is the write pipeline the handler feeds:
// a bounded, non-blocking queue drained by a single writer goroutine so that a
// slow or stalled audit write can never delay a customer's Mem0 traffic (spec
// section 6).
package proxy

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"notary/internal/gap"
	"notary/internal/interceptor"
	"notary/internal/record"
)

// RecordSink is everything the write pipeline needs downstream: somewhere to
// put a record, and a way to close it. *interceptor.AuditWriter already
// satisfies it, so proxy mode hands the pipeline the same fail-open-loud writer
// library mode uses.
type RecordSink interface {
	interceptor.Sink
	Close() error
}

// nopSink is the sink NewPipeline substitutes for an untyped nil sink, so the
// writer goroutine and Close have something safe to call. It is a value type,
// not a nil pointer, so it can never be a typed-nil interface that panics when
// called.
type nopSink struct{}

// Write discards the record.
func (nopSink) Write(record.Record) error { return nil }

// Close does nothing.
func (nopSink) Close() error { return nil }

// Compile-time proofs that the concrete sinks satisfy the interfaces.
var (
	_ RecordSink       = (*interceptor.AuditWriter)(nil)
	_ interceptor.Sink = (*Pipeline)(nil)
)

// dropIdentity is the identity of one dropped record: the three fields a
// reconcilable gap entry is built from -- the dropped record's Event, Scope and
// ID -- held in memory by the bounded tally (spec section 6).
type dropIdentity struct {
	kind  record.EventType
	scope record.Scope
	id    string
}

// Pipeline is the write pipeline: a bounded channel of records drained by one
// writer goroutine. The producer's Write never blocks -- a full queue means a
// counted drop, not a wait -- so an audit problem can never become a caller's
// problem. The pipeline satisfies interceptor.Sink, so a Task 3 handler can
// hand it to an Observer as its sink.
//
// The single writer goroutine is what makes the ledger's Seq follow enqueue
// order and keeps the ledger's own lock out of request goroutines. The price is
// paid on the other side: a crash loses whatever is still queued, and the queue
// is bounded, so the loss is bounded too.
type Pipeline struct {
	// sink is where queued records go. It is never nil: an untyped nil passed
	// to NewPipeline is replaced by nopSink. The pipeline does not create a
	// typed-nil sink itself.
	sink RecordSink
	// gaps is the hash-chained gap log the drop tally is drained into. It is
	// BORROWED: the pipeline never closes it (the sink owns its lifetime). A
	// nil gaps is tolerated -- drops are still counted, just not recorded.
	gaps *gap.Log
	// channels are the loud sinks the drop marker is written to, best-effort.
	// A nil element is skipped.
	channels []io.Writer
	// queue is the bounded hand-off from producers to the single writer.
	queue chan record.Record
	// tallyCap is the number of drop identities the tally retains before it
	// starts counting instead of naming. It is the clamped depth.
	tallyCap int

	// wg tracks the writer goroutine so Close can wait for it.
	wg sync.WaitGroup
	// closeOnce makes Close idempotent; closeErr is its memoised result.
	closeOnce sync.Once
	closeErr  error

	// mu guards the fields below: the closed flag, the drop count, and the
	// bounded tally. Write takes it to check closed and to send, so Close
	// cannot close the queue out from under an in-flight send.
	mu       sync.Mutex
	closed   bool
	drops    uint64
	held     []dropIdentity
	overflow uint64
}

// NewPipeline returns a Pipeline that queues records into a channel of capacity
// depth, drained by one writer goroutine that calls sink.Write. Dropped records
// (when the queue is full) are counted and, on Close, recorded through gaps. The
// drop marker is written to every element of channels, best-effort.
//
// channels are written from the writer goroutine, so a writer that another
// producer also writes to -- proxy.New's out is the natural example -- must be
// safe for concurrent use, or the two must be handed the same synchronizing
// wrapper (cmd/notary/proxy.go does the latter). The pipeline serializes only
// its own writes to them.
//
// depth below 1 is clamped to 1: a zero-capacity channel would make every write
// a drop, so a non-positive depth is treated as the smallest useful one rather
// than accepted. The drop tally retains up to depth identities plus a count of
// the rest (spec section 6).
//
// sink is borrowed and is closed by Close, but only after the tally is drained
// -- see Close for why the order matters. An untyped nil sink is tolerated and
// treated as a sink that writes nothing; note that a nil *interceptor.
// AuditWriter stored in a RecordSink is a NON-nil interface, so passing one
// would call Write on a nil pointer -- a caller must guard that case itself,
// exactly as library mode does.
//
// gaps is BORROWED: NewPipeline neither opens nor closes it. A nil gaps is
// tolerated -- drops are still counted, just not recorded.
func NewPipeline(sink RecordSink, gaps *gap.Log, channels []io.Writer, depth int) *Pipeline {
	if depth < 1 {
		depth = 1
	}
	if sink == nil {
		sink = nopSink{}
	}
	p := &Pipeline{
		sink:     sink,
		gaps:     gaps,
		channels: channels,
		queue:    make(chan record.Record, depth),
		tallyCap: depth,
	}
	p.wg.Add(1)
	go p.run()
	return p
}

// Write hands rec to the writer goroutine, or records it as a drop if the queue
// is full. It NEVER blocks: the send is a non-blocking select with a default
// branch, so a stalled or slow sink cannot delay the caller.
//
// A drop is not an error: it increments the drop tally and marks every loud
// channel, then Write still returns nil -- the fail-open-loud contract, so an
// audit problem can never become a caller's problem.
//
// Write is safe to call after Close, and a write that late is NOT silently
// discarded: it is counted and marked on the loud channels, but it cannot be
// gap-logged. The gap log is owned by the sink, which Close is in the middle of
// (or has finished) closing, and this branch cannot know which side of Close's
// tally drain it is on -- so no durable entry can be promised here, and the
// loud channel is the only honest one left. The late window is real: the
// command's srv.Shutdown returns after its timeout with handlers still running,
// and a handler still inside ModifyResponse calls Write after Close.
func (p *Pipeline) Write(rec record.Record) error {
	p.mu.Lock()
	if p.closed {
		// Counting the dropped record is deliberate rather than incidental:
		// this branch cannot know whether Close's one-shot tally drain has
		// already snapshotted, or whether the gap log it drains through is
		// still open, so it promises no durable entry and keeps the count --
		// the part that is always honest.
		p.drops++
		p.mu.Unlock()
		p.markDrop(rec, dropAfterClose)
		return nil
	}
	select {
	case p.queue <- rec:
		p.mu.Unlock()
		return nil
	default:
		p.recordDropLocked(rec)
		p.mu.Unlock()
		p.markDrop(rec, dropQueueFull)
		return nil
	}
}

// Drops reports how many records the pipeline has refused: records the queue
// refused because it was full, and records refused because the pipeline was
// already closed (Write's closed branch), which the queue never saw. It is for
// tests and operators.
func (p *Pipeline) Drops() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.drops
}

// recordDropLocked counts a drop and keeps its identity while the tally has
// room, counting the rest. The caller must hold p.mu.
func (p *Pipeline) recordDropLocked(rec record.Record) {
	p.drops++
	if len(p.held) < p.tallyCap {
		p.held = append(p.held, dropIdentity{
			kind:  rec.Event,
			scope: rec.Subject.Scope,
			id:    string(rec.ID),
		})
		return
	}
	p.overflow++
}

// run is the single writer goroutine. It ranges over the queue, writes each
// record through the sink, and drains the tally between appends so drops are
// recorded promptly rather than only at shutdown (spec section 6). It exits
// when Close closes the queue.
//
// Once Close has begun, the writer yields the tally to Close rather than
// draining it itself. That is what makes Close's drain-then-close-sink ordering
// load-bearing instead of incidentally redundant: after Close has set the
// closing flag, this goroutine stops draining, so Close -- running after it has
// waited for this goroutine -- is the sole drainer, and the tally is written
// while the sink (and the gap log it owns) is still open. The writer still
// drains between appends throughout normal operation, so prompt recording under
// overload is unchanged; only the shutdown window defers to Close.
func (p *Pipeline) run() {
	defer p.wg.Done()
	for rec := range p.queue {
		if err := p.sink.Write(rec); err != nil {
			p.report(err)
		}
		if p.closing() {
			continue
		}
		if err := p.drainTally(); err != nil {
			p.report(err)
		}
	}
}

// closing reports whether Close has stopped the pipeline accepting, which is
// the writer goroutine's cue to leave the tally to Close. The flag is read
// under p.mu, the same lock Close takes to set it, so once closing is observed
// the drain ordering is guaranteed rather than raced.
func (p *Pipeline) closing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Close stops accepting, drains the queue, drains the drop tally, and then
// closes the sink. It is idempotent.
//
// The order is deliberate and load-bearing: the tally is drained -- through
// gaps, which the sink owns -- BEFORE sink.Close runs. The sink is an
// AuditWriter whose Close is what closes the gap log, so closing it first would
// send every remaining drop entry into a closed log and lose it. Its error is
// the doubly-failed case only: a tally that could not be recorded, or a sink
// that could not be closed.
//
// The drain is one-shot, and that bounds what Close can promise: a Write that
// arrives after the pipeline is closed is counted and marked, but cannot be
// gap-logged (see Write). Close therefore has a window in which a late record
// is visible only on the loud channels -- stated here because "no record is
// lost silently" is the pipeline's whole purpose, and that window is the one
// place it is narrowed rather than met.
func (p *Pipeline) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		close(p.queue)
		p.mu.Unlock()

		// Wait for the writer goroutine to drain the queue before touching the
		// tally, so no writer and this goroutine drain it at once and nothing
		// in flight is missed.
		p.wg.Wait()

		drainErr := p.drainTally()
		closeErr := p.sink.Close()
		p.closeErr = errors.Join(drainErr, closeErr)
	})
	return p.closeErr
}

// drainTally writes one gap entry per retained drop identity, plus one final
// entry carrying the count of drops the tally could not name. It snapshots the
// tally under p.mu and records outside it, so a producer is never blocked on a
// gap-log write. Errors are joined, never swallowed; a nil gaps records
// nothing.
func (p *Pipeline) drainTally() error {
	p.mu.Lock()
	held := p.held
	overflow := p.overflow
	p.held = nil
	p.overflow = 0
	p.mu.Unlock()

	var errs []error
	for _, id := range held {
		if err := p.recordGap(id); err != nil {
			errs = append(errs, err)
		}
	}
	if overflow > 0 {
		if err := p.recordOverflowGap(overflow); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recordGap records one retained drop identity as a gap entry. The entry's
// Kind, Scope and CorrelationID are the dropped record's Event, Scope and ID --
// the shape ledger.GapBreaks accounts for, which is what makes the gap
// reconcilable rather than merely recorded. A nil gaps records nothing.
func (p *Pipeline) recordGap(id dropIdentity) error {
	if p.gaps == nil {
		return nil
	}
	entry := gap.Entry{
		At:            time.Now().UTC(),
		Kind:          id.kind,
		Scope:         id.scope,
		CorrelationID: id.id,
		Detail:        "proxy: audit record dropped (queue full)",
	}
	if err := p.gaps.Record(entry); err != nil {
		return fmt.Errorf("proxy: record dropped-record gap for %s: %w", id.id, err)
	}
	return nil
}

// recordOverflowGap records one final entry carrying the count of drops whose
// identities the bounded tally did not retain. It names no identity -- there is
// none to name -- which is exactly the bound stated rather than hidden. A nil
// gaps records nothing.
func (p *Pipeline) recordOverflowGap(n uint64) error {
	if p.gaps == nil {
		return nil
	}
	entry := gap.Entry{
		At: time.Now().UTC(),
		Detail: fmt.Sprintf(
			"proxy: %d further audit records dropped (queue full); identities beyond the tally bound not retained",
			n),
	}
	if err := p.gaps.Record(entry); err != nil {
		return fmt.Errorf("proxy: record dropped-record overflow gap (%d drops): %w", n, err)
	}
	return nil
}

// dropQueueFull and dropAfterClose name why a record was dropped, in the
// marker text. The after-Close reason states its own gap-log consequence,
// because that is the one drop that cannot also be gapped.
const (
	dropQueueFull  = "queue full"
	dropAfterClose = "write after close; this drop cannot be gap-logged"
)

// markDrop writes the one-line drop marker to every loud channel. It is
// best-effort -- a failing channel must not itself become an error -- and a nil
// element is skipped rather than dereferenced. The marker names only
// non-sensitive metadata: the event, the correlation ID and the scope. It never
// names the record's content.
func (p *Pipeline) markDrop(rec record.Record, reason string) {
	marker := dropMarker(reason, rec)
	for _, ch := range p.channels {
		if ch == nil {
			continue
		}
		_, _ = io.WriteString(ch, marker)
	}
}

// report writes a writer-side error to every loud channel, best-effort. This is
// the loud half of the doubly-failed case: a gap-log write that itself failed
// is reported on the process's channels rather than swallowed.
func (p *Pipeline) report(err error) {
	if err == nil {
		return
	}
	for _, ch := range p.channels {
		if ch == nil {
			continue
		}
		_, _ = fmt.Fprintf(ch, "notary: proxy write pipeline: %v\n", err)
	}
}

// dropMarker renders the single-line, human-readable marker for a dropped
// record: why it was dropped, and only the event, the correlation ID and the
// scope -- never the record's content -- so no memory text can reach a log
// through the loud path.
func dropMarker(reason string, rec record.Record) string {
	s := rec.Subject.Scope
	return fmt.Sprintf(
		"notary: proxy audit record dropped (%s): event=%s correlation=%s scope{user=%q agent=%q app=%q run=%q}\n",
		reason, rec.Event, rec.ID, s.UserID, s.AgentID, s.AppID, s.RunID,
	)
}
