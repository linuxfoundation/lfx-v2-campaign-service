# 2026-11-14 — #550 Meta mobile OS targeting (gap 15, remainder)

**Creation** — The rest of gap 15 of the Meta gap review (#550). A Meta ad set could pick mobile or
desktop but not an operating system. `metaConfig.targeting.userOs` sends Meta `user_os` values: iOS,
Android, or either with a minimum version or a version range, using the formats and version lists
of Meta's Advanced Targeting reference. Values are matched ignoring case and sent in Meta's spelling.

Every rule is a synchronous 400, re-checked in Dispatch and in the client before any request: a value
outside those formats, a version of one family with any value of the other (the reference: "You
cannot target the minimum version of one platform with the other platform"), userOs with
devicePlatforms `[desktop]` (a service rule, since an OS list matches mobile devices only), an OS
version on the awareness objective (the reference's brand-awareness note), and more than 10 values.
An OS is a placement control, not an inclusion, so no Advantage+ flag is sent.

Behind its own default-off flag, `META_OS_TARGETING_ENABLED`, with the locale flag's rules. Not gated
by `global_marketing_ops`. Not live-verified: the payload.

Tests: `user_os_test.go` (canonical values on the ad set, eleven refusals with no request,
`CheckUserOS`) and `meta_os_targeting_test.go` (flag off and on, the flag interplay, the combination,
desktop and awareness 400s, a wrong type, Dispatch's wiring). Seventeen hand mutations each fail a
test.
