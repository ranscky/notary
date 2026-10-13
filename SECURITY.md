# Security policy

## Reporting a vulnerability

Please report suspected security vulnerabilities through GitHub's **private vulnerability reporting**,
which keeps a report visible only to the maintainers until a fix is available:

1. Open the [Security tab](https://github.com/ranscky/notary/security) of this repository.
2. Choose **Report a vulnerability**.
3. Describe what you found, and how to reproduce it.

Please don't open a public issue for a suspected vulnerability.

**If you can't use that form** — it isn't listed, or your environment blocks it — open a public issue
saying only that you'd like a private channel, and put nothing about the finding in it. We'd rather
arrange a private route than have a report sit unread.

---

## What is in scope

Notary's security value is the set of claims it makes, so the most useful reports are the ones that
break one of them. Each of these is a property the project asserts somewhere; a way to defeat one is a
real finding.

- **The record can be altered without detection.** Anything that lets a record, a chain link, a
  signature or a checkpoint be changed, removed, reordered or fabricated while `notary verify` still
  reports the chain intact.
- **A confidence level can be forged.** Anything that causes an inferred claim, or a decision the memory
  layer gave no reason for, to be recorded as something Notary directly witnessed.
- **Key material can be read, or leaks.** Any path that prints or writes private key material, or that
  loads a key file which is not private to its owner.
- **Sensitive content can be read against the rules.** Anything that discloses content marked sensitive
  to a reader who was not given `--include-sensitive`.
- **A failure of the audit can be hidden.** Anything that lets a failed audit write go unrecorded, or
  lets the gap log be altered without detection.
- **The local dashboard can be reached or written to.** Anything that makes `notary serve` bind
  anywhere other than `127.0.0.1`, be readable by a foreign page, or append to the ledger.

## What is not a vulnerability

Reporting these is still welcome as feedback, but they are deliberate and documented rather than gaps:

- **A decision the memory layer gave no reason for, recorded as having no reason.** Notary reports that
  absence as its *No reason given* level on purpose. Not knowing is the honest answer, and dressing it up
  as a reason would be the actual defect.
- **Content a reader can see after passing `--include-sensitive`.** That flag is the documented way to
  reveal sensitive content, and it changes no hash. Redaction is presentation, not enforcement.
- **An attacker who already holds the signing key, or who has root on the machine holding the ledger.**
  Notary is evidence about what was recorded. It is not protection against someone who can rewrite both
  the ledger and the key.
- **The absence of enforcement.** This version blocks nothing by design. It detects and records; a
  finding that Notary failed to *prevent* an action is not a defect.
- **Anything in the dated design documents** under `docs/superpowers/`, which record decisions made at
  the time and are not statements about the current build.

## Supported versions

There are no tagged releases yet. Fixes land on the `master` branch, and `master` is the only supported
version.

## What to expect

Reports are acknowledged in the private advisory thread. This project is maintained by one person, so
please allow time for a response rather than reading silence as a decision. Once a fix is available the
advisory is published, and you're credited unless you'd rather not be.

## Please don't

- **Test against anyone else's deployment.** Notary holds a customer's audit trail. Run your tests
  against your own ledger and your own memory layer.
- **Include real memory content, real keys or a real ledger** unless something about it is necessary to
  demonstrate the issue — and redact it otherwise.
