package reconcile

import (
	"context"
	"fmt"
	"time"

	"notary/internal/mem0"
	"notary/internal/record"
)

// Window bounds a reconciliation pass.
type Window struct {
	// Since, when non-zero, excludes records whose At -- the time the Mem0
	// event happened -- precedes it. It deliberately does NOT filter on
	// RecordedAt: work items are Mem0 events, and a record written late about
	// an old event still belongs to that old event's window (design spec §4).
	//
	// The default is zero, meaning no bound.
	Since time.Time

	// Scope, when any field is non-zero, restricts the pass to records whose
	// subject scope matches every non-zero field. An all-zero Scope is no
	// filter.
	Scope record.Scope
}

// Reader is the read side of the ledger the reconciler folds. It is the
// existing ledger read method, so the reconciler depends only on a small
// interface and a test can supply an in-memory fake.
type Reader interface {
	ListRecords(from, to time.Time) ([]record.Record, error)
}

// Reconciler derives the claims the ledger cannot observe directly: what became
// of an add, whether a known memory still exists, and whether Mem0 removed one
// without saying why. It is the sole producer of Reconstructed and Internal
// records.
//
// It keeps no cursor, no queue and no side state: every pass is a pure function
// of the ledger and of Mem0's current state, which is what makes re-running it
// safe to schedule.
type Reconciler struct {
	reader Reader
	client *mem0.Client
}

// New returns a Reconciler that reads the ledger through r and queries Mem0
// through c.
func New(r Reader, c *mem0.Client) *Reconciler {
	return &Reconciler{reader: r, client: c}
}

// Reconcile reads the ledger, folds it into a worklist in dependency order,
// dispatches each stage to its producer, and returns the claims the producers
// derived.
//
// Every returned record carries a zero Seq and a zero Hash: ledger.Append alone
// assigns chain position and computes the digest, and it rejects a
// caller-supplied Seq. Nothing here attempts either (design spec §4.1).
//
// The pass fails loudly on any reader or producer error; it is not fail-open
// (§9.2). It writes nothing itself -- the caller appends what it returns, which
// is what makes --dry-run trivial.
func (rc *Reconciler) Reconcile(ctx context.Context, w Window) ([]record.Record, error) {
	from, to := w.bounds()
	records, err := rc.reader.ListRecords(from, to)
	if err != nil {
		return nil, fmt.Errorf("reconcile: read ledger: %w", err)
	}

	wl, err := buildWorklist(w.filter(records))
	if err != nil {
		return nil, fmt.Errorf("reconcile: derive worklist: %w", err)
	}

	var out []record.Record

	// Stage 1 -- unresolved adds (§9.1.1). Task 6 fills resolveAdd.
	for _, add := range wl.unresolvedAdds {
		produced, err := rc.resolveAdd(ctx, add)
		if err != nil {
			return nil, fmt.Errorf("reconcile: resolve add %s: %w", add.ID, err)
		}
		if out, err = collectStage(out, "resolveAdd", produced); err != nil {
			return nil, err
		}
	}

	// Stage 2 -- scopes to enumerate (§9.1.2), which also yields memory_kept
	// (§9.1 row 4-5). Task 7 fills resolveKept, which enumerates the scope
	// completely.
	for _, scope := range wl.scopes {
		produced, err := rc.resolveKept(ctx, scope, wl.known)
		if err != nil {
			return nil, fmt.Errorf("reconcile: resolve kept for scope %s: %w", scopeKey(scope), err)
		}
		if out, err = collectStage(out, "resolveKept", produced); err != nil {
			return nil, err
		}
	}

	// Stage 3 -- coverage candidates (§9.1.3). Task 8 fills resolveAbsent.
	produced, err := rc.resolveAbsent(wl.known, wl.searches)
	if err != nil {
		return nil, fmt.Errorf("reconcile: resolve absent: %w", err)
	}
	if out, err = collectStage(out, "resolveAbsent", produced); err != nil {
		return nil, err
	}

	// Stage 4 -- removal candidates (§9.1.4). A known memory absent from a
	// complete enumeration, corroborated against History, yields memory_dropped
	// (Internal). Task 9 owns this stage, including the per-scope enumeration
	// that must precede it, so the loop is added there:
	//
	//   for _, scope := range wl.scopes {
	//       e, err := rc.client.GetAllComplete(ctx, mem0.GetAllRequest{Filters: scopeFilters(scope)})
	//       ...
	//       produced, err := rc.resolveRemoved(ctx, e, wl.known)
	//       if out, err = collectStage(out, "resolveRemoved", produced); err != nil { ... }
	//   }
	//
	// It is left unwired here so this task makes no Mem0 call and its tests
	// need only a fake Reader.

	return out, nil
}

// bounds returns explicit, non-zero bounds for a whole-ledger read, narrowed at
// the front by w.Since.
//
// The bound must be explicit: the store filters `at >= from AND at <= to`
// literally and returns NOTHING when either bound is the zero time, so a
// Window with no Since must still pass real bounds. With zero bounds every pass
// would silently see an empty ledger and everything would look resolved -- a
// bug a naive test would not catch.
func (w Window) bounds() (from, to time.Time) {
	from = ledgerEarliest()
	if w.Since.After(from) {
		from = w.Since
	}
	return from, ledgerLatest()
}

// ledgerEarliest and ledgerLatest are the widest bounds a whole-ledger read
// needs. They are functions, not package variables, so nothing can reassign
// them and silently redefine the window.
func ledgerEarliest() time.Time {
	return time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
}

func ledgerLatest() time.Time {
	return time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
}

// collectStage appends a stage's produced records to out, after verifying that
// none carries a chain position or a digest.
//
// This is the boundary that hands records to ledger.Append, which alone assigns
// Seq and computes Hash and which rejects a caller-supplied Seq. A producer
// that set either is a bug, and it fails loudly here rather than as an opaque
// append error later.
func collectStage(out []record.Record, stage string, produced []record.Record) ([]record.Record, error) {
	for _, r := range produced {
		if r.Seq != 0 {
			return nil, fmt.Errorf("reconcile: stage %s produced record %s with seq %d; only ledger.Append assigns chain position", stage, r.ID, r.Seq)
		}
		if r.Hash != (record.Hash{}) {
			return nil, fmt.Errorf("reconcile: stage %s produced record %s with a precomputed hash; only ledger.Append computes it", stage, r.ID)
		}
	}
	return append(out, produced...), nil
}

// The per-stage producers are the reconciler's extension points. Each derives
// the claims it can justify from the ledger state it is handed, and none may
// reach the chain: ledger.Append alone assigns Seq and computes Hash.
//
// resolveAdd (the add-resolution producer, Task 6, spec §5 rows 1-3) is
// implemented in adds.go; resolveKept (the memory_kept producer, Task 7, spec §5
// rows 4-5) is implemented in kept.go. resolveAbsent remains a stub here; Tasks
// 8-9 replace the remaining bodies, and the signatures are fixed so those tasks
// plug in without touching Reconcile.

// resolveAbsent is the absent_from_search producer (Task 8, spec §5 row 6). It
// takes the known memories and the search_performed records that may cover them
// and returns the memory_dropped claims a saturated search warrants, or none.
func (rc *Reconciler) resolveAbsent(known []knownMemory, searches []record.Record) ([]record.Record, error) {
	return nil, nil
}
