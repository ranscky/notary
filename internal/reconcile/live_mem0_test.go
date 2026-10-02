//go:build mem0live

// This file talks to the REAL Mem0 API. It is opt-in behind the `mem0live` build
// tag so that it can never run in CI and the README's promise -- tests never
// touch the network -- stays true: `go test ./...` does not compile this file,
// and neither `go build` nor `go vet` sees it.
//
// Run it deliberately, with a real key in the environment:
//
//	go test -tags mem0live -run TestLiveAddReconcilesToAFixpoint -v ./internal/reconcile/
//
// It exists because the fixtures could not catch the class of bug it guards: the
// two-pass convergence defect shipped green through a full fixtured suite and a
// whole-branch review, and was found only by a human driving the real API. Every
// fixture begins with its chain pre-satisfied, so none of them could ask the
// question this test asks.
//
// Safety: it writes only to a scope it generates itself, refuses to run unless
// that scope carries liveTestScopePrefix -- so a misconfiguration cannot point it
// at an operator's real memories -- and wipes that scope on the way out, whether
// the test passed, failed or panicked part-way.
package reconcile_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
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
	// liveTestScopePrefix is the shape every scope this test will write to must
	// have. The guard is deliberate: it makes it impossible for a misconfigured
	// run to add memories to, and then delete them from, a real user's scope.
	liveTestScopePrefix = "notary-live-test-"
	// liveTestKeyEnv names the variable a per-run signing seed is placed in. The
	// seed is generated in-process and the ledger is a throwaway in t.TempDir(),
	// so no key material is written to disk anywhere.
	liveTestKeyEnv = "NOTARY_LIVE_TEST_SIGNING_KEY"
	// liveTestEventWait bounds the wait for Mem0 to finish an add. An add is
	// asynchronous; a pass that ran while it was still PENDING would write
	// nothing, which is correct behaviour and a useless test.
	liveTestEventWait = 120 * time.Second
)

// liveTestScope generates a scope belonging to nobody, and refuses to return one
// that does not carry the required prefix.
func liveTestScope(t *testing.T) record.Scope {
	t.Helper()
	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err)

	name := liveTestScopePrefix + hex.EncodeToString(buf)
	require.True(t, strings.HasPrefix(name, liveTestScopePrefix),
		"refusing to run against scope %q: the live test may only write to a generated test scope", name)
	return record.Scope{UserID: name}
}

// waitForEvent polls EventStatus until the add leaves PENDING, so the pass under
// test has something to resolve.
func waitForEvent(t *testing.T, ctx context.Context, c *mem0.Client, eventID string) {
	t.Helper()
	deadline := time.Now().Add(liveTestEventWait)
	last := ""
	for {
		resp, err := c.EventStatus(ctx, eventID)
		require.NoError(t, err, "polling the event status of %s", eventID)
		last = resp.Status

		// Mem0 works through PENDING and then RUNNING before reaching a terminal
		// status. The recorded fixtures only ever carried PENDING and SUCCEEDED,
		// so waiting merely for "not PENDING" is not enough: a RUNNING add with
		// no results is still work in progress, and a pass that ran then would
		// correctly write nothing -- which is a failing test for the wrong
		// reason, not a defect.
		if resp.Status == "SUCCEEDED" || resp.Status == "FAILED" {
			t.Logf("event %s settled as %s with %d result(s)", eventID, resp.Status, len(resp.Results))
			return
		}
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
	apiKey := os.Getenv("NOTARY_MEM0_API_KEY")
	require.NotEmpty(t, apiKey,
		"NOTARY_MEM0_API_KEY must be set in the environment; Notary reads secrets from the environment and nowhere else")

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

	baseURL := os.Getenv("NOTARY_MEM0_BASE_URL")
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
		enum, err := mc.GetAllComplete(cleanupCtx, mem0.GetAllRequest{Filters: mem0.Filters{UserID: scope.UserID}})
		if err != nil {
			t.Errorf("cleanup: enumerating scope %s: %v", scope.UserID, err)
			return
		}
		for _, m := range enum.Items() {
			if derr := mc.Delete(cleanupCtx, m.ID); derr != nil {
				t.Errorf("cleanup: deleting memory %s from %s: %v", m.ID, scope.UserID, derr)
			}
		}

		// Prove the cleanup worked rather than assuming it: a test that leaves
		// memories behind has littered a real account.
		after, err := mc.GetAllComplete(cleanupCtx, mem0.GetAllRequest{Filters: mem0.Filters{UserID: scope.UserID}})
		if err != nil {
			t.Errorf("cleanup: re-enumerating scope %s: %v", scope.UserID, err)
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

// ledgerRows reports the ledger's head sequence and row count.
func ledgerRows(t *testing.T, st store.Store) (record.Record, bool, int) {
	t.Helper()
	head, ok, err := st.Head()
	require.NoError(t, err)
	entries, err := st.SeqEntries()
	require.NoError(t, err)
	return head, ok, len(entries)
}
