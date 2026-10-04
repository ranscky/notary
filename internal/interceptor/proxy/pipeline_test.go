package proxy_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/gap"
	"notary/internal/interceptor"
	"notary/internal/interceptor/proxy"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// guardedBuffer is a bytes.Buffer safe for concurrent use. The pipeline may
// write a drop marker from the producer goroutine that calls Write and a
// writer-side report from its writer goroutine, so a plain bytes.Buffer handed
// to it as a loud channel would be a data race under -race. It is this
// package's local, unexported copy of cmd/notary/proxy_test.go's syncBuffer:
// the two are different packages, so each test package declares its own.
type guardedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p under the lock.
func (b *guardedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns the buffer's contents under the lock.
func (b *guardedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---------------------------------------------------------------------------
// shared test fixtures (Task 3 reuses these -- do not redeclare in this package)
// ---------------------------------------------------------------------------

// stallingSink is a proxy.RecordSink the test can stall. Its Write records the
// record it received, signals the first call's arrival on started, and then
// blocks until release is closed -- which is what makes the pipeline's
// non-blocking contract testable. Its Close records that it was closed and, if
// onClose is set, runs onClose first so a test can observe the world at the
// instant the sink was closed (the ordering test uses this to check the gap log
// already held the drop entry).
//
// Task 3 reuses it to stall the proxy's write path.
type stallingSink struct {
	// started is closed on the first Write; release gates every Write.
	started chan struct{}
	release chan struct{}
	// onClose, if set, runs inside Close before the sink marks itself closed.
	onClose func()

	startOnce   sync.Once
	releaseOnce sync.Once

	mu       sync.Mutex
	recs     []record.Record
	isClosed bool
}

// newStallingSink returns a stallingSink whose every Write blocks until the
// test closes its release channel.
func newStallingSink() *stallingSink {
	return &stallingSink{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// Write records rec, signals started on the first call, and blocks until
// release is closed.
func (s *stallingSink) Write(rec record.Record) error {
	s.mu.Lock()
	s.recs = append(s.recs, rec)
	s.mu.Unlock()

	if s.started != nil {
		s.startOnce.Do(func() { close(s.started) })
	}
	if s.release != nil {
		<-s.release
	}
	return nil
}

// Release unblocks every Write. It is idempotent, so a test can release in its
// body and again from a cleanup without a double-close panic.
func (s *stallingSink) Release() {
	s.releaseOnce.Do(func() { close(s.release) })
}

// Close runs onClose (if set), then marks the sink closed.
func (s *stallingSink) Close() error {
	if s.onClose != nil {
		s.onClose()
	}
	s.mu.Lock()
	s.isClosed = true
	s.mu.Unlock()
	return nil
}

// received returns a copy of the records the sink has been handed.
func (s *stallingSink) received() []record.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]record.Record(nil), s.recs...)
}

// closed reports whether the sink has been closed.
func (s *stallingSink) closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isClosed
}

// newProxyHarness wires a real ledger and gap log over a temp dir into a
// working RecordSink, mirroring internal/interceptor/library's newHarness (a
// store, a signer, a ledger and a gap log over t.TempDir()). It returns the
// sink (an *interceptor.AuditWriter over those real files), the ledger the sink
// appends to, the gap log, and the gap log's path, so a test can read records
// back and read gap entries back.
//
// Task 3 reuses it.
func newProxyHarness(t *testing.T) (proxy.RecordSink, *ledger.Ledger, *gap.Log, string) {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	l := ledger.New(st, newProxySigner(t), func() time.Time { return proxyHarnessNow })

	gapPath := filepath.Join(dir, "gaps.log")
	g, err := gap.Open(gapPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	return interceptor.NewAuditWriter(l, g, nil), l, g, gapPath
}

// proxyHarnessNow is the deterministic clock newProxyHarness's ledger stamps
// RecordedAt with.
var proxyHarnessNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// proxyTestSeed is a fixed 32-byte ed25519 seed, so every harness signs with
// the same key and the tests stay deterministic.
var proxyTestSeed = []byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
}

// newProxySigner builds a Signer from proxyTestSeed through the env KeySource,
// following the sign package's own test pattern.
func newProxySigner(t *testing.T) *sign.Signer {
	t.Helper()
	t.Setenv("NOTARY_PROXY_TEST_KEY", base64.StdEncoding.EncodeToString(proxyTestSeed))
	s, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_PROXY_TEST_KEY"})
	require.NoError(t, err)
	require.NotNil(t, s)
	return s
}

