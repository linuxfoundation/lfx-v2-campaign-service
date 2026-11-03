# 2026-11-04 — #462 Display audiences gated behind a default-off deployment flag

**Update** — The Display audience lists check each segment against the Google Ads account
only. The LF account is shared by every foundation, so a project could target with another
foundation's user list or custom segment (#468). #459 closed the same gap for
`demandGenAudience` with `GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED`. #462 now does the same for
its own inputs with `GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED`.

`Dispatch` refuses the following as not-created, before the budget, unless the flag is
exactly `"true"`: a non-empty `audienceSegments` on `display`, and a non-empty
`customAudiences` or `customAffinities` on any channel. The chart sets the flag to `"false"`.
Three inputs are not gated:

- `userInterests` are Google's own affinity and in-market categories, the same ids on every
  account, so no foundation owns one.
- `excludedPlacements` are public domains, not account resources, and they only narrow where
  a campaign appears.
- Search's `audienceSegments` are observation-only and predate #462.

Both gates now refuse through one helper, `googleAdsInputDisabled`. It returns a
`domain.PreflightRefusal`, so the asynchronous create job reports which input is disabled
and that it can be omitted. Before this change the Demand Gen gate's plain error showed only
the generic "platform campaign creation failed".
