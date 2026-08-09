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

The helper's one sharp edge is that it decides `live` WITHOUT deciding which campaign the row
is, so "skip a tombstone" and "check the filter" must be ordered by the caller — and the first
version of the by-id path ordered them wrong. Both review bots caught it independently. With
the id check below the skip, a `REMOVED` row for a DIFFERENT campaign left through the
`continue` untested, and a response made only of such rows returned `(nil, nil)`: a response
that honoured NEITHER predicate (the query names one id *and* excludes `REMOVED`) reported as
the trustworthy absence a caller acts on by creating a second campaign against the same budget.
The by-name path never had the hole because it checks its name filter on the raw row before
calling the helper. Rather than bolt a second raw-field check onto the by-id path, the fix went
into the helper: identity is now established BEFORE status, so `campaignRowIdentity` returns an
id for a tombstone too and the caller can check the filter on EVERY row instead of only the
live ones. Position, not presence, was the whole defect — a guard placed under a `continue` is
not a guard.

Reordering also settled a second finding from the same review. The claim the tombstone skip
rests on — "a tombstone is unadoptable however it arrived, so dropping it can only ever be
correct" — is true only once we know WHICH campaign the tombstone is for. Deciding status first
granted that premise without earning it: a `REMOVED` row with a cross-customer resource name,
with identity fields that disagree, or with no usable id at all returned a clean not-live
verdict, and the by-id caller reported a campaign it had never identified as absent. A row must
now say who it is before its status is allowed to mean anything.

The third finding is the mirror image: `GetCampaign` skipped tombstones before the
duplicate-details check, so a response carrying campaign 555 once as `ENABLED` and once as
`REMOVED` returned the live ref. One campaign cannot be both, so such a response has
contradicted itself and none of it is trustworthy — least of all the live row, the one a caller
would bind real spend to. It is checked after the loop, not on sight, because the rows arrive
in no guaranteed order and a leading live row must not buy trust for what follows it. Tombstones
ALONE stay an absence: that is the campaign asked about, reported unadoptable, which is what a
caller needs to hear.

## A campaign nobody can name cannot be confirmed

The same review pass found the name unchecked: a live row with an omitted or whitespace-only
`campaign.name` was returned as a successful `CampaignRef`. The name is not decoration here —
verify-before-bind means an operator reads it to confirm the id resolves to the campaign they
meant, so a ref without one asks for a confirmation that cannot be given. `Campaign.name` is
required and populated for every campaign, so an empty one in a response that `SELECT`ed it is
a truncated answer rather than a nameless campaign, and it now errors. This is the cheap side
of the asymmetry: the error costs a retry, whereas returning the ref binds real spend to a
campaign nobody could identify.

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
