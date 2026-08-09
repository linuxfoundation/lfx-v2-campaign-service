# 2026-08-08 — Adopting a campaign that already exists upstream

**Update** — `POST /projects/{project_id}/briefs/{brief_id}/campaigns/adopt` binds an
existing ad-platform campaign to an approved brief. It is the consumer for the by-id lookup
added in [LFXV2-3054](2026-08-08-lfxv2-3054-googleads-get-campaign.md). The mechanics are in
`docs/knowledge/code/internal-service.md`, `internal-dispatch.md` and
`internal-infrastructure-postgres.md`; this entry records the decisions behind them.

## The gap it closes

Every campaign operation this service offers — the metrics read, the status toggle, delete —
resolves the campaign through its stored row. A campaign launched in the Google Ads console
before a foundation onboarded, or during an outage when dispatch was failing, has no row, so
none of them can reach it. The only previous route to a row was `POST .../campaigns`, which
CREATES upstream; using it to register an existing campaign would produce a second paid
campaign next to the first.

## Why not `UpsertCampaign`

Reaching for the existing upsert would have been the small change, and it is the wrong one.
Its `DO UPDATE` arm is correct where it is used, because the row it overwrites describes the
same campaign this service is provisioning — that is how a retried dispatch converges.
Adoption's caller names an **arbitrary** upstream campaign, so the same arm silently repoints
a live binding at a different campaign, orphaning the one it used to name: still running,
still spending, no longer reachable from here, and this service never pauses or deletes
upstream on its own. Freeing a wrong binding is already an explicit operation (`DELETE`);
adoption must not become a second, implicit one.

## Why 404 and 503 are not two flavours of failure

An operator told **"no such campaign"** reasonably concludes it is not there and goes and
creates one. If the real situation was "we could not reach Google Ads", they now have two
campaigns spending against one budget. A false absence costs money; a false 503 costs a
retry. That asymmetry is why the fail-closed contract runs through all three layers rather
than being a convention at one of them, and why
`TestAdoptCampaign_UnverifiableIsNeverReportedAsAbsent` exists.

## The status field was a real defect, caught before commit

The first version stored `ref.Status` — `"ENABLED"` — into `Campaign.Status`, and it looked
right. `Campaign.Status` is this service's LIFECYCLE vocabulary, and both
`CampaignStatusDeletable` and `CampaignStatusNeedsReconciliation` default-DENY an unknown
value, so an adopted row reading `ENABLED` would have been undeletable *and* never
reconciled — outside both predicates rather than covered by one.

The fix went one level deeper than the assignment: `model.PlatformCampaignRef` no longer
carries a status at all. Whether a campaign is adoptable is the ADAPTER's decision, in the
platform's own vocabulary; passing a platform literal up would have obliged the service layer
to learn every platform's dialect to interpret it, and the only place it was consumed was the
one field that must not contain it.

Worth recording how it surfaced: the test written alongside the handler asserted
`res.Status == "ENABLED"` — it pinned the bug. The revert-check discipline caught it, by
asking "what would break if this were wrong" rather than "does this pass".

## Capability, not contract

`CampaignAdopter` is an optional dispatcher interface discovered by type assertion, like
`StatusToggler`, `MetricsReader` and `AccountLister` before it. Keeping it out of
`PlatformDispatcher` means platforms gain adoption one at a time and an unwired platform
answers 400 with no network call. Google Ads is the only implementation today.
