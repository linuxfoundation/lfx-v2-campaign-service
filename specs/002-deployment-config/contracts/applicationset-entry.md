# Contract: ApplicationSet Entry (per environment)

The "interface" this feature exposes to ArgoCD is a list element in
`apps/<env>/lfx-v2-applications.yaml`. This contract fixes the exact shape per environment
so the generated Application is correct. Values are derived from the committee-service
reference (same profile: `customResources: true`).

## Dev entry (edit existing — add `imageSuffix`)

File: `apps/dev/lfx-v2-applications.yaml`

```yaml
- name: lfx-v2-campaign-service
  repoURL: https://github.com/linuxfoundation/lfx-v2-campaign-service
  path: charts/lfx-v2-campaign-service
  targetRevision: HEAD
  namespace: lfx-v2-campaign-service
  customResources: true
  imageSuffix: /campaign-service      # ADD (R6) — makes image-updater track the real image
```

**Contract checks**:
- Resulting image-updater annotation resolves to
  `ghcr.io/linuxfoundation/lfx-v2-campaign-service/campaign-service:development`.
- `customResources: true` ⇒ the third source `custom-resources/lfx-v2-campaign-service`
  is included via `templatePatch`.
- Values consumed: `$values/values/global/...` + `$values/values/dev/...`.

## Staging entry (create)

File: `apps/staging/lfx-v2-applications.yaml`

```yaml
- name: lfx-v2-campaign-service
  repoURL: ghcr.io/linuxfoundation/lfx-v2-campaign-service
  chart: chart/lfx-v2-campaign-service
  targetRevision: <PINNED_CHART_VERSION>     # e.g. 0.1.0 — first published release
  namespace: lfx-v2-campaign-service
  customResources: true
```

## Prod entry (create)

File: `apps/prod/lfx-v2-applications.yaml` — identical to staging (chart version may match
staging or a later validated version).

```yaml
- name: lfx-v2-campaign-service
  repoURL: ghcr.io/linuxfoundation/lfx-v2-campaign-service
  chart: chart/lfx-v2-campaign-service
  targetRevision: <PINNED_CHART_VERSION>
  namespace: lfx-v2-campaign-service
  customResources: true
```

## Cross-env invariants

| Rule | Check |
|---|---|
| dev uses git source + `HEAD`; staging/prod use OCI + pinned semver | field-level diff |
| `name`, `namespace`, `customResources` identical across all three | equality |
| staging/prod `targetRevision` is an immutable semver (no `HEAD`, no floating) | regex `^\d+\.\d+\.\d+$` |
| Entry inserted at the end of the `generators[0].list.elements` array (matches existing ordering convention) | position |

## Acceptance (maps to spec)

- FR-012 (dev), FR-013 (staging), FR-014 (prod), FR-019 (namespace), SC-004 (pinning).
