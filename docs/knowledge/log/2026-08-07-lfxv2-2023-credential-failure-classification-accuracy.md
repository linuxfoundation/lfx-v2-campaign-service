# 2026-08-07 — LFXV2-2023: credential-failure classification says only what it can prove

**Update** — Eight suppressed review findings on the account-discovery PR clustered into two
statements that were repeated, identically wrong, across five files.

**The AES-GCM malformed boundary was stated as "shorter than a nonce".** It is
`NonceSize() + Overhead()`. `Seal` appends an authentication tag to every message, including an
empty one, so a blob between those two lengths is not malformed by this test — it reaches `Open`,
fails authentication, and is then classified as the application-key condition. One truncated row
would have sent a responder to look at the deployment key.
`internal/infrastructure/crypto/aesgcm.go` had the guard right the whole time; the domain-layer
docs had drifted from it.

**A GCM authentication failure was described as proving a deployment-wide outage.** It does not.
The tag check returns the same failure for a wrong or rotated application key and for one
tampered or corrupted row, and cannot distinguish them. What distinguishes them is the COUNT of
failing projects, not the sentinel. The 500 mapping stands, but for the correct reason: the
ambiguity is asymmetric — over-escalating one bad row is recoverable, under-reporting a broken key
is not.

**A fully diagnosed state was logging as `reason=unclassified`.** A connection row whose
credential column is empty short-circuits in `creds.resolve` before any decrypt, in a different
branch from every other defect, so it wrapped only the status sentinel and the reason vocabulary
had nothing to read. New `domain.ErrCredentialsAbsent` and its `credentials_absent` token fix it.
That the empty case is wrapped in its own branch is exactly why it regressed: a fix applied to the
classification switch never reached it.

Also corrected: the chart comment told a maintainer to "move a provider into the google-ads
branch", which is impossible — that branch matches a literal string. And the discovery validator's
godoc claimed a credentials-first bootstrap that `design/connection.go:333`
(`Required("account_id")`) does not yet permit; the reachable case today is re-pointing an
existing connection.
