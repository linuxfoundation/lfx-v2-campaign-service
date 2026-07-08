# Phase 0 Research: Campaign Service Deployment Configuration

All decisions below were resolved by diffing the campaign-service chart and its argocd
configuration against the committee-service (primary) and project-service (secondary)
references. Each entry: **Decision / Rationale / Alternatives considered**. There are no
remaining `NEEDS CLARIFICATION` items.

---

## R1 — Canonical container image repository path

**Decision**: The canonical image path is
`ghcr.io/linuxfoundation/lfx-v2-campaign-service/campaign-service` (as already set in
`charts/lfx-v2-campaign-service/values.yaml`). Reconcile the two other references to it:
- `values.local.example.yaml` currently `ghcr.io/linuxfoundation/lfx-v2-campaign-service`
  (missing the `/campaign-service` suffix).
- `Makefile` `docker-build` uses `linuxfoundation/lfx-v2-campaign-service` (Docker Hub
  style). This target is local-only; either align it to the GHCR path or explicitly scope
  it as a local Docker Hub build. Local example + local build must at least agree with each
  other so `make helm-install-local` (pullPolicy `Never`) resolves the image it built.

**Rationale**: `ko build github.com/linuxfoundation/lfx-v2-campaign-service/cmd/campaign-service -B`
uses base-import-paths, publishing to `${KO_DOCKER_REPO}/campaign-service`. With
`KO_DOCKER_REPO=ghcr.io/linuxfoundation/lfx-v2-campaign-service`, the published image is
`ghcr.io/linuxfoundation/lfx-v2-campaign-service/campaign-service` — matching `values.yaml`
and the committee precedent (`.../lfx-v2-committee-service/committee-api`). The deployed
environments consume `values.yaml`, so the canonical path is correct for dev/staging/prod;
the mismatches are local-dev ergonomics only (non-blocking for deployment).

**Alternatives considered**: Changing `values.yaml` to the shorter path — rejected because
it would not match what `ko` actually publishes and would break deployed pulls.

---

## R2 — Heimdall RuleSet content (currently `rules: []`) — BLOCKING for gateway auth

**Decision**: Replace the empty `rules: []` in
`charts/lfx-v2-campaign-service/templates/ruleset.yaml` with a minimal but complete rule set
covering every path the HTTPRoute sends through the Heimdall middleware, mirroring the
committee/project structure:
- **OpenAPI paths** (`/_campaigns/openapi.json|.yaml|3.json|3.yaml`, GET/HEAD):
  `oidc` + `anonymous_authenticator` (+ `oidc_contextualizer` when enabled) →
  `authorizer: allow_all` → `finalizer: create_jwt` with `aud: {{ .Values.app.audience }}`.
- **`/campaigns` application routes** (shipped fail-closed placeholder): because
  campaign-service's FGA object/relation model is not yet defined, these rules use `oidc`
  only — **no** `anonymous_authenticator` fallback, so a valid token is genuinely required —
  (+ `oidc_contextualizer` when enabled) → an unconditional `allow_all` authorizer (any
  authenticated subject; NOT fine-grained authz) → `create_jwt` with
  `aud: {{ .Values.app.audience }}`. This is documented as intentional (FR-007), not left
  empty. TODO(LFXV2-2558): once the campaigns API + FGA model are defined, re-add
  `anonymous_authenticator` together with `openfga_check` (relation/object) — the pairing
  matters, since `openfga_check` is what rejects the anonymous subject in committee's pattern.

**Rationale**: The HTTPRoute attaches the `heimdall-forward-body` middleware
(forwardAuth → Heimdall) to `/campaigns`, `/campaigns/`, and `/_campaigns/`. With
`heimdall.enabled: true` and `add_middleware: true` (set in argocd global), every routed
request is evaluated by Heimdall. An empty RuleSet means no rule matches, so Heimdall
rejects/does not authorize those requests — the service would deploy Healthy (probes are
in-cluster, not routed) but be **unreachable through the gateway**, failing SC-003. Both
reference services define one rule per routed path; campaign must too.

**Alternatives considered**:
- Leave `rules: []` and rely on probes only — rejected; violates SC-003 (gateway
  reachability) and the standard.
- Disable Heimdall for campaign — rejected; diverges from platform security posture.

---

## R3 — Env-var rendering guard: truthiness vs `hasKey`

