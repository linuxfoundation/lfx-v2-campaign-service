# LFX V2 Campaign Service

A collection of service endpoints to support Marketing Operations
campaign creation and management.

## API Endpoints

- `/livez`: `GET` — checks that the service is alive (liveness
  probe). Returns `200` with a `text/plain` body of `OK`. Does not
  depend on database availability.
- `/readyz`: `GET` — checks that the service is able to take inbound
  requests (readiness probe), including a PostgreSQL connectivity
  check. Returns `200` with a `text/plain` body of `OK` when ready,
  or `503` when not ready.
- `/metrics`: `GET` — Prometheus metrics in the text exposition
  format. Served on the same port as the API, and available in
  no-database mode and during a cold start (a scrape target that only
  appears once the database is up is exactly backwards). See
  [Metrics](#metrics) below.

These three endpoints are unauthenticated and are excluded from the
generated public API documentation. They are also absent from the Helm
chart's `HTTPRoute` and Heimdall `RuleSet`, so they are reachable only
in-cluster (kubelet probes and the Prometheus scraper), never through
the public gateway.

## Metrics

`GET /metrics` serves the Prometheus text exposition format. The
endpoint takes **no configuration** — there is no environment variable
to enable it and no separate metrics port. It is independent of the
`OTEL_*` settings below: `OTEL_METRICS_EXPORTER` defaults to `none`, so
wiring these instruments to the OTLP pipeline would leave `/metrics`
empty in the default deployment.

Service metrics:

| Metric | Type | Labels | What it answers |
| --- | --- | --- | --- |
| `campaign_dispatch_total` | counter | `platform`, `outcome` | Are campaigns actually landing on each ad platform? `outcome` is one of `success`, `skipped`, `failure`, `panic` — `panic` is separate because it is a bug in this service, not an upstream refusal. |
| `campaign_job_transitions_total` | counter | `status` | Dispatch jobs reaching each state (`running`, `succeeded`, `partial`, `failed`). A growing gap between `running` and the terminal states means jobs are getting stuck. |
| `campaign_upstream_calls_total` | counter | `platform`, `operation`, `outcome` | Upstream ad-platform API call volume and error rate. |
| `campaign_upstream_call_duration_seconds` | histogram | `platform`, `operation`, `outcome` | Upstream ad-platform latency. Timed only after the pre-platform guards pass, so local refusals do not drag the quantiles toward zero. |
| `campaign_db_pool_*` | gauge / counter | none | Database pool health: acquired, idle, total and max connections, plus canceled and empty acquires. Exported only when a pool is actually wired — see below. |

Plus the standard Go runtime and process collectors.

**Label cardinality.** No metric here carries a campaign id, brief id,
project id, job id, account id or URL as a label value — an unbounded
label creates one retained time series per distinct value and is the
classic way to take a Prometheus server down. `platform` is mapped
through a closed provider set and anything outside it collapses to
`unknown`; the outcome labels are closed enums. No metric name, label
or help string carries a credential, DSN or token.

**Absent pool metrics are meaningful.** When no database is wired (no-DB
mode, or a cold start before the pool opens) the `campaign_db_pool_*`
series are **not exported at all**, rather than exported as zeroes. A
zero is a measurement: reporting `max_connections=0` for a service
running without a database is indistinguishable from a pool that has
collapsed, and would fire a false exhaustion alert.

**Scraping.** The chart sets `prometheus.io/scrape`,
`prometheus.io/path` and `prometheus.io/port` on the pod template by
default, so a discovery-based collector picks the pod up with no further
configuration. `prometheus.io/port` is derived from `service.port` in
the template rather than hardcoded in `values.yaml`, so the scrape port
cannot drift from the port the container listens on. A deployment that
overrides `podAnnotations` replaces the map, so it must re-declare
`prometheus.io/scrape` and `prometheus.io/path` to keep being scraped.

## Environment variables

Configuration priority: CLI flags > environment variables > defaults.

### Required (startup)

When any PostgreSQL setting is supplied, the set must be complete or
the process exits non-zero. Fully omitting all database settings is
allowed for unit tests / metadata-only local runs (no-DB mode; `/readyz`
stays process-ready without a pool). In-cluster they are typically
injected from the ExternalSecret-managed Kubernetes secret
(`lfx-v2-campaign-service-secrets` in namespace
`lfx-v2-campaign-service`; keys `host`, `port`, `username`,
`password`, `dbname`, `engine`, `credential-encryption-key`).

- `PGHOST` (secret key `host`) — PostgreSQL hostname
- `PGUSER` (secret key `username`) — PostgreSQL username
- `PGPASSWORD` (secret key `password`) — PostgreSQL password
  (never logged)
- `PGDATABASE` (secret key `dbname`) — PostgreSQL database name
- `CREDENTIAL_ENCRYPTION_KEY` (secret key
  `credential-encryption-key`) — base64-encoded 32-byte AES-256
  key used to encrypt ad-platform connection credentials at rest.
  Required whenever a database is configured. **Rollout
  prerequisite:** provision this key on
  `lfx-v2-campaign-service-secrets` (via ExternalSecret sync)
  before deploying the chart revision that references it; otherwise
  the pod stays in `CreateContainerConfigError`.

The service composes the DSN in-process from these fields (no
`DATABASE_URL` env var required).

#### Local / test sample key

For laptop runs against the RDS tunnel (or any local Postgres), you
can use this **non-production** sample (base64 of the 32-byte ASCII
string `LFX-campaign-local-dev-aes-256!!`):

```sh
# !!! WARNING !!!
# This is a *TEST/EXAMPLE* key for local/dev use only.
# NEVER use this in production or shared environments!
# -----------------------------------------------------
# (test/dev/example: base64-encoded 'LFX-campaign-local-dev-aes-256!!')
# Example (use printf so the *plaintext* stays exactly 32 bytes — a trailing
# newline from echo would make it 33 and break AES-256 key length):
#   printf '%s' 'LFX-campaign-local-dev-aes-256!!' | base64
export CREDENTIAL_ENCRYPTION_KEY='TEZYLWNhbXBhaWduLWxvY2FsLWRldi1hZXMtMjU2ISE='
```

Again, do **not** use this value in shared, staging, or production clusters.
Generate a real key for those environments, for example:

```sh
openssl rand -base64 32
```

### Optional (with defaults)

- `PGPORT` (default `5432`; secret key `port`) — PostgreSQL port
- `PGENGINE` (default empty) — when set, must be `postgres` or
  `postgresql`
- `PORT` (default `8080`) — HTTP listen port (CLI flag `-p`)
- `HOST` (default `*`) — bind interface; `*` means all interfaces
  (CLI flag `-bind`)
- `DEBUG` (unset) — set to `true` to enable debug logging
  (CLI flag `-d`)
- `JWKS_URL` — key set every bearer token is verified against (defaults
  to the in-cluster Heimdall JWKS URL). Set but not an absolute URL
  fails startup rather than degrading verification.
- `JWT_AUDIENCE` (default `lfx-v2-campaign-service`) — required audience
- `JWT_ISSUER` (default `heimdall`) — required issuer
- `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` (default unset) — local
  development only: when non-empty it **disables JWT verification** and
  attributes every request to this principal, logging a `WARN` on every
  boot that sets it. Two independent guards keep it out of a deployed
  environment: the chart refuses to render it, and at runtime the pod fails
  startup rather than degrading whenever it detects it is running in a
  cluster — so the failure is a crash-loop, not a silently unauthenticated
  service. Worth knowing before debugging a rollout that will not come up.
  The runtime guard is a detection, not a proof: it looks for
  `KUBERNETES_SERVICE_HOST` and the service-account directory, so a manifest
  applied outside the chart that both sets this variable and suppresses those
  two signals (an explicit empty `KUBERNETES_SERVICE_HOST` plus
  `automountServiceAccountToken: false`) is not caught. That combination is
  three deliberate, visible changes rather than one env line, which is the
  bar the guard is built to — see `internal/infrastructure/config/config.go`.
- `NATS_URL` — NATS server URL (reserved for messaging; defaults to
  in-cluster NATS URL)
- `EVENT_URL_NAT64_PREFIXES` (default unset) — comma-separated
  RFC 6052 §2.2 **network-specific** NAT64 prefixes this cluster uses,
  e.g. `2a01:4f8:1::/48`. Only `/32`, `/40`, `/48`, `/56`, `/64` and
  `/96` are valid lengths; a malformed value or a wrong length
  **panics at startup**, deliberately, so a typo stops the pod rather
  than decoding at the wrong offset for its lifetime.

  Unset is correct for a cluster with no NAT64, and the well-known
  `64:ff9b::/96` is decoded regardless. It cannot be discovered
  in-process: a network-specific prefix is carved from the operator's
  own global unicast space and is indistinguishable from any other
  public prefix. On a cluster that uses one, leaving it undeclared is a
  live SSRF hole — the translator, not this service, makes the IPv4
  connection, so an address encoding `169.254.169.254` passes every
  check here and the fetch reaches the cloud metadata endpoint.
- `REDDIT_METRICS_ENABLED` (default unset, i.e. OFF) — opts a
  deployment IN to Reddit Ads metrics reads. Only the exact value
  `true` enables them; unset or any other value (including `TRUE` or
  a typo) fails closed, and
  `GET .../campaigns/{campaign_id}/metrics` answers 400 "not
  supported for this campaign's platform" for a Reddit campaign.
  Off by default because Reddit's reporting endpoint has no public
  documentation (LFXV2-2995) — the request shape, response shape, and
  spend currency unit are a best-effort guess, and a guessed read
  returning 200 would look authoritative to every consumer. The chart
  sets it to `"false"`
  (`charts/lfx-v2-campaign-service/values.yaml`); flip it only after
  the contract is verified against a live Reddit ad account.

### Snowflake (optional, audience building)

Read-only warehouse access, used ONLY to resolve an event's past editions when
building an audience (LFXV2-2774). These are OPTIONAL as a GROUP: unless
`SNOWFLAKE_ACCOUNT`, `SNOWFLAKE_USER` and `SNOWFLAKE_PRIVATE_KEY` are ALL set,
the warehouse is treated as unconfigured and audience building still works —
it produces a country-only audience and records the narrower scope in the
audience's inclusion summary. A partial or unusable configuration degrades the
same way rather than failing startup, because past editions ENRICH an audience
rather than making it correct.

- `SNOWFLAKE_ACCOUNT` — account identifier
- `SNOWFLAKE_USER` — user for key-pair auth
- `SNOWFLAKE_PRIVATE_KEY` — PEM private key for key-pair auth
- `SNOWFLAKE_WAREHOUSE` — optional warehouse; the account default is used when
  omitted
- `SNOWFLAKE_ROLE` — optional role; the user's default is used when omitted

### LLM / email copy (optional, `AI_*`)

The LF **LiteLLM proxy**, used ONLY to generate email copy — subject, preheader,
body and CTA — via the POST `/projects/{id}/briefs/{id}/email-copy` endpoint
(LFXV2-2775). Optional as a GROUP: unless BOTH `AI_PROXY_URL` and `AI_API_KEY`
are set, the endpoint returns 503 (service unavailable). The pod starts successfully
with or without these values configured.

- `AI_PROXY_URL` — LiteLLM proxy base URL; `/chat/completions` is appended
- `AI_API_KEY` — the **proxy's** key, not a Bedrock or Anthropic credential (the
  proxy holds those), so it cannot be replayed against a model provider directly
