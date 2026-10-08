#!/usr/bin/env bash
#
# demo.sh -- the read-path commands (verify, gaps, export, replay, explain),
# offline, in one command.
#
# What it does
#   Builds the notary binary into a throwaway temp directory and runs the
#   fixture seeder from source (go run ./testdata/demo/seed), generates a
#   THROWAWAY ed25519 signing key at runtime, seeds a small
#   fixture ledger through the real write paths (internal/ledger for the
#   records, internal/gap for one gap entry), and then walks the operator's
#   surface: verify -> gaps -> export -> replay -> explain <record-id> ->
#   explain --memory <id>. It finishes by demonstrating the two things the
#   project exists for: an edit to a stored record caught by `verify`, and an
#   unaudited operation reported by `gaps`.
#
# What it needs
#   A Go toolchain and the module's dependencies in the module cache. No
#   network, no Mem0, no LLM key, no configuration: every NOTARY_* variable it
#   uses is set here, into the temp directory.
#
# What it leaves behind
#   Nothing. The temp directory (ledger, gap log, trusted keys, JSONL output)
#   is removed on exit, and the signing key was never written to a file at all
#   -- it lives in this process's environment and is gone with it. The demo
#   commits no key material: `head -c 32 /dev/urandom | base64` makes a fresh
#   seed every run.
#
# Exit status
#   Non-zero if any step fails, or if a step that must fail (a tampered chain,
#   an outstanding gap) reports success. A demo that claims success while a
#   command inside it failed is worse than no demo.
#
# Compatibility
#   bash 3.2 (macOS) or newer: `set -euo pipefail`, no bash-4-only syntax.

set -euo pipefail

# --- where things are --------------------------------------------------------

if ! command -v go >/dev/null 2>&1; then
    printf 'demo: the go toolchain is not on PATH; this demo builds the CLI and its seeder.\n' >&2
    exit 127
fi

