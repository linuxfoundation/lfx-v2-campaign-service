# 2026-08-20 — LFXV2-2643 a schema check that could not see what it verified

**Fix** — batch of suppressed-review fixes on PR #159. The unifying defect is not any one
line: three of these are assertions whose STATED guarantee is stronger than the
thing they actually observe, and the observation is where the strength has to
live.

**The snapshot could not express the type it compared.** `schemaObjects` in
`migrate_down_live_test.go` built its column rows from
`information_schema.columns.data_type`, and carried a comment claiming "a down
that restores a column as the wrong type has not restored it". That is exactly
the case it could not detect. `data_type` is the SQL-standard type NAME with the
modifier stripped: on PG16, `NUMERIC(14,2)` and `NUMERIC(14,3)` both render as
the bare string `numeric`, and `VARCHAR(50)` and `VARCHAR(200)` both as
`character varying`. The snapshot also selected no `column_default` at all, so a
down file dropping a DEFAULT was invisible too. `campaigns.budget_amount` is
`NUMERIC(14,2)` (000002), so the blind spot covered a real, live column.

The column arm now reads `pg_attribute`, with
`format_type(a.atttypid, a.atttypmod)` for the modifier-carrying rendering and
`pg_get_expr(d.adbin, d.adrelid)` joined from `pg_attrdef` for the default.

Proven by mutation, and the CONTRAST is the evidence. Mutating 000025's down so
it also retypes an unrelated pre-existing column
(`ALTER TABLE campaigns ALTER COLUMN budget_amount TYPE NUMERIC(14,3)`) — which
leaves the column SET, names and nullability identical, so only the modifier
differs:

    PRE-FIX  : ok    ...postgres/dbtest  4.530s
    POST-FIX : FAIL  stepping DOWN from version 25 did not restore the schema
                     that migrating UP to version 24 produces:
                       UNEXPECTED (the down did not remove it):
                         column:campaigns.budget_amount numeric(14,3)
                       MISSING (the down removed too much):
                         column:campaigns.budget_amount numeric(14,2)

The DEFAULT half was mutated separately (`ALTER COLUMN variant DROP DEFAULT`):
pre-fix `ok`, post-fix FAIL naming
`variant text NOT NULL DEFAULT 'default'::text` as missing.

*A mutation-design note worth keeping.* The first attempt mutated the UP file's
`NUMERIC(14,2)` to `(14,3)` and SURVIVED the fixed test. That was not a weak
fix — it was a void experiment: `schemaAtVersion` builds its reference by
migrating a fresh database UP, so mutating an up file moves BOTH sides of the
comparison equally and they still agree. A mutation has to break the two sides
apart. For a test that compares down-stepping against an up-built reference,
only the DOWN file is a valid mutation site.

**A precondition that failed 1-in-750,000 on correct code.** `sealTextHostile`
required a sealed blob to contain a NUL byte *and* be invalid UTF-8. Either
alone already makes the value unstorable as text — verified against PG16 rather
than assumed, because a NUL is in fact a well-formed encoding of U+0000, so
"contains a NUL" is not a special case of "invalid UTF-8":

    NUL only (0x41 0x00) -> ERROR: invalid byte sequence for encoding "UTF8": 0x00
    invalid only (0x41 0xFF 0x42) -> ERROR: invalid byte sequence for encoding "UTF8": 0xff
    plain ASCII -> INSERT 0 1

So the conjunction bought no strength, and it cost a great deal: a seal here is
54 bytes, and re-measuring over 200,000 seals gives P(NUL) = 19.03%,
P(invalid UTF-8) = 100.00%, P(text-safe) = 0. Under `&&` the per-attempt hit
rate is 19%, so all 64 attempts missing is ≈1.4e-6 — a `t.Fatalf`, i.e. a red
build on a correct implementation. Under `||` it is 100%. Mutating `Encrypt` to
hex-encode (genuine encryption, deterministically text-safe) still trips the
Fatal, so the precondition kept its teeth.

**Two false statistics, one of them load-bearing.** Both this file and the
2026-08-19 fragment said "0 were text-safe and 80.8% carried a NUL byte
outright". 80.8% is P(NO NUL) — `(255/256)^54 ≈ 0.8095` — not P(NUL). The
inversion was not cosmetic: the figure was cited to argue 64 attempts was "not a
budget this is expected to spend", and at the true 19% that argument is exactly
what made the flake above real. A related sentence claimed GCM output carries
"NUL bytes, invalid UTF-8, and every byte value"; a 54-byte sample cannot carry
every byte value and carries a NUL only ~19% of the time.

**A false contract repeated in five places.** "The handler publishes the
returned row's version as the caller's ETag" was in a test godoc, a `t.Fatalf`
message, the 2026-08-19 log, `connection_repo.go` and `domain/port.go`. The
handler says the reverse in its own comment: `setCredential` discards the row
into `_` and answers 204. The companion claim that SetCredential is "the only
connection write that is NOT version-gated" is false too — only `updateConn`
parses `If-Match`; `createConn` and `deleteConn` are ungated as well. The
version bump is still worth asserting, but for ETag INVALIDATION, not
publication. Found by grepping the CLAIM across the repo rather than visiting
the reported line numbers, which is what turned four named sites into five.

**An assertion that survived an off-by-one.** `updated.Version <= created.Version`
accepts `version+2` as readily as `version+1`. Mutating the repo's UPDATE to
`version = version + 2`: the old assertion returned `ok`, the exact form
(`!= created.Version+1`, matching `connection_live_test.go:76-79`) FAILS with
"returned version 3, want exactly the created 1 + 1".

**A green run leaked a database per migration version.** `freshDatabase`'s
cleanup reported both the reconnect failure and the drop failure with `t.Logf`.
This test provisions one scratch database PER migration version, so against the
persistent local PG16 harness a silently-skipped drop accumulates a whole run's
worth while the run still reports green. Both are now `t.Errorf`: the contract
is that a green run leaves nothing behind, so failing to drop is a failure, not
a note.

**Rule** — when a comment states the guarantee an assertion provides, check that
the assertion's INPUT can distinguish the two cases the guarantee separates. All
three of the strong findings here had a correct-sounding sentence sitting
directly above a check that could not see the difference it described:
`data_type` cannot express precision, `<=` cannot express an exact increment, and
`t.Logf` cannot express a failure. The sentence is not the test.

**Fragment corrections.** Both corrected fragments belong to THIS ticket, so
both were corrected in place with a dated note rather than rewritten silently:
`2026-08-19-...-credential-roundtrip-live-tests.md` (the inverted NUL statistic
and the ETag/version-gating claim) and
`2026-08-20-...-scratch-dsn-drops-the-password.md` (see below). No other
entry's fragment was touched.

**Invented evidence in a fragment written an hour earlier.** The scratch-DSN
entry claimed "three losses in one line: the password, the `sslmode` (silently
DOWNGRADED from `require` to a hardcoded `disable`), and `connect_timeout`",
above a code block labelled "CI DSN". Actual CI
(`.github/workflows/lfx-v2-campaign-service-build.yaml:90`) sets
`?sslmode=disable` and no `connect_timeout`. Only the PASSWORD loss ever
happened; the hardcoded `sslmode=disable` matched what CI already sets. The
general rule — edit one field of a structured value, do not rebuild it from
parts — is sound and is kept, and the TLS/timeout losses are now stated as what
the construction WOULD drop rather than as what was observed. The failure mode
worth naming: reasoning from what a whitelist-rebuild *can* discard produced a
concrete three-item finding that read as evidence because it was formatted as a
diff and labelled with a source. If a line is labelled with where it came from,
it has to be pasted from there.
