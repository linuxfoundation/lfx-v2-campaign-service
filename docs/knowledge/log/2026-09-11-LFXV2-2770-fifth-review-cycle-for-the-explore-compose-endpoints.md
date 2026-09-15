# 2026-09-11 — LFXV2-2770: fifth review cycle for the explore/compose endpoints

**Fix** — A fifth local post-commit review pass over the same audience-builder explore/compose
work ([[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]], previously fixed in
[[2026-09-11-LFXV2-2770-review-fixes-for-the-explore-compose-endpoints]],
[[2026-09-11-LFXV2-2770-second-review-cycle-for-the-explore-compose-endpoints]],
[[2026-09-11-LFXV2-2770-third-review-cycle-for-the-explore-compose-endpoints]] and
[[2026-09-11-LFXV2-2770-fourth-review-cycle-for-the-explore-compose-endpoints]]) found five
further defects, all now fixed:

- `ComposeMaster`'s one arm that reports a DEFINITE orphan — the master create failed outright
  with a suppression list already confirmed created — logged and wrapped the raw HubSpot error
  instead of the already-built `"audience compose: create master list: %w"` wrapper, the only such
  arm to do so. Fixed to use the wrapped error, matching every sibling arm.
- `PreviewCount`'s orchestration-level doc comment (`internal/dispatch/audience_explorer.go`)
  still described "three answers" with both non-exact outcomes framed as an over-count, even
  though `IncompleteSizePreviewCount` — added two cycles ago — is an UNDER-count. The type's own
  doc in `internal/audience/builder_master.go` already had the correct four-outcome explanation;
  the orchestration comment, which is what a reader chasing a wrong preview number reaches first,
  did not. Rewritten to match.
- An exclusion-only rollup list (every filter is `NOT_IN_LIST`) is a legal HubSpot shape that
  `IsRollup` correctly recognizes, but `RollupChildIDs` — which deliberately skips exclusion
  references — then returns zero children for it. `Discover`'s rollup branch treated "IsRollup
  true" as "there are children to inspect instead of classifying this list," so a
  zero-children rollup fell into an unconditional `continue` and vanished from the outcome with
  no `uncertain` row and no warning, contradicting the discovery package's own "uncertain must
  surface, never be dropped" invariant. Fixed by only taking the rollup-children branch when
  `RollupChildIDs` actually returns something; otherwise the list falls through to being
  classified directly. Added `TestIsRollup_ExclusionOnly` (`internal/audience/builder_discovery_test.go`)
  pinning the shape that went unexercised.
- `bestMatch` (used by `SuppressionLists` to resolve suppression/opt-out lists) ranked candidate
  search hits on their raw, pre-normalization `Size` field. A HubSpot search hit's `hs_list_size`
  being absent (unreported) and a genuinely empty list (reported as literal zero) both normalize
  to `0` on that raw field, so the two were indistinguishable and whichever came first in
  HubSpot's response order won regardless of which was which — the wrong tie-break for a function
  whose job is to avoid under-applying a suppression. Fixed to rank through the existing `sizeOf`
  (which already returns `*int64` and keeps "no size reported" distinct from "zero"), so any hit
  with a known size — including a known zero — outranks one HubSpot reported no size for. Added
  `TestSuppressionLists_BestMatch_PrefersKnownSizeOverUnreported`
  (`internal/dispatch/audience_explorer_test.go`) driving the fix end-to-end through the real
  HubSpot search response shape.
- `ExceedsExactCap`'s doc comment (`internal/audience/builder_master.go`) named
  `MembershipPageSize * MembershipMaxPages` as the source of the 25,000 boundary; both exported
  constants were deleted as dead code two cycles ago. Corrected to name the actual (unexported)
  constants in `internal/platform/hubspot/list_memberships.go`, matching how the sibling
  `UnionExactCap` comment already referenced them.

Part of [[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]].