**Decision**: Change `deployment.yaml`'s env loop from `{{- if $config.value }}` to
`{{- if hasKey $config "value" }}` (committee's pattern), keeping `else if $config.valueFrom`.

**Rationale**: The current truthiness guard silently drops any env var whose `value` is
empty/false/zero. In campaign's own `values.yaml` this affects entries such as
`JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL: value: ''` — they render as neither `value` nor
`valueFrom`, producing a bare `- name: FOO` (invalid / surprising). `hasKey` renders an
explicit `value: ""`, which Kubernetes accepts and which matches the committee reference
exactly. This is a correctness + conformance fix.

**Alternatives considered**: Removing the empty-string placeholders from values instead —
rejected; the placeholders document intended env vars and the standard is to render them
explicitly.

---

## R4 — NATS chart resources scope

**Decision**: Do **not** add any NATS KV bucket, ObjectStore, or Stream templates to the
campaign chart at this time. Keep the standard template set to: deployment, service,
serviceaccount, httproute, heimdall-middleware, ruleset, pdb.

**Rationale**: FR-004 requires including standard resources the service *requires* and
omitting those it does not. Campaign-service currently has no NATS client wired (the
earlier inventory confirmed `NATS_URL`/`REPOSITORY_SOURCE=nats` are loaded into config but
no bucket/stream is used yet), so provisioning KV/Stream CRDs would create orphaned
infrastructure and drift from code (violating FR-011). NATS resources are added later when
the repository layer is implemented and `pkg/constants` declares bucket/subject names.

**Alternatives considered**: Pre-provision buckets to "future-proof" — rejected as
speculative (violates the simplicity workspace rule and FR-011 chart↔code parity).

---

## R5 — `serviceAccountName` guard and `topologySpreadConstraints` support

**Decision**:
1. Guard the pod `serviceAccountName` with
   `{{- if or .Values.serviceAccount.create .Values.serviceAccount.name }}` (committee
   pattern) instead of always emitting it.
2. Add `topologySpreadConstraints` support to the campaign `deployment.yaml`
   (`{{- with .Values.topologySpreadConstraints }}` block at pod-spec level) and add the
   `topologySpreadConstraints: []` default to `values.yaml`, matching committee.

**Rationale**: Conformance with the committee standard. The SA guard avoids referencing a
non-existent ServiceAccount if `create: false` is ever set. Topology spread is meaningful
at prod `replicaCount: 3`; committee sets zone/host spread in its global values, and
without chart support those values would be silently ignored. Adding support keeps the
door open to setting it in campaign's global values (optional; see R8).

**Alternatives considered**: Skip topology support (campaign global doesn't currently set
it) — acceptable minimum, but rejected in favor of matching the standard so prod HA parity
is achievable without a later chart change.

---

## R6 — Dev ArgoCD image-updater `imageSuffix`

**Decision**: Add `imageSuffix: /campaign-service` to the campaign-service entry in
`apps/dev/lfx-v2-applications.yaml` so the image-updater `image-list` resolves to the real
image `ghcr.io/linuxfoundation/lfx-v2-campaign-service/campaign-service:development`.

**Rationale**: The dev ApplicationSet `templatePatch` builds the image-updater annotation as
`ghcr.io/linuxfoundation/{{ .name }}{{ dig "imageSuffix" "" . }}:development`. Without a
suffix it points at `ghcr.io/linuxfoundation/lfx-v2-campaign-service:development`, which is
not where the image is published (the binary path adds `/campaign-service`). The
`lfx-v2-persona-service` entry establishes the precedent (`imageSuffix: /server`) for
services whose published image has a path suffix.

**Alternatives considered**: Follow committee (no `imageSuffix`) — rejected as an existing
latent inconsistency in committee (its real image is `.../committee-api`); replicating it
would leave dev auto-updates mis-targeted. This is a low-risk, corrective addition. Flag
the committee discrepancy to the platform team but do not change committee here (surgical
scope).

---

## R7 — Staging/prod chart-source and version pinning strategy

**Decision**: Register staging and prod using the **OCI chart source** pattern (matching
committee):

```yaml
- name: lfx-v2-campaign-service
  repoURL: ghcr.io/linuxfoundation/lfx-v2-campaign-service
  chart: chart/lfx-v2-campaign-service
  targetRevision: <PINNED_CHART_VERSION>
  namespace: lfx-v2-campaign-service
  customResources: true
```

and pin an immutable `image.tag: <PINNED_APP_VERSION>` in the activated
`values/staging` and `values/prod` files. The chart version and app/image version are the
same string derived from the release tag (per `ko-build-tag.yaml`:
`APP_VERSION = CHART_VERSION = <tag without 'v'>`).

**Rationale**: Matches the reference-service GitOps pattern and the spec's immutability
constraint (FR-013/014/016). Chart pin lives in `apps/<env>`, image tag in
`values/<env>` — the two-knob model documented in
`lfx-v2-argocd/docs/agent-guidance/gitops-implementation.md`.

**Open dependency (not a spec gap)**: The concrete version string does not exist until the
service repo cuts its first `v*` tag (triggering `ko-build-tag.yaml` to publish the image +
OCI chart). Until then, staging/prod entries + values are prepared with a placeholder
version (e.g. `0.1.0`) and finalized to the actual first release version at activation
time. This is the documented gate for User Stories 2 and 3.

**Alternatives considered**: Git-`HEAD` source for staging/prod (like dev) — rejected;
violates immutability requirement for pre-prod/prod.

---

## R8 — Global values completeness: secret wiring, service account, topology

**Decision**: In `values/global/lfx-v2-campaign-service.yaml`:
1. Keep the existing baseline (`replicaCount: 3`, Datadog log annotation, PDB
   `minAvailable: 2`, reduced resources, `heimdall.add_middleware: true`, reloader
   annotation).
2. Wire secret-backed env vars via `app.environment.<VAR>.valueFrom.secretKeyRef` pointing
   at the `lfx-v2-campaign-service-secrets` secret **only for the keys that actually exist**
   and are consumed by the app. The current service surface requires no secret-backed env
   var, so leave the `ExternalSecret` provisioned-but-unconsumed and document that secret
   wiring is completed when/if secret keys are defined (DevOps handoff).
3. Optionally add `serviceAccount.create: true` for explicitness (the chart already
   defaults it true, so this is cosmetic parity with committee) and
   `topologySpreadConstraints` (only after R5 adds chart support) for prod HA parity.

**Rationale**: The `ExternalSecret`/`SecretStore` already create
`lfx-v2-campaign-service-secrets`, but nothing consumes it yet (unlike committee, which
wires `M2M_AUTH_*`/`LITELLM_*`). To satisfy FR-018 the reference must be wired; but the
exact secret keys are an external DevOps artifact (AWS Secrets Manager entries tagged
`service-lfx-v2-campaign-service: enabled`). We wire what is known and document the rest as
a dependency (FR-024) rather than inventing key names.

**Alternatives considered**: Remove the ExternalSecret until secrets exist — rejected; the
custom-resources are already in place and correct, and IRSA/secret plumbing should be ready
ahead of the DevOps secret creation.

---

## R9 — Namespace, and umbrella-chart double-deploy guard

**Decision**:
- Use `lfx-v2-campaign-service` as the single namespace across all argocd apps entries,
  all `values/*` files, and both custom resources (already consistent on the branch).
- Do **not** add a `lfx-v2-campaign-service: enabled: false` disable to
  `values/global/lfx-platform.yaml` **now**, because campaign-service is not (yet) a
  subchart of the `lfx-v2-helm` umbrella. Add that disable only if/when the subchart is
  introduced there (FR-020), to prevent double deployment.

**Rationale**: Namespace consistency satisfies FR-019/SC-005. The umbrella disable is only
meaningful once the umbrella references the service; adding it prematurely has no effect and
could confuse readers. Committee's disable exists precisely because committee is in the
umbrella; campaign is not.

**Alternatives considered**: Match committee's shorter `committee-service`-style namespace —
rejected; campaign's chart/config already standardized on `lfx-v2-campaign-service`, and
changing it would touch every file for no benefit.

---

## Summary of intentional deviations from the committee reference (for FR-007/SC-007)

| Area | Campaign choice | Why intentional |
|---|---|---|
| OTEL sampling env vars | `OTEL_TRACES_SAMPLE_RATIO` (campaign, original) vs `OTEL_TRACES_SAMPLER`/`_ARG` (committee, OTel spec) | **Resolved (LFXV2-2558):** campaign's Go OTEL code + chart were aligned to the spec-standard `OTEL_TRACES_SAMPLER`/`OTEL_TRACES_SAMPLER_ARG` with a `parentbased_traceidratio` default (honors upstream parent sampling). No OTEL deviation remains. |
| NATS chart resources | None | No NATS client wired yet (R4). |
| Umbrella disable entry | Omitted | Campaign not in `lfx-v2-helm` umbrella yet (R9). |
| Ruleset FGA relations | Placeholder `allow_all` per route where FGA model undefined | Campaign FGA contract not yet authored; keeps chart deployable + documented (R2). |