- `AI_MODEL` — optional, not a secret; the model id the proxy routes on. Empty
  selects `llm.DefaultModel`.

In-cluster the two secrets come from the same ExternalSecret-managed secret as
the rest; `AI_MODEL` is a plain chart value.

### Observability (`OTEL_*`)

OpenTelemetry is opt-in. Exporters default to `none` (no collector
required for local runs).

- `OTEL_SERVICE_NAME` (default `lfx-v2-campaign-service`)
- `OTEL_SERVICE_VERSION` (default: build version)
- `OTEL_EXPORTER_OTLP_PROTOCOL` (default `grpc`) — `grpc` or `http`
- `OTEL_EXPORTER_OTLP_ENDPOINT` — collector endpoint
- `OTEL_EXPORTER_OTLP_INSECURE` (default `false`) — insecure when
  `true`
- `OTEL_TRACES_EXPORTER` (default `none`) — `otlp` or `none`
- `OTEL_METRICS_EXPORTER` (default `none`) — `otlp` or `none`
- `OTEL_LOGS_EXPORTER` (default `none`) — `otlp` or `none`
- `OTEL_PROPAGATORS` (default `tracecontext,baggage`) —
  comma-separated; `jaeger` supported
- `OTEL_TRACES_SAMPLER` (default `parentbased_traceidratio` when
  unset) — sampler type (`always_on`, `always_off`, `traceidratio`,
  `parentbased_*`, …)
