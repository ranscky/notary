package replay_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/export"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/replay"
	"notary/internal/sign"
	"notary/internal/store"
)

// fakeReader is an in-memory Reader. It returns a fixed prefix and fixed breaks
// so a test can drive Replay without a database and prove it renders, narrows,
// counts and reports exactly what the reader handed it. It records the instant
// and verifier it was called with.
type fakeReader struct {
	records []record.Record
	breaks  []ledger.Break
	err     error
	gotAt   time.Time
	gotV    *sign.Verifier
}

func (f *fakeReader) ReplayAsOf(t time.Time, v *sign.Verifier) ([]record.Record, []ledger.Break, error) {
	f.gotAt, f.gotV = t, v
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.records, f.breaks, nil
}

// mkRecord builds a renderable record: a genuine observed reason so Render can
// phrase it, and a fully-populated subject. It carries no chain fields, which
// Render does not need.
func mkRecord(t *testing.T, id record.RecordID, scope record.Scope, content *record.Content) record.Record {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"x":1}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonStoredByMem0, ev)
	require.NoError(t, err)

	var contentHash record.Hash
	for i := range contentHash {
		contentHash[i] = 0xab + byte(len(id))
	}

	return record.Record{
		ID:         id,
		At:         time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		RecordedAt: time.Date(2024, 1, 2, 3, 4, 6, 0, time.UTC),
		Event:      record.EventMemoryKept,
		Reason:     reason,
		Subject: record.Subject{
			MemoryID:    "mem-" + string(id),
			Scope:       scope,
			ContentHash: contentHash,
		},
		Content: content,
	}
}

// jsonlLines splits JSONL output into its non-empty lines.
func jsonlLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestReplayWritesOneJSONLLinePerRecord(t *testing.T) {
	a := mkRecord(t, "a", record.Scope{UserID: "u1"}, nil)
	b := mkRecord(t, "b", record.Scope{UserID: "u1"}, nil)
	rp := replay.New(&fakeReader{records: []record.Record{a, b}}, nil)

	var buf bytes.Buffer
	res, err := rp.Replay(context.Background(), replay.Request{At: time.Now()}, &buf)
	require.NoError(t, err)

	assert.Equal(t, 2, res.Records)

	lines := jsonlLines(buf.String())
	require.Len(t, lines, 2, "one JSONL line per record")
	for _, l := range lines {
		var line export.Line
		require.NoError(t, json.Unmarshal([]byte(l), &line), "each line must decode as an export.Line")
	}
	assert.Contains(t, buf.String(), `"id":"a"`)
	assert.Contains(t, buf.String(), `"id":"b"`)
}

func TestReplayScopeNarrowsAndZeroScopeIsNoFilter(t *testing.T) {
	u1 := mkRecord(t, "u1-rec", record.Scope{UserID: "u1"}, nil)
	u2 := mkRecord(t, "u2-rec", record.Scope{UserID: "u2"}, nil)
	rp := replay.New(&fakeReader{records: []record.Record{u1, u2}}, nil)

	var narrowed bytes.Buffer
	res, err := rp.Replay(context.Background(),
		replay.Request{At: time.Now(), Scope: record.Scope{UserID: "u1"}}, &narrowed)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Records)
	assert.Contains(t, narrowed.String(), `"id":"u1-rec"`)
	assert.NotContains(t, narrowed.String(), `"id":"u2-rec"`)

	var all bytes.Buffer
	res, err = rp.Replay(context.Background(), replay.Request{At: time.Now()}, &all)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Records, "an all-zero scope is no filter")
	assert.Contains(t, all.String(), `"id":"u1-rec"`)
	assert.Contains(t, all.String(), `"id":"u2-rec"`)
}

func TestReplayRedactsSensitiveUnlessIncluded(t *testing.T) {
	sens := mkRecord(t, "sens", record.Scope{UserID: "u1"},
		&record.Content{Text: "the secret value", Sensitive: true})
	rp := replay.New(&fakeReader{records: []record.Record{sens}}, nil)

	var withheld bytes.Buffer
	res, err := rp.Replay(context.Background(), replay.Request{At: time.Now()}, &withheld)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Redacted, "a sensitive record withheld must be counted")
	assert.Contains(t, withheld.String(), `"redacted":"sensitive"`)
	assert.NotContains(t, withheld.String(), "the secret value")

	var shown bytes.Buffer
	res, err = rp.Replay(context.Background(),
		replay.Request{At: time.Now(), IncludeSensitive: true}, &shown)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Redacted, "nothing hidden means nothing counted")
	assert.Contains(t, shown.String(), "the secret value")
}

func TestReplaySurfacesBreaksRatherThanSwallowingThem(t *testing.T) {
	brk := ledger.Break{RecordID: "rec-9", Seq: 5, Field: "seq", Detail: "hole at seq 4"}
	rp := replay.New(&fakeReader{breaks: []ledger.Break{brk}}, nil)

	var buf bytes.Buffer
	res, err := rp.Replay(context.Background(), replay.Request{At: time.Now()}, &buf)
	require.NoError(t, err, "a break is reported, not raised as an error")
	require.Len(t, res.Breaks, 1)
	assert.Equal(t, brk, res.Breaks[0])
}

