# 2026-11-04 — #461 Performance Max audience signal gated behind a default-off deployment flag

**Update** — `performanceMaxAudienceSignal` names an existing Audience, and the client checks
it against the Google Ads account only. The LF account is shared by every foundation, so a
project could attach another foundation's Audience as its asset group's signal (#468). On
Performance Max a signal is a hint, not a restriction, but the Audience and the customer
lists inside it still belong to another foundation. #459 closed the same gap for
`demandGenAudience` and #462 for the Display segments; #461 now does the same with
`GOOGLE_ADS_PMAX_AUDIENCE_SIGNAL_ENABLED`.

`Dispatch` refuses any non-empty `performanceMaxAudienceSignal`, whitespace included and on
any channel, as not-created and before the budget, unless the flag is exactly `"true"`. It
refuses through `googleAdsInputDisabled`, so the asynchronous create job reports the gate's
sentence. The chart sets the flag to `"false"`; ops enables it per environment with
`app.environment.GOOGLE_ADS_PMAX_AUDIENCE_SIGNAL_ENABLED.value: "true"` in the ArgoCD overlay.
The gate goes away when #468 adds a foundation-scoped check.

Not gated, because none names an existing account resource:

- `performanceMaxSearchThemes` are free text.
- The sitelinks, callouts and structured snippets #461 admits on Performance Max are text and
  the caller's own destination URLs, created as new assets in this campaign.

Tests: `TestGoogleAds_PerformanceMaxAudienceSignalGate` refuses with the flag unset, `""`,
`"false"`, `"TRUE"`, `" true"` and `"1"`, a blank audience, and an audience on Search, and
lets exactly `"true"` through to the image fetch.
`TestGoogleAds_PerformanceMaxAudienceSignalGateLeavesThemesAlone` and
`TestGoogleAds_PerformanceMaxExtensionsReachTheCampaign` run with the flag unset. Removing
the gate fails all eight refusal cases. A case-insensitive comparison fails `"TRUE"`, a
trimmed one fails `" true"`, gating on the trimmed value fails the blank case, gating only on
`performance-max` fails the Search case, and gating themes or extensions too fails the two
ungated tests.
