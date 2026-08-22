# 2026-08-19 — LFXV2-3324 preserve UUID connection lookups through the slug-to-UID fix

**Fix** — a Copilot review comment on PR #155 caught that the
`project_slug_resolver_contextualizer` addition (see
[2026-08-19-LFXV2-3324-slug-to-uid-fga-fix.md](2026-08-19-LFXV2-3324-slug-to-uid-fga-fix.md))
unconditionally ran on the shared `project-api` rule, which also covers
`design/connection.go`'s get/update/delete/test/set-credential routes.
`projectIDAttr()` there intentionally accepts a raw project UUID (to keep
historical UUID-keyed connection rows from migration 000003 reachable), but
the resolver's endpoint only does slug lookup and 404s on a UUID —
`continue_pipeline_on_error: false` turned that 404 into an outright deny,
breaking legitimate UUID-based requests.

Fix: added a Heimdall `if:` CEL guard (UUID regex against
`Request.URL.Captures.projectId`) so the resolver contextualizer only runs
for slugs, plus two mutually exclusive `openfga_check` entries — one per
branch — instead of one entry with a fallback expression, since whether a
skipped contextualizer's `Outputs` are safely empty or an error is not
documented Heimdall behavior.

See [ruleset.md](../kubernetes/ruleset.md) for the resulting branching logic.
