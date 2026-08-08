# 2026-08-08 — A live-Postgres harness, because a regex cannot fail when the fix is removed

**Update** — Added `internal/infrastructure/postgres/dbtest`: a pool against
`TEST_DATABASE_URL` with the migrations applied, a `postgres:16-alpine` service container in
the build workflow so it actually runs, and two tests that assert about the server rather than
about SQL source text.

**Why this exists.** Every test in the postgres package matches regexes over the repo's own
SQL strings. That style caught real defects and is not being replaced. But it has a ceiling
that showed up concretely during the connection-repo review: the `UPDATE ... RETURNING` fix
could not be revert-checked, because removing it produced source text that no assertion
disagreed with. The standing rule here is that a new test is verified by reverting the fix and
confirming the test fails with the right diagnostic. A test class that structurally cannot do
that is not carrying the weight it appears to carry.

**The two tests pin migrations 000013/000014, and they were revert-checked against the server.**
The claim under examination is that once 000014 dropped `campaigns_brief_id_platform_key`, a
bare `ON CONFLICT (brief_id, platform)` matches no arbiter index and fails — which is why the
repo repeats `WHERE status <> 'deleted'` at every call site, and what `campaign_repo_test.go`'s
regex is really guarding. Adding the dropped constraint back to a migrated database makes both
assertions fail (`err = <nil>, want a *pgconn.PgError`, and the upsert stops inserting). That
is the check a regex cannot perform.

**The first version of the harness was wrong in a way that hid the revert-check.** `UniqueID`
derived its value from `t.Name()` alone, on the reasoning that a name is stable across
`-count=2` where a counter or a clock is not. Stable was the wrong property to optimise for.
The harness does not drop the database between runs, and `campaign_briefs` is unique on
`(project_id, event_slug)` for any non-archived row, so the second run collided with the first
run's row: `-count=2` failed outright, and the schema revert-check died at *setup* with a 23505
before the assertion it existed to exercise ever ran. A revert-check that fails for the wrong
reason proves as little as one that passes for the wrong reason. `UniqueID` now appends
`crypto/rand` bytes and keeps the name only as a human-readable prefix.

**The CI guard is in `Pool`, not in a sentinel test.** A single `TestHarnessRunsInCI` was the
first shape and it protects only the package it sits in — the next live test written elsewhere
would skip silently forever. `Pool` now consults `verdict(dsn, ci)`, which returns fatal when a
runner has no database, so every current and future caller is covered. `verdict` takes both
values as arguments rather than reading the environment, which is what makes the important case
testable at all: "on CI with no database" cannot be exercised by a live test, because there is
no database to run it against.

**The container's DSN carries a `secretlint-disable-line` directive.** secretlint flags any
`postgres://user:pass@host` literal, and the workflow has to state one somewhere: it is the
same throwaway credential pair the file hands the service container a few lines above, for a
database that exists for the length of one job and is reachable only from inside it. The
directive is per-line, matching `internal/infrastructure/config/config_test.go:235`, rather
than a path exclusion — `.gitleaks.toml` already allowlists `*_test.go`, so excluding a path
from secretlint would leave a class of files with no scanner covering it at all.

**What was deliberately not done.** No testcontainers dependency — a service container is the
standard GitHub Actions idiom and adds no module. No per-test schema — migrating is the slow
part, and isolation by unique key is what production does anyway. The image is pinned to
`16-alpine` rather than a digest: 16 is the major this repo already states the 000013/000014
staging was verified against, and a hand-maintained digest goes stale on every upstream CVE
rebuild, which is the likelier failure than a bad rebuild of the same major.
