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

Follow-up (#317 review): the concept description and its index bullet, the `assetsign` package
doc and the `reddit_asset.go` header still said the URL was minted inside the dispatch; they now
say it is minted immediately before the Reddit post request. The create-campaigns description no
longer implies the two dispatch-time refusals are the only synchronous ones — it names the
request-shape 400s (such as `new_version` on an unsupported platform) alongside them.

Follow-up (#317 review, clock skew): `Verify` bounded an expiry at exactly `MaxTTL` from the
verifier's clock, so a 60-minute URL minted by a pod one second ahead of the serving pod was
refused. It now allows `MaxTTL + MaxClockSkew` (1 minute, a documented constant); an expiry
already past on the verifier is still refused. `TestVerifyToleratesClockSkewBetweenPods` mints
on clocks skewed in both directions and checks that skew beyond the allowance is refused.
