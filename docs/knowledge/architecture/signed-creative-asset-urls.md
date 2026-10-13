---
type: "Architecture Doc"
title: "Signed Creative-Asset URLs for Reddit"
description: "How a Reddit dispatch uses a stored creative asset: the service serves that one asset at a short-lived HMAC-signed public URL minted inside the dispatch and passed to Reddit as media_url, behind an anonymous GET/HEAD route that answers every rejection with one bare 404, off unless a signing key and https base URL are configured."
resource: "internal/assetsign/assetsign.go"
---

# Signed Creative-Asset URLs for Reddit

LFXV2-2665. Reddit Ads API v3 accepts a promoted-post image only as a **public URL that Reddit
fetches and re-hosts** while the post is created (`createPromotedPost` sends `content[].media_url`;
there is no upload endpoint — see `log/2026-08-20-reddit-author-promoted-post.md`). Images uploaded
through `upload-creative-asset` live in Postgres and were usable only by Meta. Product decision
(2026-10-13): to let `redditConfig.imageAssetId` reference a stored asset, the service serves that
asset's bytes at a short-lived, HMAC-signed URL and passes the URL as `media_url`.

## Flow

1. **Create request** — `create-campaigns` runs the Reddit dispatcher's `PreflightCreate`. Config
   alone decides: `imageAssetId` with `imageUrl` or `postUrl`, a non-UUID id, or the feature off is
   refused **synchronously with 400** (`domain.ErrCreateConfigInvalid`, mapped in `mapBriefErr`;
   the orchestrator returns it without recording an upstream call). Feature off reads
   "stored creative assets are not enabled for Reddit; supply imageUrl".
2. **Dispatch** (`internal/dispatch/reddit_asset.go`) — the same rules again, then
   `GetAssetSize(project, brief, asset)` proves the asset exists **for this brief** under an
   active brief (foreign, missing, or empty → pre-create failure, claim released, no Reddit call).
   It also checks the ids can be carried in a signed URL (`assetsign.ValidateIDs`). It does **not**
   mint the URL: it passes `reddit.CampaignInput.ImageURLSource`, which the client calls
   **immediately before the promoted-post request** that hands the URL to Reddit — after account
   verification, the token exchange and any `Retry-After` wait. Minting in Dispatch (the #310
   original) let a throttled account check spend a short TTL before Reddit fetched, ending in a
   404 and a `created_degraded` campaign with no ad. A source failure at that point is still
   pre-create (nothing exists yet), and the URL is otherwise used exactly like `imageUrl`. All downstream
   behaviour — failed / UNCONFIRMED / `created_degraded` classification and claim retention — is
   the `imageUrl` path, unchanged. `cfg` is not rewritten, so the persisted config snapshot keeps
   `imageAssetId` and never the URL. Reddit error text that echoes `media_url` is scrubbed of the
   query (`scrubErr`, which keeps `errors.Is/As`) before it reaches the job error, `Steps` or
   `AdWarning` — defense in depth: the Reddit client today reports only the status of a failed
   request, never its body.
3. **Fetch** — Reddit GETs the URL during post creation. The service verifies and serves
   (`internal/service/signed_asset.go`, mounted by `cmd/campaign-service/signed_asset_mount.go`).
   Reddit re-hosts the image, so the URL only has to live through that one call.

## Token

```
GET|HEAD /_campaigns/creative-assets/{project_id}/{brief_id}/{asset_id}?exp=<unix>&sig=<43 chars>
sig = base64url_nopad(HMAC-SHA256(key, "lfx-v2-campaign-service/creative-asset-url/v1" 0x00
        || len32(project) project || len32(brief) brief || len32(asset) asset || u64be(exp)))
```

- **Unambiguous encoding**: every id is length-prefixed, so no choice of ids can make two
  different tuples sign the same bytes; the label binds the key to this one purpose.
- **Canonical ids**: `Mint` renders brief and asset as canonical lowercase UUIDs; `Verify`
  compares exactly what arrives, so another spelling of the same UUID fails.
- **Constant-time**: the MAC is compared with `hmac.Equal` (`macEqual`, which a test swaps to
  prove it is used); the MAC is computed even when the expiry is already bad.
- **Expiry**: `exp` must be in the future and at most `MaxTTL` (60 min) away — a validly signed
  URL claiming a later expiry is refused, so a leaked key cannot mint long-lived URLs this
  service honours. Default TTL 10 min; configurable within 5–60 min. The 5-minute floor is
  what the URL must outlive: the promoted-post request it is minted for (the Reddit client's
  30 s timeout), Reddit's own fetch inside that request, and clock skew between the minting and
  serving pods.
- The signature is in the **query**, not the path, on purpose: the Reddit client's error
  redaction (`redactURL`) keeps scheme, host and path but drops the query, and otelhttp v0.69
  records `url.path` only.

## Serving route

- **Unauthenticated by design**; the signature is the authorization. Its own Goa service
  (`lfx-v2-campaign-service-signed-assets`) with no Security, hidden from the OpenAPI documents.
- **Path traversal is impossible**: `brief_id`/`asset_id` are Goa `Format(UUID)`, `project_id`
  is `^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`, `sig` is `^[A-Za-z0-9_-]{43}$`, `exp` is Int64 ≥ 1. Only
  `exp` and `sig` are read from the query; nothing else is honoured. There is no listing.
