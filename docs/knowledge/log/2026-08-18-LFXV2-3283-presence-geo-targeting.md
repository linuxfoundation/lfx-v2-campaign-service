# 2026-08-18 — LFXV2-3283 location criteria alone did not restrict delivery

**Fix** — the geo work attached the criteria and left Google's permissive default in place, so
the campaigns it "targeted" still served worldwide.

`positiveGeoTargetType` defaults to `PRESENCE_OR_INTEREST`: a user anywhere on earth who merely
shows INTEREST in the targeted country stays eligible. A campaign targeted at the US therefore
spent its budget globally — the exact out-of-region spend this ticket exists to prevent. The
criteria were attached, the ids came back, and the budget still leaked.

`geoTargetTypeSetting.positiveGeoTargetType` is now `PRESENCE` on both channel creates.

**Both payloads needed it separately.** `campaignCreate` and `demandGenCampaignCreate` are
deliberately different structs — Demand Gen rejects `networkSettings` and `manualCpc` — so a fix
on one says nothing about the other. Mutation-verified per channel: dropping the setting from
either fails exactly one test, and each channel has its own case.

Demand Gen attaches its geo criteria at the AD GROUP level, which made the placement worth
checking rather than assuming: `geoTargetTypeSetting` is a CAMPAIGN-level field in the Google Ads
API and governs how location criteria are interpreted for the whole campaign, ad-group criteria
included. It belongs on the campaign create for both channels.

**Set unconditionally**, not only when geo targets are supplied. An untargeted campaign is
unaffected — there are no criteria for the setting to qualify — and a campaign that gains
criteria later, by adoption or by a human adding them in the Google Ads UI, then restricts by
presence instead of silently reverting to the permissive default.

**This is the same family as the `targetingSetting` note already in `campaign.go`**: Google's
default is the looser reading, so silence is a choice rather than an absence of one.

**Found by copilot on #139.**
