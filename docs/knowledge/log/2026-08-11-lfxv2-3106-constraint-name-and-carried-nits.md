# 2026-08-11 — LFXV2-3106: the 23505 mapping now names the index it means

**Fix** — three statements translated "some unique index on `campaign_audiences` fired" into
`ErrAudienceBuildInFlight`, which is a claim about one specific index. Plus the five carried
nits from dealako's round-5 review on PR #106.

This is LFXV2-3059's work arriving late. It was written on that branch and reviewed there, but
the commit never reached the remote before PR #106 merged, so none of it is on `main`. LFXV2-3106
carries it across unchanged apart from this paragraph; read every reference to #106 below as
describing where the finding came from, not where the fix landed.

## The mapping said less than the sentinel

`isUniqueViolation` matches SQLSTATE `23505` and nothing else. `ErrAudienceBuildInFlight` means
something narrower: *the build lease `uq_campaign_audiences_brief_platform_building` is held by
someone else*. Three call sites in `audience_repo.go` bridged that gap with a comment reasoning
about which unique indexes the table happens to carry — today the primary key (server-generated,
so unreachable) and the lease. The reasoning was correct and the outcome was correct. What it
could not survive is a migration: the next unique index on `campaign_audiences` inherits the
sentinel silently, and no test in this package fails when it does.

`isUniqueViolationOn(err, constraint)` matches on `*pgconn.PgError.ConstraintName` as well as the
code. Postgres populates that field with the bare index name for a partial unique index, not only
for a named table constraint — verified against a live server:

```
ERROR:  23505: duplicate key value violates unique constraint "uq_probe_partial"
CONSTRAINT NAME:  uq_probe_partial
```

`isUniqueViolation` stays for the callers where the statement can violate exactly one constraint
(`connection_repo.go`, `brief_repo.go`); its doc comment now says which of the two to reach for.

## The test that had to be constructed, because no arrangement of rows reaches the case

Every lease test in `dbtest` fires the REAL lease index, so a mapping that matches any 23505 on
`campaign_audiences` answers all of them correctly. Measured, not assumed: degrading
`isUniqueViolationOn` back to SQLSTATE-only leaves all five PASSING. That is the pre-fix
behaviour, so the suite as it stood did not bind this change at all — the permissive direction
being untestable *is* the evidence the narrowing was unpinned.

`TestAudienceLeaseMappingIgnoresOtherUniqueIndexes` is the missing witness. It CREATEs a second
unique index on the table for the duration of the test, violates only that one, and asserts the
error is not `ErrAudienceBuildInFlight`. Two constraints shaped it:

- **The two indexes are separated by their PREDICATE, not their key.** The obvious separation —
  a different `platform` — is unavailable: migration 000006 CHECKs `platform IN ('hubspot')`, so
  every row the table can hold shares the lease's second key column. The probe covers
  `status = 'failed'` instead, which is outside the lease's `status = 'building'` predicate, so
  two failed rows under one brief violate the probe and never enter the lease's predicate.
- **"Not the sentinel" is not a sufficient assertion.** It passes when the insert fails for any
  unrelated reason, and the first draft of this test did exactly that: it used a second platform
  and the 23514 from `campaign_audiences_platform_valid` was not the sentinel either. The test
  asserts the error is a 23505 naming the probe, which is what caught the bad premise.

**Revert-verified in both directions.** Degrading the helper to SQLSTATE-only fails this test and
`TestIsUniqueViolationOn` (added below) and nothing else; the other five lease tests still pass. Making the comparison wrong *inside*
`isUniqueViolationOn` fails four live tests —
`TestAudienceBuildLeaseAdmitsExactlyOneConcurrentBuild`, `...FreesOnCompletion`,
`...RefusesPlainCreate`, `...RefusesUpdateBackToBuilding`. The first attempt at this verification
was worthless and worth recording: breaking the *constant* instead breaks `requiredIndexes` in
`pool.go`, which shares it, so the boot guard rejects the schema before any insert reaches a
23505. The tests failed loudly for a reason that had nothing to do with the mapping under test.