- `OTEL_TRACES_SAMPLER_ARG` (default `1.0`) — sampler argument; for
  ratio-based samplers, a value in `[0.0, 1.0]`

### Build and run locally (against lfx-v2-dev)

In **lfx-v2-dev**, Postgres is RDS. The cluster exposes it as an
`ExternalName` Service (`lfx/rds-postgres`). A plain
`kubectl port-forward svc/rds-postgres …` does **not** work
(ExternalName has no endpoints). Use a short-lived jump pod with
`socat`, then port-forward to that pod.

Credentials live in secret
`lfx-v2-campaign-service-secrets` (namespace
`lfx-v2-campaign-service`), keys: `host`, `port`, `username`,
`password`, `dbname`, `engine`.

```sh
# 0) Point kubectl at development (example path; adjust if needed)
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/lfx-v2-dev}"

# 1) Confirm the secret exists (do not print the password)
kubectl -n lfx-v2-campaign-service get secret \
  lfx-v2-campaign-service-secrets

# 2) Read the RDS hostname from the secret (safe: host only)
RDS_HOST="$(kubectl -n lfx-v2-campaign-service get secret \
  lfx-v2-campaign-service-secrets \
  -o jsonpath='{.data.host}' | base64 -d)"
RDS_PORT="$(kubectl -n lfx-v2-campaign-service get secret \
  lfx-v2-campaign-service-secrets \
  -o jsonpath='{.data.port}' | base64 -d)"
RDS_PORT="${RDS_PORT:-5432}"

# Both must be non-empty before creating the jump pod
if [ -z "$RDS_HOST" ] || [ -z "$RDS_PORT" ]; then
  echo "RDS_HOST/RDS_PORT empty — refuse to create broken tunnel" >&2
  exit 1
fi
echo "tunnel target ${RDS_HOST}:${RDS_PORT}"

# 3) Start a jump pod that listens on 5432 and dials RDS.
#    Do NOT use --command (replaces the socat entrypoint →
#    "tcp-listen:…: executable file not found").
#    Do NOT use -it (Gatekeeper blocks interactive TTYs).
#    Do NOT use --rm until you are done (you need the pod alive
#    for port-forward).
#    Delete any prior failed pod first if needed:
#      kubectl -n lfx-v2-campaign-service delete pod pg-tunnel \
#        --ignore-not-found
kubectl -n lfx-v2-campaign-service delete pod pg-tunnel \
  --ignore-not-found
kubectl -n lfx-v2-campaign-service run pg-tunnel \
  --restart=Never --image=alpine/socat -- \
  tcp-listen:5432,fork,reuseaddr \
  "tcp-connect:${RDS_HOST}:${RDS_PORT}"

kubectl -n lfx-v2-campaign-service wait --for=condition=Ready \
  pod/pg-tunnel --timeout=60s

# Confirm args include the real host (not tcp-connect::)
kubectl -n lfx-v2-campaign-service get pod pg-tunnel \
  -o jsonpath='{.spec.containers[0].args}{"\n"}'
```

