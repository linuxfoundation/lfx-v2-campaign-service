# 2026-11-05 — Image asset helpers move to image_assets.go

**Update** — Follow-up to #492's review. `base64Image`, `imageAssetNamePrefix` and
`imageAssetName` move from `demandgen_creative.go` to their own `image_assets.go`, since all
five image-bearing creative paths use them, not only Demand Gen. No behaviour changes. The
Search assets test no longer counts `"name":` substrings in the request body; it decodes the
business-name text asset's create and checks that it carries no `name` key. See
`code/internal-platform-googleads.md` § Image asset names (#491).
