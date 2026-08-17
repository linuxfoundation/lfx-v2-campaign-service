# 2026-08-17 — 1543 transient read classification

**Fix** — Five review findings on the PreSync migration Job, one of them a defect in the
previous round's own fix.

**A failed READ was reported as a schema VERDICT.** `checkSchemaVersion` wrapped every
`Scan` error in `ErrSchemaOutOfDate`, which `IsPermanentMigrationErr` classifies permanent.
A connection reset, a statement timeout, or a cancelled context between `Connect` and `Scan`
therefore made boot stop retrying against a database that was merely blipping — the pod
never comes up, and the recovery guidance tells the operator to run a migrate Job that would
not have helped. `Connect` immediately above it is deliberately left retryable with a comment
saying so; the read on that same connection is no more of a verdict than dialling it was.

Only two shapes are a statement about the schema: `pgerrcode.UndefinedTable` (nothing has
ever migrated this database) and `pgx.ErrNoRows` (the table exists and records no version).
Both are permanent until someone migrates. The classification moved into
`isUnmigratedSchemaErr` so it can be tested without a live database, and
`TestIsUnmigratedSchemaErr` pins both directions. Mutating the classifier to `return true`
compiles and fails the test on all five transient cases — a build break would have proved
nothing.

**The env allow-list had no test that could fail.** The existing assertions checked that the
allowed `PG*` keys were PRESENT, which a regression back to ranging over all of
`app.environment` also satisfies — while handing the Job `CREDENTIAL_ENCRYPTION_KEY`,
`AI_API_KEY` and `INDEXER_SERVICE_TOKEN`. Absence is the property that matters, so it needed
its own test. `TestMigrateJobEnvIsAllowListed` supplies forbidden entries through BOTH routes
(`app.environment` and `app.extraEnv` leak independently) and asserts they do not render,
while an allowed `PGSSLMODE` supplied via `extraEnv` still does — otherwise "reject
everything" would pass. Each route was mutation-verified separately.

Note the first version of that test asserted on the BARE variable names and failed on the
template's own comment, which names those variables to explain what it excludes. It asserts
on rendered `name:` entries and secret values instead. A test that matches documentation
reports a leak that is not there, which is the same defect class as a test that cannot fail.

**The Job silently mounted a ServiceAccount token.** Omitting `automountServiceAccountToken`
does not mean "no token" — Kubernetes projects the namespace default. A pod that holds
database credentials and makes no Kubernetes API calls was being handed a cluster credential
it never uses. Now explicitly `false`.

**Docs** — Three sites claimed a failed migration leaves the database on the old schema.
It does not: golang-migrate dirties the version BEFORE running a migration's SQL, and
committed statements stay committed, so a failed Job can leave the schema part-changed with
the prior release pointed at it. What halting the sync buys is that it does not get worse;
what makes the overlap survivable is expand/contract. Corrected in `migrateCmd`'s godoc, in
this ticket's earlier log fragment, and — the one that mattered most —
`internal-container.md`, which said `migrate.ErrDirty` was "reachable only in the migrate
Job, not at boot". The previous round made that false by having `VerifySchema` read
`schema_migrations`; the same document's later list already contradicted the paragraph.

`VerifySchema`'s godoc enumerated the three index sentinels as its complete failure set. It
now describes the set as a superset — index sentinels plus the version check's two — rather
than naming members, because an enumeration is falsified by the next sentinel added with
nothing failing to say so.

**Note** — One finding is NOT addressed here. The PreSync ordering is an ArgoCD annotation,
and `make helm-install` / `helm-install-local` (Makefile:180-192) are plain
`helm upgrade --install`, which ignores it: under those documented paths the Job is an
ordinary resource with no migrate-before-rollout guarantee. Adding Helm `pre-install`/
`pre-upgrade` hooks would close it, but that changes deployment behaviour on a path this
change cannot exercise and interacts with the ArgoCD hook semantics. Left for a deliberate
decision rather than folded into a review-fix commit.
