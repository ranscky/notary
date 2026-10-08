package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"notary/config"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/report"
	"notary/internal/sign"
	"notary/internal/store"
)

// newReportCmd builds the `notary report` subcommand: the command that renders
// a slice of the ledger as a folder of static HTML pages for the compliance
// lead the design names -- a reader who has to understand an agent's memory
// decisions without reading code or running a command.
//
// It names exactly one subject -- --memory <mem0-id>, or --from with --to
// defaulting to now -- and refuses both together, neither, and an explicitly
// empty --memory. The mutual exclusion is enforced explicitly rather than with
// Cobra's declarative helper, so the message can say which subject was given,
// which is missing, and what to do; that is explain's rule (explainRequest),
// and this command's flags are the same shape.
//
// The four scope flags narrow a RANGE through export.ScopeMatches -- the one
// matcher export and replay use, so the three range views compose identically --
// and are refused with --memory, where the subject already names the slice. The
// refusal is on both sides: internal/report refuses a memory id carrying a
// scope, because the read would not apply it while the page printed it, and a
// page must never assert a narrowing the read never applied.
//
// It is a read path: it opens the ledger with no signer, never appends, never
// touches the gap log beyond reading it, and holds no private key. What it does
// hold, when it verifies, is the public keyring: the chain state is computed by
// ledger.CollectBreaks -- the one definition of "is this ledger intact?", the
// same function `notary verify` runs -- so a page and the command cannot
// disagree. --no-verify skips that walk and the keyring it needs, and the
// chain-state page is still written, saying that no verification ran: an empty
// break list is what both a passing check and a skipped one look like, so a
// skipped one is never rendered as a clean one.
//
// Unlike its siblings it is exempt from the root command's ledger pre-run, and
// it refuses a ledger path with no ledger at it (requireLedgerFile): store.Open
// CREATES a missing SQLite file, and `notary report --out dist` aimed at a
// mistyped path would otherwise have an empty ledger created for it, render
// zero memories over it, and exit 0 -- an authoritative-looking compliance
// artefact describing nothing. A report is the one place where "nothing was
// there" and "I could not look" must never look the same.
func newReportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Render a slice of the ledger as static HTML pages",
		Long: "Report renders a slice of the audit ledger as a folder of static HTML\n" +
			"pages -- an index, one page per memory, one page per record, the chain\n" +
			"state in full, and the two files they link to -- for a reader who has to\n" +
			"understand an agent's memory decisions without reading code or running a\n" +
			"command.\n" +
			"\n" +
			"The slice is named by exactly one subject: --memory <mem0-id> for one\n" +
			"memory's lifecycle, or --from <RFC3339> with an optional --to <RFC3339>\n" +
			"(defaulting to now) for a range. Both together, or neither, is refused.\n" +
			"A range is bounded on each record's At (the event time), as an export's\n" +
			"range is, and --user-id, --agent-id, --app-id and --run-id narrow it to\n" +
			"one entity scope. They are refused with --memory, where the subject\n" +
			"already names the slice.\n" +
			"\n" +
			"--out DIR is required, and is refused when it already holds files unless\n" +
			"--force is given: a report writes a folder of its own rather than\n" +
			"merging its pages into whatever directory it was handed. --force writes\n" +
			"it into the directory as it is, and deletes nothing.\n" +
			"\n" +
			"By default the ledger's chain state is computed through the same break\n" +
			"collection `notary verify` uses -- so the page and the command cannot\n" +
			"disagree -- and rendered in full on verify.html: clean, or every break\n" +
			"with the record and the field that failed. That needs the trusted-key\n" +
			"file and no signing key: report never signs. --no-verify skips the walk\n" +
			"and the keyring it needs; verify.html is written anyway and says that no\n" +
			"verification ran, because a skipped check must never read as a passing\n" +
			"one. The chain state covers the whole ledger, not the slice: an intact\n" +
			"part of a tampered chain is still tampered.\n" +
			"\n" +
			"Stored content marked sensitive is withheld by default and the page says\n" +
			"so; --include-sensitive prints it. That flag governs the record's stored\n" +
			"content and nothing else, and changes no hash. The evidence block that\n" +
			"every record page carries is NOT filtered by it: it is the record's own\n" +
			"stored reason, printed exactly as the ledger hashed it, and such a\n" +
			"reason can quote memory text the sensitivity rules never marked, because\n" +
			"reconcile records whole upstream objects rather than curated fields.\n" +
			"Widening the flag does not narrow that block, which is why the pages say\n" +
			"so and why it is said here, before the artefact leaves the building.\n" +
			"\n" +
			"The ledger must already exist: report refuses a path with no ledger at it\n" +
			"rather than creating an empty one, because a report over nothing and a\n" +
			"report over a mistyped path must never look the same. It exits zero once\n" +
			"the pages are written, and non-zero when the chain state it rendered is\n" +
			"broken -- the pages are written either way, because a report of a\n" +
			"tampered ledger is exactly the artefact its reader needs.\n" +
			"\n" +
			"The setup page -- what this deployment is missing, and the command that\n" +
			"fixes each finding -- is `notary doctor --out DIR`; a report carries\n" +
			"evidence and no configuration, and never writes one. Its own pages are\n" +
			"self-contained and offline, linked relatively, so the folder travels as a\n" +
			"whole: send or copy the directory rather than one page from inside it.",
		Args: cobra.NoArgs,
		// The root command's PersistentPreRunE creates the ledger file (and its
		// parent directories) before every subcommand runs. Report opts out
		// (root.go's skipEnsureLedgerAnnotation), because that pre-run would
		// manufacture an empty ledger at a mistyped path and the report would
		// then render zero memories over it and exit 0. Report establishes the
		// ledger's existence itself, in requireLedgerFile, with an error that
		// names the path -- the same kind of opt-out, for the same kind of
		// reason, as the one `doctor` carries.
		Annotations: map[string]string{skipEnsureLedgerAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
			}
			return runReport(cmd, cfg)
		},
	}

	// Every flag is declared here and read back through cmd.Flags() in
	// reportRequest, so there is no package- or closure-level mutable state.
	cmd.Flags().String("out", "",
		"write the report into this directory (required); refused when it already holds files, unless --force")
	cmd.Flags().String("memory", "",
		"report this Mem0 memory id's lifecycle instead of a range")
	cmd.Flags().String("from", "",
		"start of the range as RFC3339 (required unless --memory); bounds each record's At, the event time")
	cmd.Flags().String("to", "",
		"end of the range as RFC3339 (defaults to now); bounds each record's At")
	cmd.Flags().Bool("include-sensitive", false,
		"print stored content marked sensitive instead of withholding it (changes only what is printed, never a hash, and it does not reach the evidence block)")
	cmd.Flags().Bool("no-verify", false,
		"skip the chain walk and the trusted-key file it needs; verify.html is still written and says no verification ran (a skipped check is never rendered as clean)")
	cmd.Flags().Bool("force", false,
		"write into --out even when it already holds files (never implied; the report deletes nothing)")
	cmd.Flags().String("user-id", "", "restrict the report to this Mem0 user id (a range only; refused with --memory)")
	cmd.Flags().String("agent-id", "", "restrict the report to this Mem0 agent id (a range only; refused with --memory)")
	cmd.Flags().String("app-id", "", "restrict the report to this Mem0 app id (a range only; refused with --memory)")
	cmd.Flags().String("run-id", "", "restrict the report to this Mem0 run id (a range only; refused with --memory)")
	return cmd
}

