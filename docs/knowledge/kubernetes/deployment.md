---
type: "Kubernetes Resource"
title: "Deployment"
description: "Helm Deployment for the campaign service, including PG* env from lfx-v2-campaign-service-secrets."
resource: "charts/lfx-v2-campaign-service/templates/deployment.yaml"
---

# Deployment

Kubernetes Deployment manifest for the campaign service, defined in the Helm
chart. Application env vars come from `values.yaml` `app.environment`,
including `PGHOST` / `PGPORT` / `PGUSER` / `PGPASSWORD` / `PGDATABASE` /
`PGENGINE` via `secretKeyRef` to `lfx-v2-campaign-service-secrets`
(keys `host`, `port`, `username`, `password`, `dbname`, `engine`).

See [charts/lfx-v2-campaign-service/templates/deployment.yaml](../../../charts/lfx-v2-campaign-service/templates/deployment.yaml).
