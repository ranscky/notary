# Tamper fixture — the P2 acceptance procedure

This directory holds no code. It documents the manual procedure an operator (or
an evaluator) runs to confirm that `notary verify` names the exact record and
field that was changed, rather than reporting a bare "invalid".

`notary verify` answers one question: *has anything been tampered with?* It
walks the ledger in `Seq` order and, for every record, checks that

- the stored `hash` recomputes from the record,
- `prev_hash` matches the predecessor's `hash` (or the genesis hash at `Seq 0`),
- the `signature` verifies under a key in the trusted keyring, and
- `seq` is exactly one past its predecessor.

A clean chain prints `ok: <n> records verified` and exits `0`. Any damage prints
one line per break — `record <id> (seq <n>): <field> — <detail>` — and exits
non-zero.

## Setup

1. Produce a signed ledger and a matching **trusted keys** file. The keyring is a
   file of base64-encoded ed25519 **public** keys, one per line; blank lines and
   `#` comments are ignored. Each key's ID is derived from the key itself, so the
   file never states a key ID.

   ```
   NOTARY_TRUSTED_KEYS_PATH=./trusted-keys.txt notary verify
   ```

2. Confirm the ledger is clean before tampering:

   ```
   $ NOTARY_TRUSTED_KEYS_PATH=./trusted-keys.txt notary verify
   ok: 5 records verified
   ```

## Tamper by hand

Edit the SQLite file directly — the same thing an attacker with file access
would do. The ledger file is `$NOTARY_DB_PATH` (default `notary.db`).

```sql
-- Change a record's event without going through the ledger.
UPDATE records SET event = 'memory_kept' WHERE seq = 3;

-- Break the chain link on a later record.
UPDATE records SET prev_hash = zeroblob(32) WHERE seq = 4;

-- Flip a signature byte.
UPDATE records SET signature = <one-byte-changed-blob> WHERE seq = 2;

-- Corrupt the stored Reason so the row no longer decodes at all.
UPDATE records SET reason_payload = 'not a valid reason' WHERE seq = 1;
```

## Observe

Run `notary verify` again. Each edit must be reported against **the record and
field that changed**, and the command must exit non-zero:

```
$ NOTARY_TRUSTED_KEYS_PATH=./trusted-keys.txt notary verify
record rec-0004 (seq 3): hash — stored hash 982b… does not match hash recomputed over the record: 935b…
notary: verification failed: 1 break(s) found
```

- An edited `event` is a `hash` break (the event is part of the record digest).
- A broken link is a `prev_hash` break (and, because `prev_hash` is part of the
  digest, usually a `hash` break on the same record).
- A changed signature byte is a `signature` break, with `Detail` distinguishing
  an **unknown key** ("unknown key") from a **bad signature** ("invalid signature").
- A record whose `Reason` no longer decodes is a `decode` break.

## Two things this procedure guards against

- **Verifying nothing must never look like verifying everything.** With no
  trusted keys configured (`NOTARY_TRUSTED_KEYS_PATH` unset or empty) — or with a
  keyring file that yields no keys — `notary verify` exits non-zero with a
  plain-language error. It never prints `ok` for a run that could not check a
  single signature.

- **The chain is walked by `Seq`, not by any time window.** Records are read in
  chain order, so a record with an unusual `at` cannot be silently skipped.
