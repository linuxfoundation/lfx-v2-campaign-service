# 2026-08-10 — HubSpot metrics: the 409 taxonomy's "pre-contact" claim was too strong

**Fix** — Third Copilot round on PR #113, one finding, in `docs/api-catalog.md`'s metrics row.

## The PRE-CONTACT label covered two HubSpot cases that do contact the channel

The row's 409 taxonomy said the first group is refused "before the channel was called." That
holds for Google Ads' account-mismatch check (resolved entirely from locally-stored account
ids) and for the shared unprovisioned/no-credential cases, but not for HubSpot's own identity
checks: `HubSpotDispatcher.ReadMetrics` calls `Client.AuthenticatedPortalID`
(`internal/dispatch/hubspot.go:174`), which sends `GET /account-info/v3/details`, before it can
compare the token's portal against the campaign's recorded one. A row that records no portal, or
a different one, is still refused before the TENANT-SCOPED METRICS REQUEST — the call that would
actually return numbers — but the channel itself has already been reached.

Reworded the taxonomy's opening sentence to key on "was the tenant-scoped metrics request
attempted," not "was the channel ever contacted," and added a note at the portal-mismatch
sentence making the AuthenticatedPortalID call explicit. Adjusted the POST-CONTACT sentence to
scope "belongs to HubSpot alone" to the tenant-scoped call succeeding-but-empty case, not to
being the only case where HubSpot reaches the channel.

## Tests

Doc-only; no behavior changed. Verified nothing in `internal/service/*_test.go` asserts on the
edited prose (it's a markdown table cell, not a code string). Ran the full suite plus
`go run ./cmd/okfvalidate ./docs/knowledge` to confirm nothing regressed.
