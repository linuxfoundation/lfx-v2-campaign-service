# Quickstart: Database Connection Health Check

**Feature**: `002-db-conn-check` | **Date**: 2026-07-09

Validate that readiness reflects PostgreSQL connectivity while
liveness stays independent. See [contracts/](./contracts/) for the
HTTP/env contract and [data-model.md](./data-model.md) for signals.

## Prerequisites

- Go toolchain matching `go.mod`
- Reachable PostgreSQL instance (local CloudNativePG, Docker, or shared env)
- Connection fields available as env vars (or via Helm secret injection in-cluster):
  - `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, `PGDATABASE`
  - `CREDENTIAL_ENCRYPTION_KEY` (base64-encoded 32-byte AES key;
    required whenever a database URL is configured — startup
    initializes the encryptor before the pool used by `/readyz`)
- For an in-cluster development database: working `kubectl` context
  (e.g. `KUBECONFIG=$HOME/.kube/lfx-v2-dev`), permission to read
  `lfx-v2-campaign-service-secrets`, and ability to create a jump
  pod / port-forward in the `lfx-v2-campaign-service` namespace

Local/test sample (NOT for production) — base64 of
`LFX-campaign-local-dev-aes-256!!`:

```bash
export CREDENTIAL_ENCRYPTION_KEY='TEZYLWNhbXBhaWduLWxvY2FsLWRldi1hZXMtMjU2ISE='
```

## 0. Build and run against lfx-v2-dev (OrbStack laptop)

In **lfx-v2-dev**, Postgres is RDS behind `lfx/rds-postgres`
(`ExternalName`). A direct Service port-forward will not work.
Use a jump pod + port-forward. Credentials:
`lfx-v2-campaign-service/lfx-v2-campaign-service-secrets`
(keys: `host`, `port`, `username`, `password`, `dbname`, `engine`).

**Terminal 1 — jump pod to RDS:**

```bash
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/lfx-v2-dev}"

kubectl -n lfx-v2-campaign-service get secret \
  lfx-v2-campaign-service-secrets

RDS_HOST="$(kubectl -n lfx-v2-campaign-service get secret \
  lfx-v2-campaign-service-secrets \
  -o jsonpath='{.data.host}' | base64 -d)"
RDS_PORT="$(kubectl -n lfx-v2-campaign-service get secret \
  lfx-v2-campaign-service-secrets \
  -o jsonpath='{.data.port}' | base64 -d)"
RDS_PORT="${RDS_PORT:-5432}"

# Both must be non-empty (empty → socat gets tcp-connect:: and hangs)
if [ -z "$RDS_HOST" ] || [ -z "$RDS_PORT" ]; then
  echo "RDS_HOST/RDS_PORT empty — refuse to create broken tunnel" >&2
  exit 1
fi
echo "tunnel target ${RDS_HOST}:${RDS_PORT}"

# Do NOT use --command (replaces socat entrypoint) or -it
# (Gatekeeper blocks interactive TTYs). Keep the pod (no --rm)
# until you finish port-forwarding.
kubectl -n lfx-v2-campaign-service delete pod pg-tunnel \
  --ignore-not-found
kubectl -n lfx-v2-campaign-service run pg-tunnel \
  --restart=Never --image=alpine/socat -- \
  tcp-listen:5432,fork,reuseaddr \
  "tcp-connect:${RDS_HOST}:${RDS_PORT}"
kubectl -n lfx-v2-campaign-service wait --for=condition=Ready \
  pod/pg-tunnel --timeout=60s
# Must show tcp-connect:<rds-host>:<port>, not tcp-connect::
kubectl -n lfx-v2-campaign-service get pod pg-tunnel \
  -o jsonpath='{.spec.containers[0].args}{"\n"}'
```

**Terminal 2 — forward laptop → jump pod (leave running):**

```bash
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/lfx-v2-dev}"
kubectl -n lfx-v2-campaign-service port-forward \
  pod/pg-tunnel 5432:5432
