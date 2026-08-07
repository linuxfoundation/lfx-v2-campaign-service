# 2026-08-06 — Meta: gate ad-set reconciliation on campaign reuse

**Fix** — Resolve remaining Copilot findings on PR #79 (LFXV2-2665).

1. **The ad-set by-name lookup ran even for a campaign this call had just created.**
   `CreateCampaign` reconciles the campaign by name, then reconciled the ad set by name
   unconditionally. When the campaign was created a few lines above, its id was allocated by
   Meta moments earlier — no prior attempt could have parented an ad set to an id that did not
   exist yet, so the GET could only ever return empty. It was still a live network call that
   could fail, and a transient failure there abandoned a freshly created campaign as an orphan
   for no reconciliation benefit. The lookup is now gated on `existingCampaignID != ""`.
   Pinned by `TestCreateCampaignSkipsAdSetLookupForFreshCampaign`; verified binding — removing
   the gate fails it on the lookup count.

2. **Missing coverage for the reuse-an-existing-ad-set branch.** The only reuse test drove
   "existing campaign + no ad set yet". `TestCreateCampaignReusesExistingAdSetByName` now
   covers "existing campaign + existing ad set": the pre-existing ad set id is adopted and the
   ad set POST is never issued.

3. **`internal-platform-meta.md` misstated how the two names are composed.** It described both
   as composed from event name, region, objective and project. Only the campaign name is —
   the ad-set name is `"<EventName> - <objective label>"`, disambiguated by the campaign it is
   queried under rather than by the name itself. The corrected text also states plainly what
   the name-based key does and does not guarantee: the campaign name is deterministic but NOT
   brief-unique (two briefs sharing event, region, objective and project compose the same
   name), which is why `findCampaignByName` fails CLOSED on more than one match, a non-numeric
   id, a status other than `PAUSED`, or a mismatched objective, and reuses only a single
   PAUSED campaign with the requested objective.

**Known limitation, now documented rather than implied.** Reaching this reconciliation on a
production retry additionally requires the orchestrator to re-invoke the dispatcher on a
RETAINED claim. Today `ClaimCampaignDispatch` hits `ON CONFLICT DO NOTHING` and the
orchestrator returns "reconciliation required" without calling the dispatcher, so the client
path is exercised by in-call reuse and tests. Wiring the retained-claim path is a separate
change and is tracked separately; the concept file now says so instead of leaving the reader
to infer that retries already flow through here.
