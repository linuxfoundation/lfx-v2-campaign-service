# 2026-11-06 — #550 Meta audience targeting: age, gender, custom audiences and the Advantage+ flag

**Creation** — `metaConfig.targeting` adds who-sees-it targeting to the Meta ad set, the top two
gaps of the Meta gap review (#550): `ageMin`/`ageMax`, `genders`, `customAudiences`,
`excludedCustomAudiences` (lookalikes included) and `advantageAudience`. Before this a Meta campaign
could target countries and placements only, so event retargeting, past-attendee audiences and
registrant suppression were impossible from LFX.

Since Graph v23.0 Meta defaults `targeting_automation.advantage_audience` to 1 and errors on a
non-default age, gender or audience inclusion without it, so any inclusion now sends the flag
explicitly — 0 unless the caller said `true` — and `true` is refused outside Meta's Advantage+
age limits (`age_min` ≤ 25, `age_max` 65). Exclusions alone leave the flag out, so that payload is
unchanged.

Every audience is read before the campaign is created and must belong to the connected ad
account; an invalid block is a synchronous 400. The block sits behind
`META_AUDIENCE_TARGETING_ENABLED` (chart default `"false"`) until a live probe confirms the
payloads, and the two audience lists need `global_marketing_ops` (`service.MetaMarketingOpsInputs`)
because the Meta ad account is shared by every foundation.

Tests: `targeting_test.go` pins the exact ad-set keys for seven shapes, ten refusals with no
request sent, and the foreign/unreadable audience cases with no POST; `meta_targeting_test.go`
covers the flag (off, `"TRUE"`, `"1"`), the 400s, a wrong-typed block, Dispatch's refusal and
wiring, and the gate classification; the service gate table adds six Meta cases. 26 hand-applied
mutations (each guard, the flag default, the inclusion rule, dedupe, the gender codes, the wiring
and the gate) each fail a test.
