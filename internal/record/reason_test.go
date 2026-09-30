package record_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/record"
)

func TestReasonKindAllowedTier(t *testing.T) {
	cases := []struct {
		kind record.ReasonKind
		want record.VisibilityTier
	}{
		{record.ReasonSearchPerformed, record.Observed},
		{record.ReasonAddAcknowledged, record.Observed},
		{record.ReasonReturnedBySearch, record.Observed},
		{record.ReasonStoredByMem0, record.Observed},
		{record.ReasonAddFailed, record.Observed},
		{record.ReasonAuditUnavailable, record.Observed},
		{record.ReasonKeptByContentMatch, record.Reconstructed},
		{record.ReasonAbsentFromSearch, record.Reconstructed},
		{record.ReasonNoFactsExtracted, record.Reconstructed},
		{record.ReasonRemovedByMem0, record.Internal},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			got, ok := tc.kind.AllowedTier()
			require.True(t, ok, "kind %q must be known", tc.kind)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("unknown kind", func(t *testing.T) {
		got, ok := record.ReasonKind("nonsense").AllowedTier()
		assert.False(t, ok)
		assert.False(t, got.Valid())
	})
}

func TestNewObservedReason(t *testing.T) {
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"a":1}`))
	require.NoError(t, err)

	t.Run("success", func(t *testing.T) {
		r, err := record.NewObservedReason(record.ReasonReturnedBySearch, ev)
		require.NoError(t, err)
		assert.Equal(t, record.Observed, r.Tier())
		assert.Equal(t, record.ReasonReturnedBySearch, r.Kind())
		require.NoError(t, r.Validate())

		got, ok := r.Observed()
		require.True(t, ok)
		assert.Equal(t, ev, got)

		_, ok = r.Reconstructed()
		assert.False(t, ok, "an observed reason has no reconstructed evidence")
		_, ok = r.InternalNote()
		assert.False(t, ok, "an observed reason has no internal note")
	})

	t.Run("kind forbids the tier", func(t *testing.T) {
		_, err := record.NewObservedReason(record.ReasonAbsentFromSearch, ev)
		require.Error(t, err, "absent_from_search is reconstructed, not observed")
	})

	t.Run("zero evidence is invalid", func(t *testing.T) {
		_, err := record.NewObservedReason(record.ReasonReturnedBySearch, record.ObservedEvidence{})
		require.Error(t, err)
	})

	t.Run("unknown kind is rejected", func(t *testing.T) {
		_, err := record.NewObservedReason(record.ReasonKind("nonsense"), ev)
		require.Error(t, err)
	})
}

func TestNewReconstructedReason(t *testing.T) {
	ev, err := record.NewReconstructedEvidence([]record.RecordID{"rec-1"}, "content-match", "v1", 0.9)
	require.NoError(t, err)

	t.Run("success", func(t *testing.T) {
		r, err := record.NewReconstructedReason(record.ReasonAbsentFromSearch, ev)
		require.NoError(t, err)
		assert.Equal(t, record.Reconstructed, r.Tier())
		assert.Equal(t, record.ReasonAbsentFromSearch, r.Kind())
		require.NoError(t, r.Validate())

		got, ok := r.Reconstructed()
		require.True(t, ok)
		assert.Equal(t, ev, got)

		_, ok = r.Observed()
		assert.False(t, ok)
		_, ok = r.InternalNote()
		assert.False(t, ok)
	})

	t.Run("empty basis is invalid", func(t *testing.T) {
		_, err := record.NewReconstructedReason(record.ReasonAbsentFromSearch, record.ReconstructedEvidence{})
		require.Error(t, err)
	})

	t.Run("kind forbids the tier", func(t *testing.T) {
		_, err := record.NewReconstructedReason(record.ReasonReturnedBySearch, ev)
		require.Error(t, err, "returned_by_search is observed, not reconstructed")
	})
}

func TestNewInternalReason(t *testing.T) {
	note, err := record.NewInternalNote("mem0 removed the memory")
	require.NoError(t, err)

	t.Run("success", func(t *testing.T) {
		r, err := record.NewInternalReason(record.ReasonRemovedByMem0, note)
		require.NoError(t, err)
		assert.Equal(t, record.Internal, r.Tier())
		assert.Equal(t, record.ReasonRemovedByMem0, r.Kind())
		require.NoError(t, r.Validate())

		got, ok := r.InternalNote()
		require.True(t, ok)
		assert.Equal(t, note, got)

		_, ok = r.Observed()
		assert.False(t, ok)
		_, ok = r.Reconstructed()
		assert.False(t, ok)
	})

	t.Run("empty note is invalid", func(t *testing.T) {
		_, err := record.NewInternalReason(record.ReasonRemovedByMem0, record.InternalNote{})
		require.Error(t, err)
	})

	t.Run("empty note string is rejected by its constructor", func(t *testing.T) {
		_, err := record.NewInternalNote("")
		require.Error(t, err)
	})

	t.Run("kind forbids the tier", func(t *testing.T) {
		_, err := record.NewInternalReason(record.ReasonReturnedBySearch, note)
		require.Error(t, err, "returned_by_search is observed, not internal")
	})
}

func TestNewObservedEvidence(t *testing.T) {
	t.Run("rejects SourceInvalid", func(t *testing.T) {
		_, err := record.NewObservedEvidence(record.SourceInvalid, []byte(`{}`))
		require.Error(t, err)
	})

	t.Run("rejects unknown source", func(t *testing.T) {
		_, err := record.NewObservedEvidence(record.EvidenceSource(99), []byte(`{}`))
		require.Error(t, err)
	})

	t.Run("rejects non-JSON payload", func(t *testing.T) {
		_, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`not json`))
		require.Error(t, err)
	})

	t.Run("rejects empty payload", func(t *testing.T) {
		_, err := record.NewObservedEvidence(record.SourceMem0Response, nil)
		require.Error(t, err)
	})

	t.Run("rejects trailing JSON content", func(t *testing.T) {
		_, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"a":1}{"b":2}`))
		require.Error(t, err)
	})

	t.Run("accepts the notary instrumentation source", func(t *testing.T) {
		_, err := record.NewObservedEvidence(record.SourceNotaryInstrumentation, []byte(`{"ok":true}`))
		require.NoError(t, err)
	})
}

