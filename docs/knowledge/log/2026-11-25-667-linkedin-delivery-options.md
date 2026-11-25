# 2026-11-25 — #667 LinkedIn Audience Expansion and Audience Network toggles

**Update** — `linkedInConfig` takes optional booleans `audienceExpansionEnabled` and
`offsiteDeliveryEnabled` (#552 gap 8), sent as the campaign's fields of the same names, behind
`LINKEDIN_ADS_DELIVERY_OPTIONS_ENABLED` (off by default). Omitted, or with the flag off, the
create body is unchanged: `offsiteDeliveryEnabled: false` (pinned in #133, because `true` opts into
the LinkedIn Audience Network's third-party placements) and no `audienceExpansionEnabled`
(LinkedIn's default, off). With the flag off, a create naming either, `false` included, is a 400
before a job exists. A non-boolean value is a 400, and so is the Audience Network on a text
create. A campaign reused by name keeps its own settings, and the result gets a warning step.

The settings readback reports both upstream-only, as `audience_expansion_enabled` and
`offsite_delivery_enabled`. This raises LinkedIn's `unknown_count` floor from 5 of 11 to 7 of 13
on a healthy created row (8 before LFXV2-2665, 12 adopted).

Not live-verified with `true`: a probe on the LF system account is owed before the flag is set.

Tests: `delivery_options_test.go` (format rule, body, text refusal before any request, reuse
warning, readback parse) and `linkedin_delivery_options_test.go` (gate, shapes, Dispatch refusal
with no requests, body end to end), plus the readback table in `linkedin_settings_test.go`.
