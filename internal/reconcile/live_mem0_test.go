//go:build mem0live

// This file talks to the REAL Mem0 API. It is opt-in behind the `mem0live` build
// tag so that it can never run in CI and the README's promise -- tests never
// touch the network -- stays true: `go test ./...` does not compile this file,
// and neither `go build` nor `go vet` sees it.
//
// Run it deliberately, with a real key in the environment, and WITH -count=1:
//
//	go test -tags mem0live -count=1 -run TestLiveAddReconcilesToAFixpoint -v ./internal/reconcile/
//
// -count=1 is not decoration. Without it a re-run REPLAYS the cached result and
// prints "ok (cached)": a PASS that never touched Mem0, which is precisely the
// fiction this file exists to avoid.
//
// It exists because the fixtures could not catch the class of bug it guards: the
// two-pass convergence defect shipped green through a full fixtured suite and a
// whole-branch review, and was found only by a human driving the real API. Every
// fixture begins with its chain pre-satisfied, so none of them could ask the
// question this test asks.
//
// Safety: it writes only to a scope it mints itself, from a random suffix, and
// that generated scope is the only one this file ever hands to Mem0. Every
// deletion is re-checked locally against that scope before it is issued, so the
// test's safety does not depend on the remote service honouring a filter. It
// wipes the scope on the way out, whether the test passed, failed or stopped
// part-way.
package reconcile_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/gap"
	"notary/internal/interceptor"
	"notary/internal/interceptor/library"
	"notary/internal/ledger"
	"notary/internal/mem0"
	"notary/internal/reconcile"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

const (
	// liveTestKeyEnv names the variable a per-run signing seed is placed in. The
	// seed is generated in-process and the ledger is a throwaway in t.TempDir(),
	// so no key material is written to disk anywhere.
	liveTestKeyEnv = "NOTARY_LIVE_TEST_SIGNING_KEY"
	// liveTestEventWait bounds the wait for Mem0 to finish an add. An add is
	// asynchronous; a pass that ran while it was still in flight would write
	// nothing, which is correct behaviour and a useless test.
	liveTestEventWait = 120 * time.Second
	// liveTestPageSize and liveTestMaxPages bound the cleanup listing.
	liveTestPageSize = 100
	liveTestMaxPages = 10
)

// liveTestScope mints a scope belonging to nobody. liveTestScopePrefix and
// requireTestScope live in livetest_scope_test.go, which is deliberately NOT
// behind this build tag: the guard the README's safety claim rests on is
// therefore compiled and exercised by the default suite, rather than existing
// only in a file that no gate ever compiles.
func liveTestScope(t *testing.T) record.Scope {
	t.Helper()
	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return requireTestScope(t, record.Scope{UserID: liveTestScopePrefix + hex.EncodeToString(buf)})
}

// listScope lists a scope's memories with plain paged GetAll, which reports what
// the API returned without additionally demanding a completeness proof.
//
// That is deliberate for deletion: GetAllComplete errs toward failure on any
// doubt, and a cleanup that declined to run because a listing looked odd would
// leave the whole scope behind. Proving completeness matters for an ABSENCE
// claim, which is why the verification step below still uses it.
func listScope(ctx context.Context, t *testing.T, c *mem0.Client, scope record.Scope) []mem0.Memory {
	t.Helper()
	var out []mem0.Memory
	for page := 1; page <= liveTestMaxPages; page++ {
		resp, err := c.GetAll(ctx, mem0.GetAllRequest{
			Filters:  mem0.Filters{UserID: scope.UserID},
			Page:     page,
			PageSize: liveTestPageSize,
		})
		if err != nil {
			t.Errorf("cleanup: listing scope %s page %d: %v", scope.UserID, page, err)
			return out
		}
		out = append(out, resp.Results...)
		if resp.Next == nil || len(resp.Results) == 0 {
			return out
		}
	}
	t.Errorf("cleanup: listing scope %s exceeded %d pages", scope.UserID, liveTestMaxPages)
	return out
}

