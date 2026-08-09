# 2026-08-08 — Verify before bind: looking a Google Ads campaign up by id

**Update** — `Client.GetCampaign` adds the by-id half of campaign adoption, and
`campaignRowIdentity` extracts the row-level identity checks so both lookups share one answer
to "which campaign is this row, and is it adoptable".

## Why by-id at all, when find-by-name already exists

Adoption binds a brief to a campaign this service never created. By name that works when the
operator knows the exact name, but the name is not the platform's identifier: it is mutable,
it is not unique across `REMOVED` campaigns, and an operator reading a campaign id out of the
Google Ads UI has the identifier already. The user's decision was **verify before binding** —
so the id has to be resolved against the platform, in this account, and the campaign's name
and status shown back, before anything is written. That is what `CampaignRef` carries: an id
alone is not something a human can confirm.

## Why the checks moved into a helper

There are now two ways to reach a campaign, and they must agree about what counts as a
trustworthy row. Duplicating the status allow-list, the resource-name shape check and the
identity-fields-agree check would let them drift — and the direction of drift matters: the
lenient one would be the by-id path, where the caller is further along, has already decided
which campaign it wants, and is about to attach real spend to it.

`campaignRowIdentity(row, describe)` returns `(id, live, err)`. `live` is false for `REMOVED`
alone. That asymmetry is the same one the by-name lookup documents: a tombstone is unadoptable
however it arrived, so dropping it can only ever be correct, while any other unrecognised
status — `UNSPECIFIED`, `UNKNOWN`, or the empty string an omitted proto field decodes to —
must error. Treating one as live returns a campaign whose serving state was never established;
treating one as a skip reduces an unverifiable response to a clean absence, which is the
licence-to-create value.

## The two by-id-specific guards

**The caller's id is validated before interpolation, as an identity.** `canonicalCampaignID`
already existed for the ids Google returns; here it also gates the id a caller supplies. It
refuses `"0"`, a value past `math.MaxInt64` and `"007"` — all digits, none of them a campaign
this client can adopt. `"007"` is the instructive one: it matches campaign 7 server-side, so
querying it would surface as a filter-not-honoured conflict, reporting confusion where the
real fault is a malformed request. The injection case is worth stating plainly: with the guard
removed, `GetCampaign("555 OR campaign.id > 0")` emits
`WHERE campaign.id = 555 OR campaign.id > 0`, which is every campaign in the account. There is
no escaper here and there does not need to be — `campaign.id` is an int64 field, compared
unquoted, and the only legal operand is digits.

**The id filter is re-checked client-side, and a mismatch errors on the whole response.** Same
disposition rule as the name filter: a row for a different campaign proves the `WHERE` clause
was not applied, so nothing in the response is trustworthy. Skipping the row instead would
leave zero matches — `(nil, nil)`, the clean absence a caller is entitled to trust.

Duplicate rows for one campaign are tolerated, as by name, but they must agree on the **name**
as well as the id, because the name is the field an operator reads before confirming.

## What this deliberately does not answer

Whether the campaign is already bound to another brief. That is this service's own state, and
answering it here would be answering a database question with an ad-platform call. It belongs
to the adopt endpoint that consumes this.

## Still not wired

As with `FindCampaignByName`, no production caller exists yet. The Goa `adopt-campaign`
endpoint that binds a verified `platform_campaign_id` to a brief is the follow-up.
