# Contract: Layered Values Files

Values are merged `chart defaults (values.yaml) → global → <env>`. This contract fixes what
each layer owns and the required per-environment fields, modeled on committee-service.

## `values/global/lfx-v2-campaign-service.yaml` (shared baseline)

Owns environment-independent production defaults. Current content is a valid baseline;
complete per R8.

```yaml
replicaCount: 3
podAnnotations:
  ad.datadoghq.com/app.logs: '[{"service": "lfx-v2-campaign-service", "source": "go"}]'
podDisruptionBudget:
  enabled: true
  minAvailable: 2
resources:
  requests: { cpu: 100m, memory: 128Mi }
  limits:   { cpu: 200m, memory: 256Mi }
heimdall:
  add_middleware: true
annotations:
  reloader.stakater.com/auto: "true"
# R8 additions when secret keys are known:
# app:
#   environment:
#     ITX_CLIENT_PRIVATE_KEY:
#       valueFrom:
#         secretKeyRef:
#           name: lfx-v2-campaign-service-secrets
#           key: <aws-secret-key-name>
# Optional parity (needs chart support from R5):
# topologySpreadConstraints: [ ... host + zone spread ... ]
```

**Contract**: MUST NOT contain env-specific values (domains, image tags, IRSA ARNs). Secret
references MUST target `lfx-v2-campaign-service-secrets` and only keys that exist.

## `values/dev/...` (present — verify)

```yaml
lfx:
  domain: "dev.v2.cluster.linuxfound.info"
  namespace: lfx-v2-campaign-service
app:
  use_oidc_contextualizer: false
  extraEnv: [ { name: HOST_IP, valueFrom: { fieldRef: { fieldPath: status.hostIP } } } ]
  otel: { endpoint: "http://$(HOST_IP):4317", insecure: "true", tracesExporter: "otlp", tracesSampleRatio: "0.2" }
image:
  tag: "development"
  pullPolicy: Always
resources: { requests: { cpu: 50m, memory: 32Mi }, limits: { cpu: 100m, memory: 64Mi } }
serviceAccount:
  annotations: { eks.amazonaws.com/role-arn: arn:aws:iam::788942260905:role/lfx-v2-campaign-service }
```

## `values/staging/...` (activate — currently all-commented)

Required fields on activation:

```yaml
image:
  tag: <PINNED_APP_VERSION>          # immutable; equals released chart/app version
lfx:
  domain: "staging.v2.cluster.linuxfound.info"
  namespace: lfx-v2-campaign-service
app:
  extraEnv: [ { name: HOST_IP, valueFrom: { fieldRef: { fieldPath: status.hostIP } } } ]
  otel: { endpoint: "http://$(HOST_IP):4317", insecure: "true", tracesExporter: "otlp", tracesSampleRatio: "0.2" }
  # environment: { ... staging-specific vars if/when the app needs them ... }
serviceAccount:
  annotations: { eks.amazonaws.com/role-arn: arn:aws:iam::844790888233:role/lfx-v2-campaign-service }
```

## `values/prod/...` (activate — currently all-commented)

```yaml
image:
  tag: <PINNED_APP_VERSION>
lfx:
  domain: "v2.cluster.lfx.dev"
  namespace: lfx-v2-campaign-service
app:
  use_oidc_contextualizer: false
  extraEnv: [ { name: HOST_IP, valueFrom: { fieldRef: { fieldPath: status.hostIP } } } ]
  otel: { endpoint: "http://$(HOST_IP):4317", insecure: "true", tracesExporter: "otlp", tracesSampleRatio: "1.0" }
serviceAccount:
  annotations: { eks.amazonaws.com/role-arn: arn:aws:iam::372256339901:role/lfx-v2-campaign-service }
```

## Invariants (validation)

| Rule | Check |
|---|---|
| dev `image.tag == "development"` and `pullPolicy: Always` | equality |
| staging/prod `image.tag` matches `^\d+\.\d+\.\d+$` | regex |
| domain per env matches the Environment table | equality |
| IRSA account ID per env matches (dev/staging/prod = 788942260905 / 844790888233 / 372256339901) | equality |
| every file carries the MIT license header | header present |
| no env-specific data in `global`; no shared baseline duplicated in env files | review |

## Acceptance (maps to spec)

- FR-015 (layering), FR-016 (image pinning), FR-017 (per-env fields), FR-018 (secret
  wiring), SC-004 (pinning), SC-005 (namespace consistency).
