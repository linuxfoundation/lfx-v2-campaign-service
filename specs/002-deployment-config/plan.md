# Implementation Plan: Campaign Service Deployment Configuration

**Branch**: `feat-LFXV2-2558-deployment` | **Date**: 2026-07-08 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/002-deployment-config/spec.md`

## Summary

Review and complete the campaign-service deployment configuration so the service is
deployable to development, staging, and production via ArgoCD GitOps, aligned to the
platform standard exemplified by `lfx-v2-project-service` and `lfx-v2-committee-service`.

The work spans two repositories:

1. **`lfx-v2-campaign-service`** (Helm chart / templating): bring the chart into
   conformance with the standard and make it renderable, lintable, and deployable. Key
   corrections identified during research: define a real Heimdall `RuleSet` for the routed
   `/campaigns` and `/_campaigns/` paths (currently `rules: []`, which breaks gateway auth),
   align the env-var rendering guard to the `hasKey` pattern, guard `serviceAccountName`,
   add `topologySpreadConstraints` support, and reconcile the container image repository
   path across `values.yaml`, the local example, and the `Makefile`.
2. **`lfx-v2-argocd`** (GitOps registration): dev is already wired; add the staging and
   production `lfx-v2-applications` ApplicationSet entries (OCI, pinned), activate the
   staging/prod values files, complete the global values (secret wiring / topology),
   correct the dev image-updater `imageSuffix`, and update service inventory docs.

The technical approach is **conformance-by-comparison**: for every configuration surface,
diff campaign-service against the committee-service reference (chosen because campaign uses
ExternalSecrets + IRSA like committee) and either match it or document an intentional
deviation.

## Technical Context

**Language/Version**: N/A (deployment configuration — YAML: Helm charts, Kubernetes
manifests, ArgoCD ApplicationSets). Service itself is Go 1.24; not modified here except
possibly chart/CI files.

**Primary Dependencies**: Helm 3, Kubernetes (Gateway API `HTTPRoute`), Traefik gateway,
Heimdall (`RuleSet` CRD, forwardAuth `Middleware`), OpenFGA (edge authorization),
External Secrets Operator (`SecretStore`/`ExternalSecret`), ArgoCD + ApplicationSets,
`ko` image builder, GHCR OCI registry, AWS Secrets Manager + IRSA.

**Storage**: N/A for this feature. Campaign-service currently has **no** NATS KV/ObjectStore/
Stream chart resources and no database wiring; the app is health-endpoint + `/campaigns`
surface only. No NATS chart templates are added (see research decision R4).

**Testing**: `helm lint` + `helm template` (chart render validation), MegaLinter
(Kubernetes/Helm), ArgoCD `Synced`/`Healthy` status, pod probe readiness, gateway
reachability checks. No unit test framework applies to YAML config.

**Target Platform**: Three Kubernetes clusters — development
(`dev.v2.cluster.linuxfound.info`, AWS acct `788942260905`), staging
(`staging.v2.cluster.linuxfound.info`, acct `844790888233`), production
(`v2.cluster.lfx.dev`, acct `372256339901`).

**Project Type**: Multi-repo deployment/GitOps configuration (two repositories + two
read-only reference repositories).

**Performance Goals**: N/A (config). Availability-oriented defaults inherited from the
standard: prod `replicaCount: 3`, PDB `minAvailable: 2`.

**Constraints**:
- Staging/prod MUST use pinned, immutable chart versions (OCI) and image tags; dev tracks
  `HEAD` with the mutable `development` tag.
- A single namespace `lfx-v2-campaign-service` across all environments and files.
- Secrets, IAM roles, and the first published release artifact are external prerequisites
  (DevOps/CloudOps + service-repo CI); this feature owns references and config only.

**Scale/Scope**: One microservice onboarded across three environments. Concretely: ~7
chart files reviewed/edited in the service repo; ~9 files created/edited in the argocd repo
(2 ApplicationSet edits, 4 values files, 1 dev ApplicationSet edit for `imageSuffix`,
README, optional backstage).

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

The project constitution at `.specify/memory/constitution.md` is an **unpopulated template**
(placeholder principles only), so there are no ratified numbered principles to gate against.
In its absence, the de-facto governing constraints for this feature are the **platform
deployment conventions** documented in the reference services and the argocd repo's
`docs/agent-guidance/`, plus the repo workspace rules. This plan treats those as the
compliance bar:

| De-facto principle (from platform conventions & workspace rules) | Status |
|---|---|
| Follow the standard chart/template set used by reference services (no bespoke deviations without rationale) | PASS — plan is conformance-driven; deviations documented in research.md |
| Staging/prod pinned & immutable; dev mutable/HEAD | PASS — encoded in FR-013/014/016 and research R7 |
| Secrets/IAM are external handoffs; repos own references only | PASS — FR-018/024, documented as dependencies |
| License header on all source/config files | PASS — all new/edited YAML carries the MIT header (FR-022) |
| Surgical changes; match existing style | PASS — edits mirror committee-service exactly |
| GPG-signed, signed-off commits; Jira ref `LFXV2-2558` | PASS — enforced at commit time (git-workflow rule) |

**Result**: PASS (no unjustified violations). Complexity Tracking is empty.

## Project Structure

### Documentation (this feature)

```text
specs/002-deployment-config/
├── plan.md              # This file (/speckit-plan command output)
├── spec.md              # Feature specification (/speckit-specify)
├── research.md          # Phase 0 output — decisions R1..R9
├── data-model.md        # Phase 1 output — configuration entities
├── quickstart.md        # Phase 1 output — validation guide
├── contracts/           # Phase 1 output — config "contracts"
│   ├── applicationset-entry.md
│   ├── values-layering.md
│   └── chart-conformance.md
├── checklists/
│   └── requirements.md  # Spec quality checklist (already created)
└── tasks.md             # Phase 2 output (/speckit-tasks — NOT created here)
```

### Source Code (repositories touched)

This feature edits configuration in two repositories (both present as workspace worktrees).
No application source directories (`src/`, `cmd/`, `internal/`) are modified except CI/chart
files in the service repo.

```text
# Repo 1: lfx-v2-campaign-service  (chart + templating; primary chart work)
charts/lfx-v2-campaign-service/
├── Chart.yaml                     # name/version/appVersion (release-managed) — no change expected
├── values.yaml                    # canonical defaults — env-var scheme, image repo (canonical)
├── values.local.example.yaml      # RECONCILE image repo path (R1)
└── templates/
    ├── deployment.yaml            # EDIT: env hasKey guard, SA guard, topologySpread (R3, R5)
    ├── service.yaml               # conformant — no change
    ├── serviceaccount.yaml        # conformant — no change
    ├── httproute.yaml             # conformant (/campaigns, /_campaigns/) — verify vs ruleset
    ├── heimdall-middleware.yaml   # conformant — no change
    ├── ruleset.yaml               # EDIT: replace `rules: []` with real rules (R2) — blocking
    └── pdb.yaml                   # conformant — no change
