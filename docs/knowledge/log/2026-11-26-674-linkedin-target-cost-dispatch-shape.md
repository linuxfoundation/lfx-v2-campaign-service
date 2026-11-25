# 2026-11-26 — #674 LinkedIn: a wrong-typed targetAmount reaching Dispatch gets the pre-flight refusal

**Fix** — PR #677 review (Copilot, `internal/dispatch/bidding.go`). A job that reached LinkedIn's
`Dispatch` without the synchronous pre-flight and carried a string or object
`linkedInConfig.bidding.targetAmount` failed in `unmarshalPlatformConfig`: the strict
`biddingConfig.UnmarshalJSON` returns a plain "bidding.targetAmount must be a number" error that
carries neither `ErrCreateConfigInvalid` nor `PreflightRefusal`, so `DispatchFailureMessage`
reduced it to a generic campaign-creation failure.

- `LinkedInDispatcher.Dispatch` now runs `checkBiddingShape` before `unmarshalPlatformConfig`
  (it is pure), so the job is refused with the same `ErrCreateConfigInvalid` pre-flight sentence
  `PreflightCreate` gives, marked nothing-created, before any LinkedIn request. The same order
  covers a wrong-typed `bidAmount` or `strategy`.
- The other lanes' `Dispatch` keep their existing order (decode, then shape check); this change is
  LinkedIn's only.

Tests: `TestLinkedIn_TargetAmountRefusedByDispatchSendsNothing` gains string, null and object
`targetAmount` rows (and a string row with the flag off), and now asserts the pre-flight refusal
sentence and the nothing-created marker for every row.
