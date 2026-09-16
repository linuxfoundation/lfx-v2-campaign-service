# 2026-09-11 — LFXV2-2770: second review cycle for the explore/compose endpoints

**Fix** — A second local post-commit review pass over the same audience-builder explore/compose
work ([[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]], already fixed once in
[[2026-09-11-LFXV2-2770-review-fixes-for-the-explore-compose-endpoints]]) found further
correctness and cleanup defects, all now fixed:

- `ComposeMaster`'s suppression-list create did not distinguish `hubspot.IsUnconfirmed` (the
  create may have reached HubSpot) from a definite failure — an unconfirmed create returned a
  bare error instead of `audience.ComposePartialError`, discarding the one signal that tells an
  operator a list may already exist under that name. It now returns the partial error with the
  suppression's deterministic NAME only (no id — an unconfirmed create has none to give).
- `PreviewCount`'s initial per-list size read summed raw `.Size int` values directly, so a list
  HubSpot reports no size for (as opposed to a genuinely empty one) silently contributed zero to
  the estimate instead of being flagged as unknown. `sizeOf` (`nil` vs `0`) now gates a new
  `IncompleteSizePreviewCount` outcome when any selected list's size is unreported. See
  [[2026-09-09-LFXV2-3040-audience-portal-provenance]] for the related distinction between "no
  data" and "data is zero" elsewhere in this package.
- `RunQA` (and `Discover`) re-fetched the same HubSpot list by id once per reference instead of
  once per call — a list excluded by more than one other list, or the searched-for candidate
  itself, cost one `GetList` round-trip per occurrence. A request-scoped
  `map[string]*hubspot.List` cache, seeded from `SearchLists` hits already in hand, is now
  threaded through `listWithFilters`, `listName`, and `exclusionNames`.
- `internal/service`'s `deref`/`derefOrEmpty` were two copies of the same "nil pointer reads as
  the empty value" helper in different files; consolidated onto the existing `derefStr`.
- `builder_types.go` still referenced a deleted `builder_lastsent.go` in its package doc comment
  ([[2026-09-11-LFXV2-2770-review-fixes-for-the-explore-compose-endpoints]] removed the file's
  only export, `LastSentEmailSearchLimit`, but left the comment naming it).
- Removed the dead exported `MembershipPageSize`/`MembershipMaxPages` constants in
  `builder_types.go` — confirmed via grep to have no usages outside their own package's tests, and
  already redundant with `internal/platform/hubspot`'s real, unexported pagination bounds; a
  reviewer finding proposing these be reconciled with the real bounds could not be applied as a
  cross-package constant expression (Go visibility rules), so the doc comment on `UnionExactCap`
  instead names where the real bound lives and why the two must track each other.
- Added test coverage the review found missing: `ValidateInclusionIDs`
  (`internal/audience/filters_test.go`), `PickNameMatches` on two IDENTICAL exact-name matches
  (`internal/audience/builder_qa_test.go`, distinct from the existing 2-or-more-exact case),
  `hubspot.IsNotFound`'s 404-only matching (`internal/platform/hubspot/lists_test.go`),
  `audienceExploreErr`'s arm ordering including a co-wrapped-sentinel case
  (`internal/service/audience_explore_test.go`, new file), and `ComposeMaster`'s
  `hubspot.IsUnconfirmed` branch driven through a real HTTP round trip rather than only the
  `ComposePartialError` type in isolation (`internal/dispatch/audience_explorer_test.go`).
- One reviewer finding on `CheckSignalMapping` (`internal/audience/builder_qa.go`) was
  investigated and found to be a false positive: the proposed change broke an existing,
  intentionally-documented test asserting that an all-blank signature set FAILs rather than
  needing verification. Left as-is, with the doc comment clarified in place so it is not
  re-flagged.

Part of [[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]].
