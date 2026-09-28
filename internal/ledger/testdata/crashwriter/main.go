// Command crashwriter appends records to a Notary ledger file and then dies in
// a controlled way, so that a test can observe exactly what a mid-write crash
// leaves behind. It lives under testdata/ so the go tool ignores it during
// ordinary builds (go build ./...); the test builds it explicitly.
//
// Usage:
//
//	crashwriter <db-path> <mode> <n> [target]
//
// Modes:
//
//	clean       Append n records and exit 0.
//	exit-after  Append n records, then os.Exit(1) the instant the final Append
//	            returns -- death right after a commit, before the store is even
//	            closed.
//	kill-mid    Append records in a loop up to n while a watchdog SIGKILLs the
//	            process as soon as <target> records have been committed. The
//	            uncatchable signal races the next Append, so the process normally
//	            dies inside an in-flight write transaction. <target> defaults to
//	            n/2.
//
// Every successfully appended record's chain position (Seq) is printed on its
// own line to stdout, so the test can see how far the writer got before it
// died. The signer key is read from the environment (NOTARY_CRASH_KEY) so the
// test can derive the matching public key and verify the surviving chain.
package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// keyEnv names the environment variable the signing key is read from.
const keyEnv = "NOTARY_CRASH_KEY"

// recordAt is the fixed instant the authoring time of every record carries. It
// is irrelevant to chain integrity; the test reads rows by Seq, not by time.
var recordAt = time.Date(2026, 9, 28, 11, 0, 0, 0, time.UTC)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: crashwriter <db-path> <mode> <n> [target]")
		os.Exit(2)
	}
	dbPath := os.Args[1]
	mode := os.Args[2]
	n, err := strconv.Atoi(os.Args[3])
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "crashwriter: bad record count %q\n", os.Args[3])
		os.Exit(2)
	}
	target := n / 2
	if len(os.Args) >= 5 {
		target, err = strconv.Atoi(os.Args[4])
		if err != nil || target < 0 {
			fmt.Fprintf(os.Stderr, "crashwriter: bad target %q\n", os.Args[4])
			os.Exit(2)
		}
	}

	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crashwriter: open store: %v\n", err)
		os.Exit(2)
	}
	// Deliberately not closed on the crash paths: exiting without Close is part
	// of what a real crash looks like.

	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: keyEnv})
	if err != nil {
		fmt.Fprintf(os.Stderr, "crashwriter: load signer: %v\n", err)
		os.Exit(2)
	}
	l := ledger.New(st, sg, time.Now)

	// startSeq is the chain position the first appended record will receive. It
	// is read from the current head so a crash run that shares a database with an
	// earlier run continues the chain instead of colliding on Seq or on the
	// position-derived record IDs.
	startSeq := uint64(0)
	if head, ok, herr := l.Head(); herr != nil {
		fmt.Fprintf(os.Stderr, "crashwriter: read head: %v\n", herr)
		os.Exit(2)
	} else if ok {
		startSeq = head.Seq + 1
	}

	switch mode {
	case "clean":
		appendRange(l, startSeq, n)
		_ = st.Close()
	case "exit-after":
		appendRange(l, startSeq, n)
		os.Exit(1)
	case "kill-mid":
		killMid(l, startSeq, n, target)
	default:
		fmt.Fprintf(os.Stderr, "crashwriter: unknown mode %q\n", mode)
		os.Exit(2)
	}
}

// appendRange appends n records starting at chain position startSeq, printing
// each committed record's Seq as it goes. It exits non-zero on any error.
func appendRange(l *ledger.Ledger, startSeq uint64, n int) {
	for i := 0; i < n; i++ {
		seq := startSeq + uint64(i)
		rec, err := buildRecord(seq)
		if err != nil {
			fmt.Fprintf(os.Stderr, "crashwriter: build record for seq %d: %v\n", seq, err)
			os.Exit(2)
		}
		if _, err := l.Append(rec); err != nil {
			fmt.Fprintf(os.Stderr, "crashwriter: append seq %d: %v\n", seq, err)
			os.Exit(2)
		}
		fmt.Printf("%d\n", seq)
	}
}

// killMid appends records up to n while a watchdog goroutine delivers an
// uncatchable SIGKILL as soon as target records have been committed. Because the
// watchdog fires the moment the count is reached, the process is most likely
// inside the next record's write transaction when it dies -- the worst moment a
// caller can arrange -- so whatever survives is genuine evidence about the
// transaction boundary.
func killMid(l *ledger.Ledger, startSeq uint64, n, target int) {
	var committed int64
	go func() {
		for atomic.LoadInt64(&committed) < int64(target) {
			// Yield so the watchdog still gets to run on a single-P scheduler
			// between the appender's syscalls.
			runtime.Gosched()
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}()

	for i := 0; i < n; i++ {
		seq := startSeq + uint64(i)
		rec, err := buildRecord(seq)
		if err != nil {
			fmt.Fprintf(os.Stderr, "crashwriter: build record for seq %d: %v\n", seq, err)
			os.Exit(2)
		}
		if _, err := l.Append(rec); err != nil {
			fmt.Fprintf(os.Stderr, "crashwriter: append seq %d: %v\n", seq, err)
			os.Exit(2)
		}
		fmt.Printf("%d\n", seq)
		atomic.AddInt64(&committed, 1)
	}
	// The watchdog never fired (target >= n): exit normally.
}

// buildRecord returns a valid record with no chain position, so the ledger
// assigns one. Its ID is derived from the target chain position, which keeps
// IDs unique within a run and across sequential runs that share a database.
func buildRecord(seq uint64) (record.Record, error) {
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"ok":true}`))
	if err != nil {
		return record.Record{}, err
	}
	reason, err := record.NewObservedReason(record.ReasonReturnedBySearch, ev)
	if err != nil {
		return record.Record{}, err
	}
	var contentHash record.Hash
	for i := range contentHash {
		contentHash[i] = byte(i + 1)
	}
	return record.Record{
		ID:     record.RecordID(fmt.Sprintf("cw-%012d", seq)),
		At:     recordAt,
		Event:  record.EventMemorySurfaced,
		Reason: reason,
		Subject: record.Subject{
			MemoryID:    "mem-crash",
			Scope:       record.Scope{UserID: "u1", AgentID: "a1"},
			ContentHash: contentHash,
		},
	}, nil
}