// proxyTestRecord builds a minimal well-formed record with the given ID, event
// and scope: it carries a valid Observed reason and a non-zero content hash, so
// a real ledger accepts it and its event, scope and id are the ones a dropped
// record's gap entry should name.
func proxyTestRecord(t *testing.T, id string, ev record.EventType, scope record.Scope) record.Record {
	t.Helper()
	evi, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"ok":true}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonReturnedBySearch, evi)
	require.NoError(t, err)
	rec := record.Record{
		ID:      record.RecordID(id),
		At:      time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
		Event:   ev,
		Reason:  reason,
		Subject: record.Subject{MemoryID: "mem-1", Scope: scope, ContentHash: record.ContentHash(id)},
	}
	require.NoError(t, rec.Validate(), "the test fixture must be a valid record")
	return rec
}

// ---------------------------------------------------------------------------
// the non-blocking contract
// ---------------------------------------------------------------------------

// TestPipelineWriteDoesNotBlockWhenTheSinkStalls is the core contract: the
// caller's goroutine never waits on the sink. The writer is stalled inside the
// sink while the queue (depth 1) fills, and the record that cannot be queued is
// a counted drop -- and every Write still returns promptly.
func TestPipelineWriteDoesNotBlockWhenTheSinkStalls(t *testing.T) {
	sink := newStallingSink()
	p := proxy.NewPipeline(sink, nil, nil, 1)
	t.Cleanup(func() {
		sink.Release()
		require.NoError(t, p.Close())
	})

	scope := record.Scope{UserID: "u1"}
	// One record reaches the writer, which then blocks inside the sink.
	require.NoError(t, p.Write(proxyTestRecord(t, "stall-1", record.EventMemorySurfaced, scope)))
	<-sink.started

	// The queue has depth 1; the next record fills it and the one after that
	// cannot be queued. Both writes must return while the sink is still
	// blocked -- the producer never waits.
	done := make(chan struct{})
	go func() {
		_ = p.Write(proxyTestRecord(t, "stall-2", record.EventMemorySurfaced, scope))
		_ = p.Write(proxyTestRecord(t, "stall-3", record.EventMemorySurfaced, scope))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked on a stalled sink; the caller's goroutine must never wait")
	}

	assert.Equal(t, uint64(1), p.Drops(), "the record that could not be queued is a drop")
}

