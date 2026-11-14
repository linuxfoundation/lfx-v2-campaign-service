# 2026-11-13 — #584 guard the shared campaign group's end for a daily-capped LinkedIn campaign

**Fix** — A LinkedIn campaign carrying both `totalBudget` and `dailyBudget` (from
`linkedInConfig.dailyBudgetUsd`) is created with a continuous schedule, with no
`runSchedule.end`. Only its campaign group's end stops it. That group is shared by every campaign
of the event and project (`"Events | <event> | <project>"`), and `ensureCampaignGroupCoversEnd`
only ever widened it. So the earlier claim that "the flight stays bounded" was false in three
cases. The capped campaign could serve past its end date when:

- the reused group already ended later;
- the group had no end;
- a later sibling create widened the group.

What changed (`internal/platform/linkedin/group_capped.go`, `ensureCampaignGroupCoversEnd`):

- A capped create accepts a reused group only when the group ends exactly at the campaign's end.
  A group ending earlier is widened to that end. A group ending later, or with no end, is refused
  before the campaign POST, with nothing created. A group the same call created already carries
  the end, so it is not checked.
- Before the first widening write of any create, one adCampaigns search filtered by the group URN
  looks for a campaign with both budgets and no end. If it finds one, the widening is refused and
  nothing is written. The search fails closed, and it runs only when a write is about to happen.
- Both refusals are `ErrCampaignGroupScheduleConflict`. They are `domain.PreflightRefusal`s and
  carry `domain.ErrCreateConfigInvalid`. The create returns `(nil, err)` and the dispatcher
  releases the claim. The job error is the refusal's own explanation, which names the remedy. The
  group is not renamed per campaign, because the data pipeline parses group names (#587).
- Still not guarded:
  - a hand edit of the group's end in Campaign Manager;
  - another dispatch widening the group while the capped campaign's POST is in flight
    (LFXV2-2665).

Account monitor (`internal/service/rules/monitor_linkedin.go`): a two-budget row with no end used
to be paced against `dailyCap × windowDays` and ignored the total. A $100 total under a $20 cap
that had spent its $100 then read as underspending. The expectation is now
`min(dailyCap × windowDays, totalBudget)`. Lifetime spend is not in the metrics, so the cap is the
whole total, not what remains of it. The old test fixture ($120 spent against a $100 total) could
not happen and was replaced.

- Tests:
  - `group_capped_test.go`: the capped end rule, the post-write case, the widening search
    (capped holder refused, ordinary campaigns widened, every fail-closed arm), and end-to-end
    refusals with no campaign POST.
  - `TestLinkedinPacingPct_LifetimeWithDailyCap`.
  - `TestLinkedinExpectedSpend_CappedNoEndNeverExceedsTotal`.
