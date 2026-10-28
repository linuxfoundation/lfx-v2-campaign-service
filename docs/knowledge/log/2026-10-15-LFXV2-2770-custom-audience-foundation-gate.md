# 2026-10-15 — Custom audience gates a project on its foundation's master list

**Fix** — a typed "gRPC India 2026" description could not be previewed: the model returned
`brand_short: "gRPC"`, the portal has no gRPC master list, and the brand-from-lists fallback
(`DominantBrand`) ran only for an event URL. The gate stayed empty and `ValidateCustomCriteria`
refused the location branches.

- `InterpretCustom` now falls back for every draft whose brand has no master list: it searches the
  portal's lists for the brand (or the label), takes the brand they are filed under, and suggests
  that brand's master (CNCF's for gRPC) with a draft note. A tie still yields no gate.
- The interpret prompt now says `brand_short` is never a hosted project or event series.
- Tests: `TestInterpretCustom_InfersTheFoundationWhenTheProjectHasNoMasterList`,
  `TestInterpretCustom_NeverGuessesBetweenTwoBrands`.
