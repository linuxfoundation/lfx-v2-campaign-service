---
description: "Task list for Campaign Service Deployment Configuration"
---

# Tasks: Campaign Service Deployment Configuration

**Input**: Design documents from `/specs/002-deployment-config/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: No automated test code applies to this deployment-configuration feature (no TDD
requested). Verification is done via `helm lint`/`helm template`, MegaLinter, and ArgoCD
sync + gateway reachability checks, captured as explicit validation tasks.

**Organization**: Tasks are grouped by user story (environment). Shared chart conformance
and the shared ArgoCD `global` values are **Foundational** because the OCI chart and the
`global` values are consumed by all three environments.

## Repositories

- **campaign-service repo**: `lfx-v2-campaign-service.git.feat-LFXV2-2558-deployment`
  (Helm chart + templating + release CI)
- **argocd repo**: `lfx-v2-argocd.git.feat-LFXV2-2558-deployment`
  (ApplicationSets, layered values, custom resources)
- Reference (read-only): `lfx-v2-committee-service`, `lfx-v2-project-service`

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: US1/US2/US3 (user-story phases only)
- Each task names the exact file and its repo.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Baseline tooling and starting state.

- [X] T001 Verify local tooling (`helm` 3.x, `yq`, `kubectl`, `argocd`) is installed and that both worktrees (`lfx-v2-campaign-service`, `lfx-v2-argocd`) are on branch `feat-LFXV2-2558-deployment`; capture a pre-change baseline render with `helm template lfx-v2-campaign-service charts/lfx-v2-campaign-service --namespace lfx-v2-campaign-service > /tmp/campaign-baseline.yaml` (campaign-service repo)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Chart conformance + shared `global` values. These are consumed by every
environment (dev via git `HEAD`, staging/prod via the published OCI chart), so they MUST be
correct before any environment can be validated.

**⚠️ CRITICAL**: No environment deployment (US1/US2/US3) can be validated until this phase
is complete. T002 (RuleSet) is the highest-risk item — an empty ruleset blocks all gateway
traffic.

- [X] T002 Replace `spec.rules: []` in `charts/lfx-v2-campaign-service/templates/ruleset.yaml` with a complete rule set: an `allow_all` rule for the `/_campaigns/openapi.*` paths and one rule per `/campaigns` route group (`oidc` + `anonymous_authenticator` (+ `oidc_contextualizer` when enabled) → `openfga_check` when `.Values.openfga.enabled` else `allow_all` → `create_jwt` finalizer with `aud: {{ .Values.app.audience }}`), using rule-id convention `rule:lfx:lfx-v2-campaign-service:<resource>:<action>`, per `contracts/chart-conformance.md` and research R2 (campaign-service repo)
- [X] T003 Update `charts/lfx-v2-campaign-service/templates/deployment.yaml`: (a) change env loop guard from `{{- if $config.value }}` to `{{- if hasKey $config "value" }}` (R3); (b) guard `serviceAccountName` with `{{- if or .Values.serviceAccount.create .Values.serviceAccount.name }}` (R5); (c) add a pod-spec `{{- with .Values.topologySpreadConstraints }}` block (R5) (campaign-service repo)
- [X] T004 [P] Add `topologySpreadConstraints: []` default (with explanatory comment) to `charts/lfx-v2-campaign-service/values.yaml` to back the new deployment block (R5) (campaign-service repo)
- [X] T005 [P] Reconcile the image repository path in `charts/lfx-v2-campaign-service/values.local.example.yaml` so local `pullPolicy: Never` installs resolve the locally built image, consistent with the canonical `ghcr.io/linuxfoundation/lfx-v2-campaign-service/campaign-service` (R1) (campaign-service repo)
- [X] T006 [P] Reconcile or explicitly scope-as-local the `docker-build` image path (`DOCKER_IMAGE`) in `Makefile` so it agrees with `values.local.example.yaml` (R1) (campaign-service repo)
- [X] T007 [P] Verify `.github/workflows/ko-build-tag.yaml` still publishes the image and the OCI chart (`chart/lfx-v2-campaign-service`) with cosign signing + SLSA provenance; make no change unless a gap is found (FR-010) (campaign-service repo)
- [X] T008 Validate the chart after T002–T004: run `helm lint charts/lfx-v2-campaign-service` and `helm template ...` for default + dev/staging/prod-equivalent value combos; assert RuleSet is non-empty, empty-string env vars render explicitly, and probes/ports are intact (SC-001; quickstart US1 steps 1–3) (campaign-service repo)
- [X] T009 Complete `values/global/lfx-v2-campaign-service.yaml`: keep the existing baseline (replicaCount 3, Datadog annotation, PDB minAvailable 2, resources, `heimdall.add_middleware: true`, reloader annotation) and add `app.environment.*.valueFrom.secretKeyRef` entries targeting `lfx-v2-campaign-service-secrets` for the known secret keys (e.g. `ITX_CLIENT_PRIVATE_KEY`); if secret keys are not yet defined, add a documented placeholder comment and leave wiring pending (R8, FR-018) (argocd repo)
- [X] T010 [P] Ensure the MIT license header is present on every newly created file in both repos (FR-022)

**Checkpoint**: Chart is conformant, lints/renders, and shared `global` values are complete —
environment deployment can now proceed.

---

## Phase 3: User Story 1 - Deploy to development (Priority: P1) 🎯 MVP

**Goal**: Campaign-service deploys to the dev cluster via ArgoCD, is Healthy, and is
reachable through the dev gateway.

**Independent Test**: `argocd app get lfx-v2-campaign-service` shows Synced/Healthy, the pod
passes `/livez`+`/readyz`, and `curl https://lfx-api.dev.v2.cluster.linuxfound.info/_campaigns/openapi.json` returns 200.

