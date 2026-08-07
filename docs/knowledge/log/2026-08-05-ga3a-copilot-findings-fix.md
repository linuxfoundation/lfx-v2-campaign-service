# 2026-08-05 — GA-3a: close suppressed Copilot findings

**Fix** — Closed 5 suppressed Copilot findings on GA-3a (`internal/platform/googleads/ad_copy.go`):
corrected the `composeAdCopy`/RSA doc comment and the concept doc
(`internal-platform-googleads.md`) to describe weight-capping (CJK/full-width runes count double,
per `truncateWeighted`) instead of plain rune-capping; fixed the `minDescriptions` error message
to name only `eventName` as a remedy, since `defaultDescriptions` returns nil whenever `eventName`
is empty regardless of `project`; fixed `TestComposeAdCopy`'s project-omission assertion (`&&` →
`||`) so it actually fails when the project-specific description survives; and repaired the split
`nolint:unused` directive/rationale comment on `adTextAsset` whose two sentences had been
transposed.
