# 2026-10-01 — Dependabot dependency security updates

**Fix** — Updated Go dependencies to patched versions for Dependabot alerts
#12–#21: OpenTelemetry stable modules `v1.45.0`, log modules `v0.21.0`, the
matching Prometheus exporter `v0.67.0`, gRPC `v1.83.2`, and Apache Thrift
`v0.24.0`. Regenerated module checksums with `go mod tidy` and documented the
security baseline in the OpenTelemetry utilities and Snowflake client concepts.
Aligned the service's semantic convention imports to `v1.43.0` to match the
updated SDK's default resource schema and preserve resource initialization.
