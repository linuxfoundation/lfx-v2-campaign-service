# 2026-08-07 — LFXV2-2023: the keyword plan broke two of its own rules

**Update** — Two review findings on the merged plan (#81), both cases of the document stating a
rule and then violating it elsewhere in itself.

**Fix 1 — a validation helper that could not validate.**
`keywordCriterionResourceName(adGroupID, criterionID string) string` interpolated two
request-body-controlled ids into a resource path and returned only a string, so it had no way to
reject either. That is the exact injection shape `numericID` exists to stop —
`internal/platform/googleads/adgroup_ad.go` says so outright: "so an id interpolated into a
resourceName can't alter the resource path". It also made the plan's own listed invalid-ID test
unwritable, since there would be no failure to assert. The signature now returns
`(string, error)` and rejects either malformed component before a mutate operation is built.

**Fix 2 — the PR order published an endpoint ahead of its implementation.**
PR 1 shipped the Goa design, the OpenAPI document, and the handlers; PR 2 shipped the Google Ads
adapter. Between those two merges `main` would publish two documented Google Ads keyword
endpoints that returned 400 for every caller. The plan's own phase-boundary rule already forbids
this for the `change-bid` enum member — publishing something "in the generated client and the
OpenAPI document while the handler rejects them" is "an advertised capability that does not
exist" — and the rule does not stop applying because the unimplemented thing is an endpoint
rather than an enum member.

The sequence is now PR **A** (platform client, internal-only, publishes nothing) then PR **B**
(design + handlers + dispatcher methods together). Both stay under the 1000-line cap. PR A is
dead code between the merges, which is acceptable in a way the alternative is not: unreachable
internal code misleads nobody, a published OpenAPI operation does.

**Note** — The bad ordering was defended by analogy: `get-campaign-metrics` behaved this way
between its foundation PR and its first adapter. Precedent describes what was done, not what is
right, and an analogy is not an argument against a rule the same document states explicitly. Both
findings survived review of the plan only because a plan is prose — nothing compiles it, so a
contradiction between two of its sections costs nothing until someone implements from it.
