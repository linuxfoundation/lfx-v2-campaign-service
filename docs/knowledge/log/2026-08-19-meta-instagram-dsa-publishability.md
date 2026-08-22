# 2026-08-19 — Meta: bind Instagram identity and EU DSA disclosure so ads publish

**Fix** — An end-to-end review of a live PAUSED Meta ad found the campaign
hierarchy correct but the ad NOT publishable: Meta greyed out Publish and flagged
"Please add Instagram account" on the ad and "Please add Advertiser" / "Please add
Payer" on the ad set. Both are fields the client never sent, so the ad was
API-accepted yet blocked at the publish gate.

Three optional `CampaignInput` fields (plumbed through the `metaConfig` dispatch
struct, like the existing `pixelId`) now close that gap. They are trimmed once in
`CreateCampaign` and attached only when non-empty, so Facebook-only /
non-regulated flows are unchanged and Meta never receives an empty string it would
reject:

- **`InstagramUserID`** (config `instagramUserId`) → `instagram_user_id`, a
  TOP-LEVEL adcreative field, sibling to `object_story_spec` (not nested in it).
  Required whenever the ad set requests an Instagram placement — the DEFAULT
  placements include Instagram Feed, which is why the reviewed ad tripped the flag
  despite the Page's connected Instagram account showing pre-selected in the
  editor. Legacy Graph field name: `instagram_actor_id`.
- **`DSABeneficiary` / `DSAPayor`** (config `dsaBeneficiary` / `dsaPayor`) →
  `dsa_beneficiary` / `dsa_payor` on the ad set. Meta blocks publish for regulated
  locations until both are present.

These deliberately live in per-campaign config, NOT the connection's persisted
`providerConfig` (whose keys are exhaustive-by-DB-column,
`domain/model/connection.go`), so the fix adds no stored column or migration. The
trade-off: a launch-ready config must supply all three when the ad set uses
Instagram placement and/or targets a regulated location.

No hard validation was added: a "reject Instagram placement without an
`InstagramUserID`" gate would fire on the default-placement path and break the
existing CreateCampaign suite (all of which run default placements with no IG
account) while changing unrelated behavior. The capability is additive; supplying
the config is what makes the ad publishable.

Tests: `internal/platform/meta/client_test.go` gains
`TestCreateCampaignBindsInstagramAndDSAFields` (fields reach the right payload
level, `instagram_user_id` top-level not nested, values trimmed) and
`TestCreateCampaignOmitsInstagramAndDSAWhenUnset` (whitespace-only inputs treated
as absent, keys omitted from both payloads). The concept file
[`code/internal-platform-meta.md`](../code/internal-platform-meta.md) gained a
"Publishability: Instagram identity and DSA disclosure" section.
