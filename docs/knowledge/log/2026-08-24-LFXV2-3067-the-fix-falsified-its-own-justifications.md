# 2026-08-24 — a fix falsified the comments that justified the guard it replaced

**Fix** — gating the budget readback on the period (see
[the-period-decides-which-amount](2026-08-24-LFXV2-3067-the-period-decides-which-amount.md))
made **six** comments false in one commit. Every one of them explained a *different* guard by
describing the mechanism that fix removed:

> `googleAdsUpstreamBudgetAmount` reads whichever amount is present WITHOUT consulting the
> period

That sentence was load-bearing in each place — it was the stated *reason* the surrounding
check existed. Once the dispatcher selected by period, each site was justifying a real guard
with a mechanism that no longer existed.

## Where they were

| Site | What it justified |
|---|---|
| `internal/platform/googleads/campaign_settings.go` ×3 | the period-vs-amount cross-check, the absent-period pass, the TrimSpace-for-comparison |
| `docs/knowledge/code/internal-platform-googleads.md` ×3 | "two budget guards, not one", the no-trim rule, the absent/UNKNOWN pass |
| `internal/platform/googleads/campaign_settings_test.go` ×3 | three test rationales |

Nine clauses across three layers, all restating one mechanism.

## The correction is not a reword — the guard's PURPOSE changed

The cross-check was documented as *protecting the dispatcher* from a field-selection bug. With
the dispatcher no longer selecting by presence, that protection is redundant — which invites
deleting the guard. It must not be: it is an **independent response-integrity check** about the
row contradicting **itself**. A `DAILY` row carrying only `total_amount_micros` is malformed at
the source, and reporting a budget from it by *either* field would attribute to the campaign a
number the platform's own invariant says cannot describe it. There is no way to tell which of
the period and the amount is the wrong half, so it is refused rather than reconciled.

Restating it that way makes it independent of how any consumer selects fields — which is what
stops the next change to the dispatcher falsifying it again.

## The finding

**A comment that explains one component by describing another's internals has a dependency
nothing checks.** These were not stale in the ordinary "the code moved on" sense; they were
*consequences of the fix in the same commit*, and the compiler, the tests and `okfvalidate` all
stayed green. Two were caught by review, and a grep for the shared phrase — not for the two
named lines — found the other seven.

The related lesson from
[editing-a-line-adopts-its-claim](2026-08-19-LFXV2-3067-normalise-one-side-default-one-population.md):
the sweep must be for the CLASS. Fixing the two sites a reviewer names leaves the rest, and a
fix commit that cites a real finding is the most convincing form of incomplete work.

## What was deliberately NOT changed

The earlier log fragments
(`2026-08-19-LFXV2-3067-budget-period-amount-cross-check.md`,
`2026-08-20-LFXV2-3067-absent-provenance-and-padded-enum.md`) still contain the old sentence.
They are dated historical records of what was true when written, and rewriting them would
destroy the record this bundle exists to keep. Only claims that describe CURRENT behaviour were
corrected.