// runReport resolves the request, opens the ledger read-only, computes the
// chain state, renders the pages and reports what was written. It returns a
// non-nil error -- and so a non-zero exit -- for every failure and for one
// non-failure: a broken chain, whose pages are written before the error is
// returned, because the exit code is an interface too and a report over a
// tampered ledger must not report success to a pipeline reading only that.
//
// Nothing is written before every refusal has happened: the flags are checked,
// the output directory is checked, the ledger's existence is established, and
// only then is anything opened -- and the renderer itself refuses before it
// creates the output directory, so a refused run leaves no directory behind.
func runReport(cmd *cobra.Command, cfg *config.Config) error {
	out := cmd.OutOrStdout()

	// One instant for the whole run: the range's default end and the instant the
	// pages date themselves from are the same moment, so a report cannot cover a
	// window that ends after it was made.
	now := time.Now()
	req, err := reportRequest(cmd, func() time.Time { return now })
	if err != nil {
		return err
	}

	outDir, err := cmd.Flags().GetString("out")
	if err != nil {
		return fmt.Errorf("reading --out: %w", err)
	}
	force, err := cmd.Flags().GetBool("force")
	if err != nil {
		return fmt.Errorf("reading --force: %w", err)
	}
	if err := checkReportOutDir(outDir, force); err != nil {
		return err
	}
	if err := requireLedgerFile(cfg.DBPath); err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening ledger %s: %w", cfg.DBPath, err)
	}
	defer func() { _ = st.Close() }()

	// The ledger is opened for read: report never appends, so it passes no
	// signer, and it never holds a private key.
	l := ledger.New(st, nil, nil)

	if req.Verify {
		verifier, err := reportVerifier(cfg)
		if err != nil {
			return err
		}
		// CollectBreaks runs the chain walk, the gap cross-check and the gap
		// log's own integrity -- the three checks `notary verify` reports --
		// and its error already carries the context that command prints, so it
		// is returned as it is. The breaks go into the request unchanged: the
		// renderer walks nothing of its own.
		breaks, err := ledger.CollectBreaks(l, st, cfg.GapLogPath, verifier)
		if err != nil {
			return err
		}
		req.Breaks = breaks
	}
	req.VerifiedAt = now.UTC()

	// cmd.Context() is nil for a command not run through Execute (the test seam
	// constructs one directly), so fall back to a background context.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	res, err := report.New(l).Render(ctx, req, outDir)
	if err != nil {
		return err
	}

	// What the run wrote, in the operator's terms: Result's counts, with Pages
	// counting every file -- the four fixed files and the two assets included,
	// which is the number of files they are holding. The arithmetic is stated
	// rather than left to be worked out.
	fmt.Fprintf(out,
		"report: wrote %s to %s: %s, %s, %s, plus the index, the chain state page and the two assets\n",
		countText(res.Pages, "page", "pages"), outDir,
		countText(res.Memories, "memory", "memories"),
		countText(res.Records, "record", "records"),
		countText(res.Redacted, "record with content withheld", "records with content withheld"))

	// The chain state, in the artefact's own words, so the summary and the page
	// cannot describe one run differently.
	switch {
	case !req.Verify:
		fmt.Fprintf(out,
			"report: the chain state is not verified: --no-verify skipped the walk, "+
				"so this report claims nothing about the chain\n")
	case len(req.Breaks) == 0:
		fmt.Fprintf(out,
			"report: the chain state is clean: verification ran and collected no break in the whole ledger\n")
	default:
		breaks := countText(len(req.Breaks), "break", "breaks")
		fmt.Fprintf(out,
			"report: the chain state is broken: %s found; verify.html names the record and the field of each one\n",
			breaks)
		// The pages are written and the summary says so: the operator keeps the
		// artefact, and the exit code still refuses to call a tampered ledger a
		// success.
		return fmt.Errorf(
			"report: the chain does not verify: %s found; the pages were written to %s and verify.html "+
				"names the record and the field of each one", breaks, outDir)
	}
	return nil
}

