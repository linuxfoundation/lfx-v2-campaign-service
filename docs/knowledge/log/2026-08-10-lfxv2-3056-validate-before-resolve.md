# 2026-08-10 — Campaign ID validation happens before connection resolution in adoption

**Fix** — The LookupCampaign dispatcher validated platform-supplied campaign IDs only
AFTER resolving (and decrypting credentials for) the project's connection. Consequently,
a malformed ID like `"007"` returned the connection's status error (409 when the connection
is unusable) instead of the documented permanent input fault (400 for `ErrInvalidPlatformCampaignID`).

Campaign ID validation now happens BEFORE resolving the connection, in the dispatcher layer.
A malformed ID is a permanent input fault regardless of connection state, so it should produce
the same 400 error either way. Validating first also avoids decrypting credentials for a
request that can never succeed and guarantees the permanent fault masks any contingent
connection fault.

## Why the ordering matters

The API contract (docs/api-catalog.md) explicitly distinguishes:
- `400` for a malformed `platform_campaign_id`
- `409` when the project's connection is unusable

The two are behaviorally different, and NEITHER of them is a wait-and-retry. A 400 says the
request itself is wrong: fix the id and re-request. A 409 says the project's connection needs
a configuration change before any adoption can work — connect the project's own ad account,
select an ad account on the connection, or repair its stored credentials
(`internal/service/brief.go:592-607`). Adoption's genuinely transient and unverifiable
outcomes are `503`, not `409`, so a client that reads a 409 as "the platform is down, try
again later" will retry forever without making the change that would let the request succeed.

When a malformed ID was checked AFTER resolution, a caller with an unusable connection was
sent to repair the connection for a request that would still have failed on the id afterwards
— pointed at the contingent fault instead of the permanent one that actually blocked them.

## The fix

`GoogleAdsDispatcher.LookupCampaign` now calls `googleads.ValidateCampaignID(platformCampaignID)`
BEFORE `resolveOwnedGoogleAdsClient`. The validation function — new in this commit — wraps
the existing `canonicalCampaignID` check and returns `googleads.ErrNotACampaignID` for
permanent faults.

Campaign IDs are validated as canonical base-10 spellings of positive int64 values. Rejections
include leading-zero spellings like `"007"`, zero, values past math.MaxInt64, and any
non-numeric input.

## No other paths are affected

The reordering is specific to `LookupCampaign`. Other adopt/lookup paths like `ToggleStatus`
and `ReadMetrics` receive campaign IDs from persisted rows (already validated once), not as
caller inputs, so they do not carry the same burden. The ordering problem was unique to
adoption because it takes `platformCampaignID` as a raw input parameter.

## Tests

`TestGoogleAds_LookupCampaign_MalformedIDIs400EvenWithUnusableConnection` covers the
documented ordering: it uses the REAL dispatcher with a deliberately unusable connection
and verifies that malformed IDs are rejected with `ErrInvalidPlatformCampaignID` (400)
before any platform call is made. A regression that moves the validation AFTER resolution
will fail this test because the connection error will win.

## Documentation updates

- `docs/api-catalog.md` clarified that malformed IDs are validated before the connection
  state is checked, so they always return 400 regardless of connection availability.
- `docs/knowledge/code/internal-dispatch.md` added a note to the CampaignAdopter section
  stating the ordering guarantee.