- [X] T011 [US1] Add `imageSuffix: /campaign-service` to the `lfx-v2-campaign-service` element in `apps/dev/lfx-v2-applications.yaml` so the image-updater tracks the real image `ghcr.io/linuxfoundation/lfx-v2-campaign-service/campaign-service:development` (R6, FR-012); confirm the entry also has git source, `targetRevision: HEAD`, `namespace: lfx-v2-campaign-service`, `customResources: true` (argocd repo)
- [X] T012 [US1] Verify/adjust `values/dev/lfx-v2-campaign-service.yaml` against `contracts/values-layering.md`: `image.tag: development` + `pullPolicy: Always`, `lfx.domain: dev.v2.cluster.linuxfound.info`, `namespace: lfx-v2-campaign-service`, dev IRSA role `arn:aws:iam::788942260905:role/lfx-v2-campaign-service`, OTEL host-IP, reduced resources (FR-016, FR-017) (argocd repo)
- [ ] T013 [US1] Deploy to dev and validate per quickstart US1: ArgoCD Synced/Healthy, pod Ready, and dev gateway `/_campaigns/openapi.json` returns 200 (SC-002, SC-003) (dev cluster) — **deferred**: requires cluster + `argocd` CLI access. Config is complete and renders correctly against dev overlays; this is a runtime validation gate.

**Checkpoint**: MVP — campaign-service is running and routable in development.

---

## Phase 4: User Story 2 - Promote to staging (Priority: P2)

**Goal**: A pinned campaign-service release is deployed to staging via the OCI chart source.

**Independent Test**: staging ApplicationSet entry uses OCI + pinned semver, staging values
pin an immutable image tag, ArgoCD reports Synced/Healthy, and the staging gateway returns
200.

**⚠️ External gate**: Requires the first published OCI chart + image (a `v*` tag on the
campaign-service repo). Config may be authored ahead of time and finalized with the real
version at activation (research R7).

