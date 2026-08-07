# 2026-08-05 — LFXV2-2023 GA-3b: close the outstanding Copilot review thread

**Update** — Resolve outstanding Copilot review thread on PR #67 GA-3b
(`internal/platform/googleads/adgroup_ad.go:217` and `:258`, current lines ~214/~253).

`createAdGroupAndAd` accepted the ad group and ad resource names returned by
`firstResourceName`/`adGroupAdID` without checking they belonged to THIS account —
only `adGroupAdID` checked resource *kind*. A wrong-customer resource name in a
malformed/substituted 2xx could be accepted and its id persisted as `AdGroupID`.

Generalized `validateCampaignResource` into `(*Client).validateResourceKind(kind,
resourceName string, requireNumericID bool) error`, which checks segment count,
resource kind, AND that `pathParts[1] == c.account.CustomerID`. `createAdGroupAndAd`
now calls it for both the ad-group resource (`"adGroups"`, numeric id) and the ad
resource (`"adGroupAds"`, composite id — `requireNumericID=false`, since
`adGroupAdID` validates the composite shape separately) before trusting either.
`validateCampaignResource` is now a thin wrapper over the same helper.
