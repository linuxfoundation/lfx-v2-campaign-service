# 2026-09-28 — LFXV2-2665: seven rationales still argued against the old `OK: true`

**Docs** — round 24 of review, raised by the repo-code reviewer. No behaviour change.

Round 9 made an inconclusive probe answer `OK: false`. Seven sites went on justifying themselves
against the behaviour that change removed.

`ProbeInconclusive` in `googleads`, `microsoft` and `reddit` each carries a dedicated
`ErrTokenRequestRejected → false` arm, and each explained it the same way: without the arm the
sentinel would "report the connection as OK with an advisory — unproven reported as healthy". It
would not. An unrecognised error inherits the `true` default, an inconclusive probe answers
`OK: false`, and the operator sees a failed test either way. The three `token_refusal_test.go`
files repeated the claim in a comment and again in a failure message, so a test that fired would
have explained itself wrongly to whoever read the output.

The arm is still right and still not redundant; only the reason had gone stale. What the default
actually costs is the STATUS axis and the routing: a request this service composed and the token
endpoint refused would be rendered as a platform that could not be reached — an advisory inviting
the operator to wait out somebody else's outage on a connection no waiting repairs — instead of
the typed `500` that pages the people who own the malformed request. All six sites now say that.

`internal-platform-linkedin.md` had the mirror of it for the org walk: folding a non-`429` `4xx`
into the inconclusive bucket would "answer 'healthy' for a permanently broken cross-check
forever". Same correction — `OK: false` either way, since an inconclusive outcome is not a healthy
one; what is lost is a verdict naming what to repair, traded for an advisory inviting a retry that
cannot succeed.

Worth naming, because this is the fourth round in which stale prose was the finding: the three
concept files for these packages — `internal-platform-googleads.md`,
`internal-platform-microsoft.md`, `internal-platform-reddit.md` — already described the CURRENT
consequence correctly. The documentation was right and the code comments beside the code were
wrong, which is the direction this repo's conventions least expect. A sweep that reads the concept
files to check the comments would have found nothing.
