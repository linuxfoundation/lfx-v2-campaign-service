# 2026-08-18 — LFXV2-3314 a campaign with no start date is not paced

**Fix** — `ComputePacing` returned a confident measurement for a campaign with a budget, an end
date, and **no start date**: 500% overspending for a campaign that was exactly on plan.

`start_date` is nullable (migration 000002 declares `start_date DATE` with no `NOT NULL`) and
`model.Campaign.StartDate` is a pointer, so this is a storable production state rather than a
hypothetical. An absent start defaulted to `now`, which makes the flight begin at this instant;
`daysBetween` then floors the elapsed period at one day, so a 30-day window of spend was compared
against a single day of plan. The result was a `budget_constrained` action item raised against a
healthy campaign.

**This is the future-dated-flight defect arriving through the other door.** The `now.Before(start)`
guard added earlier in this ticket cannot catch it, because `start` was just set TO `now` — the
comparison is against itself. An absent start is only safe when the end is also absent, which the
zero-length-flight check already handles.

The lesson generalises past this field: a nil-defaulted input can defeat a guard written against
the value it defaults to. The guard and the default have to be considered together.
