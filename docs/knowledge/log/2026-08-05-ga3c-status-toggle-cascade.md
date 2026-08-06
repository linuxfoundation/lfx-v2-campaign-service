# 2026-08-05 — GA-3c: dispatcher-level status-toggle cascade

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

**Update** — Closed a suppressed Copilot finding on PR #68: the PAUSE-path partial-cascade wrap
(campaign succeeds, then `UpdateAdGroupAndAdStatus` fails) only had a 5xx regression test
(`TestGoogleAds_ToggleStatus_PauseCascadeStopsOnChildFailure`), not a definite-4xx one — the more
important case, since a definite 4xx is what `wrapUnconfirmed`'s ambiguity classification would
otherwise let through as a clean error. Added
`TestGoogleAds_ToggleStatus_PauseCascadeChildDefiniteFailureIsUnconfirmed`, which fails
`adGroups:mutate` with a definite 400 after the campaign pause succeeds and asserts the returned
error is `Unconfirmed() == true`. The ACTIVATE-cascade docs (`internal-platform-googleads.md`,
`internal-dispatch.md`, and the `ToggleStatus` function comment) were checked and already
correctly describe ACTIVATE as refused in GA-3c and re-enabled in GA-4 — no doc changes needed.
