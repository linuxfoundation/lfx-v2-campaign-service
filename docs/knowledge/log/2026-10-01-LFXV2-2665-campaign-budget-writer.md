# 2026-10-01 — LFXV2-2665: campaign budget write (B1, Google Ads first slice)

**Creation** — a new optional dispatcher capability, `BudgetWriter`, an orchestrator entry point,
a `PATCH .../campaigns/{campaignId}/budget` method, and the Google Ads implementation behind it.
This is the first slice: one platform, one field pair, the whole refusal surface.

## Why the budget is not just another campaign field

On Google Ads the budget is a **separate resource** (`campaign_budget`), not a column on the
campaign. Three facts live only there and nowhere on the campaign row this service stores: the id
of the budget resource the campaign is attached to, whether that budget is `explicitly_shared`,
and its `period`. So the write cannot be a campaign mutate at all — it addresses the budget, which
is why `campaign_budget.id` joined `settingsQueryFields` and `BudgetID` joined `CampaignSettings`.

The pacing consequence is the one worth remembering: `amount_micros` (DAILY) and
`total_amount_micros` (CUSTOM_PERIOD) are **different fields**, mutually exclusive. A caller asking
for a lifetime amount on a daily budget is not a unit conversion this service can perform — it
would have to change the budget's period, which changes delivery for every campaign attached to
it. That request is refused 409, never translated.

## Ordered refusals, all before any mutate

Three things make a budget unwritable and each is checked before the platform is touched: the
budget is shared (`explicitly_shared`), the shared flag could not be read at all, or no budget id
came back. The create path pins `explicitly_shared: false`, so only an **adopted** campaign can
reach the shared refusal — which is exactly the case where another campaign's delivery is at
stake.

At the service layer the ordering is the same discipline: validation (NaN checked first, then the
range, then the type token), then `If-Match` (428/412), then the campaign's state, then the
channel and provisioning refusals — all of them ahead of the version claim, so a request that was
never going to succeed does not take the write lock and turn a concurrent writer's request into a
spurious 409.

## Confirm, then persist

Platform first, row second. The budget columns are written only after the upstream mutate is
confirmed, and only the budget columns — a `created_degraded` campaign keeps its marker, because
paying the budget change is not evidence the degradation cleared. An UNCONFIRMED upstream result
holds the lock through a cooldown rather than releasing it, and persists nothing: the mutate is
sent idempotent, so re-applying converges. That is the opposite of `keyword-actions`, where a
`REMOVE` cannot be replayed safely — worth stating explicitly because the two endpoints otherwise
look alike.

## Validation lives in one place

`ValidateBudgetMicros` was **extracted** from the campaign create path, not copied. A second
definition drifting from the first is how a fixed bound quietly reopens; one function means the
create and update paths cannot disagree about what a valid budget is.

## Instrumentation

`opWriteBudget` is recorded like every other upstream call, and
`TestUpstreamCallsAreInstrumented`'s derived completeness gate did its job on the first run: it
parses the orchestrator for every `recordUpstream` token and failed the moment the new one existed
without a table case driving it. Satisfied by driving the real orchestrator path, not by widening
the gate.

Refs: LFXV2-2665
