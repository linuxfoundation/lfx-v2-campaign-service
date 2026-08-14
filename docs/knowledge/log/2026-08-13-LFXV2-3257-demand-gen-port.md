# 2026-08-13 — LFXV2-3257 demand gen port

**Creation** — Ported Google Demand Gen campaign creation from the legacy
Express path (`lfx-self-serve` `campaign-proxy.service.ts`'s
`createDemandGenCampaign`), which is what serves this channel on app.lfx.dev
today and is the behavioural reference. Not a from-scratch feature: the
reference is ~75 lines because Demand Gen creates no ads.

`CreateDemandGenCampaign` is a SEPARATE method rather than a widened
`CreateCampaign`. The channels disagree on required fields — `campaignCreate`
always sends `networkSettings` and `manualCpc`, and Demand Gen accepts neither
(it bids with `targetSpend` and has no Search network) — so a shared payload
would either send fields the API rejects or make every Search create carry
optional ones. It also leaves the Search path, the one verified end to end
against a real account, provably untouched.

Creates budget → campaign (DEMAND_GEN, targetSpend, PAUSED) → ad group, and
deliberately NO ad and NO keywords: Demand Gen creatives are image/video assets
a human uploads in the Google Ads UI, so a generated text ad would be one the
channel cannot serve. The legacy path ends the same way.

The partial-result contract matches `CreateCampaign` and is the part the legacy
TS lacks: every step that may have committed returns a NON-NIL result carrying
what is known, so an orphan budget stays reconcilable instead of being reported
as nothing-created.

`googleAdsConfig` gains `channel`. Absent or `"search"` keeps the existing
behaviour — every caller predating the field omits it and means Search — and
`"demand-gen"` selects the new path. An unrecognised value is REFUSED before any
upstream call rather than defaulted: defaulting a typo'd `"demandgen"` to Search
would spend the Demand Gen budget on search ads and report success.

**Note** — Geo is NOT ported. The legacy version attaches location criteria at
AD-GROUP level (Demand Gen rejects campaign-level ones) using its own
`GEO_TARGET_MAP`, but this client has no geo support at all: `CampaignInput`
carries no `GeoTargets` and the Search path sets no criteria either. Adding a
Demand-Gen-only geo path would mean inventing a geo-constant mapping the rest of
the client does not have, and giving one channel targeting the other lacks.
Parity on geo is its own change, applying to both channels. Until then a created
campaign is geo-untargeted and the closing step says so, so the absence is not
read as targeting that was applied.
