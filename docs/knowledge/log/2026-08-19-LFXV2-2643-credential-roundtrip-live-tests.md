# 2026-08-19 — LFXV2-2643 credential round-trip against the live BYTEA column

**Verification** — four live-database tests added to
`internal/infrastructure/postgres/dbtest/credential_roundtrip_live_test.go`. No
production code changed; this entry records what the repo can now prove that it
could not before, and what an audit of the ticket's scope got wrong.

The ticket says it is "DIRECTLY BLOCKED ON LFXV2-2559 (Postgres provisioning)".
That line is stale. LFXV2-2559 was released 2026-07-30 and CI already
self-provisions: `.github/workflows/lfx-v2-campaign-service-build.yaml` runs a
`postgres:16-alpine` service container and sets `TEST_DATABASE_URL`, and the
`dbtest` harness has been in the tree for some time with ~14 `*_live_test.go`
files. Nothing here was blocked.

The gap that WAS real is narrower than the audit claimed, and worth stating
precisely because two of the five bullets were already covered:

- **Migrations down** is now covered, by
  `TestLiveMigrationsGoDownAndUpAgain` in `migrate_down_live_test.go`. An
  earlier revision of this entry recorded the case as unreachable — "testing it
  would mean widening the production API purely for a test" — and that was
  simply false. `migrations.FS` is already exported
  (`internal/infrastructure/postgres/migrations/migrations.go:13`), and
  `postgres.Migrate` does nothing with it a test cannot do: build an `iofs`
  source, point a migrator at a DSN, call a direction. Driving `Down` needs no
  new production API and no change to `Migrate`. The true statement is much
  narrower and is only about the startup path: `postgres.Migrate` calls `Up`,
  never `Down`.

  The claim is corrected here rather than deleted because of what it cost. It
  did not merely fail to describe the code — it presented a gap as CLOSED BY
  NECESSITY, which is the form of a wrong claim that stops anyone re-examining
  it. Everything it ruled out turned out to be a single test file.
- **Brief-repo version gating** is already covered, live, by
  `TestConfirmBriefApprovedWaitsForAnInFlightWithdrawal` in
  `audience_lease_live_test.go` — which does the harder thing, asserting the
  gate holds against an in-flight uncommitted withdrawal.

What was genuinely missing was the credential path. Every pre-existing
credential assertion in `connection_live_test.go` writes a literal such as
`[]byte("ciphertext-v1")`. That value is printable ASCII, so it survives a
column typed as text, a driver that transcodes on the wire, and an encryptor
that never ran — none of which it can distinguish. Real AES-256-GCM output
cannot: it is a random nonce followed by ciphertext and a tag, carrying NUL
bytes, invalid UTF-8, and every byte value. So the round-trip test seals a
deliberately non-ASCII plaintext with the PRODUCTION `crypto.AESGCM` and writes
it through the real repository.

The assertion set is three-part on purpose, because the obvious version of this
test proves nothing. Asserting only that the decrypted plaintext matches the
original passes for an encryptor that stores cleartext — cleartext decrypts to
itself. The test therefore also asserts the NEGATIVE: the stored column is not
the plaintext and contains no recognizable fragment of it. Third, the stored
bytes are byte-identical to what `Encrypt` returned, which is what makes this a
statement about the DATABASE rather than about crypto — if the column mangled
one byte, GCM authentication would fail and the failure would be read as
"decryption is broken" when the defect is in storage.

`SetCredential` had no live coverage at all, though it is the write the
credential-rotation path actually calls. It is also the only connection write
that is not version-gated, so the version bump is asserted rather than assumed:
the handler publishes the returned row's version as the caller's ETag.

**Follow-up — the BYTEA-vs-TEXT premise was nondeterministic.** The round-trip
test exists to catch the `credentials` column being typed TEXT, and it can only
do that if the bytes it writes are bytes TEXT refuses. It wrote a bare
`Encrypt` output — but GCM prefixes a RANDOM nonce, so "contains a NUL byte and
invalid UTF-8" was a property of each sample, not a guarantee. On a run that
drew text-safe ciphertext, a TEXT column round-trips it unchanged and the test
PASSES, missing the exact regression it was written for, with nothing to signal
the near miss.