// waitForEvent polls EventStatus until the add reaches a terminal status, so the
// pass under test has something to resolve.
func waitForEvent(t *testing.T, ctx context.Context, c *mem0.Client, eventID string) {
	t.Helper()
	deadline := time.Now().Add(liveTestEventWait)
	last := ""
	for {
		resp, err := c.EventStatus(ctx, eventID)
		require.NoError(t, err, "polling the event status of %s", eventID)
		last = resp.Status

		if resp.Status == "SUCCEEDED" {
			require.NotEmpty(t, resp.Results,
				"Mem0 reported the add SUCCEEDED (event %s) but returned no memory, so there is nothing to certify", eventID)
			t.Logf("event %s settled as SUCCEEDED with %d result(s)", eventID, len(resp.Results))
			return
		}

		// FAILED is terminal, but it is NOT this test's subject: with a failed
		// add the pass legitimately certifies nothing, and the assertions below
		// would then fail naming the convergence defect. Say what actually
		// happened. This is the same lesson as the RUNNING status below: an
		// inconclusive state must not be funnelled into a message about a
		// different cause.
		if resp.Status == "FAILED" {
			t.Fatalf("Mem0 FAILED the add (event %s): a service failure, not a reconciler defect, so there is nothing to reconcile", eventID)
		}

		// Mem0 works through PENDING and then RUNNING before reaching a terminal
		// status. The recorded fixtures only ever carried PENDING and SUCCEEDED,
		// so waiting merely for "not PENDING" is not enough: a RUNNING add with
		// no results is still work in progress, and a pass that ran then would
		// correctly write nothing -- a failing test for the wrong reason.
		t.Logf("event %s is %s (%d result(s)); still working", eventID, resp.Status, len(resp.Results))

		if time.Now().After(deadline) {
			t.Fatalf("event %s did not reach a terminal status within %s (last status %q)",
				eventID, liveTestEventWait, last)
		}
		time.Sleep(2 * time.Second)
	}
}

