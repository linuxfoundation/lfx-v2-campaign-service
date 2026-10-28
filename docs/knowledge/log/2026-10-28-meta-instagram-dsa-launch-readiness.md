# 2026-10-28 — Meta: EU DSA disclosures required for EU targets, refused synchronously

**Update** — Meta creates were not launch-ready: `instagramUserId`, `dsaBeneficiary` and
`dsaPayor` already existed in `metaConfig`, but nothing required them, and the checks that did
exist (a malformed IGSID, a one-sided DSA pair) ran only in the client, after the 202, as an
opaque job failure. An EU-targeted create without disclosures "succeeded" into an ad set Meta
will not publish ("Please add Advertiser" / "Please add Payer").

`meta.CheckPublishIdentity` (`internal/platform/meta/publish_identity.go`) now owns the rules,
and `MetaDispatcher.PreflightCreate` runs it, so each is a synchronous 400 before any job:

- **EU rule:** when `geoTargets` (after `validateGeoTargets`) include any of the 27 EU member
  states or AX, GF, GP, MQ, RE, YT, MF, both `dsaBeneficiary` and `dsaPayor` are required. This
  follows Meta's ad-set reference: "For ad sets targeting the EU and/or associated territories,
  the dsa_payor and dsa_beneficiary fields are required."
- A one-sided pair, or a disclosure over 256 runes or with a control character, is refused.
- A non-numeric `instagramUserId` is refused. An absent one is not, because that would refuse
  every current Meta caller. The client now binds `instagram_user_id` to the creative only when
  an Instagram placement (Feed, Stories, Reels) is enabled.

`Dispatch` and the client run the same check again for any path that skipped the pre-check.
The design's `config` description and example document the fields, and `make apigen` was
re-run. Tests: `TestCheckPublishIdentity`, `TestUsesInstagram`,
`TestCreateCampaignBindsInstagramOnlyForAnInstagramPlacement`,
`TestCreateCampaignRefusesEUWithoutDSABeforeAnyRequest`,
`TestMeta_PreflightCreate_PublishIdentity`, `TestMeta_DispatchRefusesEUWithoutDSABeforeAnyRequest`,
`TestCreateCampaigns_MetaEUWithoutDSAIsSynchronous400`. See
[internal/platform/meta](../code/internal-platform-meta.md).

**Rollout:** a caller that targets the EU on Meta without the disclosures now gets a 400 where
it used to get an unpublishable campaign. Self Serve collects the fields behind a capability
flag.
