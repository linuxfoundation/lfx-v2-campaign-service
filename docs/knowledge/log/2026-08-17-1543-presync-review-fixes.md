# 2026-08-17 — 1543 PreSync migration review fixes

**Fix** — Four review findings on the PreSync migration Job, two of them operational.

**The hook referenced a ServiceAccount that does not exist yet.** `serviceAccountName` was set
whenever `serviceAccount.create` OR `serviceAccount.name` was truthy, but a chart-created account
carries no hook annotation, so ArgoCD applies it in the ordinary Sync wave — AFTER the PreSync
hook. On a first install the hook pod would reference an account that has not been created,
failing the sync before any other resource applied. Only an EXPLICITLY NAMED (externally managed)
account is referenced now. Falling back to the default costs nothing: the Job talks to PostgreSQL
and makes no Kubernetes API calls, so it needs no bound identity.

**The Job received the entire serving environment.** The env loop ranged over all of
`app.environment`, handing a one-shot migration pod `CREDENTIAL_ENCRYPTION_KEY`, `AI_API_KEY` and
`INDEXER_SERVICE_TOKEN` — verified by rendering the chart, not by reading it. Now an ALLOW-LIST of
`DATABASE_URL` and `PG*`. Deliberately an allow-list rather than a deny-list: a deny-list is
falsified by the next secret added to values.yaml, silently. `extraEnv` was the second route to
the same leak and is filtered the same way; a decoy `SECRET_LEAK` entry confirms it is dropped
while `PGSSLMODE` passes.

**`VerifySchema` could report a stale database healthy.** It checked indexes only, never
`schema_migrations`. A database at an older version can carry every required index and still lack
a column this binary selects — migration 000025 adds `conversion_pixel_id`, so a v24 database
passes every index assertion and then errors on the first Reddit query, with `/readyz` reporting
healthy throughout. That is the readiness signal lying about the thing it exists to gate, and it
is reachable without a broken deployment: a selective sync skipping the hook, a restore from an
older snapshot, or a restart after a migration failed partway.

`checkSchemaVersion` now refuses an older version and a dirty row. A NEWER database is accepted
deliberately — during a rollout the hook has already run while the previous release is still
serving, and expand/contract guarantees the older binary works against the newer schema. Refusing
it would fail every old pod the moment the hook completed.

The expected version is DERIVED from the embedded migrations rather than hardcoded, and a test
recomputes it independently. Mutating the function to return a hardcoded 24 fails that test with
`want 25` — which is the stale-by-one state the check exists to refuse.

**Docs** — The `migrateCmd` godoc had the safety argument inverted. It claimed running before the
Deployment rolls "keeps the previous release from being migrated out from under"; that is exactly
what does happen — the hook completes while the OLD ReplicaSet is still serving. What makes the
overlap safe is expand/contract migration authoring, not the ordering. What the ordering genuinely
buys is failure handling: a failed Job halts the sync with logs and leaves the prior ReplicaSet on
the old schema, rather than crash-looping a new pod on a half-migrated database. Stating it the
old way appeared to relax the authoring rule the repo depends on.