In a **second** terminal (leave this running — stopping it causes
`connection refused` on `/readyz`):

```sh
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/lfx-v2-dev}"

# 4) Forward laptop:5432 -> jump pod:5432
kubectl -n lfx-v2-campaign-service port-forward \
  pod/pg-tunnel 5432:5432
# Expect:
#   Forwarding from 127.0.0.1:5432 -> 5432
# Later, when the service pings, you may also see:
#   Handling connection for 5432
```

In a **third** terminal — build, load creds, run:

```sh
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/lfx-v2-dev}"

# Always use the tunnel endpoint on the laptop, not the RDS FQDN.
# If you export PGHOST from the secret's `host` key, readyz will
# time out (laptop cannot reach private RDS directly).
export PGHOST=127.0.0.1
export PGPORT=5432
export PGUSER="$(kubectl -n lfx-v2-campaign-service get secret \
  lfx-v2-campaign-service-secrets \
  -o jsonpath='{.data.username}' | base64 -d)"
export PGPASSWORD="$(kubectl -n lfx-v2-campaign-service get secret \
  lfx-v2-campaign-service-secrets \
  -o jsonpath='{.data.password}' | base64 -d)"
export PGDATABASE="$(kubectl -n lfx-v2-campaign-service get secret \
  lfx-v2-campaign-service-secrets \
  -o jsonpath='{.data.dbname}' | base64 -d)"
# **Note**: This is a Local-dev sample only (see "Local / test sample key" above).
# NEVER use this in production or shared environments!
export CREDENTIAL_ENCRYPTION_KEY='TEZYLWNhbXBhaWduLWxvY2FsLWRldi1hZXMtMjU2ISE='


# Sanity-check before starting (must be 127.0.0.1, not the RDS FQDN)
echo "PGHOST=$PGHOST PGPORT=$PGPORT PGDATABASE=$PGDATABASE"
# optional: confirm the tunnel accepts TCP
nc -z 127.0.0.1 5432 && echo "tunnel port open"

make build
make run
# On startup, the log line "dependency container initialized" must
# show database=127.0.0.1:5432/<dbname> — if it shows the RDS
# hostname, stop and fix PGHOST.
```

