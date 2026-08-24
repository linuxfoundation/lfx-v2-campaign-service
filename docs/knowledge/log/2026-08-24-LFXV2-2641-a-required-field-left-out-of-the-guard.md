# 2026-08-24 — a fail-closed guard that skips one of the three Required ids

**Google Ads keywords** — `GetKeywordPerformance` rejects a GAQL row whose criterion id or ad
group id is missing, on the stated reasoning that an absent id means the SELECT and the decode
struct have drifted apart. `campaign.id` is selected by the same query and is listed in
`Required("criterion_id", "ad_group_id", "campaign_id", ...)` on the `google-ads-keyword`
design type, but it was not in the guard. A row missing it was returned as `campaign_id: ""`.

## Why the omission is not cosmetic

The read's entire scope is "the project's OWN campaigns" — it takes a campaign id list and
builds `campaign.id IN (...)`. A row that comes back unable to say which campaign it belongs to
is exactly the drift the guard exists to catch, and it is the one field a caller needs to
associate the keyword with anything. Emitting `""` also contradicts the generated contract:
`Required` means the client validator expects a value, so the service promises a field it did
not observe.

**The three ids were held to two different standards for no stated reason.** When a guard
enumerates fields, the enumeration is a claim about which ones matter — check it against the
`Required(...)` list rather than against the neighbouring code, because the neighbouring code is
what the omission was copied from.

## The boundary needs its own test

`TrimSpace` is what the sibling checks use, so `"   "` must be rejected too. Two tests, not one:

- absent `campaign.id` → error
- whitespace-only `campaign.id` → error

A mutation dropping the whole clause is killed by both; a mutation that keeps the clause but
drops `TrimSpace` is killed **only by the second**. Without it, a fix that compares
`row.Campaign.ID == ""` looks fully verified while a whitespace id still reaches the caller.
**One test per exit path, not one per finding.**