// TestPipelineDrainsAGapEntryForEachDrop pins that a dropped record leaves a
// durable gap entry in the shape ledger.GapBreaks accounts for: the dropped
// record's Event, Scope and ID as the entry's Kind, Scope and CorrelationID.
func TestPipelineDrainsAGapEntryForEachDrop(t *testing.T) {
	gapPath := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(gapPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	sink := newStallingSink()
	p := proxy.NewPipeline(sink, g, nil, 1)
	t.Cleanup(func() {
		sink.Release()
		_ = p.Close()
	})

	scope := record.Scope{UserID: "u1", AgentID: "a1"}
	dropped := proxyTestRecord(t, "dropped-1", record.EventMemorySurfaced, scope)

	require.NoError(t, p.Write(proxyTestRecord(t, "first", record.EventMemorySurfaced, scope)))
	<-sink.started
	require.NoError(t, p.Write(proxyTestRecord(t, "queued", record.EventMemorySurfaced, scope)))
	require.NoError(t, p.Write(dropped))
	require.Equal(t, uint64(1), p.Drops())

	sink.Release()
	require.NoError(t, p.Close())

	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	require.Len(t, entries, 1, "exactly one dropped record yields exactly one gap entry")
	assert.Equal(t, dropped.Event, entries[0].Kind)
	assert.Equal(t, dropped.Subject.Scope, entries[0].Scope)
	assert.Equal(t, string(dropped.ID), entries[0].CorrelationID)
}

// TestPipelineCloseDrainsQueuedRecords pins that records enqueued before Close
// reach the ledger: Close is not a way to lose the queue.
func TestPipelineCloseDrainsQueuedRecords(t *testing.T) {
	sink, l, g, _ := newProxyHarness(t)
	p := proxy.NewPipeline(sink, g, nil, 8)

	scope := record.Scope{UserID: "u1"}
	ids := []string{"q1", "q2", "q3"}
	for _, id := range ids {
		require.NoError(t, p.Write(proxyTestRecord(t, id, record.EventMemorySurfaced, scope)))
	}

	require.NoError(t, p.Close())

	for _, id := range ids {
		rec, err := l.GetRecord(record.RecordID(id))
		require.NoError(t, err, "a record enqueued before Close must be in the ledger after it")
		assert.Equal(t, record.RecordID(id), rec.ID)
	}
}

// TestPipelineCloseDrainsTheTallyBeforeClosingTheSink pins the ordering the
// trap warns about: Close drains the drop tally (which needs the gap log) BEFORE
// it closes its sink, because the sink owns the gap log and closing it first
// would lose every drop entry still in the tally. The fake sink records the gap
// log's contents at the instant its Close runs.
//
// It is load-bearing, not merely observable: the writer goroutine is stalled on
// a record and Close has already begun, so the writer yields the tally to Close
// (see run). That makes Close the sole drainer, so swapping sink.Close() and
// drainTally() inside Close leaves the gap empty at the moment the sink is
// closed and fails this test.
func TestPipelineCloseDrainsTheTallyBeforeClosingTheSink(t *testing.T) {
	gapPath := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(gapPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	var (
		gapAtClose   []gap.Entry
		readAtClose  bool
		readCloseErr error
	)
	sink := newStallingSink()
	sink.onClose = func() {
		gapAtClose, readCloseErr = gap.Read(gapPath)
		readAtClose = true
	}

	var marks guardedBuffer
	p := proxy.NewPipeline(sink, g, []io.Writer{&marks}, 1)

	scope := record.Scope{UserID: "u1"}
	require.NoError(t, p.Write(proxyTestRecord(t, "first", record.EventMemorySurfaced, scope)))
	<-sink.started
	require.NoError(t, p.Write(proxyTestRecord(t, "queued", record.EventMemorySurfaced, scope)))
	require.NoError(t, p.Write(proxyTestRecord(t, "dropped-1", record.EventMemorySurfaced, scope)))
	require.Equal(t, uint64(1), p.Drops())

	// Close blocks until the writer goroutine finishes, so stop accepting from a
	// second goroutine and let the sink release the writer below.
	closeDone := make(chan struct{})
	go func() {
		_ = p.Close()
		close(closeDone)
	}()

	// Wait until Close has stopped the pipeline accepting. A probe write that
	// arrives before that is a queue-full drop and one that arrives after it is
	// a write-after-close drop; both are counted, so the Drops tally alone
	// cannot tell them apart, and the marker is what distinguishes them. Once
	// the after-close marker is seen, the writer goroutine is guaranteed --
	// through p.mu -- to observe the closure and leave the tally to Close,
	// making Close the sole drainer. The stray probes overflow the depth-1
	// tally, which is fine: the test asserts the named drop is durable, not
	// that it is the only entry.
	probe := proxyTestRecord(t, "probe", record.EventMemorySurfaced, scope)
	require.Eventually(t, func() bool {
		_ = p.Write(probe)
		return strings.Contains(marks.String(), "write after close")
	}, 2*time.Second, time.Millisecond, "Close never stopped the pipeline accepting")

	sink.Release()
	<-closeDone

	require.True(t, readAtClose, "the sink's Close must have run")
	require.NoError(t, readCloseErr)
	require.NotEmpty(t, gapAtClose,
		"the gap held no entry at the moment the sink was closed -- Close drained the tally after closing its sink")

	found := false
	for _, e := range gapAtClose {
		if e.CorrelationID == "dropped-1" {
			found = true
			break
		}
	}
	assert.True(t, found,
		"the dropped record's gap entry must be durable before the sink is closed")
}

// TestPipelineBoundedTallyCountsTheDropsItCannotName pins the bounded tally: it
// holds up to depth identities and a running count of the rest. With depth 1,
// one identity is retained and the remaining drops are named only by count, so
// the ledger shows a bounded number of named gaps plus an honest total.
func TestPipelineBoundedTallyCountsTheDropsItCannotName(t *testing.T) {
	gapPath := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(gapPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	sink := newStallingSink()
	p := proxy.NewPipeline(sink, g, nil, 1) // the tally holds one identity
	t.Cleanup(func() {
		sink.Release()
		_ = p.Close()
	})

	scope := record.Scope{UserID: "u1"}
	held := proxyTestRecord(t, "held", record.EventMemorySurfaced, scope)

	require.NoError(t, p.Write(proxyTestRecord(t, "first", record.EventMemorySurfaced, scope)))
	<-sink.started
	require.NoError(t, p.Write(proxyTestRecord(t, "queued", record.EventMemorySurfaced, scope)))
	require.NoError(t, p.Write(held)) // retained identity
	const overflow = 3
	for i := 0; i < overflow; i++ {
		require.NoError(t, p.Write(proxyTestRecord(t, fmt.Sprintf("overflow-%d", i), record.EventMemorySurfaced, scope)))
	}
	require.Equal(t, uint64(1+overflow), p.Drops())

	sink.Release()
	require.NoError(t, p.Close())

	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	require.Len(t, entries, 2, "one entry for the held identity plus one for the overflow count")
	assert.Equal(t, string(held.ID), entries[0].CorrelationID, "the retained identity is named")
	assert.Contains(t, entries[1].Detail, fmt.Sprintf("%d", overflow),
		"the final entry carries the count of drops it could not name")
}

// TestPipelineClampsADepthBelowOne pins that a depth below one is clamped to
// one rather than accepted: a zero-capacity channel would make every write a
// drop, so a non-positive depth must behave as depth one.
func TestPipelineClampsADepthBelowOne(t *testing.T) {
	for _, depth := range []int{0, -1, -100} {
		t.Run(fmt.Sprintf("depth=%d", depth), func(t *testing.T) {
			sink := newStallingSink()
			p := proxy.NewPipeline(sink, nil, nil, depth)
			t.Cleanup(func() {
				sink.Release()
				require.NoError(t, p.Close())
			})

			scope := record.Scope{UserID: "u1"}
			// One record in flight, one queued; the third cannot be queued.
			// A zero-capacity channel would have made every write a drop.
			require.NoError(t, p.Write(proxyTestRecord(t, "c1", record.EventMemorySurfaced, scope)))
			<-sink.started
			require.NoError(t, p.Write(proxyTestRecord(t, "c2", record.EventMemorySurfaced, scope)))
			require.NoError(t, p.Write(proxyTestRecord(t, "c3", record.EventMemorySurfaced, scope)))

			assert.Equal(t, uint64(1), p.Drops(), "a depth below one must behave as depth one")
		})
	}
}

// ---------------------------------------------------------------------------
// channels, close and lifecycle
// ---------------------------------------------------------------------------

// TestPipelineMarksEveryChannelOnDrop pins that the drop marker reaches every
// configured loud channel and that a nil element is skipped rather than
// panicking.
func TestPipelineMarksEveryChannelOnDrop(t *testing.T) {
	sink := newStallingSink()
	var a, b bytes.Buffer
	p := proxy.NewPipeline(sink, nil, []io.Writer{&a, nil, &b}, 1)
	t.Cleanup(func() {
		sink.Release()
		require.NoError(t, p.Close())
	})

	scope := record.Scope{UserID: "u1"}
	require.NoError(t, p.Write(proxyTestRecord(t, "first", record.EventMemorySurfaced, scope)))
	<-sink.started
	require.NoError(t, p.Write(proxyTestRecord(t, "queued", record.EventMemorySurfaced, scope)))
	require.NoError(t, p.Write(proxyTestRecord(t, "dropped-x", record.EventMemorySurfaced, scope)))

	assert.Contains(t, a.String(), "dropped-x", "the first channel is marked")
	assert.Contains(t, b.String(), "dropped-x", "a later channel is marked too, not just the first")
}

// TestPipelineCloseIsIdempotentAndDrainsTheWriter pins that Close can be called
// more than once safely, that it closes the sink exactly once, and that the
// writer goroutine does not leak.
func TestPipelineCloseIsIdempotentAndDrainsTheWriter(t *testing.T) {
	sink := newStallingSink()
	p := proxy.NewPipeline(sink, nil, nil, 4)

	require.NoError(t, p.Close())
	assert.NotPanics(t, func() { require.NoError(t, p.Close()) }, "Close must be idempotent")
	assert.True(t, sink.closed(), "Close must close the sink")
}

// TestPipelineWriteAfterCloseIsSafe pins that a write following Close never
// panics on a send to a closed channel. It is sequential; the concurrent
// companion below is what pins the racing case.
func TestPipelineWriteAfterCloseIsSafe(t *testing.T) {
	sink := newStallingSink()
	p := proxy.NewPipeline(sink, nil, nil, 4)

	require.NoError(t, p.Close())

	scope := record.Scope{UserID: "u1"}
	assert.NotPanics(t, func() {
		_ = p.Write(proxyTestRecord(t, "late", record.EventMemorySurfaced, scope))
	}, "a write after Close must never panic on a closed channel")
}

// TestPipelineWriteAfterCloseIsCountedAndMarked pins the fix for the pipeline's
// one silent-loss path: a write that arrives after Close can neither be queued
// nor gap-logged (Close has drained the tally, and the sink that owns the gap
// log is closing), so it must at least be counted and named on the loud
// channels. Without this, a handler still in flight when srv.Shutdown returns
// would have its record vanish with no trace anywhere.
func TestPipelineWriteAfterCloseIsCountedAndMarked(t *testing.T) {
	sink := newStallingSink()
	var out bytes.Buffer
	p := proxy.NewPipeline(sink, nil, []io.Writer{&out}, 4)
	require.NoError(t, p.Close())

	before := p.Drops()
	late := proxyTestRecord(t, "late-after-close", record.EventMemorySurfaced, record.Scope{UserID: "u1"})
	require.NoError(t, p.Write(late))

	assert.Equal(t, before+1, p.Drops(), "a write after Close must be counted, not silently discarded")
	assert.Contains(t, out.String(), "late-after-close", "the late drop must be marked loudly")
	assert.Contains(t, out.String(), "write after close",
		"the marker must say the drop cannot be gap-logged")
}

// TestPipelineConcurrentWritesDuringCloseAreSafe pins the Critical-class
// invariant the plan names (plan:33): a write racing Close must never send on a
// closed channel or panic, and no record accepted before Close may be lost. It
// runs N writer goroutines against a concurrent Close, so it is the test that
// fails if the p.mu guard around Write's send is removed -- under -race as a
// data race on the closing flag, and often as a "send on closed channel" panic.
//
// The queue is deliberately larger than the whole workload, so no write is
// ever refused for being full: every write is either accepted before Close and
// drained to the sink, or refused by Close and counted. Those two halves must
// add up to the whole workload, which is how "no accepted record is lost" is
// checked here.
func TestPipelineConcurrentWritesDuringCloseAreSafe(t *testing.T) {
	sink := newStallingSink()
	sink.Release() // non-blocking sink: the writer always drains

	const depth = 2048 // larger than the workload below, so nothing drops
	// The markers are captured so the test can assert WHICH refusal kind was
	// counted -- see the attribution assertions below. Writers and the writer
	// goroutine reach the channel concurrently, so the buffer is guarded.
	var marks guardedBuffer
	p := proxy.NewPipeline(sink, nil, []io.Writer{&marks}, depth)

	const writers, perWriter = 8, 128
	const total = writers * perWriter

	// Build records on the test goroutine: proxyTestRecord uses require, which
	// must not run on a spawned goroutine.
	records := make([]record.Record, total)
	for i := range records {
		records[i] = proxyTestRecord(t, fmt.Sprintf("cw-%d", i), record.EventMemorySurfaced, record.Scope{UserID: "u1"})
	}

	start := make(chan struct{})
	var begun sync.WaitGroup // each writer signals after its first write
	begun.Add(writers)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			_ = p.Write(records[w*perWriter])
			begun.Done()
			for i := 1; i < perWriter; i++ {
				_ = p.Write(records[w*perWriter+i])
			}
		}(w)
	}

	close(start)
	// Wait until every writer is under way, so Close races in-flight writes
	// rather than an idle pipeline.
	begun.Wait()
	require.NoError(t, p.Close())
	wg.Wait()

	// No panic and no data race: the guarded send never touched the closed
	// queue, so Close did not close it out from under a producer.
	//
	// Every write was either accepted before Close or refused by it, and the
	// closed branch counts each refusal. The queue is larger than the whole
	// workload, so no write was ever refused for being FULL: accepted plus
	// refused must equal the whole workload, which is what pins that no
	// accepted record was lost and that every refusal is attributed.
	received := sink.received()
	assert.Equal(t, uint64(total), uint64(len(received))+p.Drops(),
		"every write was either accepted and drained to the sink or refused after Close")

	// The sum above cannot tell WHICH kind of refusal it balanced against: a
	// queue-full drop would satisfy it identically, and a queue-full drop here
	// would be a record refused for a reason that cannot occur -- the queue is
	// larger than the whole workload. The two refusals carry different marker
	// text, so the markers decide which kind was counted.
	//
	// The after-close half is made unconditional by one probe write of our own,
	// after Close and after every writer has returned: the closed branch refuses
	// it deterministically, so the assertion cannot depend on the scheduling
	// question of whether any concurrent write outlived Close. Every refusal
	// the concurrent phase counted, if any, is held to the same test by the
	// second assertion: the queue-full marker must not appear.
	_ = p.Write(proxyTestRecord(t, "probe-after-close", record.EventMemorySurfaced, record.Scope{UserID: "u1"}))
	assert.Contains(t, marks.String(), "write after close",
		"the counted refusals must be attributed to Close, not to the queue being full")
	assert.NotContains(t, marks.String(), "queue full",
		"the queue is larger than the whole workload, so no refusal may be a queue-full drop")

	inPool := make(map[string]bool, total)
	for _, r := range records {
		inPool[string(r.ID)] = true
	}
	assert.NotEmpty(t, received, "writes accepted before Close reached the sink")
	for _, r := range received {
		assert.True(t, inPool[string(r.ID)], "the sink only ever received a record a writer wrote")
	}
}
