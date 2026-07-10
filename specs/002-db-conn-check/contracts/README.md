# Contracts: Database Connection Health Check

Hand-authored contract notes for review. The authoritative HTTP surface remains the Goa design + `make apigen` output under `gen/http/`.

## What changes

| Surface | Change |
|---------|--------|
| `GET /livez` | **No contract change** — still 200 `text/plain` `OK\n`, unauthenticated |
| `GET /readyz` | **Semantics only** — same 200 / 503 shapes; 503 now also when DB connectivity check fails |
| Request/response schemas | Unchanged |
| Auth | Unchanged (none) |
| Public OpenAPI inclusion | Unchanged (`swagger:generate` false) |

## Env contract (pod → process)

| Env var | Secret key | Required |
|---------|------------|----------|
| `PGHOST` | `host` | Yes |
| `PGPORT` | `port` | No (defaults to `5432`) |
| `PGUSER` | `username` | Yes |
| `PGPASSWORD` | `password` | Yes |
| `PGDATABASE` | `dbname` | Yes |
| `PGENGINE` (optional) | `engine` | No |
| `CREDENTIAL_ENCRYPTION_KEY` | `credential-encryption-key` | Yes when a DB URL is configured |

Secret in lfx-v2-dev: `lfx-v2-campaign-service-secrets`
(namespace `lfx-v2-campaign-service`).

See [health-db.openapi.yaml](./health-db.openapi.yaml) for the unchanged HTTP excerpt with updated readiness description.
