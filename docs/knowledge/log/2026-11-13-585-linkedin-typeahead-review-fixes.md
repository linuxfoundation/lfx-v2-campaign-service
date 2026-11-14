# 2026-11-13 — #585 LinkedIn targeting typeahead: review fixes

**Fix** — Pre-PR review of `search-linkedin-ads-targeting-entities` (#585).

- Quota: the answer cache is keyed by facet and the whole case-folded query, so it serves a
  repeated identical search, not the next keystroke; the comment that said it kept per-keystroke
  searches off the shared LF token's `adTargetingEntities` quota is corrected. In its place, a
  server-side floor: `query` must be at least 2 characters (`linkedin.MinTypeaheadQueryLen`), as
  sent (`MinLength(2)` in the design) and after trimming (`NormalizeTypeaheadQuery`). The design
  description and `docs/api-catalog.md` ask callers to debounce.
- 429: the typeahead request is sent once (`withSendOnce`, the bid write's existing per-call
  opt-out), so a throttle answers 503 at once instead of after up to three backoffs that each
  spend the quota again.
- Contract: the design's `query` pattern is now `^\P{Cc}*[^\s\p{Cc}]\P{Cc}*$` (was `\S`), so the
  generated decoder and OpenAPI refuse a control character as the runtime does. `\S` cannot sit
  in the middle (`^\P{Cc}*\S\P{Cc}*$`): RE2's `\S` matches a control character, so `go\x00lang`
  would pass. Invalid UTF-8 stays a runtime-only refusal.
- Docs: the limits are 2-100 characters on the raw value, and the trimmed value must still be at
  least 2; the earlier "1-100 characters after trimming" was wrong on both counts.
- Tests: the request-shape capture in `targeting_entities_test.go` is an `atomic.Value` (it was a
  plain string written on the server goroutine), and the hijack handler no longer calls
  `t.Fatal` off the test goroutine. `TestLinkedInTargetingTypeaheadIsACampaignManagerRead` moved
  out of the middle of the `userListsPath` doc comment.
