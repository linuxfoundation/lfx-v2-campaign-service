# 2026-08-07 — Google Ads campaign lookup, and the first free text to reach a GAQL WHERE clause

**Update** — `Client.FindCampaignByName` closes the last platform gap in campaign
lookup: linkedin, twitter, microsoft and meta all had one; googleads and reddit had
none, and googleads is the lead platform for LFXV2-2023.

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

## The client-side re-check is deliberate duplication

The name filter and the `REMOVED` exclusion are both in the `WHERE` clause and both
re-checked over the returned rows. That is not belt-and-braces. If the escaping ever
regressed, the injected query still returns **2xx with rows** — for other campaigns — and
a server-side-only filter would hand one back as an exact match. The client-side equality
check converts that from a silent wrong binding into a visible zero-match or ambiguity
error, which is the failure mode that can be noticed.

Rows are deduplicated by id before the count, so the same campaign arriving on several
rows is not misreported as ambiguous and does not block a legitimate adoption.
