# 2026-08-19: LFXV2-3324 parity test tightening

**Update** — Tightened `assertProjectAPIAuthz` in `charts/lfx-v2-campaign-service/parity_test.go` to assert that the `project-api` rule includes the `project_slug_resolver_contextualizer` AND that the `openfga_check` object field specifically reads `.Outputs.project_slug_resolver_contextualizer.uid`.

The previous check was vacuous after LFXV2-3324's slug-to-UID resolution fix: `Captures.projectId` still appeared in the rendered block but only inside the contextualizer's `slug:` input field, not in the security-relevant `object:` field where it must be replaced with the resolved UID. The old assertion would pass even if someone accidentally reverted the `object:` field back to the raw slug, missing the exact security regression it exists to catch.

The tightening ensures the test actually pins the security invariant: the `object` field references the contextualizer's resolved UID output, preventing accidental reverts of the LFXV2-3324 fix.

Flagged independently by Copilot and Cursor on PR #155 during review.
