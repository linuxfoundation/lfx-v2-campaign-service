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

Both endpoints are unauthenticated and are excluded from the generated
public API documentation.

## Environment variables

Configuration priority: CLI flags > environment variables > defaults.

### Required (startup)

These must be set for the process to start. Missing or incomplete
values cause a non-zero exit. In-cluster they are typically injected
From the ExternalSecret-managed Kubernetes secret
(`lfx-v2-campaign-service-secrets` in namespace
`lfx-v2-campaign-service`; keys `host`, `port`, `username`,
`password`, `dbname`, `engine`).

- `PGHOST` (secret key `host`) — PostgreSQL hostname
- `PGUSER` (secret key `username`) — PostgreSQL username
- `PGPASSWORD` (secret key `password`) — PostgreSQL password
  (never logged)
- `PGDATABASE` (secret key `dbname`) — PostgreSQL database name

The service composes the DSN in-process from these fields (no
`DATABASE_URL` env var required).

### Optional (with defaults)

- `PGPORT` (default `5432`; secret key `port`) — PostgreSQL port
- `PGENGINE` (default empty) — when set, must be `postgres` or
  `postgresql`
- `PORT` (default `8080`) — HTTP listen port (CLI flag `-p`)
- `HOST` (default `*`) — bind interface; `*` means all interfaces
  (CLI flag `-bind`)
- `DEBUG` (unset) — set to `true` to enable debug logging
  (CLI flag `-d`)
- `JWKS_URL` — JWT JWKS endpoint (reserved for auth; defaults to
  Heimdall JWKS URL in-cluster)
- `JWT_AUDIENCE` (default `lfx-v2-campaign-service`) — expected JWT
  audience
- `JWT_ISSUER` (default `heimdall`) — expected JWT issuer
- `NATS_URL` — NATS server URL (reserved for messaging; defaults to
  in-cluster NATS URL)

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
- `OTEL_TRACES_SAMPLE_RATIO` (default `1.0`) — ratio in `[0.0, 1.0]`

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

# Sanity-check before starting (must be 127.0.0.1, not the RDS FQDN)
echo "PGHOST=$PGHOST PGPORT=$PGPORT PGDATABASE=$PGDATABASE"
# optional: confirm the tunnel accepts TCP
nc -z 127.0.0.1 5432 && echo "tunnel port open"

make build
make run
# On startup, the log line "database pool initialized" must show
# database=127.0.0.1:5432/<dbname> — if it shows the RDS hostname,
# stop and fix PGHOST.
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

## Development

Common workflow targets (see the `Makefile` for the full list):

```sh
make apigen        # generate API code from design/ (required before first build)
make fmt           # format Go code (gofmt + simplify)
make check-fmt     # verify formatting (used in CI)
make lint          # run golangci-lint
make test          # run tests with race detector and coverage
make build         # build a local binary
make build-release # build a static release binary for Linux
make run           # build and run locally (needs PG* env; see above)
```

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
3. Append a dated entry to `docs/knowledge/log.md`:
   `## YYYY-MM-DD` followed by `**Update** — <what changed and why>.`

**Validate before pushing:**

```sh
go run ./cmd/okfvalidate ./docs/knowledge
```

This is the same check `.github/workflows/validate-okf.yml` runs in CI.

Agents are expected to do this bookkeeping automatically (see `CLAUDE.md`);
developers making manual changes should follow the same convention.
