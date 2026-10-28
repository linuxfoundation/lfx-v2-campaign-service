# 2026-10-28 — audience QA check 4 is not applicable to a Post-Event send (#244)

**Fix** — `run-audience-qa` takes an optional `stage`, in the same vocabulary as a brief's stage
(one `briefStageEnum()` in the design now spells it for all three attributes, and
`TestPublishedStageEnumMatchesEmailStageNames` pins the published copies). For `Post-Event`,
check 4 (`current_registrants`) is reported with the new check-level verdict `NOT APPLICABLE`, no
findings and a `note`, instead of a false CRITICAL on a thank-you send whose audience is this
edition's attendees. It is reported rather than omitted, because an omitted check 4 means "no event
name was given". `CombineVerdicts` ignores it, so `overall` never takes it. The dispatch layer's
"send would reach no one" guard still overrides it. An absent stage, or any other stage, runs check
4 exactly as before. The check-4 result type is now `AudienceQaStageScopedCheck`, which is the base
check plus the fourth verdict and `note`, so the other three checks keep the three-verdict enum.
See [internal-dispatch](../code/internal-dispatch.md) and the `qa/run` row of
`docs/api-catalog.md`.
