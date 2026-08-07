# 2026-08-05 — LFXV2-2023 GA-3c: partial-cascade wrapping and the ACTIVATE guard

**Update** — Review fixes for GA-3c dispatcher-level status-toggle cascade (PR #68). Two issues
addressed from the review cycle: (1) Partial-cascade wrapping — after the PAUSE path's
`UpdateCampaignStatus` succeeds, any subsequent failure from `UpdateAdGroupAndAdStatus` (even a
definite 4xx) is now wrapped as `unconfirmedToggleError` to signal that the state is ambiguous
(parent changed but child outcome is unknown), matching the pattern sibling adapters use. This
fix applies to both PAUSE (line 299) and ACTIVATE (lines 307, 310) paths; ACTIVATE is currently
unreachable but will be re-enabled in GA-4. (2) ACTIVATE servability guard — reverted to refusing
ACTIVATE unconditionally in GA-3c. GA-3b provisions ad group + ad but no keyword/audience targeting
criteria, so activating would report false success (the exact scenario `ErrCampaignNotProvisioned`
prevents). The check now refuses ACTIVATE with a message referencing GA-4's targeting provisioning
requirement, replacing the misleading "no fully-created ad group + ad" message. Updated
`GoogleAdsDispatcher.ToggleStatus` guard (line 271) and test expectations
(`TestGoogleAds_ToggleStatus_ActivateCascadesChildrenFirst` and
`TestGoogleAds_ToggleStatus_ActivateCascadeStopsOnCampaignFailure` renamed and updated to verify
ACTIVATE is refused).