// reportRequest resolves a report.Request from cmd's flags: the subject (one
// memory, or a range), the scope that narrows a range, --include-sensitive, and
// --no-verify as Verify: false.
//
// Exactly one subject is required, and the mutual exclusion is enforced here,
// explicitly, rather than with Cobra's declarative helper, so the message can
// say which of the two was given, which is missing, and what to do -- the shape
// explainRequest established. The order of the checks is deliberate: an
// explicitly empty --memory is its OWN refusal, never answered by "a subject is
// required", because the store matches an empty memory_id -- the column's
// DEFAULT -- and an empty filter would report every memory-less record in the
// ledger as though it were one memory.
//
// The clock is passed in rather than read here, so a test can pin what --to
// defaults to; nothing else in this function consults it.
func reportRequest(cmd *cobra.Command, now func() time.Time) (report.Request, error) {
	var r report.Request

	memoryGiven := cmd.Flags().Changed("memory")
	memory, err := cmd.Flags().GetString("memory")
	if err != nil {
		return report.Request{}, fmt.Errorf("reading --memory: %w", err)
	}
	fromGiven := cmd.Flags().Changed("from")
	from, err := cmd.Flags().GetString("from")
	if err != nil {
		return report.Request{}, fmt.Errorf("reading --from: %w", err)
	}
	toGiven := cmd.Flags().Changed("to")
	to, err := cmd.Flags().GetString("to")
	if err != nil {
		return report.Request{}, fmt.Errorf("reading --to: %w", err)
	}
	scope, scopeFlags, err := reportScope(cmd)
	if err != nil {
		return report.Request{}, err
	}

	switch {
	case memoryGiven && fromGiven:
		return report.Request{}, fmt.Errorf(
			"report: give either --memory <mem0-id> or --from <RFC3339>, not both "+
				"(--memory %q and --from %q were both given): one subject names the slice", memory, from)
	case memoryGiven && toGiven:
		return report.Request{}, fmt.Errorf(
			"report: give either --memory <mem0-id> or a --from/--to range, not both "+
				"(--memory %q and --to %q were both given): one subject names the slice", memory, to)
	case !memoryGiven && !fromGiven && toGiven:
		return report.Request{}, fmt.Errorf(
			"report: --to %q was given without --from: a range needs the instant it starts at "+
				"(and --memory <mem0-id> names one memory instead of a range)", to)
	case !memoryGiven && !fromGiven:
		return report.Request{}, fmt.Errorf(
			"report: a subject is required: give --memory <mem0-id> for one memory's lifecycle, or " +
				"--from <RFC3339> with an optional --to <RFC3339> for a range")
	case memoryGiven && memory == "":
		return report.Request{}, fmt.Errorf(
			"report: --memory needs a non-empty Mem0 memory id; an empty one would " +
				"report every record whose memory id is unset, not one memory")
	case !memoryGiven && from == "":
		return report.Request{}, fmt.Errorf(
			"report: --from needs a non-empty instant: give the start of the range as RFC3339 " +
				"(for example 2026-09-28T12:00:00Z)")
	case memoryGiven && len(scopeFlags) > 0:
		return report.Request{}, fmt.Errorf(
			"report: --memory %q cannot be combined with %s: the four scope flags (--user-id, --agent-id, "+
				"--app-id, --run-id) narrow a RANGE, and a memory id already names the slice, so the pages "+
				"would assert a narrowing the read never applied; drop them, or name the slice with --from and --to",
			memory, strings.Join(scopeFlags, ", "))
	}

	if memoryGiven {
		// A memory subject reaches the renderer alone: no window and no scope.
		// internal/report refuses both combinations for the same reason, so the
		// two layers agree rather than one of them quietly ignoring a flag.
		r.MemoryID = memory
	} else {
		fromTime, err := time.Parse(time.RFC3339, from)
		if err != nil {
			return report.Request{}, fmt.Errorf(
				"report: parsing --from %q as RFC3339 (for example 2026-09-28T12:00:00Z): %w", from, err)
		}
		r.From = fromTime

		if to == "" {
			r.To = now()
		} else {
			toTime, err := time.Parse(time.RFC3339, to)
			if err != nil {
				return report.Request{}, fmt.Errorf(
					"report: parsing --to %q as RFC3339 (for example 2026-09-28T12:00:00Z): %w", to, err)
			}
			r.To = toTime
		}
		// A backwards range is refused here, naming the flags that were typed,
		// rather than left to the renderer's own validation -- which refuses it
		// too, but cannot say which flag to change.
		if r.To.Before(r.From) {
			return report.Request{}, fmt.Errorf(
				"report: --to %s is before --from %s: the range would hold no record and the pages would "+
					"describe an empty slice; swap the two instants", to, from)
		}
		r.Scope = scope
	}

	includeSensitive, err := cmd.Flags().GetBool("include-sensitive")
	if err != nil {
		return report.Request{}, fmt.Errorf("reading --include-sensitive: %w", err)
	}
	r.IncludeSensitive = includeSensitive

	noVerify, err := cmd.Flags().GetBool("no-verify")
	if err != nil {
		return report.Request{}, fmt.Errorf("reading --no-verify: %w", err)
	}
	// --no-verify reaches the renderer as Verify: false, never as an empty break
	// list: len(Breaks) == 0 is what both a passing check and a skipped one look
	// like, and Request.Verify exists beside Request.Breaks only to tell them
	// apart (Review Focus 5).
	r.Verify = !noVerify

	return r, nil
}

