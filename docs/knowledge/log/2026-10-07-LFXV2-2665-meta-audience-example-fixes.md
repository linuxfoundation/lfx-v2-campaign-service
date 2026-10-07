# 2026-10-07 — LFXV2-2665 possible OpenAPI examples; no wall-clock test bound (#285)

**Fix** — Three review findings on #285:

- **`buckets` property example (Greptile).** Goa filled the `MetaAdsAudience.buckets` property
  example by repeating the bucket type's one example, which gave three identical placement
  buckets. No response can contain that, because rows are merged per segment and age_gender is
  listed first. The attribute now has its own example: one age_gender bucket, then one placement
  bucket.
- **Timing test (Greptile).** `TestGetAudienceInsights_DuplicateKeyCheckIsLinear` had a fixed 1s
  wall-clock bound, which is flaky on slow CI. Two tests replace it:
  - `WideRowsAreJudgedCorrectly` checks correctness on 50,000 keys under a 60s hang guard only.
    It includes a foreign `campaign_id` overridden by a trailing case-folded `Campaign_ID`.
  - `DuplicateKeyCheckScalesLinearly` is a ratio test: the fastest of five runs, each after a forced GC, and 4x the keys
    must cost under 10x the time. Linear measures about 3-4x. With the pairwise scan restored it
    measured 15.6x and failed.
- **`AudienceLastSentEmail` example (Copilot).** The composed example published populated list
  arrays beside `lists_unavailable: true`, which is impossible. The type now has a type-level
  example with `lists_unavailable: false`, and the two list attributes have resolved-list
  examples. This is an example-only change.
- `internal/service/meta_audience_wire_example_test.go` now walks every example in all four
  generated specs (v2 and v3, gen and kodata) and checks those invariants.
