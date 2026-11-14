---
type: "Go Package"
title: "pkg/utils"
description: "Package utils provides OpenTelemetry SDK setup utilities."
resource: "pkg/utils"
---

# pkg/utils

Package utils provides OpenTelemetry SDK setup utilities.

## Dependency security baseline

The whole OpenTelemetry set moves together on one upstream release train, so
the exporters always match the SDK they wrap. On the `v1.47.0` train: the stable
API, SDK and OTLP trace/metric exporters, and — now that the log API and SDK
are stable — `otel/log` and `otel/sdk/log`, are `v1.47.0`; the OTLP log
exporters (`otlploggrpc`, `otlploghttp`) are `v0.23.0` and the Prometheus
exporter `v0.69.0`; contrib `otelhttp` is `v0.72.0` and the Jaeger propagator
`v1.47.0`. `v1.47.0` of `otel/sdk/log` fixes the `AttributeValueLengthLimit`
bypass (Dependabot alert #24); the earlier baseline fixed endpoint URL
disclosure in exporter configuration logs (GHSA-8wmf-6v46-5gfg), log gRPC
exporter environment TLS certificate handling (GHSA-w34q-cm8f-9c5x), and log
batch processor busy-spinning when its export buffer is full
(GHSA-hjf4-fphr-2h65).

The global logger provider is installed with `otel.SetLoggerProvider`; the
`go.opentelemetry.io/otel/log/global` package is deprecated as of `v1.47.0`
and staticcheck (SA1019) fails lint on it.

Resource initialization imports semantic conventions `v1.43.0`, matching the
SDK's default resource schema URL. `TestNewResource_NoSchemaConflict` checks
that merging the service attributes with the default resource succeeds.

The transitive gRPC dependency is pinned to `v1.83.2`, covering the HTTP/2 DATA
frame memory exhaustion and xDS header-validation vulnerabilities tracked by
GHSA-vp52-pcj8-j9qc, GHSA-2v4p-qf9q-27wj, and GHSA-qc2q-p7wx-3px3.
This includes the follow-up missing-authority/Host-header panic fix recorded
in the Go vulnerability database as GO-2026-6443.

See [pkg/utils](../../../pkg/utils).
