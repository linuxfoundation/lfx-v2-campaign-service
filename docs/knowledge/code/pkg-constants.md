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
not bound the wire — the generated validator counts CHARACTERS of the encoded string, which
it sees only after the JSON decoder has read the whole body. The value is derived from the
creative-asset upload's 30-MiB DECODED ceiling (`maxCreativeStoredBytes`, enforced in the
handler): base64 expands by 4/3, so a maximum-size image is 40 MiB of base64 exactly — which
is what the design declares as `MaxLength(41943040)`, the ENCODED ceiling, since OpenAPI
`maxLength` counts characters of the JSON string. Plus the JSON envelope, which is why the
cap is 42 MiB and not 40. **Raising that ceiling in `design/` requires raising this constant
in the same change**,
or maximum-size uploads begin failing with `413`. Note the chart parity test does not cover
it — that test extracts only `Env…` constants from `constants.go` — so this documentation is
the only place the lockstep obligation is recorded outside the constant's own comment.

`PodMemoryLimitBytes`, `UploadAdmissionBudgetBytes`, `UploadAdmissionWeightBytes` and
`UploadAdmissionWait` (also in `http.go`) size the concurrent-upload admission bound applied by
`middleware.UploadAdmission`. `MaxRequestBodyBytes` bounds one body; these bound how many uploads
may allocate at once, which is what stands between a burst of legal uploads and an OOM-killed pod.

The budget is derived rather than chosen: `UploadAdmissionBudgetBytes` is `PodMemoryLimitBytes / 4`,
reserving the majority of the pod for the runtime, the DB pool and every non-upload request.
`PodMemoryLimitBytes` duplicates the chart's `resources.limits.memory`, a number the Go build
cannot read — so `TestPodMemoryLimitMatchesChart` parses `values.yaml` and fails when the two
drift, and `TestUploadAdmissionBudgetLeavesHeadroom` asserts the headroom property against the
pod limit rather than restating the budget's own formula. **Changing the chart's memory limit
requires changing `PodMemoryLimitBytes` in the same change**; the test names whichever one lags.

`DefaultReadTimeout` bounds an entire request read, not just the headers. Without it a slowloris
holds an admission permit indefinitely, exhausting the budget with requests that never complete —
so it is part of the same control, not an unrelated timeout.

See [pkg/constants](../../../pkg/constants).
