---
type: "Kubernetes Resource"
title: "Deployment"
description: "Helm Deployment for the campaign service, including PG*, CREDENTIAL_ENCRYPTION_KEY, SNOWFLAKE_* and the AI_PROXY_URL / AI_API_KEY credentials from lfx-v2-campaign-service-secrets, plus plain non-secret values such as AI_MODEL."
resource: "charts/lfx-v2-campaign-service/templates/deployment.yaml"
---

# Deployment

Kubernetes Deployment manifest for the campaign service, defined in the Helm
chart. Application env vars come from `values.yaml` `app.environment`,
including `PGHOST` / `PGPORT` / `PGUSER` / `PGPASSWORD` / `PGDATABASE` /
`PGENGINE` and `CREDENTIAL_ENCRYPTION_KEY` via `secretKeyRef` to
`lfx-v2-campaign-service-secrets` (keys `host`, `port`, `username`,
`password`, `dbname`, `engine`, `credential-encryption-key`). The encryption
key is required whenever a database URL is configured because startup
initializes the AES-GCM encryptor before opening the pool used by `/readyz`.

`SNOWFLAKE_ACCOUNT` / `SNOWFLAKE_USER` / `SNOWFLAKE_PRIVATE_KEY` also come via
`secretKeyRef` to `lfx-v2-campaign-service-secrets` (keys `snowflake-account`,
`snowflake-user`, `snowflake-private-key`), each marked `optional: true` — unlike
the Postgres/encryption vars, an unset Snowflake secret does not block startup:
the warehouse client is a read-only enrichment for the email channel's
past-editions audience lookup (`internal/platform/snowflake`), and its absence
just narrows a built audience to country-only rather than failing the pod.
`SNOWFLAKE_WAREHOUSE` / `SNOWFLAKE_ROLE` are plain (non-secret) values, empty by
default so a cluster with no override uses the account's default.

`AI_PROXY_URL` / `AI_API_KEY` follow the Snowflake pattern exactly: `secretKeyRef`
to `lfx-v2-campaign-service-secrets` (keys `ai-proxy-url`, `ai-api-key`), both
`optional: true`. Generated email copy is an enrichment on the same terms — a
cluster with no LiteLLM provisioning must still start, and the `GenerateEmailCopy`
endpoint returns 503 ServiceUnavailable when unconfigured. The secret is the **proxy's**
key, not a Bedrock or Anthropic credential. `AI_MODEL` is a plain (non-secret) value,
empty by default: a model id is not a credential, and empty selects `llm.DefaultModel`.
All three are printed by `Config.String()` — the model verbatim, the key through
`redactSecret`, and the URL through `redactAIProxyURL`, which keeps only the scheme,
renders the host as `xxxxx`, and masks the whole value if it will not parse or its
scheme is neither `http` nor `https`. The host is masked because it can itself BE the
pasted secret — `AI_PROXY_URL=https://sup3r-s3cret/` is a well-formed absolute URL.
That reduction is deliberate: `Config.String()` renders at startup before
`llm.NewClient` can reject a bad value, so it is the only place a pasted credential
would otherwise land in the log. What survives is enough for the thing being
diagnosed — whether a proxy and key are configured at all.

`REDDIT_METRICS_ENABLED` is likewise a plain (non-secret) value, defaulting to
`"false"`. It is a feature gate rather than a credential: the Reddit reporting
contract this service implements is UNVERIFIED (LFXV2-2995) — the request and
response shapes are inferred, not confirmed by Reddit — so the metrics read is
shipped disabled and a cluster must opt in explicitly. The gate fails closed:
any value other than exactly `"true"` (including an empty string, `"1"`, or
`"TRUE"`) leaves the capability off, so a typo in a values override cannot
accidentally enable an unverified integration. The env var is read per call
(`RedditDispatcher.ReadMetrics`), not at construction, so flipping it needs a
pod restart to pick up the new `app.environment` value but no rebuild — and the
disabled path costs one env read. With the gate off the read returns
`ErrMetricsUnsupported`, which the service maps to the same 400 a platform with
no metrics adapter at all returns.

Probes: `livez` restarts a hung process (never touches the DB); `readyz` gates
traffic on DB connectivity. The `startupProbe` on `/readyz` carries a ~90s
`failureThreshold` budget for a database cold start. This budget is meaningful
because the process does NOT exit when the DB is unreachable at boot — the
container boots in 503 mode and retries the pool in the background (see the
`internal/container` concept), so `/readyz` stays 503 and the pod is kept alive
across the window rather than crash-looping.

**Rolling back is not the deploy run backwards.** `pool.go` only ever calls `m.Up()`,
so reverting the image leaves the schema at whatever version the newer binary migrated it
to. That is harmless for a migration that only ADDS a column the old code ignores, and it
is not harmless for one that adds a CONSTRAINT the old code has no error mapping for: the
old binary meets a 23505 it was never written to see and answers 500 where the new one
answers 409. The audience build lease (`000018`) is the current example.

So for `000018` the order is **database first, then image**: run its `.down.sql` (via
`migrate ... goto 17`) while the new image is still serving, then roll the Deployment
back. The reverse order leaves a window whose length is however long the rollback takes
to notice. `000018`'s down uses `DROP INDEX CONCURRENTLY` precisely so the drop does not
block writes from the pod still serving during that window.

**Database-first is a property of the individual down migration, not a rule for the
repo.** It is safe exactly when the down is benign to the binary STILL SERVING during the
window — and database-first is the ordering that maximises that window, so a down that is
not benign is worst run first. `000018` qualifies: dropping the lease returns the new
binary to the old behaviour (two concurrent builds for one brief each create a full set of
HubSpot lists) without breaking any statement it issues. Others plainly do not.
`000005_create_campaign_audiences_table.down.sql` does `DROP TABLE campaign_audiences`,
and `000015_brief_actor_columns.down.sql` drops `created_by` / `updated_by` on
`campaign_briefs` — both remove schema the current binary reads and writes, so running
either ahead of the image rollback turns the whole window into 500s. (`000015`'s own
header already says it exists for migration symmetry, not as a routine operation: the
actor is recorded nowhere else, so its down destroys the audit trail outright.) Rollback
ORDER has to be decided per migration, and for a multi-version `goto` per migration
CROSSED — `goto 17` from 18 runs one down; `goto 4` runs fourteen, and the order is only
as safe as the least benign of them.

Two things this does not mean. It is not an argument for skipping the down migration and
leaving the constraint in place, which trades a documented procedure for a silent
error-surface regression. And it is not a claim that the newer schema is what makes the
old binary unsafe — the old binary's behaviour on the old schema is by definition what
rolling back is asking for, including whatever the new constraint existed to prevent.

See [charts/lfx-v2-campaign-service/templates/deployment.yaml](../../../charts/lfx-v2-campaign-service/templates/deployment.yaml).
