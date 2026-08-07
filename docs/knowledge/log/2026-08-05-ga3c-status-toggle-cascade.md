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
