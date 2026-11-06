# 2026-11-06 — #518 Demand Gen: a created Audience per ad group

**Update** — each `demandGenAdGroups` entry can now set its own `demographics`. This is the same
object as the campaign-level `demandGenDemographics`, and campaign-service builds that group a
new Audience of its own before the budget. The live KubeCon NA 2026 Demand Gen campaign
(24269004892) has a different Audience on each group. "Video - Retargeting" has six user lists,
and "Video - Prospecting" has age 25–54, three user lists and a custom audience. Until now LFX
could build that shape only from Audiences made in the Google Ads UI.

- Shape: confirmed on #518 before building. One change from the issue's sketch: the
  campaign-level `demandGenDemographics` is not refused beside per-group demographics. A
  group's own `demographics` overrides it for that group, and every other group still gets the
  campaign-level Audience, so today's requests are unchanged.
- One builder: `buildDemographicPlan` serves both levels. It holds the dimensions, the #529 rule
  that leaves out a dimension naming everyone, and the user-list and custom-audience validators.
  A `demographicOwner` makes each refusal name the field the caller set. The NAME_ALREADY_IN_USE
  reuse (`ensureDemandGenDemographicAudience`) is the same code too.
- Names: `<campaign name> | <entry name> | Demographics <hash>`, so each group's Audience has
  its own name and retry key.
- Refusals, all before any write: `demographics` beside that entry's `audience`; `{}`; and a
  campaign-level `demandGenDemographics` when every entry sets its own. The campaign-level field
  beside any entry's `audience` stays refused.
- Gates: the same as the campaign-level field. `GOOGLE_ADS_DEMOGRAPHIC_TARGETING_ENABLED` covers
  any entry `demographics`. `GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED` and `global_marketing_ops`
  cover its `audienceSegments` / `customAudiences`
  (`demandGenAdGroups entry N demographics.audienceSegments`).
- Partials: the Audiences are created campaign-level first, then in entry order. A definite
  failure returns `(nil, err)` only when nothing is in place. Otherwise the partial names every
  Audience so far in `demandGenAdGroupAudiences`, where an UNCONFIRMED entry has its planned name
  only. `CreateOutcome` never reports `budget_name_taken` for such a partial, and `reconcileRef`
  keeps each entry's `demographicAudience` and `demographicAudienceName`.

No new Google request shape: each create is the body #510/#524 sent, and the probes for those
accepted it. A persisted per-group create has not been run live.

Tests: `demandgen_group_demographics_test.go` (the per-group cascade, the override, the
refusals, reuse, the failure partials, CreateOutcome) and the dispatch mapping and gate tests.
They also cover the gated-fields classification (`demandGenAdGroups[].demographics.*`) and the
reconcile key. Each was checked to fail with its fix reverted.
