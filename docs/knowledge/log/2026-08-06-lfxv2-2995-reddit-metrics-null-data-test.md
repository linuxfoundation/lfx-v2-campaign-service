# 2026-08-06 — Reddit metrics: null data field handling and documentation (LFXV2-2995)

**Update** — Added validation to reject a null `data` field as a malformed response. `json.Unmarshal`
accepts JSON `null` for a slice and leaves `rows` nil, so `{"data":null}` previously reached
`len(rows) == 0` and was reported as genuine zero-activity metrics. This contradicts the documented
contract that only an explicit empty array means genuine zero activity; a null field is either a
schema error or a malformed upstream response. The client now rejects a nil slice with the same
transport/decode error used for other malformed metrics responses. Added
`TestGetCampaignMetrics_NullDataFieldIsDecodeError` to cover this case; test confirmed binding by
reverting the fix and verifying it fails with the expected diagnostic.

Updated `docs/knowledge/code/internal-platform-reddit.md` OKF frontmatter `description` from
"Reddit Ads API v3 client: OAuth2 token refresh and Campaign -> Ad Group -> Ad creation." to
"Reddit Ads API v3 client: OAuth2 token refresh, Campaign -> Ad Group -> Ad creation, best-effort
campaign metrics reads (UNVERIFIED contract)." to accurately reflect the package's expanded role
and surface area. This change is required by CLAUDE.md:19-23, which mandates that the frontmatter
description stay synchronized with the knowledge page's documented capabilities when they change.
