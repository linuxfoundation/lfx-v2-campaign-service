# Contract: Chart Conformance (campaign-service vs standard)

This contract enumerates the required state of the campaign-service Helm chart, expressed as
verifiable checks against the committee/project reference standard. Each row is either
**conformant** (no action) or an **edit** with its research reference.

## Template set

| Template | Required? | State |
|---|---|---|
| `deployment.yaml` | yes | present — EDIT (R3 env guard, R5 SA guard + topology) |
| `service.yaml` | yes | conformant |
| `serviceaccount.yaml` | yes | conformant |
| `httproute.yaml` | yes | conformant — verify path↔ruleset parity |
| `heimdall-middleware.yaml` | yes | conformant |
| `ruleset.yaml` | yes | **EDIT — `rules: []` must become real rules (R2, blocking)** |
| `pdb.yaml` | yes | conformant |
| `nats-*.yaml` | no (R4) | intentionally absent |
| `_helpers.tpl`, HPA, ServiceMonitor, ConfigMap, Secret, Ingress | no | absent (matches standard) |

## Deployment template checks

```
GIVEN app.environment has an entry `FOO: { value: "" }`
WHEN the chart renders
THEN the container env contains `- name: FOO` with `value: ""`   # requires hasKey guard (R3)
```

```
GIVEN serviceAccount.create=false AND serviceAccount.name=""
WHEN the chart renders
THEN the pod spec omits serviceAccountName                       # requires SA guard (R5)
```

```
GIVEN topologySpreadConstraints is set in values
WHEN the chart renders
THEN the pod spec includes topologySpreadConstraints             # requires chart support (R5)
```

## RuleSet ↔ HTTPRoute parity check (R2)

```
GIVEN httproute.yaml routes /campaigns, /campaigns/, /_campaigns/ through heimdall middleware
WHEN heimdall.enabled=true
THEN ruleset.yaml MUST contain a matching rule for each routed path group
     (openapi → allow_all; /campaigns → oidc + authz + create_jwt)
AND spec.rules MUST NOT be empty
```

## Image path check (R1)

```
values.yaml image.repository == ghcr.io/linuxfoundation/lfx-v2-campaign-service/campaign-service   # canonical
values.local.example.yaml image.repository reconciled to match local build output
Makefile docker-build image path reconciled or explicitly scoped local-only
```

## Render / lint gate

```
helm lint charts/lfx-v2-campaign-service                         # 0 errors
helm template lfx-v2-campaign-service charts/lfx-v2-campaign-service --namespace lfx-v2-campaign-service   # valid manifests
helm template ... -f <dev/staging/prod-equivalent values>        # valid for each env combo
MegaLinter KUBERNETES_HELM / KUBERNETES_DIRECTORY                 # pass
```

## Release automation check (FR-010)

```
.github/workflows/ko-build-tag.yaml publishes:
  - image ghcr.io/linuxfoundation/lfx-v2-campaign-service/campaign-service:<version>
  - OCI chart ghcr.io/linuxfoundation/lfx-v2-campaign-service/chart/lfx-v2-campaign-service:<version>
  - cosign signature + SLSA provenance
(present on branch — verify unchanged / functional)
```

## Acceptance (maps to spec)

- FR-001..FR-011, SC-001 (lint/template + template set), SC-003 (gateway reachability via
  ruleset), SC-007 (deviations documented).
