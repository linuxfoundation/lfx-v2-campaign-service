# 2026-08-20 — LFXV2-3324 UUID-branch parity pinning and regex dedup

**Fix** — `assertProjectAPIAuthz` in `charts/lfx-v2-campaign-service/parity_test.go`
pinned only the slug branch of the `project-api` rule's two mutually exclusive
`openfga_check` entries. Nothing asserted that the second, UUID-branch
`openfga_check` existed, read the raw `Captures.projectId` object, or that its
`if:` guard was the exact negation of the slug branch's guard — deleting the
UUID-branch entry entirely still passed every assertion. Since a UUID-form
`:projectId` (the historical migration-000003 rows) makes both slug-branch
guards false, a future edit that dropped or mis-guarded the UUID branch would
silently stop enforcing `openfga_check` for every UUID-form request, with the
parity test still green.

Tightened the function to require exactly two `openfga_check` authorizers and
two `campaign_manager` relations, assert the UUID branch's object reads the raw
`project:{{- .Request.URL.Captures.projectId -}}` capture, and assert both a
negative (`!Request...matches(...)`) and a positive (`Request...matches(...)`)
`if:` guard are present.

**Update** — The UUID regex
(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
was hand-duplicated across all three `if:` guards in
`charts/lfx-v2-campaign-service/templates/ruleset.yaml` (the resolver
contextualizer, and both `openfga_check` entries). Hoisted it into a single
Helm template var (`$uuidRe`) defined once in the `execute:` block and
referenced by all three guards, so the slug/UUID branches provably share one
source string instead of three copies that could drift out of exact-negation
sync. Rendered output is unchanged (verified via `helm template`).

Both flagged by @dealako on PR #155.
