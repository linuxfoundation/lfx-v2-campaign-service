---
type: "Go Package"
title: "pkg/constants"
description: "Application-wide constants, including PG*, DATABASE_URL and AI_* environment variable names."
resource: "pkg/constants"
---

# pkg/constants

Package constants defines application-wide constants, including HTTP defaults
and environment variable names for JWT, NATS, OpenTelemetry, and PostgreSQL
(`PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, `PGDATABASE`, `PGENGINE`,
`DATABASE_URL`, `CREDENTIAL_ENCRYPTION_KEY`).

`EnvAIProxyURL` / `EnvAIAPIKey` / `EnvAIModel` (`AI_PROXY_URL`, `AI_API_KEY`, `AI_MODEL`)
name the LF LiteLLM proxy the email-copy generator talks to. The first two are optional
secret refs in the chart; `AI_MODEL` is a plain value because a model id is not a
credential, and unset selects `llm.DefaultModel`. Declaring them here rather than reading
`os.Getenv` inside `internal/platform/llm` is what keeps that package environment-free —
`Config` is injected — and what brings them under the chart parity test below.

`EVENT_URL_NAT64_PREFIXES` is a comma-separated list of the cluster's network-specific
RFC 6052 NAT64 prefixes for the event-URL fetcher's SSRF guard. Empty is correct where
there is no NAT64 and is the default, because a wrong value is worse than none; where
NAT64 IS in use and this is unset, an address encoding `169.254.169.254` passes every
check inside the service. `charts/.../parity_test.go` fails if a constant declared here is
never injected by the chart, which is what makes "declared but never supplied" a test
failure rather than a silently disabled feature.

`MaxRequestBodyBytes` (in `http.go`) is not an environment variable but a security
ceiling: it bounds how many bytes the server reads from ANY request body before answering
`413`, and `middleware.MaxBodyBytes` applies it on every route (see
[internal/middleware](internal-middleware.md)). It exists because a Goa `MaxLength` does
not bound the wire — the generated validator tests the already-base64-DECODED slice, which
it sees only after the JSON decoder has read the whole body. The value is derived from the
creative-asset upload's 30-MiB `MaxLength`: base64 expands by 4/3, so a maximum-size image
is 40 MiB of base64 exactly, plus the JSON envelope, which is why the cap is 42 MiB and not
40. **Raising `MaxLength` in `design/` requires raising this constant in the same change**,
or maximum-size uploads begin failing with `413`. Note the chart parity test does not cover
it — that test extracts only `Env…` constants from `constants.go` — so this documentation is
the only place the lockstep obligation is recorded outside the constant's own comment.

See [pkg/constants](../../../pkg/constants).