- [ ] T014 [US2] Add the `lfx-v2-campaign-service` OCI list element to `apps/staging/lfx-v2-applications.yaml` (`repoURL: ghcr.io/linuxfoundation/lfx-v2-campaign-service`, `chart: chart/lfx-v2-campaign-service`, `targetRevision: <PINNED_CHART_VERSION>`, `namespace: lfx-v2-campaign-service`, `customResources: true`) per `contracts/applicationset-entry.md` (FR-013) (argocd repo)
- [ ] T015 [US2] Activate `values/staging/lfx-v2-campaign-service.yaml` (currently all commented): set pinned `image.tag: <PINNED_APP_VERSION>`, `lfx.domain: staging.v2.cluster.linuxfound.info`, `namespace: lfx-v2-campaign-service`, OTEL host-IP block, and staging IRSA role `arn:aws:iam::844790888233:role/lfx-v2-campaign-service` (FR-016, FR-017) (argocd repo)
- [ ] T016 [US2] Validate staging per quickstart US2: ArgoCD Synced/Healthy and staging gateway `/_campaigns/openapi.json` returns 200 (SC-003, SC-004) (staging cluster)

**Checkpoint**: campaign-service runs on a pinned release in staging without affecting dev.

---

## Phase 5: User Story 3 - Promote to production (Priority: P3)

**Goal**: The validated release is deployed to production via the OCI chart source with
pinned versions.

**Independent Test**: prod ApplicationSet entry uses OCI + pinned semver, prod values pin an
immutable image tag, ArgoCD reports Synced/Healthy, and the prod gateway returns 200.

**⚠️ External gate**: Requires successful staging validation (US2) (research R7).

- [ ] T017 [US3] Add the `lfx-v2-campaign-service` OCI list element to `apps/prod/lfx-v2-applications.yaml` (same shape as staging; chart version may match staging or a later validated version) per `contracts/applicationset-entry.md` (FR-014) (argocd repo)
- [ ] T018 [US3] Activate `values/prod/lfx-v2-campaign-service.yaml` (currently all commented): set pinned `image.tag: <PINNED_APP_VERSION>`, `lfx.domain: v2.cluster.lfx.dev`, `namespace: lfx-v2-campaign-service`, `app.use_oidc_contextualizer: false`, OTEL sampler (`tracesSampler: "parentbased_traceidratio"`, `tracesSamplerArg: "1.0"`), prod resources, and prod IRSA role `arn:aws:iam::372256339901:role/lfx-v2-campaign-service` (FR-016, FR-017) (argocd repo)
- [ ] T019 [US3] Validate prod per quickstart US3: ArgoCD Synced/Healthy and prod gateway `/_campaigns/openapi.json` returns 200 (SC-003, SC-004) (prod cluster)

**Checkpoint**: campaign-service is deployed across all three environments.

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: Inventory, documentation, and conditional guards spanning all environments.

- [X] T020 [P] Add a campaign-service row to the LFX V2 Services table in `README.md` (argocd repo) (FR-021)
- [X] T021 [P] Add `.backstage-lfx-v2-campaign-service.yaml` Backstage catalog entity following the `.backstage-lfx-v2-traefik.yaml` pattern (argocd repo) (FR-021, optional)
- [X] T022 [P] Author `specs/002-deployment-config/deployment-inventory.md` — a file-by-file inventory of every campaign-service deployment config file across both repos with its conformance status, plus the external prerequisites (first OCI release, IRSA roles per account, AWS Secrets Manager tagged secrets) and which environment each blocks (FR-023, FR-024, SC-006, SC-008) (campaign-service repo / spec dir)
- [ ] T023 Add `lfx-v2-campaign-service: { enabled: false }` to `values/global/lfx-platform.yaml` ONLY if/when campaign-service is added as a subchart of `lfx-v2-helm` (currently N/A — verify and note) (argocd repo) (R9, FR-020) — **N/A**: campaign-service is deployed as a standalone ArgoCD Application, not a subchart of `lfx-v2-helm`; no umbrella toggle needed.
- [ ] T024 Run the full `quickstart.md` validation across all activated environments and record results; confirm single-namespace consistency (`grep`) and license headers (SC-005, SC-006) — **deferred**: requires cluster + `argocd` CLI access (not available in this environment).

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: no dependencies.
- **Foundational (Phase 2)**: depends on Setup — **blocks all environment deployments**.
- **US1 (Phase 3)**: depends on Foundational. Delivers the MVP (dev).
- **US2 (Phase 4)**: depends on Foundational + a published OCI release; independent of US1
  at the config level (shares only the `global` values).