// reportScope reads the four scope flags, returning the scope they name and the
// names of the ones actually given -- so a refusal can say what the operator
// typed rather than listing all four. It mirrors exportRequest's scope loop.
func reportScope(cmd *cobra.Command) (record.Scope, []string, error) {
	var (
		scope record.Scope
		given []string
	)
	for _, dim := range []struct {
		flag string
		dst  *string
	}{
		{"user-id", &scope.UserID},
		{"agent-id", &scope.AgentID},
		{"app-id", &scope.AppID},
		{"run-id", &scope.RunID},
	} {
		v, err := cmd.Flags().GetString(dim.flag)
		if err != nil {
			return record.Scope{}, nil, fmt.Errorf("reading --%s: %w", dim.flag, err)
		}
		*dim.dst = v
		if cmd.Flags().Changed(dim.flag) {
			given = append(given, "--"+dim.flag)
		}
	}
	return scope, given, nil
}

// checkReportOutDir refuses an --out that cannot hold a report: a missing value,
// a path that is not a directory, or a directory that already holds something
// without --force. It writes nothing and creates nothing.
//
// The refusal is the difference between "you asked for this folder" and "it
// merged into my documents folder": a report writes a directory of its own, and
// a path that does not exist yet is fine -- the renderer creates it -- while a
// directory with files in it is the operator's, not the report's. --force is
// never implied by another flag, writes the pages into the directory as it is,
// and deletes nothing that was already there.
func checkReportOutDir(dir string, force bool) error {
	if dir == "" {
		return errors.New(
			"report: --out is required: give the directory the pages are written into, which the report " +
				"creates when it does not exist (for example --out ./report)")
	}

	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("report: reading --out %s: %w", dir, err)
	case len(entries) == 0, force:
		return nil
	}

	return fmt.Errorf(
		"report: --out %s is not empty (%s): a report writes a folder of its own, and merging its pages into "+
			"a directory that already holds something would leave them among files they do not belong to; give "+
			"an empty directory, or one that does not exist yet, or pass --force to write into %s as it is -- "+
			"--force writes the pages and deletes nothing",
		dir, countText(len(entries), "entry", "entries"), dir)
}

