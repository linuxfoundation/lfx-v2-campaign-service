# Campaign Service Deployment Configuration Inventory

**Feature**: 002-deployment-config | **Updated**: 2026-07-08 | **Refs**: LFXV2-2558

A file-by-file inventory of every campaign-service deployment configuration file across both
repositories, its conformance status against the reference standard (committee-service), and
the external (non-repository) prerequisites that gate each environment. Satisfies FR-023,
FR-024, SC-006, SC-008.

## Repository 1 — `lfx-v2-campaign-service` (Helm chart + release CI)

| File | Status | Notes |
|---|---|---|
| `charts/lfx-v2-campaign-service/Chart.yaml` | ✅ conformant | name/version(0.0.1 CI-replaced)/appVersion pattern |
| `charts/lfx-v2-campaign-service/values.yaml` | ✅ updated | canonical image path; added `topologySpreadConstraints: []` (T004, R5) |
| `charts/lfx-v2-campaign-service/values.local.example.yaml` | ✅ fixed | image path → `.../campaign-service` (T005, R1) |
| `templates/deployment.yaml` | ✅ fixed | `hasKey` env guard, SA guard, topology block (T003, R3/R5) |
| `templates/service.yaml` | ✅ conformant | port 8080, selector |
| `templates/serviceaccount.yaml` | ✅ conformant | gated on `serviceAccount.create` |
| `templates/httproute.yaml` | ✅ conformant | `/campaigns`, `/campaigns/`, `/_campaigns/` |
| `templates/heimdall-middleware.yaml` | ✅ conformant | gated on `add_middleware` |
| `templates/ruleset.yaml` | ✅ fixed | `rules: []` → openapi + campaigns collection/item rules (T002, R2) |
| `templates/pdb.yaml` | ✅ conformant | min/max XOR validation |
| `Makefile` | ✅ fixed | docker image path → canonical GHCR (T006, R1) |
| `.ko.yaml` | ✅ conformant | build id `campaign-service` |
| `.github/workflows/ko-build-tag.yaml` | ✅ verified | publishes image + OCI chart + cosign + SLSA (T007, FR-010) |
| `.github/workflows/ko-build-main.yaml` | ✅ conformant | `development` + SHA tags on main |
| NATS templates (kv/object/stream) | ⛔ intentionally absent | no NATS client wired yet (R4) |

## Repository 2 — `lfx-v2-argocd` (GitOps registration)

| File | Status | Notes |
|---|---|---|
| `apps/dev/lfx-v2-applications.yaml` | ✅ updated | entry present; added `imageSuffix: /campaign-service` (T011, R6) |
| `apps/staging/lfx-v2-applications.yaml` | ⏳ pending release | OCI entry to add at first release (T014, gated) |
| `apps/prod/lfx-v2-applications.yaml` | ⏳ pending release | OCI entry to add at first release (T017, gated) |
| `values/global/lfx-v2-campaign-service.yaml` | ✅ completed | added topology, serviceAccount, documented secret wiring (T009, R8) |
| `values/dev/lfx-v2-campaign-service.yaml` | ✅ conformant | dev domain/tag/IRSA/resources (verified T012) |
| `values/staging/lfx-v2-campaign-service.yaml` | ⏳ pending release | activate + pin image tag (T015, gated) |
| `values/prod/lfx-v2-campaign-service.yaml` | ⏳ pending release | activate + pin image tag (T018, gated) |
| `custom-resources/lfx-v2-campaign-service/SecretStore.yaml` | ✅ conformant | AWS SM via IRSA |
| `custom-resources/lfx-v2-campaign-service/ExternalSecret.yaml` | ✅ conformant | tag `service-lfx-v2-campaign-service: enabled` |
| `README.md` | ✅ updated | service table row (T020) |
| `.backstage-lfx-v2-campaign-service.yaml` | ✅ created | catalog Component (T021) |
| `values/global/lfx-platform.yaml` umbrella disable | ⛔ N/A | only when campaign becomes a subchart of `lfx-v2-helm` (T023, R9) |

Legend: ✅ done/conformant · ⏳ pending an external gate · ⛔ intentionally absent / N/A.

## External prerequisites (out of repo — gate deployment)

| Prerequisite | Owner | Gates |
|---|---|---|
| First published OCI chart + image (`v*` tag → `ko-build-tag.yaml`) | service-repo CI / release manager | staging (US2), prod (US3) |
| IRSA IAM role `lfx-v2-campaign-service` in AWS acct `788942260905` | DevOps/CloudOps | dev ExternalSecret sync |
| IRSA IAM role `lfx-v2-campaign-service` in AWS acct `844790888233` | DevOps/CloudOps | staging ExternalSecret sync |
| IRSA IAM role `lfx-v2-campaign-service` in AWS acct `372256339901` | DevOps/CloudOps | prod ExternalSecret sync |
| AWS Secrets Manager entries tagged `service-lfx-v2-campaign-service: enabled` | DevOps/CloudOps | any env consuming secrets |
| Cluster access + `argocd` CLI for sync/health validation | platform | US1/US2/US3 validation (T013/T016/T019/T024) |

## Environment readiness snapshot

| Environment | Config status | Deploy status |
|---|---|---|
| development | ✅ complete (chart + GitOps) | ⏳ awaiting cluster sync/validation (T013) |
| staging | ⏳ config gated on first OCI release | not started |
| production | ⏳ config gated on validated staging | not started |
