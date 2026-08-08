# 2026-08-07 — Google Ads campaign lookup, and the first free text to reach a GAQL WHERE clause

**Update** — `Client.FindCampaignByName` closes the Google Ads half of the campaign-lookup
gap. linkedin (`findMatch`), twitter, microsoft and meta all grew a find-by-name; googleads
and reddit had none (**Reddit is still open**). It is also the first such lookup that is
**exported** — the others are called only from inside their own client's create path.

## Why the contract is shaped the way it is

The other clients grew a find-by-name to make CREATE idempotent. This one serves that too, but
exists now for **adoption**: binding a brief to a campaign that already exists on the platform
and that this service never created (goal point 2). Both uses share one fail-closed rule:
exactly one live match → `(id, nil)`; no live match → `("", nil)`; more than one, or anything
unverifiable → `("", error)`. Absence and ambiguity must be different values because **both
callers act destructively on an absence** — create reads `("", nil)` as licence to create,
adoption as nothing to adopt. A false absence buys a duplicate paid campaign; an arbitrary
pick among same-name campaigns binds a brief to the wrong one. Google Ads permits duplicate
names within an account, so the ambiguous branch is reachable rather than defensive.

## GAQL had never seen free text

Every prior GAQL query here interpolates a digits-only id (`customerIDRE`) or an allow-listed
constant (`validMetricsWindows`). Neither can carry a quote, so the package had **no
string-literal escaper at all** — and its absence looked like a style choice, not a gap. A
campaign name is the first caller-controlled string to reach a `WHERE` clause, and it cannot
be allow-listed: without escaping, `x' OR campaign.id > '0` closes the literal and the client
sends a clause matching **every campaign in the account**. `gaqlStringLiteral` escapes
backslash FIRST then the quote; reversing that re-escapes the backslash the quote escape just
introduced and releases the quote, pinned by a `backslash then quote` case.

**An allow-list is not an escaping strategy, it is a way of avoiding the need for one — and the
first value that cannot be allow-listed is where the missing escaper becomes a vulnerability.**
Grep how a query's other operands are constrained before assuming a builder is safe.

## The client-side re-check is deliberate duplication — and a skip would have undone it

The name filter and the `REMOVED` exclusion are both in the `WHERE` clause and both re-checked
over the returned rows: if the escaping ever regressed, the injected query still returns **2xx
with rows** — for other campaigns — and a server-side-only filter hands one back as an exact
match. The first cut *skipped* a non-matching row, a mechanism that looked like
defence-in-depth while its disposition quietly defeated it: **a skip that reduces an
unverifiable response to a clean absence is a false-absence bug.** Dropping every injected row
leaves zero matches — `("", nil)`, the licence-to-create value — converting the regression into
the outcome it was detected to prevent. A name mismatch is now an **error**.

Status is the opposite case, and the asymmetry is the point. `REMOVED` **is** a per-row skip,
because a tombstone is unadoptable however it arrived. Every *other* status errors: Google can
answer `UNSPECIFIED` or `UNKNOWN`, an omitted field decodes to `""`, and treating an
unrecognised status as live returns a campaign whose serving state was never established.

**Over-broad rejection is a false absence too.** The first cut rejected every
`unicode.IsControl` rune plus U+2028/U+2029 (Zl/Zp, which `IsControl` misses). Google Ads
prohibits only **NUL, LF and CR** in `Campaign.name`; TAB, the line separators and zero-width
joiners are legal, and adoption targets campaigns this service never sanitised. **Reject only
what the upstream field itself cannot hold** — rejection then costs no reachable lookup, and
the rest is safe since the query rides in a JSON body that `encoding/json` escapes.

## Identity evidence is checked whenever it is present, not only as a fallback

When `campaign.id` is absent the id is recovered from the resource name, validated against the
full documented shape rather than the lenient `resourceID` helper (see the concept file).
Review caught the sharper version: validating only in the *fallback* makes the check reachable
exactly when the row is least suspicious. Both fields are selected, so both are evidence of
what the row IS — **a lenient parser reused as identity evidence** is the general trap.

## A JSON `null` is not an empty result set — at either level

A top-level `null` unmarshals into `searchResponse` **without error** and leaves it zero
valued — nil rows, no page token, indistinguishable from a genuine empty page. For a caller
that reads absence as licence to create, an unverifiable response became a duplicate paid
campaign. Google's real empty page is `{}` or `{"results":[]}`, both well-formed objects, so
rejecting the null costs nothing legitimate.

Review found the same shape one level in: `{"results":null}` decodes to the identical nil row
set, and the absence test's fixture — `Encode(searchResponse{Results: nil})` — emitted exactly
that, so the test asserting a clean absence was asserting on a page Google never sends. `results`
now decodes through a `searchRows` type that rejects an explicit null; an **omitted** key is
still legal, because `Unmarshaler` is not called for it and `{}` is Google's own empty page.
**A fixture that encodes the zero value of the response struct is not the same as the wire
shape the server produces — and a guard applied to the outer document has an inner twin.**

## Trimming quietly redefined the contract

The lookup originally ran `strings.TrimSpace(name)`, justified by the create path, where
`composeName` already trims — which is exactly why the trim was pointless *there* and not
pointless for **adoption**, the caller supplying a name this service did not compose. Adopting
`"  foo  "` would return the campaign named `"foo"`: a different campaign, from a method whose
contract is an exact-name match, with the ambiguity hidden rather than reported if both
existed. The name is now used verbatim; `TrimSpace` only *detects* whitespace-only input.
**General form: a normalisation applied for caller A's convenience is a silent contract change
for caller B.**