- **US3 (Phase 5)**: depends on Foundational + successful US2 validation (promotion gate).
- **Polish (Phase 6)**: depends on the environments being registered; T020–T022 can run any
  time after Foundational.

### Within Foundational

- T002 (ruleset) and T003 (deployment.yaml) are independent files but both feed the
  validation gate T008. T003 edits the same file sequentially (single task).
- T004, T005, T006, T007, T010 are `[P]` (distinct files).
- T008 depends on T002+T003+T004. T009 (argocd global) is independent of the chart edits and
  can run in parallel with T002–T008.

### Within each user story

- ApplicationSet entry + values edits (different files) can be prepared together; validation
  task runs last and depends on the prior two + Foundational.

### Parallel Opportunities

- Foundational: `T004`, `T005`, `T006`, `T007`, `T009`, `T010` in parallel; then `T008`.
- Polish: `T020`, `T021`, `T022` in parallel.
- With staff + a published release, US2 and US3 config authoring (T014/T015, T017/T018) can be
  drafted in parallel, but prod validation (T019) still gates on staging validation (T016).

---

## Parallel Example: Foundational chart edits

```bash
# After T002 (ruleset) and T003 (deployment.yaml) are in progress, these touch distinct files:
Task: "Add topologySpreadConstraints default to charts/lfx-v2-campaign-service/values.yaml"      # T004
Task: "Reconcile image path in charts/lfx-v2-campaign-service/values.local.example.yaml"         # T005
Task: "Reconcile docker-build image path in Makefile"                                            # T006
Task: "Verify ko-build-tag.yaml OCI chart publish"                                               # T007
Task: "Complete values/global/lfx-v2-campaign-service.yaml secret wiring (argocd repo)"          # T009
```

---

## Implementation Strategy

### MVP First (User Story 1)

1. Phase 1 Setup.
2. Phase 2 Foundational (chart conformance — especially T002 ruleset — + shared `global`).
3. Phase 3 US1 (dev wiring + validation).
4. **STOP and VALIDATE**: campaign-service Synced/Healthy and routable in dev.

### Incremental Delivery

1. Setup + Foundational → chart correct & released.
2. US1 → dev deployed & validated (MVP).
3. Cut first `v*` release (external) → US2 staging → validate.
4. US3 prod → validate.
5. Polish (README, backstage, inventory) any time after Foundational.

### External prerequisites (documented, not code tasks)

- First published OCI chart + image via `v*` tag — gates US2/US3.
- IRSA IAM roles `lfx-v2-campaign-service` in accounts 788942260905 / 844790888233 /
  372256339901 (DevOps/CloudOps).
- AWS Secrets Manager entries tagged `service-lfx-v2-campaign-service: enabled` — gates
  successful `ExternalSecret` sync.

---

## Notes

- `[P]` = different files, no incomplete-task dependencies.
- `[US#]` labels appear only on environment-phase tasks (Setup/Foundational/Polish have none).
- Every changed line should trace to a spec requirement (FR/SC) or a research decision (R#).
- Commit after each task or logical group, GPG-signed + signed-off, `Refs: LFXV2-2558`.
- US2/US3 config can be authored before the first release exists; only the pinned version
  string and deploy validation wait on the external release gate.
