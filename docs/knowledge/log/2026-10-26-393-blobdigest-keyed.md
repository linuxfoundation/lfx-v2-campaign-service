# 2026-10-26 — BlobDigest is keyed, and the review fixes on #393

**Fix** — three findings from the review round on PR #393, one commit.

**The truncation argument was wrong, so the digest is keyed now.** `dbtest.BlobDigest` printed
`"%d bytes sha256[:8]=%x"` and its doc comment claimed the truncation was what closed the
offline verification oracle. It is not. An unkeyed digest beside an exact byte length lets
anyone holding a CI log line hash each candidate of that length and compare, and truncating to
eight bytes leaves that intact: a wrong guess collides in eight bytes with probability 2^-64,
so a match still confirms the secret. Truncation only begins to help once the guess space
approaches the digest width, which is not the low-entropy case these assertions exist for. The
same comment also called eight bytes 2^64 of collision resistance — the birthday bound is
2^32; 2^64 is the preimage figure — but the number was never the point.

`BlobDigest` now renders `"%d bytes hmac[:8]=%x"`, an HMAC-SHA-256 under `blobDigestKey`: 32
bytes from `crypto/rand`, generated once per test process and never printed. Equal inputs still
render equal inside the run that printed them, which is all any call site needs — every one
renders both operands it compares into ONE failure message — and the line cannot be recomputed
offline. The key deliberately does not survive the process: nothing compares digests across
runs, and a stable key would be back to a value an attacker could eventually obtain and reuse.
Eight bytes is still what gets printed, because a failure message only has to tell two blobs
apart.

`TestBlobDigestReportsShapeNotValue` gained the property that actually closes the oracle: the
printed digest must not contain `hex(sha256(input)[:8])`, pinned against the unkeyed digest of
its own input, so a revert to `sha256.Sum256` fails the test rather than only renaming a
label. The two call-site comments and
[`2026-10-26-178-security-review-fixes.md`](2026-10-26-178-security-review-fixes.md) carried
the same false claim and are corrected.

**`secretlint-disable-line` has to be a trailing comment.** MegaLinter was red on this branch
with ten secretlint errors, all one mistake: the marker was written on the line BEFORE the
DSN literal, where it suppresses nothing. The convention in
[`../architecture/megalinter-secrets.md`](../architecture/megalinter-secrets.md) is a per-line
trailing comment, and the rest of the repo already spells it that way — `config_test.go:240`
carries a full DSN literal with a trailing marker and has never been flagged. Eight
literals in `dsnredact_test.go` and one in `cmd/verify-metrics/backend_test.go` now carry a
trailing `// secretlint-disable-line -- <reason>` naming the fixture each one serves; the
ineffective tokens on the preceding `//nolint:gosec` lines are gone. The tenth site is prose —
a markdown line cannot carry a Go comment — so
[`2026-10-26-178-newpool-dsn-redaction.md`](2026-10-26-178-newpool-dsn-redaction.md) was
respelled instead, naming the probe's user, host and database rather than rendering a
`user:pass@host` shape.

**A comment outlived the code it described.** `connect_redaction_test.go` said the
`SafeDSNErrFor` check was load-bearing because "NewPool below still redacts at the call site,
with the explicit-vs-environment choice still live". Commit `001a5e2`, in this same PR, removed
that call. Neither arm of `connectAndMigrate` calls `SafeDSNErrFor` now, so the check pins the
helper's own contract for its remaining direct callers — the live tests, which call it at over
forty sites across `invalid_index`, `migrate_down` and `stale_upsert`, each passing the scratch
dsn it built. There a caller still chooses the dsn it redacts against, so the wrong-DSN seam is
real and the check is still load-bearing.
