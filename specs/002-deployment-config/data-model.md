# Phase 1 Data Model: Deployment Configuration Entities

This feature has no application data model. The "entities" are **configuration objects**
and their relationships across the two repositories. This model captures their fields,
validation rules, and per-environment variation so `tasks.md` can be generated precisely.

---

## Entity: Helm Chart (`lfx-v2-campaign-service`)

The packaged deployment definition in the service repo; published as an OCI artifact for
staging/prod.

| Field | Source | Rule |
|---|---|---|
| `name` | `Chart.yaml` | `lfx-v2-campaign-service` (fixed) |
| `version` | `Chart.yaml` | `0.0.1` placeholder; replaced by CI at release with tag-derived semver |
| `appVersion` | `Chart.yaml` | `"latest"`; overridden by `image.tag` per env |
| `image.repository` | `values.yaml` | `ghcr.io/linuxfoundation/lfx-v2-campaign-service/campaign-service` (canonical, R1) |
| `image.tag` | `values.yaml` (`""`) | Empty → `Chart.AppVersion`; overridden per env |
| template set | `templates/` | MUST equal {deployment, service, serviceaccount, httproute, heimdall-middleware, ruleset, pdb}; no NATS templates (R4) |

**State / lifecycle**: `local dev (values.local) → dev (HEAD) → tagged release (OCI vX.Y.Z)
→ staging (pinned) → prod (pinned)`.

**Validation**: `helm lint` clean; `helm template` renders valid manifests for
default, dev, staging, and prod value combinations; MegaLinter Kubernetes/Helm pass.

### Sub-entity: Deployment template

| Field | Rule |
|---|---|
| env loop | MUST use `hasKey $config "value"` guard (R3) |
| `serviceAccountName` | MUST be guarded by `serviceAccount.create or .name` (R5) |
| `topologySpreadConstraints` | MUST render from `.Values.topologySpreadConstraints` when set (R5) |
| probes | liveness `/livez`; readiness+startup `/readyz`; port `web`; unchanged |
| container port | `web` = `service.port` (8080) |
| `securityContext.allowPrivilegeEscalation` | `false` |

### Sub-entity: RuleSet template (Heimdall)

| Field | Rule |
|---|---|
| `spec.rules` | MUST be non-empty; one rule per HTTPRoute-routed path group (R2) |
| openapi rule | `allow_all` authorizer + `create_jwt` finalizer (`aud: app.audience`) |
| `/campaigns` rule(s) | Shipped (fail-closed placeholder): `oidc` only (no `anonymous_authenticator`, so a valid token is required) (+ contextualizer) → unconditional `allow_all` → `create_jwt`. TODO(LFXV2-2558): once the campaigns API + FGA model exist, re-add `anonymous_authenticator` and replace `allow_all` with `openfga_check` (relation/object) so the anonymous subject is rejected by the FGA check |
| rule `id` convention | `rule:lfx:lfx-v2-campaign-service:<resource>:<action>` |

**Invariant**: every path in `httproute.yaml` that passes through the heimdall middleware
MUST have a matching RuleSet rule (chart↔route parity).

---

## Entity: ApplicationSet Entry (one per environment, in `lfx-v2-argocd`)

A list element in `apps/<env>/lfx-v2-applications.yaml` that tells ArgoCD how to deploy
campaign-service in that environment.

| Field | dev | staging | prod |
|---|---|---|---|
| `name` | `lfx-v2-campaign-service` | same | same |
| chart source | `repoURL: github.com/.../lfx-v2-campaign-service` + `path: charts/lfx-v2-campaign-service` | `repoURL: ghcr.io/.../lfx-v2-campaign-service` + `chart: chart/lfx-v2-campaign-service` | same as staging |
| `targetRevision` | `HEAD` | pinned semver | pinned semver |
| `namespace` | `lfx-v2-campaign-service` | same | same |
| `customResources` | `true` | `true` | `true` |
| `imageSuffix` | `/campaign-service` (ADD, R6) | n/a | n/a |
| values files consumed | `global` + `dev` | `global` + `staging` | `global` + `prod` |

**State**: dev = **present** (needs `imageSuffix` add); staging/prod = **absent** (must be
created).

**Validation**: entry parses within the list generator; `customResources: true` causes the
`custom-resources/lfx-v2-campaign-service/` source to be included by the `templatePatch`.

