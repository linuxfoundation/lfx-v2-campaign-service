# 2026-10-13 — Signed asset URLs: mint late, validate every setting (#310 review)

**Fix** — Three late review findings on the Reddit signed creative-asset URLs. (1) The URL was
minted in `RedditDispatcher.Dispatch`, before `CreateCampaign` verified the account; a throttled
verification (`Retry-After: 60`) could spend a short TTL before Reddit fetched the image, ending in
a 404 and a `created_degraded` campaign with no ad. Dispatch now only checks the asset and passes
`reddit.CampaignInput.ImageURLSource`, which the client calls immediately before the promoted-post
request; the TTL floor rises from 1m to 5m to cover that request, Reddit's fetch and clock skew.
(2) `assetsign.New` returned "off" before validating, so a malformed key with no base URL, or a 61m
TTL while off, started cleanly; every supplied value is now validated first and all errors are
reported together. (3) create-campaigns' description claimed the HubSpot A/B conflict was the only
synchronous refusal; it now also lists the Reddit config-only 400s, as does the api-catalog row.
See `architecture/signed-creative-asset-urls.md`.