# The repository root is derived from this script's own path, so the script can
# be run from anywhere.
script_dir=$(cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(cd -- "$script_dir/.." && pwd)
cd -- "$repo_root"

# --- the demo's own workspace ------------------------------------------------

demo_tmp=$(mktemp -d "${TMPDIR:-/tmp}/notary-demo.XXXXXXXX")
cleanup() {
    rm -rf "$demo_tmp" || true
}
trap 'cleanup' EXIT

bin_dir="$demo_tmp/bin"
mkdir -p "$bin_dir"

# A fresh, throwaway signing key, generated here and never committed: a demo
# key is still a key, so this repository carries none.
NOTARY_SIGNING_KEY=$(head -c 32 /dev/urandom | base64)
export NOTARY_SIGNING_KEY

# Everything the demo writes lives in the temp directory.
NOTARY_DB_PATH="$demo_tmp/ledger.db"
NOTARY_GAP_LOG_PATH="$demo_tmp/notary-gaps.log"
NOTARY_TRUSTED_KEYS_PATH="$demo_tmp/trusted-keys.txt"
export NOTARY_DB_PATH NOTARY_GAP_LOG_PATH NOTARY_TRUSTED_KEYS_PATH

# The built binary goes on PATH under its own name, so the command lines this
# script prints are the ones a reader would type.
PATH="$bin_dir:$PATH"
export PATH

printf 'notary demo -- the read paths (verify, gaps, export, replay, explain), offline, in one throwaway directory\n\n'
printf '  workspace      %s\n' "$demo_tmp"
printf '  signing key    generated now (head -c 32 /dev/urandom | base64), never written to disk\n'
printf '  ledger         %s\n' "$NOTARY_DB_PATH"
printf '  gap log        %s\n' "$NOTARY_GAP_LOG_PATH"

# --- helpers -----------------------------------------------------------------

# step <n/total> <what you are about to see> <why it matters>
step() {
    printf '\n===============================================================================\n'
    printf 'STEP %s -- %s\n' "$1" "$2"
    printf '%s\n' "$3"
    printf '===============================================================================\n'
}

# run <command>... -- prints the command, then runs it. A non-zero exit aborts
# the demo through `set -e`.
run() {
    printf '\n$ %s\n' "$*"
    "$@"
}

# run_expecting_failure <command>... -- prints the command and requires a
# NON-zero exit. The two steps that show what notary does with a broken ledger
# are only a demonstration if the command actually refuses to pass: a zero exit
# here fails the demo.
run_expecting_failure() {
    printf '\n$ %s\n' "$*"
    status=0
    "$@" || status=$?
    if [ "$status" -eq 0 ]; then
        printf '\ndemo: %s exited 0, but this step exists to show it failing.\n' "$1" >&2
        exit 1
    fi
    printf '\n    (exit status %d -- and that non-zero exit is the point)\n' "$status"
}

# seed <mode> -- runs the fixture seeder from source, through the real write
# paths. Prints the command it runs.
seed() {
    printf '\n$ go run ./testdata/demo/seed %s\n' "$1"
    ( cd -- "$repo_root" && go run ./testdata/demo/seed "$1" )
}

# count_lines <file> -- a portable line count with no leading whitespace.
count_lines() {
    wc -l < "$1" | tr -d ' \t'
}

# --- 0. build ----------------------------------------------------------------

step "setup 1/3" "build the binary" \
"    Everything below runs this build. The seeder is run from source, because it
     is a fixture generator that lives outside the module's shipped build."

run go build -o "$bin_dir/notary" ./cmd/notary

step "setup 2/3" "seed the fixture ledger" \
"    Records are signed, so they cannot be written by hand: the seeder appends a
     small fixed set of records through internal/ledger -- the one write path --
     and one gap entry through internal/gap. Every record carries the
     content-derived idempotency key its real producer would give it, so running
     the seeder again against the same file appends nothing."

seed seed

step "setup 3/3" "write the trusted-keys file" \
"    The commands verify and replay check signatures against a file of trusted
     PUBLIC keys and never need the private one. The public half of the throwaway
     key is derived from it, into the temp directory: still no committed key
     material."

printf '\n$ go run ./testdata/demo/seed pubkey > %s\n' "$NOTARY_TRUSTED_KEYS_PATH"
( cd -- "$repo_root" && go run ./testdata/demo/seed pubkey > "$NOTARY_TRUSTED_KEYS_PATH" )
printf '    1 public key written, %s characters of base64\n' "$(wc -c < "$NOTARY_TRUSTED_KEYS_PATH" | tr -d ' \t')"

# --- 1..6: the surface -------------------------------------------------------

step "1/9" "verify -- is anything tampered with?" \
"    Walks the chain in order: each record's hash must recompute over its own
     bytes, link to its predecessor, and carry a signature from a trusted key.
     It cross-checks the gap log too, and exits non-zero on any break."

run notary verify

step "2/9" "gaps -- what did not get audited?" \
"    Reports every gap entry no record accounts for. The gap log is empty here,
     so this is the clean report -- which is what makes step 8's loud one legible."

run notary gaps

step "3/9" "export -- the ledger as a reviewable range" \
"    One stable JSON object per record, on stdout, so an audit can be diffed and
     grepped. Here it covers the whole fixture ledger."

printf '\n$ notary export --from 2026-08-31T00:00:00Z --to 2026-10-05T00:00:00Z > %s/export.jsonl\n' "$demo_tmp"
notary export --from 2026-08-31T00:00:00Z --to 2026-10-05T00:00:00Z > "$demo_tmp/export.jsonl"
printf '    export.jsonl: %s records; the first one, truncated for display:\n' "$(count_lines "$demo_tmp/export.jsonl")"
head -n 1 "$demo_tmp/export.jsonl" | cut -c 1-200
printf '    ...\n'

step "4/9" "replay -- the ledger as it stood at one instant" \
"    The knowledge instant, not the event instant: the records whose write time
     is at or before --at. It verifies the prefix it returns before printing it,
     so a ledger that was edited after that instant cannot be replayed as if it
     had not been."

printf '\n$ notary replay --at 2026-10-05T00:00:00Z > %s/replay.jsonl\n' "$demo_tmp"
notary replay --at 2026-10-05T00:00:00Z > "$demo_tmp/replay.jsonl"
printf '    replay.jsonl: %s records; the last one, truncated for display:\n' "$(count_lines "$demo_tmp/replay.jsonl")"
tail -n 1 "$demo_tmp/replay.jsonl" | cut -c 1-200
printf '    ...\n'

step "5/9" "explain <record-id> -- why does this one record exist?" \
"    The single-subject view, in prose, for a reader who must not read code. The
     sentence is export.Phrase's, the same deterministic sentence the range view
     prints -- explain never paraphrases."

run notary explain demo-search-1#1

step "6/9" "explain --memory <id> -- what happened to this memory?" \
"    One line per record in chain order, each carrying its event, reason, tier and
     BOTH instants: when the event happened, and when notary wrote the claim. Look
     at the last line of the output: a claim about 1 September, recorded on
     1 October -- a Reconstructed claim written long after the event it describes."

run notary explain --memory mem-demo-1

# --- 7..8: the two properties the project exists for ------------------------

step "7/9" "tamper -- someone edits a stored record" \
"    This is what the hash chain is for. The edit below is raw SQL against the
     database file -- no API, no CLI -- and it changes the memory text of the
     record that a search returned, leaving the stored hash and signature alone.
     explain still reads the row happily; verify must not be fooled."

seed tamper

step "7b/9" "the edit, as the read path sees it" \
"    Nothing here recomputes a hash: explain is a read path. This is exactly why
     a reader must not treat a rendered row as evidence on its own."

run notary explain demo-search-1#1

step "7c/9" "verify again -- it names the record and the field" \
"    The recomputed hash no longer matches the stored one, so verify fails and
     says which record and which field. (The signature is still valid: it covers
     the stored hash, which the edit did not touch -- which is why the hash check
     is the one that catches this.)"

run_expecting_failure notary verify

step "8/9" "a gap -- an operation that was never audited" \
"    A shell script cannot make a record that could not be written appear. It can
     only be recorded that it was not: the seeder appends one gap entry through
     the real gap log, standing in for a record the store rejected. The gap log is
     a separate file with its own hash chain, so a gap cannot be erased quietly."

seed gap

step "8b/9" "gaps -- the outstanding gap, named" \
"    The entry carries the kind, the scope, the correlation id of the missing
     record and why it is missing -- never the memory text, which stayed where it
     belonged. It exits non-zero, so it can gate a pipeline."

run_expecting_failure notary gaps

step "8c/9" "verify -- both domains, both failures, in one report" \
"    verify cross-checks the ledger against the gap log, so the same command
     reports the edited record AND the unaudited span. (After a gap, verify is
     not green until the missing record is reconciled back into the ledger.)"

run_expecting_failure notary verify

# --- 9: what just happened ---------------------------------------------------

step "9/9" "done" \
"    One command, nothing but a Go toolchain: the chain verified, the range
     exported, an instant replayed, two memories explained, an edited record
     caught by name and field, and an unaudited gap reported. No network, no
     Mem0, no LLM key, no committed key material."

printf 'The temp directory is removed on exit: %s\n' "$demo_tmp"
