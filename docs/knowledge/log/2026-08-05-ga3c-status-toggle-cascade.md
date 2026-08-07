# 2026-08-05 — GA-3c: dispatcher-level status-toggle cascade

**Update** — Added the GA-3c dispatcher-level status-toggle cascade
(`internal/dispatch/googleads.go`), the third sub-1000-line slice split off the original
GA-3 PR (after GA-3a/GA-3b). Rewrote `GoogleAdsDispatcher.ToggleStatus`, which previously
refused ACTIVATE unconditionally with "the create path provisions only a campaign shell" —
stale now that GA-3b's create path provisions a real ad group + ad. It now cascades like the
reddit adapter: PAUSE flips the campaign FIRST (stops delivery immediately even if the child
update fails/is unconfirmed) then the ad group/ad via `Client.UpdateAdGroupAndAdStatus`;
ACTIVATE flips the children FIRST and the campaign LAST, so a campaign never reports ENABLED
before its ad group/ad already do. ACTIVATE is still refused with `ErrCampaignNotProvisioned`
up front when either child id is unknown (a duplicate-name orphan or unconfirmed GA-3b create
has no id to cascade to). New `googleAdsChildIDs` reads the ad-group/ad ids from the
campaign's persisted `Result` blob. Added
`TestGoogleAds_ToggleStatus_PauseCascadesToChildren`,
`TestGoogleAds_ToggleStatus_ActivateCascadesChildrenFirst`,
`TestGoogleAds_ToggleStatus_PauseCascadeStopsOnChildFailure`, and `TestGoogleAdsChildIDs` to
`internal/dispatch/googleads_test.go`. Updated `internal-platform-googleads.md` (new "Status
toggling (GA-3c)" section, replacing the GA-3b placeholder), `internal-dispatch.md`'s
Google Ads paragraph in "Status toggle (optional capability)" (was still describing the
pre-GA-3b PAUSE-only/no-cascade shape), and the code index bullet.

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
