# Quickstart: Validating Campaign Service Deployment Configuration

This guide provides runnable validation for each user story. It proves the configuration is
correct without duplicating implementation details (those live in `tasks.md`). Paths assume
the workspace worktrees:

- Service repo: `lfx-v2-campaign-service.git.feat-LFXV2-2558-deployment`
- GitOps repo:  `lfx-v2-argocd.git.feat-LFXV2-2558-deployment`

## Prerequisites

- `helm` 3.x, `yq`, and (for cluster checks) `kubectl` + `argocd` CLIs configured for the
  target cluster.
- For User Story 1 gateway check: access to the dev cluster / ArgoCD.
- Reference: see `contracts/` for the exact expected shapes.

---

## US1 — Chart conformance + dev deployment (P1)

### 1. Chart renders and lints (SC-001)

```bash
cd charts/lfx-v2-campaign-service
helm lint .
helm template lfx-v2-campaign-service . --namespace lfx-v2-campaign-service > /tmp/campaign-default.yaml
```

**Expected**: `helm lint` reports 0 failures; template output is valid YAML.

### 2. RuleSet is non-empty and covers routed paths (R2, SC-003)

```bash
yq 'select(.kind == "RuleSet") | .spec.rules | length' /tmp/campaign-default.yaml
# Expected: > 0

# Every heimdall-routed HTTPRoute path has a matching rule:
yq 'select(.kind=="HTTPRoute") | .spec.rules[].matches[].path.value' /tmp/campaign-default.yaml
yq 'select(.kind=="RuleSet")  | .spec.rules[].match.routes[].path'   /tmp/campaign-default.yaml
```

**Expected**: `/campaigns`, `/campaigns/`, `/_campaigns/...` all have corresponding RuleSet
coverage.

### 3. Env-var rendering handles empty values (R3)

```bash
yq 'select(.kind=="Deployment") | .spec.template.spec.containers[0].env[] | select(.name=="JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL")' /tmp/campaign-default.yaml
```

**Expected**: renders `name:` + `value: ""` (not a bare `- name:` and not omitted).

### 4. Image path is canonical and consistent (R1)

```bash
yq '.image.repository' charts/lfx-v2-campaign-service/values.yaml
# Expected: ghcr.io/linuxfoundation/lfx-v2-campaign-service/campaign-service
grep -R "lfx-v2-campaign-service" charts/lfx-v2-campaign-service/values.local.example.yaml Makefile
# Expected: local example + Makefile reconciled per R1
```

### 5. Dev ApplicationSet entry (FR-012, R6)

```bash
cd ../../lfx-v2-argocd.git.feat-LFXV2-2558-deployment   # adjust to your path
yq '.spec.generators[0].list.elements[] | select(.name=="lfx-v2-campaign-service")' apps/dev/lfx-v2-applications.yaml
```

**Expected**: git source, `targetRevision: HEAD`, `namespace: lfx-v2-campaign-service`,
`customResources: true`, `imageSuffix: /campaign-service`.

### 6. Dev cluster deployment healthy (SC-002, SC-003)

```bash
argocd app get lfx-v2-campaign-service            # after sync
kubectl -n lfx-v2-campaign-service get pods
kubectl -n lfx-v2-campaign-service rollout status deploy/lfx-v2-campaign-service
```

**Expected**: ArgoCD `Synced` + `Healthy`; pod `Ready` (probes `/livez`,`/readyz` pass).

```bash
# Gateway reachability (dev hostname). OpenAPI is allow_all, so reachable without a token:
curl -sS -o /dev/null -w '%{http_code}\n' https://lfx-api.dev.v2.cluster.linuxfound.info/_campaigns/openapi.json
```

**Expected**: HTTP `200` (proves HTTPRoute + Heimdall RuleSet path works end to end).

---

## US2 — Staging promotion (P2)

> Gated on the first published OCI chart/image (R7). Perform once a `v*` release exists.

### 1. Staging entry + values pinned

```bash
yq '.spec.generators[0].list.elements[] | select(.name=="lfx-v2-campaign-service")' apps/staging/lfx-v2-applications.yaml
# Expected: OCI repoURL, chart: chart/lfx-v2-campaign-service, targetRevision pinned semver

yq '.image.tag' values/staging/lfx-v2-campaign-service.yaml     # Expected: pinned semver (not "development")
yq '.lfx.domain' values/staging/lfx-v2-campaign-service.yaml    # Expected: staging.v2.cluster.linuxfound.info
```

### 2. Staging sync + reachability

```bash
argocd app get lfx-v2-campaign-service   # staging context
curl -sS -o /dev/null -w '%{http_code}\n' https://lfx-api.staging.v2.cluster.linuxfound.info/_campaigns/openapi.json
```

**Expected**: `Synced`/`Healthy`; HTTP `200`.

---

## US3 — Production promotion (P3)

> Gated on validated staging release (R7).

```bash
yq '.spec.generators[0].list.elements[] | select(.name=="lfx-v2-campaign-service")' apps/prod/lfx-v2-applications.yaml
yq '.image.tag' values/prod/lfx-v2-campaign-service.yaml        # Expected: pinned semver
yq '.lfx.domain' values/prod/lfx-v2-campaign-service.yaml       # Expected: v2.cluster.lfx.dev

argocd app get lfx-v2-campaign-service   # prod context
curl -sS -o /dev/null -w '%{http_code}\n' https://lfx-api.v2.cluster.lfx.dev/_campaigns/openapi.json
```

**Expected**: `Synced`/`Healthy`; HTTP `200`.

---

## Cross-cutting checks

```bash
# Namespace consistency (SC-005): single namespace everywhere
grep -R "namespace" values/*/lfx-v2-campaign-service.yaml custom-resources/lfx-v2-campaign-service/ apps/*/lfx-v2-applications.yaml | grep campaign
# Expected: only lfx-v2-campaign-service

# License headers (FR-022)
head -3 values/staging/lfx-v2-campaign-service.yaml   # Expected: MIT header present
```

## External prerequisites (document, don't block on)

- Published OCI chart + image (`v*` tag on service repo) — gates US2/US3.
- IRSA IAM roles `lfx-v2-campaign-service` in accounts 788942260905 / 844790888233 /
  372256339901.
- AWS Secrets Manager entries tagged `service-lfx-v2-campaign-service: enabled` — gates
  successful `ExternalSecret` sync.
