# 2026-08-10 — HubSpot metrics: second Copilot round, all doc/wording defects

**Fix** — Seven suppressed Copilot findings on PR #113, all in the HubSpot email-metrics path
added since the round fixed in `2026-08-10-hubspot-metrics-review-fixes.md`. None changed
behavior; all closed a place where the docs, the design example, or a log entry said something
the code no longer did.

## The account.go / AuthenticatedPortalID summary line went stale

`2026-08-10-lfxv2-3058-email-metrics-endpoint.md`'s opening line said "nothing in
`internal/platform/hubspot` changes here," written before this PR's own LFXV2-3073 section
added `internal/platform/hubspot/account.go` and `Client.AuthenticatedPortalID`. Corrected to
point at that section instead of contradicting it two headings later.

## The same file's mismatch paragraph fought its own LFXV2-3073 section

The "canonical 409 sentence" section said HubSpot "emits neither `account_not_selected` nor an
account mismatch" — true when written, false once LFXV2-3073 (below it in the same file) added
the portal-mismatch 409. Reworded to state only the part still true (no ad-account mismatch,
since email has no ad account) and point at the LFXV2-3073 section for what HubSpot does check.

## `brief.go`'s ErrConnectionNotUsable arm named "ad-platform" for a HubSpot-reachable error

`resolveHubSpotClient` tags the same three ErrConnectionNotUsable reasons (inactive, credentials
undecodable, credentials incomplete) as the ad-platform resolvers, so this arm's 409 reaches
HubSpot connections too. Its message said "this project's ad-platform connection is not ready,"
sending an email-channel caller to check a system they never connected. Reworded to "channel
connection," matching the arm below it that already made this distinction for the default
503 case.

## `errors.go`'s ErrCampaignAccountMismatch comment claimed the platform is never contacted

True for Google Ads (the mismatch is caught from locally-resolved account ids), no longer true
for HubSpot: `AuthenticatedPortalID` calls `/account-info/v3/details` before the mismatch check
can even run. Reworded to the claim that holds for every path to this sentinel — the
tenant-scoped metrics read itself is never attempted — rather than the platform-contact claim
that only held for one of them.

## The metrics-response example asserted a HubSpot id shape but not a HubSpot id value

`campaign-metrics.platform_campaign_id` had no example, on the stated grounds that its format
is channel-specific and any one value would be a claim about the others. But `email` being
optional means Goa fabricates the rest of the example as an email-channel response regardless
(documented in the same file), so the published example was already committed to being a
HubSpot response — it just lacked the matching id. Added a bare-numeric example
(`"104670127234"`) consistent with the rest of that forced example.

## `api-catalog.md` collapsed two different provenance remedies into one sentence

The metrics row said a recorded-but-different portal and a never-recorded portal "need the same
repair: point the connection back at the original portal, or re-dispatch" — but `brief.go`
already returns two different 409 messages for them (`ErrCampaignAccountMismatch` says
reconnect; the narrower `ErrCampaignProvenanceUnknown` says re-dispatch, added in the prior
round specifically because there is nothing to reconnect to for that case). Split the catalog
sentence to match.

## Tests

No behavior changed, so no new test asserts on prose. Verified the two message strings this
touches (`internal/service/brief.go`'s ErrConnectionNotUsable 409 and the unchanged
ErrCampaignAccountMismatch / ErrCampaignProvenanceUnknown 409s) are not asserted on by any
existing test (`grep` across `internal/service/*_test.go`), so no test needed updating; ran the
full suite plus `go run ./cmd/okfvalidate ./docs/knowledge` to confirm nothing regressed.
