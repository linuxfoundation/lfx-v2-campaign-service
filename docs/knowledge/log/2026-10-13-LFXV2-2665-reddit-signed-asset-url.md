# 2026-10-13 — Reddit creatives from stored assets via signed URLs (LFXV2-2665)

**Update** — `redditConfig.imageAssetId` lets a Reddit campaign use an image stored through
`upload-creative-asset`. Reddit only accepts a public `media_url`, so the dispatch proves the asset
belongs to the brief, mints a short-lived HMAC-SHA256 URL (`internal/assetsign`) and hands it to the
existing author-a-post path; the service serves that one asset at
`GET|HEAD /_campaigns/creative-assets/{project}/{brief}/{asset}?exp&sig` — anonymous by design,
signature verified before any database read, ownership re-checked at serve time, every rejection
one bodyless 404, `no-store`, bytes charged to the shared `AssetReserver`. New env
`CREATIVE_ASSET_URL_SIGNING_KEY` (Secret key `creative-asset-url-signing-key`),
`CREATIVE_ASSET_PUBLIC_BASE_URL`, `CREATIVE_ASSET_URL_TTL`; either of the first two unset is the
feature off, and a Reddit config naming an asset is then refused with a synchronous 400 via the new
`domain.ErrCreateConfigInvalid` preflight sentinel. The RuleSet gains one anonymous GET/HEAD rule
scoped to exactly that route. No migration. See `architecture/signed-creative-asset-urls.md`.
