# 2026-08-08 — Adopting a campaign that already exists upstream

**Update** — `POST /projects/{project_id}/briefs/{brief_id}/campaigns/adopt` binds an
existing ad-platform campaign to an approved brief. It is the consumer for the by-id lookup
added in [LFXV2-3054](2026-08-08-lfxv2-3054-googleads-get-campaign.md). The mechanics are in
`docs/knowledge/code/internal-service.md`, `internal-dispatch.md` and
`internal-infrastructure-postgres.md`; this entry records the decisions behind them.

## The gap it closes

Every campaign operation this service offers — metrics, toggle, delete — resolves the campaign
through its stored row. A campaign launched in the Google Ads console before a foundation
onboarded, or during an outage, has no row. The only previous route to a row was `POST
.../campaigns`, which CREATES upstream; using it to register an existing campaign would produce
a second paid campaign beside the first.

## Why not `UpsertCampaign`

Reaching for the existing upsert would have been the small change, and it is the wrong one. Its
`DO UPDATE` arm is correct where it is used: the row it overwrites describes the same campaign
this service is provisioning, which is how a retried dispatch converges. Adoption's caller names
an **arbitrary** upstream campaign, so the same arm silently repoints a live binding, orphaning
the campaign it used to name — still running, still spending, and this service never pauses or
deletes upstream on its own. Freeing a wrong binding is already an explicit operation
(`DELETE`); adoption must not become a second, implicit one.

## Why 404 and 503 are not two flavours of failure

An operator told **"no such campaign"** concludes it is not there and creates one. If the real
situation was "we could not reach Google Ads", they now have two campaigns spending against
one budget. A false absence costs money; a false 503 costs a retry. That asymmetry is why the
fail-closed contract runs through all three layers rather than being a convention at one of
them, and why `TestAdoptCampaign_UnverifiableIsNeverReportedAsAbsent` exists.

## The status field was a real defect, caught before commit

The first version stored `ref.Status` — `"ENABLED"` — into `Campaign.Status`, this service's
LIFECYCLE vocabulary, where both `CampaignStatusDeletable` and
`CampaignStatusNeedsReconciliation` default-DENY an unknown value: the row would have been
undeletable *and* never reconciled. The fix went a level deeper than the assignment —
`model.PlatformCampaignRef` now carries no status at all, because adoptability is the
ADAPTER's decision, in the platform's own vocabulary. It surfaced only because the test
asserted `res.Status == "ENABLED"`, which is what asking "what would break if this were
wrong" buys over "does this pass".

## Round N+1: the arm a copied switch left out

The error switch was written against the metrics and toggle switches and is one arm short:
`domain.ErrSystemConnectionNotUsable` was missing. It is reachable — `LookupCampaign` resolves
through `resolveGoogleAdsClient`, whose deferred `systemScoped` **wraps rather than replaces**
— so `errors.Is` still reported `ErrConnectionNotUsable` and the general arm answered a 409
telling the caller to repair a connection their project does not have. The sentinel exists to
prevent exactly that misdirection, so omitting the arm made the tag decorative.

Three more of the same shape came out of the bot pass, and all three are about what a NEW write
path forgets rather than what it gets wrong. The adopted row did not carry the account it was
verified under, so `googleAdsCreationCustomerID` read "unknown" and the mismatch guards — which
treat unknown as permission to proceed — stopped protecting it. It did not stamp `created_by`
or `updated_by`, so an adopted campaign had no audit trail. And a locally-rejected malformed id
fell through to the 503 default, telling the caller to retry input that can never succeed. Each
is a column or an arm that every SIBLING path already has; none is visible from the new code
alone. The lesson is the same one twice over: diff a new write against the existing writes for
the same table, and a new error switch against the existing switches, field by field.

The generalisable part is WHICH arm went missing. Copying a switch reproduces the arms that fire
in ordinary testing and drops the one for a condition nothing local produces; both remaining arms
returned a `ConflictError`, so the code did not look incomplete. The new test asserts the response
TYPE rather than its message, which is what makes the deleted arm fail rather than fall through.
