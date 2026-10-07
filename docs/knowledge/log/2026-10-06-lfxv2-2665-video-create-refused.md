# 2026-10-06 — LFXV2-2665 Video (YouTube) creation refused before the budget mutate

**Fix** — `CreateVideoCampaign` shipped a seven-step create cascade for a campaign type the
Google Ads API cannot create. Google's
[Video overview](https://developers.google.com/google-ads/api/docs/video/overview) states it
without qualification: "You cannot create new Video campaigns or update existing ones using
the Google Ads API", and "Video campaigns cannot be created or mutated using the Google Ads
API". Raised on PR #272 and confirmed against Google's own documentation. Fetching and
reporting on Video campaigns *are* supported — only creation is impossible.

The cost of leaving it was money rather than an error. The budget is step 1 and
`campaigns:mutate` is step 2, so every Video request created a real `CampaignBudget` on the
account and then failed at the campaign — one orphaned budget per attempt. Worse, the retry
path cannot clean up after itself here: a retry composes the same deterministic budget name
and dies at `DUPLICATE_NAME` before reaching the campaign step, so the orphan it made on the
first attempt is never reconciled. That is precisely the stranding the refuse-don't-drop
doctrine exists to prevent, and it does not become acceptable because the refusal comes from
upstream rather than from a field value.

`CreateVideoCampaign`'s first statement now returns `ErrVideoCreateUnsupported` — a sentinel,
not a bare `fmt.Errorf`, so dispatch and its tests can tell "Google cannot do this" apart from
"this request was malformed". `(nil, err)` is the pre-create half of the partial-result
contract, so dispatch's existing `notCreated` arm releases the orchestrator's claim unchanged;
no dispatch code moved.

**Placement was the one real decision.** The obvious home, `preflightCampaignKind` /
`ValidateCampaignInputKind`, is wrong: dispatch calls that validator at
`internal/dispatch/googleads.go` BEFORE the adoption branch, so a refusal there would kill
legitimate Video ADOPTION — a path Google does support — along with creation. The refusal
belongs in the create function, which only the create switch reaches.

The cascade is retained, unexported and unreachable, as `createVideoCampaignCascade` with a
`//nolint:unused`. Deleting it would mean rebuilding the channel from the documentation the day
Google opens creation; kept, that day costs removing one block and re-verifying
`videoBiddingStrategies`, which was always the one thing a code reading could not settle.
`video_test.go` still exercises the cascade in full against that function.

Two things the tests pin deliberately. `TestCreateVideoCampaign_RefusesWithoutSendingAnything`
fails if Google is contacted **at all**, and the dispatch test asserts `!cap.sawBudget` — because
"returns an error" is satisfied equally well by a cascade that bought a budget first, which is
the exact outcome being prevented. And `TestGoogleAds_VideoCallToActionsAreOptional` no longer
creates an ad to show optionality; it shows it by WHICH refusal comes back — the four required
lists stop at their own validation refusals, while an omitted `callToActions` reaches the
channel refusal. That keeps the validator honest while the create path is closed, so the
optionality is still correct the day it reopens rather than rediscovered.
