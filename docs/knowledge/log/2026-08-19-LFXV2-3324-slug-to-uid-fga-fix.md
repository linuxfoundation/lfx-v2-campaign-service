# 2026-08-19 — LFXV2-3324 project slug-to-UID FGA object fix

**Fix** — the `project-api` RuleSet rule checked `campaign_manager` against
`project:{{ .Request.URL.Captures.projectId }}`, but `:projectId` is a project
**slug** on this API (self-serve's `campaign-service.service.ts` sends slugs,
never UIDs), while OpenFGA tuples for `campaign_manager`/`marketing_ops` are
keyed on project **UID**. The check could never match — a total lockout of the
entire project-nested campaign API once this authz code was promoted past dev.

Fix: run the new `project_slug_resolver_contextualizer` (defined centrally in
`lfx-v2-helm`, calling `lfx-v2-project-service`'s `GET
/projects/slug-to-uid/{slug}`) before `openfga_check`, and read the resolved
UID from `.Outputs.project_slug_resolver_contextualizer.uid` instead of the raw
capture. Query-service's slug index was considered and rejected as the
resolver source — it is eventually consistent, which is unacceptable for an
authorization-critical check right after project create/rename.

Part of a 3-repo sequence tracked by LFXV2-3324 (subtask of epic LFXV2-2231,
gap G7): `lfx-v2-project-service` PR #100 (resolver endpoint) →
`lfx-v2-helm` PR #165 (contextualizer) → this change (RuleSet update).

See [ruleset.md](../kubernetes/ruleset.md) for the resolution mechanics.
