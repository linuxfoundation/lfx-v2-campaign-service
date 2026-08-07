---
type: "Kubernetes Resource"
title: "Deployment"
description: "Helm Deployment for the campaign service, including PG*, CREDENTIAL_ENCRYPTION_KEY, and SNOWFLAKE_* from lfx-v2-campaign-service-secrets."
resource: "charts/lfx-v2-campaign-service/templates/deployment.yaml"
---

# Deployment

Kubernetes Deployment manifest for the campaign service, defined in the Helm
chart. Application env vars come from `values.yaml` `app.environment`,
including `PGHOST` / `PGPORT` / `PGUSER` / `PGPASSWORD` / `PGDATABASE` /
`PGENGINE` and `CREDENTIAL_ENCRYPTION_KEY` via `secretKeyRef` to
`lfx-v2-campaign-service-secrets` (keys `host`, `port`, `username`,
`password`, `dbname`, `engine`, `credential-encryption-key`). The encryption
key is required whenever a database URL is configured because startup
initializes the AES-GCM encryptor before opening the pool used by `/readyz`.

`SNOWFLAKE_ACCOUNT` / `SNOWFLAKE_USER` / `SNOWFLAKE_PRIVATE_KEY` also come via
`secretKeyRef` to `lfx-v2-campaign-service-secrets` (keys `snowflake-account`,
`snowflake-user`, `snowflake-private-key`), each marked `optional: true` — unlike
the Postgres/encryption vars, an unset Snowflake secret does not block startup:
the warehouse client is a read-only enrichment for the email channel's
past-editions audience lookup (`internal/platform/snowflake`), and its absence
just narrows a built audience to country-only rather than failing the pod.
`SNOWFLAKE_WAREHOUSE` / `SNOWFLAKE_ROLE` are plain (non-secret) values, empty by
default so a cluster with no override uses the account's default.

`REDDIT_METRICS_ENABLED` is likewise a plain (non-secret) value, defaulting to
`"false"`. It is a feature gate rather than a credential: the Reddit reporting
contract this service implements is UNVERIFIED (LFXV2-2995) — the request and
response shapes are inferred, not confirmed by Reddit — so the metrics read is
shipped disabled and a cluster must opt in explicitly. The gate fails closed:
any value other than exactly `"true"` (including an empty string, `"1"`, or
`"TRUE"`) leaves the capability off, so a typo in a values override cannot
accidentally enable an unverified integration. The env var is read per call
(`RedditDispatcher.ReadMetrics`), not at construction, so flipping it needs a
pod restart to pick up the new `app.environment` value but no rebuild — and the
disabled path costs one env read. With the gate off the read returns
`ErrMetricsUnsupported`, which the service maps to the same 400 a platform with
no metrics adapter at all returns.

Probes: `livez` restarts a hung process (never touches the DB); `readyz` gates
traffic on DB connectivity. The `startupProbe` on `/readyz` carries a ~90s
`failureThreshold` budget for a database cold start. This budget is meaningful
because the process does NOT exit when the DB is unreachable at boot — the
container boots in 503 mode and retries the pool in the background (see the
`internal/container` concept), so `/readyz` stays 503 and the pod is kept alive
across the window rather than crash-looping.

See [charts/lfx-v2-campaign-service/templates/deployment.yaml](../../../charts/lfx-v2-campaign-service/templates/deployment.yaml).