- **Order**: the HMAC is verified **before any database access**; an unsigned request costs one
  MAC. Then the asset is re-checked at serve time (`GetAssetSize`, then `GetAsset`, both scoped
  to project + brief + an active brief), so an asset whose brief was archived after minting
  stops being served.
- **One generic 404**: decode failures and every service rejection reach the generated encoder's
  default arm and are rendered by the mount's formatter as a **bodyless 404 with identical
  headers** — feature off, malformed id or query, bad signature, expired, tampered field,
  missing/foreign/archived asset, a stored type outside PNG/JPEG. The service returns a plain
  sentinel, not a Goa named error, precisely so no `goa-error` header can differ between causes.
  The only other status is a bodyless **503**, and only after a signature verified (store not
  yet bound during a cold start, a store error, or the asset-memory budget exhausted). No internal
  or upstream error text is ever written.
- **Response**: `Content-Type` = the verified stored MIME type (`image/png` | `image/jpeg`),
  `Content-Length`, `X-Content-Type-Options: nosniff`, `Content-Disposition: inline`,
  `Cache-Control: no-store`. `no-store` rather than `private, max-age=<remaining>`: the URL has
  one intended reader fetching once, so caching buys nothing, while any cache (gateway, a CDN
  added later, the fetcher's own) is a copy that can outlive the signature. HEAD is supported
  (Goa forbids response bodies on HEAD, which is also why the error responses are bodyless).
- **Memory bound**: the bytes are priced with `GetAssetSize` and reserved on the **same
  process-wide `dispatch.AssetReserver`** the Meta and Microsoft dispatches charge (`Reserve`, exported for
  this), before `GetAsset` loads them; the reservation is released when the generated handler
  closes the body. The Reddit dispatch itself reserves nothing: it holds a URL, not bytes, and
  the reserver's contract is to charge memory actually held.
- **Logging**: asset id and expiry only — never the signature or the URL, on mint or serve.

## Configuration and exposure

| Variable | Source | Rule |
| --- | --- | --- |
| `CREATIVE_ASSET_URL_SIGNING_KEY` | Secret `lfx-v2-campaign-service-secrets` key `creative-asset-url-signing-key` (optional ref) | standard base64, ≥ 32 bytes |
| `CREATIVE_ASSET_PUBLIC_BASE_URL` | chart value (empty by default) | `https` origin only: host, no userinfo/path/query/fragment |
| `CREATIVE_ASSET_URL_TTL` | chart value `10m` | Go duration, 5m–60m |

Every value that is SET is validated first — even when the other half is missing and the feature
would be off — and all unusable values are reported together. Then: either of the first two unset
→ feature **off** (route 404s, `imageAssetId` refused with 400). Any value set but unusable → the
container refuses to start (`assetsign.New`), because a security
control that silently degrades on a typo is worse than a failed rollout. `Config.String()` masks
the key.

Gateway: the HTTPRoute's existing `/_campaigns/` PathPrefix already forwards the route through
Heimdall. The RuleSet adds ONE explicitly anonymous rule, `signed-creative-asset:get` — GET/HEAD
only, exactly `/_campaigns/creative-assets/:projectId/:briefId/:assetId`, `anonymous_authenticator`
→ `allow_all` → `create_jwt`, no wildcard. Every other `/_campaigns/**` path stays on the
deny_all placeholder. `charts/.../signed_asset_rule_test.go` pins the scope, that no other rule
became anonymous-only, and that the route is forwarded.

## Residual risk (accepted)

- Anyone holding a live URL can fetch **that one image** until it expires. Acceptable: tokens are
  minted only server-side inside the dispatch and never returned to API clients, they expire in
  minutes, and Reddit publishes the image anyway.
- Gateway or fetcher access logs may record the full URL. Same bound: one image, minutes.
- The route reveals, to a holder of a valid URL only, whether the asset is still servable (200
  vs 404) and capacity (503) — nothing a valid-URL holder could not infer.
- Key rotation invalidates only URLs minted within the last TTL; an in-flight dispatch that
  loses its fetch degrades like any other post-authoring failure (`created_degraded`).
- No database migration: `GetAsset`/`GetAssetSize` already scope by project, brief and active brief.

## Tests

- `internal/assetsign/assetsign_test.go` — round trip, each field tampered, fields swapped or
  absorbed, expired / at-expiry / beyond `MaxTTL`, wrong key, malformed signatures,
  constant-time compare used, config validation (short key, http base, path/query/userinfo, TTL bounds).
  Every partial combination of the three settings (#310 review), and all errors reported together.
- `internal/platform/reddit/image_url_source_test.go` — the late URL is obtained after account
  verification and before the post; a source failure is pre-create with no mutation.
- `TestRedditAsset_MintedAfterAccountCheckSurvivesASlowPrePostStep` — an hour-long pre-post
  step no longer expires the URL; its expiry is measured from after the account check.
- `cmd/campaign-service/signed_asset_mount_test.go` — headers and bytes, HEAD, reservation
  released, every rejection an identical bodyless 404, store untouched for unverified requests,
  ownership re-check, feature-off 404, budget-exhausted 503.
- `internal/dispatch/reddit_asset_test.go` — asset path signs a URL that verifies, `imageUrl`
  path unchanged, neither keeps the no-image path, every refusal pre-create with zero Reddit
  calls, synchronous preflight refusal, signature scrubbed from echoed upstream errors.
- `internal/service/create_config_invalid_test.go`, `internal/container/signed_asset_wiring_test.go`,
  `internal/infrastructure/config/signed_asset_config_test.go`.