## Carried nits from the review

**The concurrency test's docstring claimed a race it does not run.** Eight goroutines are released
together, but `CreateAudienceForApprovedBrief` opens with `SELECT ... FOR UPDATE` on the brief, so
they queue at the BRIEF row lock and each reaches its `INSERT` with the previous one committed.
The index still decides the outcome; the inserts are not simultaneous. Docstring rewritten to say
so — the test is unchanged and still valid.

**`ConfirmBriefApproved` never said where `expectedVersion` comes from.** Its only producer is the
second return value of `CreateAudienceForApprovedBrief`: the brief version observed under the lock
at the moment the lease was taken. That pairing is the whole meaning of the check — not "the brief
is approved" but "the brief has not changed since the claim that authorised this build". Now
stated on the interface.

**000018's one-statement warning sat ~80 lines above the statement.** Restated directly above the
`CREATE UNIQUE INDEX CONCURRENTLY`, where someone about to append a second statement is looking.

**Two appended log sections used `**Kind:** Fix`** instead of this file's own `**Fix** — <what>`
form. Reformatted.

**A failure message named a migration that does not exist.** `TestEveryUniquePartialIndexIsRequired`
said the schema "cannot be true while 000013 and 000020 exist"; the lease index is 000018 and there
is no 000020. The number was wrong when it was written, and the message only ever prints on a
failure — so the misdirection would land on whoever was already debugging a broken registry query.

## The probe index had to be scoped, not just named

Copilot caught this in the first version of the test above, as a suppressed comment. The probe was
`CREATE UNIQUE INDEX ... ON campaign_audiences (brief_id) WHERE status = 'failed'` — a schema-wide
object with a table-wide predicate, which is precisely the thing `dbtest.go:66-72` forbids: "Tests
therefore share a schema and MUST NOT share rows — use UniqueID for every identifier a test writes."
An index name and a predicate are both identifiers a test writes.

Reachability measured rather than assumed, in both directions. No test in the suite leaves two
failed audiences under one brief, so the unscoped form was not failing today. The mechanism is real
though: two such rows are legal (the lease covers `'building'` only; nothing constrains `'failed'`),
and with a pair present Postgres refuses — `could not create unique index ... Key (brief_id)=(...)
is duplicated`. The first unrelated test to persist that pair would have broken this one for a
reason having nothing to do with the mapping it pins.

The predicate now also carries `AND brief_id = '<this brief>'::uuid` and the index name carries the
brief id, so neither can collide with another test or an earlier run. The brief id is interpolated
because `CREATE INDEX` takes no bind parameters; it is a UUID Postgres minted and the test read back
through `RETURNING`, and it is re-parsed with `uuid.Parse` first so a later change to the helper
cannot quietly make this a place an arbitrary string reaches DDL.

## A unit test so the witness cannot skip

`TestAudienceLeaseMappingIgnoresOtherUniqueIndexes` is the stronger test — only a real server proves
a partial unique index populates `ConstraintName` — but it SKIPS when `TEST_DATABASE_URL` is unset,
and a skipped test prints `ok`. `TestIsUniqueViolationOn` is a table-driven unit test over synthetic
`*pgconn.PgError` values (matching name, a different name under the same 23505, an empty name, the
right name under 23514, a 23503, a non-`PgError`, nil) that runs everywhere. It includes a wrapped
case deliberately: every production caller reaches the helper through at least one `%w`, so a
version matching on the concrete type would pass a bare-value test and fail on every real row.
Degrading the helper to SQLSTATE-only fails two of its subtests.

## Verification

`go build`, `go vet`, `golangci-lint run ./...` clean. Full suite green against a live
PostgreSQL 16 (`-p 1`; the two live packages cannot migrate the same database concurrently —
see `2026-08-10-lfxv2-3059-ci-postgres-concurrent-migrate-deadlock.md`).