Smoke-check readiness (expects `200` / `OK` while the tunnel is up):

```sh
curl -sS -w "\nHTTP %{http_code}\n" http://127.0.0.1:8080/readyz
# expect body OK and HTTP 200

curl -sS -w "\nHTTP %{http_code}\n" http://127.0.0.1:8080/livez
# expect body OK and HTTP 200 (even if readyz would be 503)
```

Cleanup: stop the port-forward (Ctrl-C), then delete the jump pod:

```sh
kubectl -n lfx-v2-campaign-service delete pod pg-tunnel \
  --ignore-not-found
```

#### Troubleshooting

- **`tcp-listen:…: executable file not found`** — used `--command`.
  Recreate with `run … -- args` (no `--command`).
- **Gatekeeper TTY warning / blocked** — used `-it`. Omit `-it`.
- **Pod args show `tcp-connect::`** — `RDS_HOST`/`RDS_PORT` were
  empty at create. Re-export from secret, delete pod, recreate.
- **Startup log shows RDS FQDN as database** — `PGHOST` was taken
  from secret `host`. Use `export PGHOST=127.0.0.1` and restart.
- **`connection refused` on 127.0.0.1:5432** — port-forward not
  running. Restart Terminal 2.
- **`context deadline exceeded` with `PGHOST=127.0.0.1`** — jump
  pod dialing wrong/empty target, or tunnel stalled. Check pod
  args; recreate tunnel.

#### Alternatives

- **VPN / direct RDS access** — if your laptop can reach the RDS
  FQDN, skip the jump pod; set `PGHOST`/`PGPORT` from the secret
  `host`/`port` keys and `make run`.
- **Local Docker / Homebrew Postgres** — no tunnel; use
  `PGHOST=127.0.0.1` with local credentials.
- **CloudNativePG ClusterIP Service** — `kubectl port-forward
  svc/<cnpg-rw-service> 5432:5432` works without a jump pod.

See also `specs/002-db-conn-check/quickstart.md` for readiness /
liveness validation scenarios.

### Run in a local Kubernetes cluster

Prefer `make run` (above) for day-to-day Go iteration. To exercise the
Helm chart — probes, secret refs, and env wiring — build an image and
install with the local values override.

`make helm-install-local` installs into namespace `lfx` (see
`HELM_NAMESPACE` in the Makefile). The chart still requires secret
`lfx-v2-campaign-service-secrets` (keys: `host`, `port`, `username`,
`password`, `dbname`) in that same namespace for the required `PG*`
env refs. Without it the pod stays in `CreateContainerConfigError`.

Pick one of these before installing. Set `HELM_NAMESPACE` once and
pass it to the final `make helm-install-local` (Makefile assigns
`HELM_NAMESPACE=lfx` with `=`, so an env prefix does not override).

```sh
# Default local release namespace (Option A / C)
HELM_NS=lfx

# Option A — copy the secret from lfx-v2-dev into a local cluster.
# Use distinct source and destination contexts (adjust names to match
# `kubectl config get-contexts`). Rebuild a clean Secret so server-
# managed fields (uid, resourceVersion, …) are not re-applied.
SRC_CONTEXT="${SRC_CONTEXT:-lfx-v2-dev}"
DST_CONTEXT="${DST_CONTEXT:-kind-kind}"

kubectl --context="$SRC_CONTEXT" get secret \
  lfx-v2-campaign-service-secrets \
  -n lfx-v2-campaign-service -o json \
  | jq '{
      apiVersion: .apiVersion,
      kind: .kind,
      type: .type,
      metadata: { name: .metadata.name, namespace: "lfx" },
      data: .data
    }' \
  | kubectl --context="$DST_CONTEXT" apply -f -

# Option B — install into the namespace that already has the secret
HELM_NS=lfx-v2-campaign-service

# Option C — point PG* at a local database in values.local.yaml
# (override PGHOST/PGPORT/PGUSER/PGPASSWORD/PGDATABASE with `value:`
#  entries instead of secretKeyRef; see values.yaml for the keys)
```