func TestNewObservedEvidenceCanonicalises(t *testing.T) {
	ev1, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"b":1,"a":2.50}`))
	require.NoError(t, err)
	ev2, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"a":2.50,"b":1}`))
	require.NoError(t, err)
	ev3, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{ "a": 2.50 , "b": 1 }`))
	require.NoError(t, err)

	assert.Equal(t, ev1, ev2, "key order must not perturb the canonical payload")
	assert.Equal(t, ev1, ev3, "insignificant whitespace must not perturb the canonical payload")
}

func TestNewReconstructedEvidence(t *testing.T) {
	t.Run("rejects empty basis", func(t *testing.T) {
		_, err := record.NewReconstructedEvidence(nil, "rule", "v1", 0.5)
		require.Error(t, err)
		_, err = record.NewReconstructedEvidence([]record.RecordID{}, "rule", "v1", 0.5)
		require.Error(t, err)
	})

	t.Run("rejects empty rule", func(t *testing.T) {
		_, err := record.NewReconstructedEvidence([]record.RecordID{"r"}, "", "v1", 0.5)
		require.Error(t, err)
	})

	t.Run("rejects empty rule version", func(t *testing.T) {
		_, err := record.NewReconstructedEvidence([]record.RecordID{"r"}, "rule", "", 0.5)
		require.Error(t, err)
	})

	t.Run("accepts complete evidence", func(t *testing.T) {
		_, err := record.NewReconstructedEvidence([]record.RecordID{"r"}, "rule", "v1", 0.5)
		require.NoError(t, err)
	})
}

func TestReasonZeroValueIsInvalid(t *testing.T) {
	var r record.Reason
	require.Error(t, r.Validate())

	_, ok := r.Observed()
	assert.False(t, ok)
	_, ok = r.Reconstructed()
	assert.False(t, ok)
	_, ok = r.InternalNote()
	assert.False(t, ok)
}

func TestReasonEncodeParseRoundTrip(t *testing.T) {
	t.Run("observed", func(t *testing.T) {
		ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"b":1,"a":2.50}`))
		require.NoError(t, err)
		r, err := record.NewObservedReason(record.ReasonReturnedBySearch, ev)
		require.NoError(t, err)

		b1, err := r.Encode()
		require.NoError(t, err)
		b2, err := r.Encode()
		require.NoError(t, err)
		assert.Equal(t, b1, b2, "Encode must be deterministic")

		got, err := record.ParseReason(b1)
		require.NoError(t, err)
		assert.Equal(t, r, got)
		assert.Equal(t, record.Observed, got.Tier())
		assert.Equal(t, record.ReasonReturnedBySearch, got.Kind())
	})

	t.Run("reconstructed", func(t *testing.T) {
		ev, err := record.NewReconstructedEvidence(
			[]record.RecordID{"rec-1", "rec-2"}, "content-match", "v3", 0.75)
		require.NoError(t, err)
		r, err := record.NewReconstructedReason(record.ReasonKeptByContentMatch, ev)
		require.NoError(t, err)

		b, err := r.Encode()
		require.NoError(t, err)
		got, err := record.ParseReason(b)
		require.NoError(t, err)
		assert.Equal(t, r, got)

		gotEv, ok := got.Reconstructed()
		require.True(t, ok)
		assert.Equal(t, ev, gotEv, "basis, rule, rule version and confidence must survive the round trip")
	})

	t.Run("internal", func(t *testing.T) {
		note, err := record.NewInternalNote("mem0 removed the memory")
		require.NoError(t, err)
		r, err := record.NewInternalReason(record.ReasonRemovedByMem0, note)
		require.NoError(t, err)

		b, err := r.Encode()
		require.NoError(t, err)
		got, err := record.ParseReason(b)
		require.NoError(t, err)
		assert.Equal(t, r, got)

		gotNote, ok := got.InternalNote()
		require.True(t, ok)
		assert.Equal(t, note, gotNote)
	})
}