// TestLiveAddReconcilesToAFixpoint is the end-to-end guard: one real add must be
// resolved AND certified kept by a SINGLE reconcile pass, and re-running must
// append nothing.
func TestLiveAddReconcilesToAFixpoint(t *testing.T) {
	// -short asks for a fast suite, and this test is neither fast nor offline, so
	// it stands down. A skip is right here -- the operator asked for speed, not
	// because anything is misconfigured -- unlike the missing key below, which is
	// a FAILURE precisely so that it cannot pass as "did not run".
	if testing.Short() {
		t.Skip("the live test needs the network and real Mem0 seconds; skipped under -short")
	}

	apiKey := os.Getenv(config.EnvMem0APIKey)
	require.NotEmpty(t, apiKey,
		"%s must be set in the environment; Notary reads secrets from the environment and nowhere else",
		config.EnvMem0APIKey)

	seed := make([]byte, 32)
	_, err := rand.Read(seed)
	require.NoError(t, err)
	t.Setenv(liveTestKeyEnv, base64.StdEncoding.EncodeToString(seed))

	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "live.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: liveTestKeyEnv})
	require.NoError(t, err)

	g, err := gap.Open(filepath.Join(dir, "gaps.log"))
	require.NoError(t, err)

	l := ledger.New(st, sg, nil)
	aw := interceptor.NewAuditWriter(l, g, nil)

	baseURL := os.Getenv(config.EnvMem0BaseURL)
	if baseURL == "" {
		baseURL = config.DefaultMem0BaseURL
	}
	mc := mem0.NewClient(baseURL, apiKey, nil)

	scope := liveTestScope(t)
	t.Logf("live test scope: %s", scope.UserID)

	// Cleanup wipes the whole scope rather than tracking individual memory ids,
	// so an assertion failing part-way through still leaves the account clean.
	t.Cleanup(func() {
		cleanupCtx := context.Background()

		for _, m := range listScope(cleanupCtx, t, mc, scope) {
			// Re-check locally before deleting. The test must not depend on the
			// remote service honouring the filter it was sent: if a listing ever
			// returned something out of scope, refusing it is the difference
			// between cleanup and collateral damage.
			if m.UserID != scope.UserID {
				t.Errorf("cleanup: REFUSING to delete memory %s: it reports user_id %q, not the test scope %q",
					m.ID, m.UserID, scope.UserID)
				continue
			}
			if derr := mc.Delete(cleanupCtx, m.ID); derr != nil {
				t.Errorf("cleanup: deleting memory %s from %s: %v", m.ID, scope.UserID, derr)
			}
		}

		// Then prove the wipe worked, with the STRICT enumeration: this is an
		// absence claim, so completeness is exactly what must be proven. A
		// failure here is reported rather than assumed away, and the deletions
		// above have already run regardless.
		after, err := mc.GetAllComplete(cleanupCtx, mem0.GetAllRequest{Filters: mem0.Filters{UserID: scope.UserID}})
		if err != nil {
			t.Errorf("cleanup: re-enumerating scope %s to verify the wipe: %v", scope.UserID, err)
			return
		}
		if n := after.Len(); n != 0 {
			t.Errorf("cleanup left %d memory(ies) in %s", n, scope.UserID)
		} else {
			t.Logf("cleanup verified: scope %s is empty", scope.UserID)
		}
	})

	// 1. One REAL add, through the library interceptor, so the ledger holds a
	//    real add_requested record for the pass to act on.
	it := library.New(mc, aw, scope, nil)
	t.Cleanup(func() { require.NoError(t, it.Close()) })

	resp, err := it.Add(ctx, "live-"+scope.UserID, []string{
		"Notary live test: the shared office printer lives on floor 3.",
	})
	require.NoError(t, err, "the live add must succeed")
	require.NotEmpty(t, resp.EventID, "a live add must return an event id")
	t.Logf("mem0 event: %s (status %s)", resp.EventID, resp.Status)

	waitForEvent(t, ctx, mc, resp.EventID)

	rc := reconcile.New(l, mc)

	// 2. ONE pass must produce BOTH claims. Requiring two passes is the defect
	//    this test exists to catch, so it is asserted directly rather than
	//    tolerated: the add's resolution is what reveals the memory it produced.
	first, err := rc.Reconcile(ctx, reconcile.Window{})
	require.NoError(t, err)

	byEvent := map[record.EventType][]record.Record{}
	for _, r := range first {
		byEvent[r.Event] = append(byEvent[r.Event], r)
	}
	require.Len(t, byEvent[record.EventAddResolved], 1,
		"one pending add in a fresh scope must resolve to exactly one add_resolved")
	require.Len(t, byEvent[record.EventMemoryKept], 1,
		"ONE pass must certify the produced memory kept; needing a second pass is the two-pass convergence defect")
	assert.Equal(t, record.ReasonStoredByMem0, byEvent[record.EventMemoryKept][0].Reason.Kind())

	for _, r := range first {
		_, aerr := l.Append(r)
		require.NoError(t, aerr, "appending the first pass's claims")
	}
	_, _, rowsAfterFirst := ledgerRows(t, st)

	// 3. A second pass must derive nothing the first had not, and appending it
	//    must therefore add no rows: the documented promise, from the real API.
	second, err := rc.Reconcile(ctx, reconcile.Window{})
	require.NoError(t, err)
	// The second pass re-derives the memory_kept claim -- idempotently, which is
	// the design -- so this is both true and load-bearing: an empty second pass
	// would make the fixpoint check below pass vacuously.
	require.NotEmpty(t, second, "the second pass must still derive the already-present claim")

	derived := make(map[record.RecordID]bool, len(first))
	for _, r := range first {
		derived[r.ID] = true
	}
	for _, r := range second {
		assert.Truef(t, derived[r.ID],
			"a re-run derived %s, which the previous pass had not: the pass is not at its fixpoint", r.ID)
	}
	for _, r := range second {
		_, aerr := l.Append(r)
		require.NoError(t, aerr, "appending the second pass's claims")
	}

	_, _, rowsAfterSecond := ledgerRows(t, st)
	assert.Equal(t, rowsAfterFirst, rowsAfterSecond,
		"re-running against an unchanged store must append nothing")
}

// ledgerRows reports the ledger's head record, whether it has one, and its row
// count.
func ledgerRows(t *testing.T, st store.Store) (record.Record, bool, int) {
	t.Helper()
	head, ok, err := st.Head()
	require.NoError(t, err)
	entries, err := st.SeqEntries()
	require.NoError(t, err)
	return head, ok, len(entries)
}
