# 2026-08-08 — Adopting an existing Google Ads campaign instead of creating a second one

**Update** — `GoogleAdsDispatcher.Dispatch` can now call `FindCampaignByName` before creating,
and adopt a campaign already carrying the deterministic composed name instead of creating a
second one. It is **opt-in**, per `googleAdsConfig.AdoptExisting`; only a verified absence
licenses the create that follows.

## Why opt-in, and what the default path actually does

`ComposeName` is deterministic in the brief, so a retried dispatch asks for the same campaign
name a previous attempt may already have created. With adoption off — the default — that retry
does not create a second live campaign: Google enforces name uniqueness among non-removed
records, so the second create is rejected and the dispatch fails. The retry is BLOCKED, not
silently duplicated. That is the failure this setting converts into a success, and it is why the
setting is worth having.

Which rejection arrives depends on how far the retry gets, and the usual answer is the first
one. The client creates the BUDGET before the campaign (their names differ — `LFX | Budget …`
vs the campaign name — so they collide independently), so a retry normally stops at
`CampaignBudgetError.DUPLICATE_NAME`; `CampaignError.DUPLICATE_CAMPAIGN_NAME` is reached only
when the budget mutate does not collide. Both families are handled and tested
(`isDuplicateBudgetNameErr` / `isDuplicateCampaignNameErr`) — see the section below, which spells
out why the two are easy to confuse.

It is not the default because the guarantee has a hole with teeth. Uniqueness covers non-removed
campaigns only, so once a campaign is REMOVED the name is free again — and an unconditional
lookup in the dispatcher would rebind a brief to whatever now carries that name, which is the
soft-delete rebind defect. Leaving adoption to an explicit request keeps the decision with the
caller who knows which campaign they mean, rather than with a name that is only conditionally
unique.

The lookup's fail-closed contract is what makes this safe to act on: `FindCampaignByName`
errors on anything it cannot verify (a transport failure, a row whose name does not match the
`WHERE` clause, a campaign in another customer), so an absence returned here is a *verified*
absence rather than a failure that looks like one. Acting destructively on `("", nil)` is only
defensible because that value cannot be produced by a lookup that merely failed.

## Two things adoption must not quietly change

**Input validation cannot depend on remote state.** Adoption returns before
`CreateCampaign` runs, and `CreateCampaign` is where the input is validated. So a NaN budget,
an over-long name or a malformed registration URL would have failed cleanly on a first
dispatch and *silently succeeded* on the retry that found a campaign to adopt — the same
request, accepted or rejected according to what happened to be sitting in the ad account.
`Client.ValidateCampaignInput` now runs the same non-mutating preflight before the lookup. It
delegates to the same `preflightCampaign` helper `CreateCampaign` uses; a second copy of the
checks would pass review once and drift on the next change to either.

**The persisted row must still record the request.** `campaignFromGoogleAdsAdoption`
initially omitted `applyCampaignConfig`, so adopted rows persisted with NULL
`budget_amount`, `budget_type` and `config_snapshot`. Those columns hold the CALLER-SUPPLIED
config for the dispatch — what was asked for — not a readback of platform state; the create
path stamps them from the same `cfg` before anything is read back. Leaving them NULL did not
express "unknown", it lost the request, and made adopted rows disagree in shape with every
sibling adapter's for no recoverable reason.

What adoption does not do is push that config upstream: the campaign keeps whatever budget
and settings it was created with, and this path creates no budget and no ad group. The row
records the request; the platform keeps its state; the two can legitimately disagree and
nothing in this service reconciles them. That last part is worth stating plainly rather than
gesturing at a metrics read, which was the first draft of this paragraph and was wrong:
`ReadMetrics` returns impressions, clicks, cost and CTR — performance, not configuration — so
no amount of reading it tells you the upstream budget. Closing the gap needs a settings
readback this service does not have, tracked as LFXV2-3067.

## Follow-on (review round 1) — adoption had to become opt-in

The first cut ran the lookup on EVERY dispatch. That is correct for the case it was written
for (a retry after an ambiguous create) and wrong for the case it actually reaches most
often.

`ComposeName` is deterministic in Project/EventName/NameSuffix and is **unchanged by a soft
delete of the local campaign row**, while `getCampaignByPlatformQuery` excludes deleted rows.
So after a documented delete the orchestrator reads the pair as "never dispatched"
(`campaign_repo.go`), takes the fresh-claim path (`orchestrator.go`), and calls `Dispatch`
again. The unconditional lookup then found the campaign the delete had walked away from —
still live, still spending — adopted it, and persisted the NEW request's budget and config
against it while pushing nothing upstream. The caller was told a campaign was provisioned.
Nothing anywhere surfaced the rebind.

Worse, the retry case adoption was written for is largely unreachable: a dispatch that dies
mid-flow leaves a RETAINED partial claim, which the orchestrator reports as "reconciliation
required" without calling `Dispatch` again at all. So the path that reached adoption in
practice was the one where it was wrong.

`googleAdsConfig.adoptExisting` (default `false`) is the fix. Adoption is now a deliberate
act — which is what binding an already-existing campaign to a brief always was — and with the
flag off a post-delete re-dispatch goes down the create path, where the name collision
surfaces as a retained partial requiring reconciliation. Visible beats silent.