func TestParseReasonRejectsBadInput(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		_, err := record.ParseReason(nil)
		require.Error(t, err)
	})

	t.Run("not JSON", func(t *testing.T) {
		_, err := record.ParseReason([]byte("not json"))
		require.Error(t, err)
	})

	t.Run("missing version tag", func(t *testing.T) {
		_, err := record.ParseReason([]byte(`{"kind":"returned_by_search","tier":"observed"}`))
		require.Error(t, err)
	})

	t.Run("unknown version tag", func(t *testing.T) {
		_, err := record.ParseReason([]byte(`{"v":"notary/reason/v99","kind":"returned_by_search","tier":"observed"}`))
		require.Error(t, err)
	})

	t.Run("tampered tier/kind mismatch is rejected", func(t *testing.T) {
		// absent_from_search is reconstructed; relabelled as observed it must fail.
		raw := []byte(`{"v":"notary/reason/v1","kind":"absent_from_search","tier":"observed",` +
			`"observed":{"source":"mem0_response","payload":{"a":1}}}`)
		_, err := record.ParseReason(raw)
		require.Error(t, err)
	})
}

func TestReconstructedReasonOmitsUnsetConfidence(t *testing.T) {
	ev, err := record.NewReconstructedEvidence([]record.RecordID{"r-1"}, "absent_from_search", "1", 0)
	require.NoError(t, err)
	r, err := record.NewReconstructedReason(record.ReasonAbsentFromSearch, ev)
	require.NoError(t, err)
	b, err := r.Encode()
	require.NoError(t, err)
	assert.NotContains(t, string(b), "confidence",
		"an unset confidence must be absent, not rendered as 0")

	// An absent confidence must still decode back to the zero value.
	got, err := record.ParseReason(b)
	require.NoError(t, err)
	assert.Equal(t, r, got)
}

func TestReconstructedReasonKeepsNonZeroConfidence(t *testing.T) {
	ev, err := record.NewReconstructedEvidence([]record.RecordID{"r-1"}, "absent_from_search", "1", 0.5)
	require.NoError(t, err)
	r, err := record.NewReconstructedReason(record.ReasonAbsentFromSearch, ev)
	require.NoError(t, err)
	b, err := r.Encode()
	require.NoError(t, err)
	assert.Contains(t, string(b), `"confidence":0.5`)

	got, err := record.ParseReason(b)
	require.NoError(t, err)
	assert.Equal(t, r, got)
}