Then:

```sh
# 1) Copy the example override (gitignored once renamed)
cp charts/lfx-v2-campaign-service/values.local.example.yaml \
   charts/lfx-v2-campaign-service/values.local.yaml
# Edit values.local.yaml as needed (encryption key sample is included).

# 2) Build the image (pullPolicy: Never in the local values file)
make docker-build

# 3) Load the image into your local cluster if needed (kind example):
#    kind load docker-image \
#      ghcr.io/linuxfoundation/lfx-v2-campaign-service/campaign-service:latest

# 4) Install / upgrade the chart (uses HELM_NS from above)
make helm-install-local HELM_NAMESPACE="$HELM_NS"
```

`values.local.example.yaml` documents the copy path and
`make helm-install-local` target. Uninstall with `make helm-uninstall`.

## Development

Common workflow targets (see the `Makefile` for the full list):

```sh
make all           # clean → apigen → fmt → lint → test → build
make clean         # remove bin/ and coverage.out
make apigen        # generate API code from design/ (required before first build)
make fmt           # format Go code (gofmt + simplify)
make check-fmt     # verify formatting (used in CI)
make lint          # run golangci-lint
make test          # run tests with race detector and coverage
make build         # build a local binary
make build-release # build a static release binary for Linux
make run           # build and run locally (needs PG* env; see above)
```

### MegaLinter (local)

CI runs MegaLinter on pull requests and merge-queue entries
(`.github/workflows/mega-linter.yaml`, Go flavor `v9.1.0`). Config lives in
`.mega-linter.yml`. To reproduce
locally with Docker or OrbStack:

```sh
docker pull oxsecurity/megalinter-go:v9.1.0
docker run --rm \
  -e DEFAULT_WORKSPACE=/tmp/lint \
  -e GOTOOLCHAIN=auto \
  -e MEGALINTER_CONFIG=.mega-linter.yml \
  -v "$PWD:/tmp/lint:rw" \
  oxsecurity/megalinter-go:v9.1.0
```

Reports are written under `megalinter-reports/` (gitignored). The first
pull is large; a full run often takes several minutes.

For a faster secrets-only check matching the CI gitleaks step:

```sh
gitleaks detect --source . --config .gitleaks.toml
```

Note: if this checkout is a git worktree, the containerized `git_diff`
linter may fail because the worktree `.git` path is not visible inside
the container. That does not affect GitHub Actions (normal
`actions/checkout`).

## Knowledge Base (OKF)

`docs/knowledge/` is an [Open Knowledge Format (OKF)](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
bundle — plain markdown with YAML frontmatter — that gives humans and AI
agents a structured map of this repo's architecture, Kubernetes resources,
Go packages, and feature specs. Start at
[`docs/knowledge/index.md`](docs/knowledge/index.md).

**When to update it:** after merging a feature PR, changing an API
endpoint, adding or modifying a Helm resource, or changing a package's
responsibility.

**How to update it:**

1. Edit the relevant existing concept file under `docs/knowledge/**`, or add
   a new one with OKF frontmatter (`type`, `title`, `description`) if no
   existing concept covers the change. Do **not** regenerate with
   `go run ./cmd/okfgen` — that tool bootstraps new subtrees and will
   overwrite hand-edited concept files.
2. Add or update the concept's `* [Title](url) - description` bullet in the
   relevant `index.md`.
3. Add a new file `docs/knowledge/log/YYYY-MM-DD-<slug>.md` (slug = ticket +
   short description), with a first H1 dated to match the filename followed
   by `**Update** — <what changed and why>.` One file per entry — this keeps
   concurrent PRs from ever editing the same log file.

**Validate before pushing:**

```sh
go run ./cmd/okfvalidate ./docs/knowledge
```

This is the same check `.github/workflows/validate-okf.yml` runs in CI.

Agents are expected to do this bookkeeping automatically (see `CLAUDE.md`);
developers making manual changes should follow the same convention.
