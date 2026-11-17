# 2026-11-17 — #581 a wrong-typed adAccountId is refused synchronously

**Fix** — A `linkedInConfig.adAccountId` that is present but not a JSON string (a number, object,
array or boolean) is now a synchronous `400 invalid campaign config: linkedInConfig.adAccountId:
...` before any job exists (lfx-one on #629). `PreflightCreate` decodes the config best-effort, and
encoding/json skips a wrong-typed field, so `"adAccountId": 500928401` left `cfg.AdAccountID`
empty and the account check was skipped: the request got a job and a 202, and `Dispatch` then
failed it in `unmarshalPlatformConfig`.

- New `checkAdAccountIDShape` in `internal/dispatch/linkedin.go`, a raw-JSON shape check like
  `checkBiddingShape`, `checkConversionsShape` and `checkTargetingShape`, wrapped with
  `domain.WrapPreflightRefusal`. Keys match case-insensitively, as encoding/json does.
- An explicit `null` reads as omitted (the connection's account), as a null `employerExclusions`
  does; bidding, conversions and targeting refuse null because null there would silently change
  the campaign, while a null account id means only "the default".
- `PreflightCreate` runs it before the account check; `Dispatch` runs it before the strict decode,
  so the job fails with the same sentence the synchronous path answers.
- `docs/api-catalog.md` is unchanged: its ad-account section lists no shape errors.
- Tests: `TestLinkedIn_AdAccountIDRefusals` gains number, object and array cases (pre-flight
  refused, `Dispatch` refused before any upstream call, nothing sent) and a null case
  (`ErrPreflightNotApplicable`). With the check disabled the three wrong-typed cases fail with
  `ErrPreflightNotApplicable`.