Naming the code precisely, because the two are easy to confuse and the client carries the same
warning: the budget mutate is rejected with `CampaignBudgetError.DUPLICATE_NAME` and the
campaign mutate with `CampaignError.DUPLICATE_CAMPAIGN_NAME`. Both exist in v23 and both are
already handled and tested on `main` (`isDuplicateBudgetNameErr` / `isDuplicateCampaignNameErr`,
`campaign.go`). Google enforces uniqueness among non-removed campaigns only, which is the same
fact the by-id lookup relies on when it says a name is not unique across `REMOVED` campaigns —
so a re-dispatch after an UPSTREAM delete legitimately creates, while one after a LOCAL-only
soft delete collides, which is exactly the case that must not silently rebind.

`TestGoogleAds_Adoption_IsOptInAndOffByDefault` binds it: the fake API `t.Errorf`s if the
search endpoint is touched at all, and still answers with the live campaign, so a regression
fails on the call AND on the adopted id.

### And an adopted row is `created_degraded`

It was `created`. It is not a clean create: no budget, no ad group and no ad were made, and
this request's config was never applied upstream — the campaign may be serving under settings
nobody in this system chose. `twitter.go` already stamps `created_degraded` for exactly this
shape (its `Reused` case). Both statuses are terminal to `isReusableCampaign`, so this does
not invite a re-dispatch; it makes the row say what happened, which is all an operator
reconciling against the platform has to go on.

## Note on the test

`TestGoogleAds_Adoption_FindsAndAdoptsExistingCampaign` uses `atomic.Bool` for its
handler-side call flags. The completed HTTP response does establish a happens-before, so
`-race` is clean with plain bools too — but that argument has to be reconstructed by every
reader, and the next test to copy the shape may not preserve the property it depends on.

## Two review findings that were both about accuracy, not behaviour

The lookup's contract comment said adoption reads `("", nil)` as "licence to report nothing to
adopt". That is the future by-name adopt ENDPOINT, not this caller: adopt-on-create falls
through to `CreateCampaign`, so a false absence here produces a second paid campaign next to the
one already running — the exact outcome adoption exists to prevent. The fail-closed rule is the
same either way; the cost is not, and the comment named the wrong one.

The second was subtler and is worth recording as a class. Extracting `campaignPreflight` put a
type declaration between `CreateCampaign`'s doc block and `CreateCampaign`, and Go binds a doc
comment to whatever declaration FOLLOWS it — so the entire create-cascade contract (partial
results, the UNCONFIRMED classification, which mutate leaves what committed) silently became
`campaignPreflight`'s documentation and `go doc Client.CreateCampaign` printed nothing. No build
error, no vet warning, and a diff that reads as a pure addition. `TestCreateCampaignKeepsItsDocComment`
now asserts the method has a doc that names it and still carries the contract's three load-bearing
words. It is scoped to that one method deliberately: the package-wide version of the rule fires on
struct-field comments and comments inside function bodies, and a test that fails on things nobody
intends to change gets turned off rather than obeyed.

## The campaign that most needed stopping was the one we could not stop

A third finding was about behaviour, and it only exists because of this change.

An adopted campaign is stamped `created_degraded`, deliberately: the campaign exists upstream,
but no budget, ad group or ad was created and this request's config was never pushed to it, so
`created` would assert a wiring this path never does. Separately, `ToggleCampaignStatus`
refused every status toggle on a non-toggleable status, `created_degraded` among them, for two
stated reasons — it would activate an incomplete campaign, and it would overwrite the
reconciliation marker with the run state.

Put together, they produce the wrong outcome for exactly one input. `FindCampaignByName` treats
`ENABLED` and `PAUSED` alike as live, so the campaign this dispatch adopts may already be
serving and spending. It is now bound to a brief, visible in the product, and permanently
unpausable through the service — while `GoogleAdsDispatcher.ToggleStatus` explicitly supports
pausing a campaign with no child ids, precisely so a shell can be stopped. The guard meant to
protect an incomplete campaign was withholding the brake from a campaign that was running.

The remedy is direction-aware, not status-aware. ACTIVATE stays refused for every provisioning
status. PAUSE is allowed for `created_degraded` alone, because that status is the only one that
means *the campaign definitely exists upstream* — `pending`, `group_created` and `unconfirmed`
may have no campaign at all, so a pause would be a mutation against an id whose meaning is
unknown, and they stay refused in both directions.

The second stated reason survives intact rather than being traded away: pausing **reconciles
nothing**, so writing `paused` over `created_degraded` would erase the only record that the
wiring is unverified. The pause path therefore does not persist at all — no version bump, no
index event — and returns the campaign at its unchanged status, which is what the row now says.
The platform call is declarative, so a repeat pause is a no-op upstream and idempotence does not
depend on a stored run state.

`CampaignStatusToggleable` was left direction-blind and still returns false for
`created_degraded`; the exception lives at the call site. A `toggleable(status, direction)`
signature would have read better at this one call and invited every other caller — none of which
asks a directional question — to pass a direction and silently acquire the exception.

Four sub-tests bind it: activating a degraded campaign is still a 409 whose message names the
pause that remains available, pausing one reaches the platform and leaves the row alone, and
`pending`/`group_created`/`unconfirmed` are refused a pause as well.