---

## Entity: Values Files (layered overrides, in `lfx-v2-argocd`)

Applied in order `global → <env>` on top of chart defaults.

| File | Role | State | Key fields |
|---|---|---|---|
| `values/global/lfx-v2-campaign-service.yaml` | shared baseline | present (needs completion, R8) | `replicaCount: 3`, PDB `minAvailable: 2`, Datadog annotation, resources, `heimdall.add_middleware: true`, reloader annotation, (+ secret `valueFrom` refs, optional topology/SA) |
| `values/dev/lfx-v2-campaign-service.yaml` | dev overrides | present (verify) | `image.tag: development`, `pullPolicy: Always`, dev domain, `use_oidc_contextualizer: false`, OTEL host-IP, IRSA role (dev acct), reduced resources |
| `values/staging/lfx-v2-campaign-service.yaml` | staging overrides | **placeholder (commented)** — ACTIVATE | pinned `image.tag`, staging domain, OTEL, IRSA role (staging acct), env vars |
| `values/prod/lfx-v2-campaign-service.yaml` | prod overrides | **placeholder (commented)** — ACTIVATE | pinned `image.tag`, prod domain `v2.cluster.lfx.dev`, namespace, OTEL sampler, IRSA role (prod acct), env vars |

**Per-environment invariants**:
- dev `image.tag` MUST be mutable (`development`); staging/prod MUST be immutable semver
  (FR-016).
- `lfx.domain` per env: dev `dev.v2.cluster.linuxfound.info`; staging
  `staging.v2.cluster.linuxfound.info`; prod `v2.cluster.lfx.dev`.
- `serviceAccount.annotations.eks.amazonaws.com/role-arn` account IDs: dev `788942260905`,
  staging `844790888233`, prod `372256339901`.

---

## Entity: Custom Resources (External Secrets, in `lfx-v2-argocd`)

| File | Kind | State | Rule |
|---|---|---|---|
| `custom-resources/lfx-v2-campaign-service/SecretStore.yaml` | `SecretStore` | present | AWS SecretsManager via IRSA (`serviceAccountRef: lfx-v2-campaign-service`), region `us-west-2` |
| `custom-resources/lfx-v2-campaign-service/ExternalSecret.yaml` | `ExternalSecret` | present | target secret `lfx-v2-campaign-service-secrets`; `dataFrom.find.tags.service-lfx-v2-campaign-service: enabled`; refresh 10m |

**Relationship**: `ExternalSecret` → produces `lfx-v2-campaign-service-secrets` →
consumed by Deployment env `valueFrom.secretKeyRef` (wired in `values/global`, R8) →
authenticated via the ServiceAccount named `lfx-v2-campaign-service` (IRSA role from
`values/<env>`).

---

## Entity: Environment

| Attribute | development | staging | production |
|---|---|---|---|
| cluster domain | `dev.v2.cluster.linuxfound.info` | `staging.v2.cluster.linuxfound.info` | `v2.cluster.lfx.dev` |
| AWS account | `788942260905` | `844790888233` | `372256339901` |
| chart source | git `HEAD` | OCI pinned | OCI pinned |
| image tag | `development` (mutable) | pinned | pinned |
| namespace | `lfx-v2-campaign-service` | `lfx-v2-campaign-service` | `lfx-v2-campaign-service` |
| activation gate | none (ready) | first published OCI release | first published OCI release |

---

## Cross-repository relationship diagram (textual)

```
lfx-v2-campaign-service (chart)                lfx-v2-argocd (GitOps)
──────────────────────────────                ──────────────────────────
Chart.yaml/values/templates  ──published──▶  ghcr OCI chart + image
                                             │
                                             ├─ apps/<env>/lfx-v2-applications.yaml (entry)
                                             │     └─ references chart source + version
                                             ├─ values/global + values/<env> (overrides)
                                             └─ custom-resources/<svc> (SecretStore/ExternalSecret)
                                                   └─ ArgoCD renders → K8s cluster (per env)
                                                        └─ Deployment/Service/HTTPRoute/RuleSet/PDB/SA
```

External (non-repo) prerequisites gating each environment: published OCI release
(service-repo CI), IRSA IAM roles per account, AWS Secrets Manager entries tagged for the
service (DevOps/CloudOps).
