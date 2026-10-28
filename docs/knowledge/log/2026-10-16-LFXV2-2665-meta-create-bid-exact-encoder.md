# 2026-10-16 — Meta create-time bids use the exact minor-unit encoder

**Update** — merging #316 into the bidding-strategy branch (LFXV2-2665).

- `CreateBidMinorUnits` now encodes through `minorUnitsExact`, the exact encoder #316 gave the
  bid lever and the budget. It still refuses an amount that is not a whole minor unit, and now
  refuses a product past `MaxExactMinorUnits` ("too large to represent exactly") instead of
  checking against int64's limit. The int64-only helper `bidTooLargeForOffset` is gone.
- The create path's 5× daily-budget overflow check stays as a backstop; under the exact bound no
  encoded bid reaches it.
- Tests: `TestBidEncoders_RefuseAProductPastTheExactBound` replaces the int64-bound test, the
  overflow test now expects the bid's own refusal, and the 5× minimum's JPY cases use whole yen,
  since a fractional JPY budget is refused by the budget encoder since #235.
