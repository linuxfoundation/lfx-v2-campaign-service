# 2026-11-16 — #584 a LinkedIn lifetime budget can carry a daily cap

**Creation** — LF often runs a LinkedIn ad set with a lifetime budget and a daily cap (OSS Japan
2026: $1,000 lifetime, $20 daily). campaign-service sent `dailyBudget` or `totalBudget`, never both.

- Request: optional `linkedInConfig.dailyCapUsd`, valid only with `lifetimeBudget: true`. The
  campaign then carries `totalBudget` (budgetUsd) and `dailyBudget` (the cap).
- Rules (`linkedin.ValidateDailyCap`): lifetime only; every daily rule ($10 minimum at wire
  precision, finite, above zero); not above the lifetime total.
- Checked three times before anything exists:
  - `PreflightCreate` gives a synchronous 400, including `checkDailyCapShape` for a string or
    null;
  - `Dispatch` repeats the same checks, so a null cap can never become an uncapped campaign;
  - `CreateCampaign` checks before the campaign group POST.
- Budget edit (`WriteBudget`): a campaign reporting both amounts was refused as ambiguous. It is
  now lifetime-paced. A lifetime change writes `totalBudget` alone and leaves the cap in force;
  it is refused as an amount (400) below the cap (`linkedin.ValidateTotalCoversCap`). A daily
  change stays a 409.
- The settings readback still reports both-budget campaigns `unknown`; that behaviour is
  unchanged and now documented.
- Tests:
  - `TestValidateDailyCap` and `TestValidateTotalCoversCap`;
  - `TestCreateCampaign_LifetimeBudgetWithDailyCap`: the wire body, and zero POSTs on a refusal;
  - `TestLinkedIn_DailyCapPreflight`, `TestLinkedIn_DailyCapRefusedInDispatch` and
    `TestLinkedIn_DailyCapReachesTheCampaign` (the body and the snapshot);
  - `TestLinkedIn_WriteBudget_LifetimeWithDailyCap`, and two new guard cases.
  - Removing the Dispatch check, the body field or the edit guard each fails a test.
- Not verified live: the first live LinkedIn create (cs#579) will confirm the two-field shape.