// requireLedgerFile refuses a ledger path with no ledger at it.
//
// store.Open -- and the SQLite driver under it -- CREATES a missing database
// file. Everywhere else that is a convenience; for a report it is a trap:
// `notary report --out dist --from ...` aimed at a mistyped path would have an
// empty ledger created for it, render zero memories over it and exit 0 -- an
// authoritative-looking compliance artefact describing nothing, over a path the
// operator mistyped, that no reader could tell apart from a real empty ledger.
//
// So this command carries the root command's opt-out
// (skipEnsureLedgerAnnotation, the annotation `doctor` also carries) and
// establishes the ledger's existence itself, before anything is opened, with an
// error that names the path the operator gave. A path that exists but is not a
// ledger still fails, one step later, in store.Open -- naming the same path.
func requireLedgerFile(path string) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf(
			"report: no ledger at %s: a report never creates one, because a report over an empty ledger and "+
				"a report over a path that does not exist must never look the same; check %s, and run "+
				"`notary doctor` if this deployment has not written a ledger yet",
			path, config.EnvDBPath)
	case err != nil:
		return fmt.Errorf("report: cannot read the ledger at %s: %w", path, err)
	case info.IsDir():
		return fmt.Errorf("report: the ledger path %s is a directory, not a ledger file", path)
	}
	return nil
}

// reportVerifier loads the trusted keys a verified report needs and returns the
// verifier over them.
//
// A keyring that is not configured, or that holds no key, is a refusal rather
// than a report with an empty chain state: the operator asked for a report that
// carries the chain's integrity, and silently downgrading to one that claims
// nothing -- leaving them to find out by reading verify.html -- is exactly the
// quiet sameness this command exists to refuse. --no-verify is the explicit way
// to ask for a report without the chain state, and the error names it, so both
// ways forward are on the operator's screen. `report` never loads the PRIVATE
// key: it signs nothing.
func reportVerifier(cfg *config.Config) (*sign.Verifier, error) {
	if cfg.TrustedKeysPath == "" {
		return nil, fmt.Errorf(
			"report: no trusted keys are configured, so the chain state cannot be computed: set %s to a file "+
				"of base64-encoded ed25519 public keys, one per line, or pass --no-verify to write a report "+
				"whose verify.html says that no verification ran",
			config.EnvTrustedKeysPath)
	}
	keyring, err := sign.LoadTrustedKeys(cfg.TrustedKeysPath)
	if err != nil {
		return nil, fmt.Errorf("loading trusted keys: %w", err)
	}
	if len(keyring) == 0 {
		return nil, fmt.Errorf(
			"report: no trusted keys were found in %s, so the chain state cannot be computed: add at least "+
				"one base64-encoded ed25519 public key, one per line, or pass --no-verify",
			cfg.TrustedKeysPath)
	}
	return sign.NewVerifier(keyring), nil
}

// countText renders a count with its noun, in the form the report's own pages
// use ("2 memories", "1 memory"), so a summary reads as English rather than as
// "1 memories".
func countText(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
