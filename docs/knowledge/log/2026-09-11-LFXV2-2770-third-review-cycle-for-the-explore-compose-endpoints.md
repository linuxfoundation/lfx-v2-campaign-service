# 2026-09-11 — LFXV2-2770: third review cycle for the explore/compose endpoints

**Fix** — A third local post-commit review pass over the same audience-builder explore/compose
work ([[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]], previously fixed in
[[2026-09-11-LFXV2-2770-review-fixes-for-the-explore-compose-endpoints]] and
[[2026-09-11-LFXV2-2770-second-review-cycle-for-the-explore-compose-endpoints]]) found further
defects, all now fixed:

- `composeErr` (`internal/service/audience_explore.go`) unconditionally populated the wire
  response's `Suppression` field from `&partial.Suppression`, which is a struct-field address and
  therefore never nil — so a master-create-only failure (no exclusions requested, or the
  suppression create itself failed) put a `suppression` object on the wire with blank
  `list_id`/`name`/`hubspot_url`, despite `AudienceComposedList` declaring all three `Required` in
  the design. This misreported a list that was never created as one the operator must go reconcile
  in HubSpot. Fixed by gating on the same `partial.Suppression.ListID != "" ||
  partial.Suppression.Name != ""` check `composePartialMessage` already used, so `Suppression`
  stays nil when no suppression list exists.
- `PreviewCount`'s doc comments (`internal/audience/builder_master.go`, `AudiencePreviewCount` in
  `design/audience_builder.go`, and the `preview-audience-count` row in `docs/api-catalog.md`) had
  gone stale after [[2026-09-11-LFXV2-2770-second-review-cycle-for-the-explore-compose-endpoints]]
  added `IncompleteSizePreviewCount`: they still described every non-exact estimate as an
  over-count in the safe direction. `IncompleteSizePreviewCount` actually UNDER-counts, since it
  omits any list HubSpot reported no size for entirely — the one direction that must never be
  presented as exact. All three docs now describe both directions and point at `reason` as the
  place a caller learns which one applies.
- `list_ids` (`preview-audience-count`'s payload and `AudienceComposeMasterInput`) and
  `exclude_list_ids` (`AudienceComposeMasterInput`) in `design/audience_builder.go` had no upper
  bound, so an oversized id array could turn one request into an unbounded HubSpot filter-branch
  tree — `MasterListFilter`/`MasterListWithSuppressionFilter` build one AND branch per id — or an
  unbounded membership sweep. Added `MaxLength(200)` to all three, comfortably above any real
  selection.

Part of [[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]].
