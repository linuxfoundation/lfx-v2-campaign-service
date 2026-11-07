# 2026-11-06 — #550 Meta call-to-action and conversion event

**Creation** — Gaps 4 and 5 of the Meta gap review (#550). Every Meta ad carried a fixed
`LEARN_MORE` button, and every conversions campaign optimized for a fixed `PURCHASE` pixel event,
which a free event's registration never fires. `metaConfig.callToAction` (LEARN_MORE, SIGN_UP,
BUY_TICKETS, APPLY_NOW, BOOK_NOW, SUBSCRIBE, DOWNLOAD, WATCH_MORE, CONTACT_US) and
`metaConfig.conversionEvent` (PURCHASE, COMPLETE_REGISTRATION, LEAD, SUBMIT_APPLICATION, SUBSCRIBE;
conversions objective only) replace them. Omitted, both payloads are byte-for-byte the old ones.
`REGISTER_NOW` was in the gap table but is not a value of Meta's call-to-action enum, so it is
refused.

Each sits behind its own default-off flag (`META_CALL_TO_ACTION_ENABLED`,
`META_CONVERSION_EVENT_ENABLED`) until a live probe, and every refusal — flag off, outside the list,
an event on a non-conversions objective, a wrong JSON type — is a synchronous 400 and is re-checked in
the job before any request.

Tests: `creative_options_test.go` (the button on every creative, the event beside the pixel, six
refusals with no request sent) and `meta_creative_options_test.go` (both flags off and on, each flag
gating only its own field, wrong types, Dispatch's refusal and wiring). Every guard, default and
wiring line was mutated by hand and each mutation fails a test.

PR #559 review: the result now records `ConversionEvent`, the `custom_event_type` of the ad set THIS
run created (PURCHASE by default on conversions, empty otherwise), and leaves it empty when the ad set
was reused by name — the `BidApplied` rule — so the conversions metrics read (gap 6) filters on the
event the live ad set really optimizes for, not on the config snapshot. `metaIdentityKeys` /
`checkMetaIdentityShape` are renamed `metaStringConfigKeys` / `checkMetaStringFieldShape`, since they
now type-check the two creative options too, and the flag tests set both flags explicitly.
