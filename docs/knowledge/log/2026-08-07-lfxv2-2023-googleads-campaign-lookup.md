# 2026-08-07 — Google Ads campaign lookup, and the first free text to reach a GAQL WHERE clause

**Update** — `Client.FindCampaignByName` closes the Google Ads half of the remaining
campaign-lookup gap. linkedin (`findMatch`), twitter, microsoft and meta all grew a
find-by-name; googleads and reddit had none. **Reddit is still open** — googleads goes
first because it is the lead platform for LFXV2-2023.

It is also the first lookup that is **exported**. The others are called only from
inside their own client's create path; this one is called from the dispatch layer for
adoption too, which lives in a different package.

## Why now, and why the contract is shaped the way it is

The other clients grew a find-by-name to make CREATE idempotent — don't double-create
when a retry lands after the first attempt already committed. This one serves that too,
but the reason it exists now is **adoption**: binding a brief to a campaign that already
exists on the platform and that this service never created. That is goal point 2, and
until now nothing in the service could do it for Google Ads.

Both uses make the same decision from the result, so they share one method and one
fail-closed rule:

- exactly one live match → `(id, nil)`
- no live match → `("", nil)`
- more than one → `("", error)`
- anything unverifiable → `("", error)`

Absence and ambiguity must be different values because **both callers act destructively
on an absence**. The create path reads `("", nil)` as licence to create; the adoption
path reads it as licence to report nothing to adopt. A false absence therefore buys a
duplicate paid campaign, and an arbitrary pick among same-name campaigns binds a brief
to the wrong one. Google Ads permits duplicate campaign names within an account, so the
ambiguous branch is reachable rather than defensive.

## The real cost of this change: GAQL had never seen free text

Every prior GAQL query in this package interpolates a digits-only id (`customerIDRE`) or
an allow-listed constant (`validMetricsWindows` in `metrics.go`). Neither can carry a
quote, so the package had **no string-literal escaper at all** — and its absence looked
like a style choice rather than a gap.

A campaign name is the first caller-controlled string to reach a `WHERE` clause, and it
cannot be allow-listed: an operator may name a campaign anything Google accepts. Without
escaping, `x' OR campaign.id > '0` closes the literal and the remainder becomes query
syntax. The revert-check makes the consequence concrete — with escaping removed, the
client sends:

```
... WHERE campaign.name = 'x' OR campaign.id > '0' AND campaign.status != 'REMOVED'
```

which matches **every campaign in the account**, and the caller then binds a brief to
whichever came back first.

`gaqlStringLiteral` escapes backslash FIRST and then the quote. Reversing that order
re-escapes the backslash the quote escape just introduced and releases the quote — the
classic ordering trap, pinned by a `backslash then quote` case. Control characters are
rejected rather than escaped: GAQL has no portable escape for them, Google Ads forbids
them in a name (`sanitizeNamePart` strips them on the create path for the same reason),
and such a name therefore cannot match a real campaign, so rejecting costs no reachable
lookup.

**The general lesson: an allow-list is not an escaping strategy, it is a way of avoiding
the need for one — and the first value that cannot be allow-listed is where the missing
escaper becomes a vulnerability.** Grep for how a query's other operands are constrained
before assuming a package's query builder is safe by construction.

## The client-side re-check is deliberate duplication — and a skip would have undone it

The name filter and the `REMOVED` exclusion are both in the `WHERE` clause and both
re-checked over the returned rows. That is not belt-and-braces. If the escaping ever
regressed, the injected query still returns **2xx with rows** — for other campaigns — and
a server-side-only filter would hand one back as an exact match.

The first cut of the re-check *skipped* a non-matching row, and that was wrong in a way
worth recording, because the mechanism looked like defence-in-depth while the disposition
quietly defeated it. **A skip that reduces an unverifiable response to a clean absence is
a false-absence bug.** Dropping every injected row leaves zero matches, which is `("", nil)`
— the licence-to-create value. The escaping regression would have been detected and then
converted into exactly the outcome it was detected to prevent. The original injection test
asserted that clean absence, so it was pinning the unsafe behaviour.

So a name mismatch is now an **error**: the server was asked for an exact match, and a row
that is not one means the filter did not take effect, which invalidates the whole response
rather than that one row.

Status is the opposite case, and the asymmetry is the point. `REMOVED` **is** a per-row
skip, because a tombstone is unadoptable no matter why it arrived — dropping it can only
ever be correct. Every *other* status errors: Google can answer `UNSPECIFIED` or `UNKNOWN`,
an omitted field decodes to `""`, and treating an unrecognised status as live returns the
id of a campaign whose serving state was never established.

The id fallback got the same treatment. When `campaign.id` is absent, the campaign id is
recovered from the resource name — but by validating the full documented shape
(`customers/{this account}/campaigns/{digits}`), not by the package's `resourceID` helper.
`resourceID` returns the trailing path segment, which is right for parsing a mutate
response (an entity we just created) and wrong here: it reads `garbage/4242` as campaign
`4242` and accepts a resource name scoped to a **different customer**. This resource name
is the sole identity evidence for a campaign about to have a brief bound to it.

**Second general lesson: `unicode.IsControl` covers category Cc only.** U+2028 LINE
SEPARATOR and U+2029 PARAGRAPH SEPARATOR are Zl/Zp — `IsControl` returns false for both,
yet they terminate a line to many parsers. They are rejected explicitly. Category Cf
(zero-width joiner, variation selectors) is deliberately *allowed*: it appears in ordinary
emoji sequences and terminates nothing in GAQL, so rejecting it would fail a lookup for a
campaign that genuinely exists — a false absence again.

Rows are deduplicated by id before the count, so the same campaign arriving on several
rows is not misreported as ambiguous and does not block a legitimate adoption.