Makefile                           # RECONCILE docker-build image path (R1)
.ko.yaml / .github/workflows/      # release automation present (verify ko-build-tag OCI publish)

# Repo 2: lfx-v2-argocd  (GitOps registration; dev done, staging/prod pending)
apps/
├── dev/lfx-v2-applications.yaml   # EDIT: add imageSuffix: /campaign-service (R6)
├── staging/lfx-v2-applications.yaml  # EDIT: add campaign OCI list entry (pinned)
└── prod/lfx-v2-applications.yaml     # EDIT: add campaign OCI list entry (pinned)
values/
├── global/lfx-v2-campaign-service.yaml   # EDIT: complete (secret refs / topology) (R8)
├── dev/lfx-v2-campaign-service.yaml      # present/active — verify
├── staging/lfx-v2-campaign-service.yaml  # ACTIVATE (uncomment, pin image tag)
└── prod/lfx-v2-campaign-service.yaml     # ACTIVATE (uncomment, pin image tag)
custom-resources/lfx-v2-campaign-service/
├── SecretStore.yaml               # present — conformant
└── ExternalSecret.yaml            # present — conformant
README.md                          # SHOULD: add campaign row to service table (FR-021)
.backstage-lfx-v2-campaign-service.yaml  # SHOULD: optional catalog entity (FR-021)

# Read-only reference repos (the "standard"):
#   lfx-v2-committee-service (primary reference — ExternalSecrets + IRSA + map env)
#   lfx-v2-project-service   (secondary reference)
```

**Structure Decision**: Multi-repo configuration change. Primary chart conformance work in
`lfx-v2-campaign-service/charts/`; GitOps registration in `lfx-v2-argocd/{apps,values,
custom-resources}`. The committee-service configuration is the authoritative template to
diff against, since campaign-service shares its ExternalSecret + IRSA + map-based-env
profile. Deviations that are intentional (e.g. campaign's OTEL `tracesSampleRatio` var
scheme vs committee's `tracesSampler`/`tracesSamplerArg`) are recorded in research.md.

## Complexity Tracking

> No constitution violations to justify. Section intentionally empty.
