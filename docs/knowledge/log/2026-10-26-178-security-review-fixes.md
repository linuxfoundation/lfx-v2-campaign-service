# 2026-10-26 — the DSN redaction's own review round (#178, #161)

**Fix** — the security review of the #178 branch found five things, four of them real holes in
the redaction this branch had just written and one a latent hazard in code it did not touch.
All five are fixed here.

**The backstop can destroy the token the comparison looks for.** `SafeDSNErr` compared the
configured identifiers against `redact.URLUserinfo`'s OUTPUT only. That helper is URL-shaped
and destructive on prose: it rewrites everything up to the last pre-query `@` as `***@` and
truncates at `?`. So any identifier containing either character is gone before the comparison
runs — and Azure Database for PostgreSQL issues usernames shaped `user@servername`. With
`cfg.User = "svc@pgsrv"`, the server's own `28P01 password authentication failed for user
"svc@pgsrv"` reached the log as `***@pgsrv"`: the server half of the configured username,
intact, past a guard that believed it had found nothing. The comparison is now made against
the RAW text as well: `dsnIdentifiersPresent(dsn, raw) || dsnIdentifiersPresent(dsn, msg)`.
Both arms run because neither subsumes the other — the backstop can also CREATE a match the
raw text does not contain, by collapsing surrounding characters into one.
`TestSafeDSNErrWithholdsAnAtShapedUsername` pins it, and asserts the fault kind still survives
the withhold, because the failing behaviour omitted the username entirely rather than
withholding the message.

**A sibling that was never brought through the guard.** `cmd/verify-metrics/backend.go`'s
`openReadOnlyPool` is a hand-rolled sibling of `postgres.NewPool` — same three arms, same
order — and its connect and ping arms returned the driver's error directly. pgconn's
`ConnectError` embeds a `user=%s database=%s` fragment on every connection failure. Both arms
now go through `postgres.SafeDSNErr`, with the DSN source named by a package constant so the
operator is told which of the two places to go and fix. The redacted text is interpolated with
`%s` rather than wrapped with `%w` deliberately: wrapping would leave the driver's original
message reachable through `errors.As` for any later caller to print, and nothing in this
command inspects the chain — `run` matches only `flag.ErrHelp` — so there is no behaviour to
preserve. `TestOpenReadOnlyPoolDoesNotLeakIdentifiers` covers it.

**A SQLSTATE is checked for shape before it is concatenated.** `dsnFaultKind` appended
`pgErr.Code` to the withheld message. That value is the server's, and the withhold exists
precisely because the server's text is not trusted. It is now gated by `isSQLState`: exactly
five characters, each `A-Z` or `0-9`. A SHAPE check, not a membership check — a code outside
the closed set is still a code and still identifies a fault kind, and an allowlist would go
stale against every PostgreSQL release. Hand-written rather than a regexp because it runs on
an error path. `TestDSNFaultKindRejectsAHostileSQLState` covers the near-misses and keeps a
positive control so the test cannot pass by rejecting everything.

**The digest beside the length was an oracle.** Six sites across the two live-test files
printed a full unsalted `sha256.Sum256(blob)` next to an exact byte length, and two of them
digest decrypted PLAINTEXT. That pair is an offline verification oracle: anyone holding the
log line can confirm a guess at a low-entropy secret without touching the service. All six now
go through `dbtest.BlobDigest`, which truncates to eight bytes — 2^64 of collision resistance,
which nothing in this suite will collide across, and far too short to confirm a guess against.
One exported helper rather than six copies, for the reason `SafeDSNErr` is exported: two sites
disagreeing about what "safe to print" means is how the first leak got in.

**Note** — `redactMigrateErr`'s exemptions are tested with `errors.Is`, so an exemption covers
the WHOLE CHAIN: anything wrapped ABOVE an exempt sentinel escapes redaction too, including
text a future caller adds. For the three index sentinels that is intended — the wrapping text
is package-composed remediation DDL. For `errDSNNotURLScheme` the argument rests on its CALL
SITES (`pool.go:1058` returns it bare), not on the sentinel, so wrapping a DSN-bearing message
around it at any future site would leak through an exemption that still looks correct where it
is written. No defect today; the hazard is now written down beside the exemption list, and
`TestDSNNotURLSchemeIsReturnedBare` pins the call site rather than the sentinel, which is the
half that can actually change.

**Verification** — every fix was mutation-tested, and each mutation reverted from a scratchpad
copy after it was observed. Reverting the `||` to the redacted-text comparison alone failed
only `TestSafeDSNErrWithholdsAnAtShapedUsername`; removing the `isSQLState` gate failed only
`TestDSNFaultKindRejectsAHostileSQLState`; returning either arm of `openReadOnlyPool` raw
failed only `TestOpenReadOnlyPoolDoesNotLeakIdentifiers`. `BlobDigest` was the one fix with no
runtime coverage, because the live tests SKIP without `TEST_DATABASE_URL`:
`TestBlobDigestReportsShapeNotValue` closes that, pinning that the bytes never appear, that
the length survives, and that the hex is sixteen characters rather than sixty-four.
