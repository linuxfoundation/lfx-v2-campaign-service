---
type: "Go Package"
title: "pkg/constants"
description: "Application-wide constants, including PG* and DATABASE_URL environment variable names."
resource: "pkg/constants"
---

# pkg/constants

Package constants defines application-wide constants, including HTTP defaults
and environment variable names for JWT, NATS, OpenTelemetry, and PostgreSQL
(`PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, `PGDATABASE`, `PGENGINE`,
`DATABASE_URL`, `CREDENTIAL_ENCRYPTION_KEY`).

`EVENT_URL_NAT64_PREFIXES` is a comma-separated list of the cluster's network-specific
RFC 6052 NAT64 prefixes for the event-URL fetcher's SSRF guard. Empty is correct where
there is no NAT64 and is the default, because a wrong value is worse than none; where
NAT64 IS in use and this is unset, an address encoding `169.254.169.254` passes every
check inside the service. `charts/.../parity_test.go` fails if a constant declared here is
never injected by the chart, which is what makes "declared but never supplied" a test
failure rather than a silently disabled feature.

See [pkg/constants](../../../pkg/constants).
