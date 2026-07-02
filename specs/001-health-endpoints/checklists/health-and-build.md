# Requirements Quality Checklist: Health Endpoints & Build/CI

**Purpose**: Validate that the requirements in the spec (health endpoints + build/CI/deploy scope) are complete, clear, consistent, and measurable before implementation.
**Created**: 2026-07-01
**Feature**: [spec.md](../spec.md)
**Depth**: Standard · **Audience**: Author (pre-commit self-check)

**Note**: This checklist tests the QUALITY OF THE REQUIREMENTS ("unit tests for English"), not the behavior of the code. Each item asks whether something is well-specified, not whether it works.

## Requirement Completeness

- [x] CHK001 Are the success response bodies for both endpoints specified within the functional requirements, rather than only appearing in the Assumptions section? [Completeness, Spec §FR-001/FR-002, §Assumptions] — Resolved: FR-001/FR-002/FR-005 now define the body as `OK\n`.
- [ ] CHK002 Is the exact set of conditions that makes the service "ready" at initial release explicitly defined, given that no external dependencies are wired yet? [Completeness, Spec §FR-008, §Assumptions]
- [x] CHK003 Are the "non-recoverable errors" that must trigger self-termination enumerated or otherwise characterized? [Gap, Spec §FR-009] — Resolved: FR-009 scopes the only current condition to startup failure (non-zero exit); retained as an extension point.
- [ ] CHK004 Is a readiness latency/performance expectation defined (a target exists for liveness in SC-002 but none for readiness)? [Gap, Spec §SC-002/SC-003]
- [x] CHK005 Are the criteria that make each container-publish workflow "working" defined (tag derivation, version source, target platforms, SBOM/signing/provenance)? [Completeness, Spec §FR-015] — Resolved: FR-015 now covers version/tag/digest, SBOM, Cosign signing, SLSA provenance, and linux/amd64+arm64 platforms.
- [ ] CHK006 Are the defining characteristics of the "production-build" binary specified (target OS/arch, static linking, embedded version metadata)? [Completeness, Spec §FR-011]
- [ ] CHK007 Is a Helm chart version-bump requirement documented for changes touching chart manifests, as the reference project requires? [Gap, Spec §FR-012, §Assumptions]

## Requirement Clarity

- [ ] CHK008 Is "success status" quantified for each endpoint instead of left generic? [Clarity, Spec §FR-001/FR-002]
- [x] CHK009 Is the specific formatting tool/standard for the new format step named (e.g., which Go formatter)? [Ambiguity, Spec §FR-011] — Resolved: FR-011 names `go fmt ./...` + `gofmt -s -w` (+ `gofmt -l` check).
- [ ] CHK010 Is "deployable artifact" defined consistently and clearly distinguished from the local production-build binary? [Clarity, Spec §FR-011/FR-012]
- [ ] CHK011 Is "able to accept inbound requests" expressed in observable terms that a test can assert? [Clarity, Spec §FR-002/FR-003]
- [ ] CHK012 Is "matching / following the reference project service" bounded to specific attributes rather than left open-ended? [Ambiguity, Spec §FR-013/FR-015]

## Requirement Consistency

- [x] CHK013 Are functional requirement IDs sequential and in order (FR-015 currently appears before FR-013/FR-014)? [Consistency, Spec §Functional Requirements] — Resolved: FRs reordered to FR-013, FR-014, FR-015.
- [ ] CHK014 Is terminology for the runtime platform used consistently ("orchestration platform" vs "Kubernetes" vs "cluster")? [Consistency, Spec §User Stories, §Success Criteria]
- [ ] CHK015 Do the readiness-gating statements in FR-003, SC-005, and User Stories 2/3 align without conflict? [Consistency, Spec §FR-003/§SC-005]
- [ ] CHK016 Are the endpoint paths (`/livez`, `/readyz`) referenced consistently across requirements, edge cases, and assumptions? [Consistency, Spec §FR-012, §Edge Cases]

## Acceptance Criteria & Measurability

- [ ] CHK017 Can "the app builds and serves the two endpoints end-to-end" be objectively verified via a stated criterion? [Measurability, Spec §FR-014]
- [ ] CHK018 Are pass/fail criteria for each local workflow step (build, format, lint, test, production-build) individually measurable? [Measurability, Spec §FR-011, §SC-001]
- [ ] CHK019 Is "working, deployable container images" given an objective acceptance criterion the author can check? [Measurability, Spec §FR-015]
- [ ] CHK020 Are the Success Criteria verifiable by the author locally, without relying on unstated infrastructure? [Measurability, Spec §Success Criteria]

## Scenario & Edge Case Coverage

- [ ] CHK021 Are requirements defined for the readiness "not ready during startup" transition, not just the steady state? [Coverage, Spec §Edge Cases, §FR-003]
- [ ] CHK022 Is liveness behavior during graceful shutdown captured as a requirement, not only as an acceptance scenario? [Coverage, Spec §User Story 1, §FR-009]
- [ ] CHK023 Are requirements defined for how probes reach the endpoints relative to the auth gateway (direct-to-pod vs routed through the gateway)? [Gap, Spec §FR-004]
- [ ] CHK024 Is the stability of the endpoint contract specified for when future readiness dependencies are added? [Coverage, Spec §FR-008]
- [ ] CHK025 Are CI regression scenarios (a publish workflow failing or producing an unusable image) addressed in requirements? [Gap, Spec §FR-015]

## Non-Functional Requirements

- [ ] CHK026 Are observability requirements (logging/tracing/metrics behavior, or intentional exclusion) specified for the high-frequency probe endpoints? [Gap, Spec §Edge Cases]
- [ ] CHK027 Are security requirements for the unauthenticated endpoints specified (no sensitive data in responses; not gated by the auth middleware/ruleset)? [Coverage, Spec §FR-004/FR-005]

## Dependencies & Assumptions

- [ ] CHK028 Is the assumption that the existing Helm probes are correct validated against the endpoints being added? [Assumption, Spec §Assumptions, §FR-012]
- [ ] CHK029 Is the dependency on the reference project service's conventions documented with a concrete source/version reference? [Dependency, Spec §FR-013]
- [ ] CHK030 Is the "no external runtime dependencies wired" assumption stated as a condition that changes readiness requirements if it later becomes false? [Assumption, Spec §FR-008, §Assumptions]

## Ambiguities & Conflicts

- [x] CHK031 Is it clear whether release/tag automation extras (signing, SLSA/provenance) are in or out of scope, given the tension between "matching the project service" (FR-015) and "no net-new pipelines" (Assumptions)? [Conflict, Spec §FR-015, §Assumptions] — Resolved: full parity chosen; Cosign + SLSA + SBOM are in scope (repair existing scaffold). Conflict wording removed.
- [x] CHK032 Is the response body a defined contract (e.g., `OK`) or only an example, risking divergent implementation and tests? [Ambiguity, Spec §Assumptions, §FR-010] — Resolved: body contract fixed to `OK\n` in FR-005 and Assumptions.

## Notes

- Check items off as the spec is updated to satisfy them: `[x]`
- Items marked `[Gap]` indicate a requirement that appears to be missing entirely.
- Items marked `[Ambiguity]`/`[Conflict]` indicate wording that should be tightened or reconciled.
- Resolving these before `/speckit-plan` reduces downstream rework.
