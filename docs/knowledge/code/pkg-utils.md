---
type: "Go Package"
title: "pkg/utils"
description: "Package utils provides OpenTelemetry SDK setup utilities."
resource: "pkg/utils"
---

# pkg/utils

Package utils provides OpenTelemetry SDK setup utilities.

## Dependency security baseline

The OpenTelemetry stable SDK and OTLP trace/metric exporters are pinned to
`v1.45.0`, the log SDK and exporters to `v0.21.0`, and the Prometheus exporter to
the matching `v0.67.0` release in `go.mod`. These releases fix endpoint URL
disclosure in exporter configuration logs (GHSA-8wmf-6v46-5gfg), log gRPC
exporter environment TLS certificate handling (GHSA-w34q-cm8f-9c5x), and log
batch processor busy-spinning when its export buffer is full
(GHSA-hjf4-fphr-2h65).

Resource initialization imports semantic conventions `v1.43.0`, matching the
SDK's default resource schema URL. `TestNewResource_NoSchemaConflict` checks
that merging the service attributes with the default resource succeeds.

The transitive gRPC dependency is pinned to `v1.83.2`, covering the HTTP/2 DATA
frame memory exhaustion and xDS header-validation vulnerabilities tracked by
GHSA-vp52-pcj8-j9qc, GHSA-2v4p-qf9q-27wj, and GHSA-qc2q-p7wx-3px3.
This includes the follow-up missing-authority/Host-header panic fix recorded
in the Go vulnerability database as GO-2026-6443.

See [pkg/utils](../../../pkg/utils).