Measured, the odds were reassuring and beside the point: over 200,000 seals of
this file's plaintext, 0 were text-safe and 80.8% carried a NUL byte outright.
A property that holds 99.99…% of the time is still not the property the test
claimed to rest on. `sealTextHostile` now ASSERTS it — a bounded 64-attempt
search (bounded so a no-longer-binary `Encrypt` fails loudly instead of
hanging), with exhaustion as a `t.Fatal`.

The proof is an A/B against a scratch migration that retypes the column to
TEXT, with `Encrypt` mutated to hex-encode its output — genuine encryption,
deterministically text-safe, i.e. the lucky-nonce run made reproducible. The
ORIGINAL assertion set PASSES against the TEXT column; the fixed test FAILS on
its precondition. That difference is the whole finding.

**Mutations** — the down-migration test needed three rounds, and the two
SURVIVORS are the useful part of the record:

- Emptying an index-only down file (000026's `DROP INDEX
  idx_campaign_jobs_retention`) SURVIVED a single `Down()`-to-zero: the end
  state drops every table, and a dropped table takes its indexes with it, so
  the mutation was invisible. Fixed by stepping down one version at a time and
  comparing the schema after each step.
- With stepping in place, emptying 000013's and 000021's down files STILL
  survived — because the reference schema was produced by walking a SECOND
  database down through the same mutated files, so both sides acquired the
  identical defect and the comparison always matched. A reference must be built
  by migrating a fresh database UP; that is the one construction that cannot
  inherit the fault it is meant to detect.

Each of the five emptied-down-file mutations now fails, as do the opposite
class (a down that drops a sibling table) and a wrong-type restore (re-adding
`variant` as INTEGER) — the last of which is why the schema snapshot compares
column types and constraint definitions, not just object names.

The four original mutations, each compiling, each reverted:

- Making `Encrypt`/`Decrypt` a passthrough fails the round-trip test. Checked
  twice: with the early passthrough guard disabled (`if false &&`) the negative
  column assertion still fails it on its own — "the credentials column holds the
  PLAINTEXT" — so the guard is not doing the work the assertion claims.
- Dropping `version = version + 1` from the `SetCredential` UPDATE fails with
  "returned version 1, want greater than the created 1".
- Removing the `isUniqueViolation -> domain.ErrConflict` mapping in `Create`
  fails with the raw `SQLSTATE 23505` reaching the caller — which the handler
  would answer 500 instead of 409.
- Dropping `AND status <> 'deleted'` from `SetCredential`'s WHERE fails the
  soft-deleted arm, which returned nil instead of `ErrNotFound` — a rotation
  against a deleted connection reporting success.

All four ran against a local PostgreSQL 16 and pass under `-count=2`, which is
the check that the `UniqueID` discipline holds across runs against a database
the harness does not drop.

**Follow-up — the failure messages report shape, not the secret.** Two
assertions originally printed the decrypted blob and its expected plaintext with
`%q` (`"decrypted plaintext = %q, want %q"`). Nothing confidential leaked: both
values are test literals sealed with the test key. The shape is still wrong, for
a reason that does not depend on today's values — this is the file a test
handling a REAL credential gets copied from, and the message renders into CI job
output. It also contradicts what the bundle already says the codebase does:
`internal-dispatch.md` drops the JSON unmarshal cause on a decrypted blob
precisely because it might one day quote plaintext bytes, keeping a suffix
"built from the project id, never from plaintext", and `internal-service.md`
records that the one cause that IS logged comes "from ciphertext and key
material only, never from plaintext".

Both now report length plus `sha256.Sum256` of each side, the pattern the same
file already used for the stored-column assertion. The digest is what keeps the
diagnostic usable: length alone cannot separate "same size, different bytes"
from a passing run, so dropping to lengths only would have traded a leak for a
test that fails without saying why. Mutating `Decrypt` to return a same-length
wrong plaintext yields `got 26 bytes sha256=b2cf0dec… want 26 bytes
sha256=48fa99d3…`; shortening it by three bytes yields `got 23 bytes … want 26
bytes …`, so the two fault classes stay distinguishable. The
`bytes.Contains(..., []byte("secret"))` assertion was left alone — it prints
only the fixed literal it searched for, never the blob.