# Expect: Forwarding from 127.0.0.1:5432 -> 5432
```

**Terminal 3 — build, load creds, run:**

```bash
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/lfx-v2-dev}"

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
# Local/test sample only — see Prerequisites above.
export CREDENTIAL_ENCRYPTION_KEY='TEZYLWNhbXBhaWduLWxvY2FsLWRldi1hZXMtMjU2ISE='

# Must print 127.0.0.1 — not the RDS FQDN from secret `host`
echo "PGHOST=$PGHOST PGPORT=$PGPORT PGDATABASE=$PGDATABASE"

make build
make run
# Startup log "dependency container initialized" must show
# database=127.0.0.1:5432/... (RDS hostname means PGHOST is wrong)
```

Then continue with the health curls in section 2. Full narrative
also lives in the root `README.md` under
"Build and run locally (against lfx-v2-dev)".

## 1. Automated tests (no real DB required)

```bash
make test
# or
go test -race ./internal/service/ \
  ./internal/infrastructure/postgres/ \
  ./internal/infrastructure/config/ \
  ./internal/container/
```

**Expect**: Unit tests pass for ready-with-ping-ok,
not-ready-on-ping-fail, livez-ok-when-ping-would-fail, and
missing-config startup failure paths.

## 2. Local run — database healthy

```bash
export PGHOST=127.0.0.1
export PGPORT=5432
export PGUSER=<user>
export PGPASSWORD=<password>
export PGDATABASE=<dbname>
# Local/test sample only — see Prerequisites above.
export CREDENTIAL_ENCRYPTION_KEY='TEZYLWNhbXBhaWduLWxvY2FsLWRldi1hZXMtMjU2ISE='

make run
# or: go run ./cmd/campaign-service
```

In another terminal:

```bash
curl -sS -o /tmp/readyz.body -w "%{http_code}\n" http://127.0.0.1:8080/readyz
# expect: 200 and body OK

curl -sS -o /tmp/livez.body -w "%{http_code}\n" http://127.0.0.1:8080/livez
# expect: 200 and body OK
```

## 3. Local run — database unreachable

Keep the already-running service up. Do **not** restart with a bad
host/port — startup migrates and pings the DB, so the process exits
before `/readyz` is available. Instead stop the port-forward / jump
pod (or stop local Postgres), then:

```bash
curl -sS -o /tmp/readyz.body -w "%{http_code}\n" http://127.0.0.1:8080/readyz
# expect: 503

curl -sS -o /tmp/livez.body -w "%{http_code}\n" http://127.0.0.1:8080/livez
# expect: 200 (liveness unchanged)
```

## 4. Missing credentials (fail-fast)

```bash
unset PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE
go run ./cmd/campaign-service
# expect: non-zero exit; process does not serve as ready
```

## 5. In-cluster (after Helm env wiring)

1. Confirm the ExternalSecret-managed secret has keys
   `host`, `port`, `username`, `password`, `dbname`, and
   `credential-encryption-key` (base64 32-byte AES key).
2. Deploy chart revision that injects `PG*` and
   `CREDENTIAL_ENCRYPTION_KEY` via `secretKeyRef`.
3. With DB up: pod Ready; `kubectl exec`/`curl` `/readyz` → 200.
4. Simulate DB outage (network policy / scale DB down):
   `/readyz` → 503, pod not Ready; `/livez` still 200 (no restart
   solely from DB down).

## 6. Observability smoke (optional)

With OTEL exporters enabled (`OTEL_*` env per existing service
docs):

1. Hit `/readyz` with DB up and down.
2. Confirm DB-related spans appear from
   `github.com/exaring/otelpgx` (registered on the pool). Readiness
   failures return HTTP 503; debug-level logs may include
   `readyz: service not ready` when log level is debug.
3. Confirm password does not appear in span attributes or log
   fields.
