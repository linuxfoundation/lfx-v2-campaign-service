# 2026-08-18 — LFXV2-3314 the rules ignored the campaign's run state

**Fix** — `Campaign.Status` carries TWO kinds of value and the rules only understood one of them.

`campaign.go:130-138` says it plainly: a provisioning state stamped at create (`pending` /
`created` / `created_degraded`) and a **run state** set by the status toggle (`active` /
`paused`), sharing one column. `isActive` listed only the provisioning states, which produced
three errors at once:

- **`paused` raised underspending** — telling an operator to fix a campaign they had
  deliberately stopped. Zero spend is the intended outcome of pausing.
- **`pending` raised underspending** — on a campaign that may never have reached the platform,
  so its spend figure is not evidence about pacing.
- **`active` raised nothing at all** — the one status where delivery is unambiguously expected.

The pacing rules were additionally never gated on status in any form; only `zero_delivery` was.

**A related gap in the same pass.** `zero_delivery` still fired on a window that PRECEDES the
flight. The overlap fix had been applied to pacing and not to the delivery rule, so `last_month`
for a campaign that started this month reported a correct zero — the campaign did not exist yet
— as a campaign that failed to start. Rule evaluation is now gated on
`windowOverlapDays > 0` for the same reason pacing already refused it.

**I rejected the run-state finding the first time it was raised, and I was wrong.** I searched
for `CampaignStatus*` constants, found no `paused`, and reported the finding as false. The
constants are named `CampaignRunActive` / `CampaignRunPaused` and sit eleven lines above the
ones I read. A grep for the VALUE (`"paused"`) rather than the expected symbol NAME would have
found them immediately — the same lesson as [symbol-name-is-a-claim], inverted: I let an assumed
naming convention stand in for the vocabulary itself.

**And an ordering bug I introduced while fixing it.** `pacingFor` reads `windowOverlapDays`, so
deriving the overlap after calling it handed pacing a zero meaning "not yet calculated" rather
than "no overlap", making every row incomputable. Two tests caught it immediately.
