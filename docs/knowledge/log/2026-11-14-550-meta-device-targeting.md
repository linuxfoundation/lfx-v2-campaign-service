# 2026-11-14 — #550 Meta device targeting

**Creation** — Gap 15 of the Meta gap review (#550). A Meta ad set always served on every device.
`metaConfig.targeting.devicePlatforms` (`mobile`, `desktop`) sends one device as the ad set's
`targeting.device_platforms`; both, or none, is Meta's default and sends nothing, so the payload is
unchanged. A device is a placement control, not an audience inclusion, so no Advantage+ flag is
sent. Desktop-only with the Audience Network placement, which serves in mobile apps only, is refused
before any request (`meta.CheckDevicePlacements`, in the client and in `checkMetaTargeting`), so it
cannot orphan a campaign at the ad set.

Behind its own default-off flag, `META_DEVICE_TARGETING_ENABLED`, with the same rules as the locale
flag: a block that sets only `devicePlatforms` needs no `META_AUDIENCE_TARGETING_ENABLED`. Not
gated by `global_marketing_ops`.

Tests: `targeting_options_test.go` (one device sent, both send nothing, the refusals with no
request, `CheckDevicePlacements`) and `meta_targeting_options_test.go` (flag off and on, the flag
interplay, a wrong type, Dispatch's wiring and refusal). Ten of eleven hand mutations fail a test.
The survivor drops the placements from Dispatch's own call to `checkMetaTargeting`: the client
refuses the same pair before any request, so the job's outcome is identical (an equivalent mutant;
the dispatcher check is defence in depth).
