# 2026-08-06 — GA-3b review round (dealako): documentation and test-hygiene fixes

**Update** — Addressed five minor review findings from dealako's CHANGES_REQUESTED on PR #67.
All were doc-accuracy and test-coverage hardening; no blocking behavioral issues. Fixes:

1. **ToggleStatus doc comment + error message** (`internal/dispatch/googleads.go:250-274`): The prior
text claimed ACTIVATE was blocked because "no ad group/ad exists yet to cascade to," which GA-3b
now makes false. Reworded both the doc comment (lines 250-261) and the 409 error message (lines
268-272) to correctly state that the dispatcher-level cascade (GA-3c) is not wired yet and GA-4
targeting is absent — NOT because children don't exist. The UpdateCampaignStatus comment in
`internal/platform/googleads/campaign.go` was already accurate.

2. **CampaignResult field-state documentation** (`internal/platform/googleads/campaign.go:126-135`):
The prior comment excluded the ambiguous outcome where a 2xx response returns a missing/malformed
resource name, so an ad group may exist while AdGroupID remains empty. Expanded the comment to
describe the fields in terms of which IDs are KNOWN to the client, not merely whether upstream
resources were created, and noted that AdGroupName is paired with AdGroupID for reconciliation.

3. **httptest handler goroutine safety** (`internal/platform/googleads/campaign_test.go:737-751,
adgroup_ad_test.go:116-125`): The `decode(t, r)` helper called `t.Fatalf` from inside an httptest
handler goroutine, which is not safe — `FailNow` is only valid on the test goroutine. Extracted a
handler-safe `decodeRequest(r)` function that returns an error, updated `TestCreateAdGroupAndAd_HappyPath`
to use it inside handlers, and collected decode errors under `sync.Mutex` to report after
CreateCampaign returns. Mirrors `httptest-handler-state-needs-synchronized-handoff` in
`docs/reviews/knowledge-base/test-hygiene.md`.

4. **Documentation accuracy: weighted character limits** (`docs/knowledge/log/2026-08-04-ga3b-adgroup-ad-cascade.md:10`):
The log entry claimed "no double-width halving unlike Microsoft," but the implementation applies
weighted character counting where CJK/full-width runes count as 2 (matching Microsoft's rule).
Corrected the entry to state "with weighted character counting where CJK/full-width characters
count as 2." (The api-catalog and internal-platform-googleads files were already correct.)

5. **Test comment accuracy** (`internal/dispatch/googleads_test.go:508-511`): The comment for
`TestGoogleAds_ToggleStatus_ActivateIsNotProvisioned` claimed "the create path provisions only a
campaign shell — no ad group, ad, or keywords," which GA-3b now makes false. Updated to correctly
state that GA-3b creates the ad group and ad, but targeting is absent and the cascade is not wired.

All five items were additive documentation/test fixes. No code behavior changed; no tests were added
or removed. Existing tests already covered the partial-cascade classification and the ad-copy
mappings reaching the wire.
