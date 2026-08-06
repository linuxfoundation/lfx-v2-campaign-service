# 2026-08-05 — GA-3b: resource-account and resource-kind validation

**Update** — Fixed resource-kind validation in `adGroupAdID`
(`internal/platform/googleads/adgroup_ad.go`): the parser validated only the trailing
numeric segment (`111~222`) of an AdGroupAd resourceName, so a resource of the wrong kind
(e.g. `customers/1/campaigns/111~222` instead of `customers/1/adGroupAds/111~222`) would be
incorrectly accepted as a confirmed AdGroupAd. This defeated the malformed-success handling
that prevents an ad ID from a wrong-type response being persisted. Fixed by validating the
resource KIND (the path segment must be `adGroupAds`, not `campaigns` or another type) before
accepting the composite numeric IDs. Added test cases for wrong-resource-kind and missing-kind
scenarios. All tests (`go test ./... -race`), linters (`go vet`, `gofmt`), and builds
(`go build ./...`) pass clean. Address PR #67 review feedback.

**Fix** — Resolve outstanding Copilot review thread on PR #67 GA-3b
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
