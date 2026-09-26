# 2026-09-26 — LFXV2-2665: a delete landing mid-test, and two claims this branch made false

**Fix** — Round 12 of the connection-probe work. Three findings taken, one declined for the second
round running.

## A connection deleted mid-test was reported as found

The test endpoint reads the connection TWICE: `testConn` reads the row for the baseline, and then
the prober's own `resolveOwned` reads it again to build a client. A delete landing between those
two reads makes the second answer `domain.ErrNotFound`.

That fell to `testConnUpstream`'s default arm, which answered **200** with "connection found, but
google ads verification could not be completed". The second clause is true and the first is the
half that stopped being true — the caller is told a connection it no longer has is merely
untested, and the endpoint declares 404 for exactly this. The baseline read already maps the
sentinel that way, so the two reads were disagreeing about the same row seconds apart.

There is now an explicit `ErrNotFound` arm returning `mapErr(perr)`. Reading the sentinel as the
CONNECTION's absence rather than some platform-side 404 is safe on this path and only on this
path: every prober resolves through `creds.resolveOwned`, which never consults the LF system
scope — `probe_owned_resolver_test.go` pins that — so it can only mean the project's own row is
gone. `TestTestConnUpstream_DeletedMidTestIs404` was confirmed to fail against the default arm
before it was kept; it reported the 200 verbatim.

## This branch made two documented claims false, in two places each

`valueShapes` in `internal/bootstrap/sysacct.go` gathers the shape rules an id is held to, and its
comment split the providers into "design `Pattern()`" and "runtime validator only", putting Google
Ads and Reddit in the second group. This branch added `Pattern` and `MaxLength` for both, so the
split as written is now wrong — in the Go comment and again in `internal-bootstrap.md`.

What is NOT wrong is the two-source structure itself, and the corrected text says so rather than
collapsing it: a `Pattern` binds the HTTP transport, and this installer writes past it straight to
the repository, so a row written by bootstrap, by a migration, or before the pattern existed never
passed through Goa at all. Microsoft is where the two sources still differ in substance, and
`valueValidators` is where that is explained.

## The credential-only justification, phrasings four and five

Round 9 corrected the justification for `OK: false` on an inconclusive probe. Round 10 swept for
it and found two more. Round 11's review found a third phrasing. Round 12's found a fourth — the
`ErrConnectionProbeInconclusive` row of `internal-service.md`'s response table, which said
"nothing was learned, so the credential did not authenticate against the provider" while the
prose immediately below it explained why that is untrue.

Sweeping once more, for the assertion rather than any of its phrasings, turned up a fifth in
`connection_test.go`'s LinkedIn inconclusive subtest. Both now state the conjunction, and the
LinkedIn one adds what makes the old wording not merely imprecise but false there: reaching that
arm REQUIRES the credential baseline to have passed.

Five rounds of the same claim resurfacing in a new paraphrase is the actual finding. A grep
catches a sentence; it does not catch an assertion, and this one is duplicated across a design
file, a generated catalog, three bundle concepts, a domain sentinel, a service arm and two test
comments.

## Declined again: the Microsoft `account_id` int64 range gap

Raised in round 11 and again here at higher confidence, with the added point that
`connection_validation_test.go:408` "confirms this gap rather than preventing it". That is what
the test is for, and it says so: the design's `Pattern` plus `MaxLength(19)` cannot express an
int64 range, `design/connection.go:1012-1015` names that as the one residual, and the test pins
BOTH halves so nobody deletes the runtime validator on the grounds that the design already bounds
the length.

Closing it in the create/update path needs a shared non-platform validator — the reviewer's own
fix says so — because `internal/service` does not import `internal/platform/*`. That is a layering
change on a different endpoint from the one this branch built, and the harm meanwhile is bounded
in the direction that matters here: the value stores, and the connection test this branch added
reports the connection as broken instead of healthy. Tracked as its own ticket.