func TestReplayEmptyPrefixWritesNothingAndReturnsZeroResult(t *testing.T) {
	rp := replay.New(&fakeReader{}, nil)

	var buf bytes.Buffer
	res, err := rp.Replay(context.Background(), replay.Request{At: time.Now()}, &buf)
	require.NoError(t, err)
	assert.Equal(t, replay.Result{}, res, "an empty prefix is a zero result with a nil error")
	assert.Empty(t, buf.String())
}

// errReadFailed is the sentinel a failing reader returns, so the test can prove
// Replay's %w wrap preserves it rather than merely returning some error.
var errReadFailed = errors.New("ledger read failed")

func TestReplayWrapsReadError(t *testing.T) {
	rp := replay.New(&fakeReader{err: errReadFailed}, nil)

	var buf bytes.Buffer
	res, err := rp.Replay(context.Background(), replay.Request{At: time.Now()}, &buf)
	require.Error(t, err)
	require.ErrorIs(t, err, errReadFailed, "the %w wrap must preserve the reader's sentinel")
	assert.Equal(t, 0, res.Records)
	assert.Empty(t, buf.String(), "nothing is written when the read fails")
}

// --- byte-identity: the guard on the shared encoder -------------------------

// replayTestSeed is a fixed 32-byte ed25519 seed for the ledger these tests
// build, so its signatures are deterministic.
var replayTestSeed = []byte{
	0x30, 0x2f, 0x2e, 0x2d, 0x2c, 0x2b, 0x2a, 0x29,
	0x28, 0x27, 0x26, 0x25, 0x24, 0x23, 0x22, 0x21,
	0x20, 0x1f, 0x1e, 0x1d, 0x1c, 0x1b, 0x1a, 0x19,
	0x18, 0x17, 0x16, 0x15, 0x14, 0x13, 0x12, 0x11,
}

var (
	replayFrom = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	replayTo   = time.Date(2024, 6, 30, 0, 0, 0, 0, time.UTC)
	// replayFixedNow is the ledger clock: every appended record is stamped with
	// it, so ReplayAsOf(replayFixedNow) selects the whole prefix.
	replayFixedNow = time.Date(2024, 6, 20, 12, 0, 0, 0, time.UTC)
)

// newReplayLedger builds a real ledger -- a temp SQLite store, a signer over a
// fixed seed, and a fixed clock -- so a test can compare the two render paths
// against storage instead of a fixture.
func newReplayLedger(t *testing.T, n int) (*ledger.Ledger, *sign.Verifier) {
	t.Helper()
	t.Setenv("NOTARY_REPLAY_TEST_KEY", base64.StdEncoding.EncodeToString(replayTestSeed))
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_REPLAY_TEST_KEY"})
	require.NoError(t, err)
	pub := ed25519.NewKeyFromSeed(replayTestSeed).Public().(ed25519.PublicKey)

	st, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	l := ledger.New(st, sg, func() time.Time { return replayFixedNow })
	for i := 0; i < n; i++ {
		_, err := l.Append(appendableRecord(t,
			record.RecordID(fmt.Sprintf("rec-%d", i+1)),
			replayFrom.Add(time.Duration(i)*time.Hour), nil))
		require.NoError(t, err)
	}
	v := sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})
	return l, v
}

// appendableRecord returns a valid, appendable record with a genuine observed
// reason so Render can phrase it, mirroring the export suite's buildRecord.
func appendableRecord(t *testing.T, id record.RecordID, at time.Time, content *record.Content) record.Record {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"ok":true}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonReturnedBySearch, ev)
	require.NoError(t, err)

	var contentHash record.Hash
	for i := range contentHash {
		contentHash[i] = 0x40 + byte(i%8) + byte(len(id))
	}

	rec := record.Record{
		ID:      id,
		At:      at,
		Event:   record.EventMemorySurfaced,
		Reason:  reason,
		Subject: record.Subject{MemoryID: "mem-" + string(id), Scope: record.Scope{UserID: "u1"}, ContentHash: contentHash},
		Content: content,
	}
	require.NoError(t, rec.Validate(), "the fixture must be a valid record")
	return rec
}

// TestReplayIsByteIdenticalToExport is the guard on Task 3's shared encoder:
// the same three records are rendered once through export.Export and once
// through replay.Replay, and the whole JSONL output must be equal byte for
// byte. If the two paths ever stop sharing the encoder -- or render differently
// -- this fails.
func TestReplayIsByteIdenticalToExport(t *testing.T) {
	l, v := newReplayLedger(t, 3)

	// Make one record sensitive: redaction is part of the line format, so the
	// byte-identity must hold on the redacted shape too.
	sens := appendableRecord(t, "sens-rec", replayFrom.Add(3*time.Hour),
		&record.Content{Text: "a sensitive note", Sensitive: true})
	_, err := l.Append(sens)
	require.NoError(t, err)

	e := export.New(l)
	var exportOut bytes.Buffer
	_, err = e.Export(context.Background(), export.Request{From: replayFrom, To: replayTo}, &exportOut)
	require.NoError(t, err)

	rp := replay.New(l, v)
	var replayOut bytes.Buffer
	res, err := rp.Replay(context.Background(), replay.Request{At: replayFixedNow}, &replayOut)
	require.NoError(t, err)
	require.Empty(t, res.Breaks, "the prefix the ledger built must verify clean")

	require.NotEmpty(t, exportOut.String())
	assert.Equal(t, exportOut.String(), replayOut.String(),
		"a replayed line must be byte-identical to an exported one")
}
