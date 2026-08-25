# 2026-08-24 — a deferred constraint comes due in the release it named

**Fix** — migration `000029` adds `CHECK (byte_size <= 31457280)` to `creative_assets`, the upper
bound `000028` deliberately deferred:

> An upper bound is deliberately NOT set here -- it has to equal the upload endpoint's request
> limit, and that endpoint does not exist yet, so it lands with it rather than being guessed now
> and silently disagreeing later.

This PR *is* that endpoint's release, and the bound had not landed. Every migration numbered above
`000028` was checked: none mentions `byte_size`. A deferral that names a release is a promise with
a due date, and nothing enforced the date.

## Which number, and why the other one is wrong

Two figures are in play and they are not the same quantity — they sit on opposite sides of a
decode boundary:

| figure | what it measures | where |
| --- | --- | --- |
| `31457280` (30 MiB) | **decoded** bytes | `maxCreativeStoredBytes`, `internal/service` |
| `41943040` (40 MiB) | base64 **characters** | the design's `MaxLength`, the wire schema |

`byte_size` is the **decoded** column. The service writes `ByteSize: int64(len(p.Bytes))` — a
`len()` on the already-decoded slice — and the column's existing CHECK ties it to
`octet_length(bytes)`, which counts stored octets. So the bound that "equals the upload endpoint's
request limit" *for this column* is `31457280`.

Using the base64 figure would have admitted rows a third larger than the handler accepts: exactly
the "silently disagreeing later" the deferral existed to prevent. The number is right only when
the unit is named with it.

## Why the database and not only the handler

The handler's `len(p.Bytes) > maxCreativeStoredBytes` guards the **HTTP path alone**. `byte_size`
is caller-supplied on the INSERT — `createCreativeAssetQuery` binds it as `$4` — so a repository
caller, a backfill, or any future writer reaches the row without passing that check.

That gap also undercut a bound documented elsewhere: `internal/dispatch`'s `maxVariantAssetBytes`
reasoning assumes a tripping asset adds **at most 30 MiB**. Nothing below the handler enforced
that assumption. A table-level CHECK makes it an invariant of the data rather than a promise each
writer independently keeps.

## Why the narrowing is safe

`README.md`'s expand/contract rule says a migration that **narrows** must not break a running N-1
binary. Adding a CHECK is a narrowing, so this needed proof rather than assertion.

`000028` is already in `main`, so the table may exist in a deployed environment — the tempting
shortcut ("it's new in this PR") is **false**. The actual safety argument is one level down: in
`main` the creative-asset repo exists but is **never bound into the container**. There is no
`SetCreativeAssetRepo` or `NewCreativeAssetRepo` call anywhere outside this release, so no live
code path can INSERT, and the table is necessarily **empty** wherever `000028` has been applied.
No existing row can violate the bound, and no old writer starts failing. The endpoint that writes
the table arrives in the same release as the constraint, already refusing anything larger.

Stated as a plain CHECK rather than `NOT VALID` + `VALIDATE`: with no rows to scan the ACCESS
EXCLUSIVE lock is momentary, and a plain CHECK is enforced for future writes immediately instead
of leaving a window where it is declared but not enforced.

## What pins it

`TestCreativeAssets_ByteSizeUpperBoundIsEnforcedByTheTable` inserts **directly**, bypassing the
repository — going through `CreateAsset` would test whichever value the caller set, while a direct
insert tests the **column**. Both edges are asserted, because a bound is only pinned if the test
notices it moving either way: 30 MiB + 1 must raise SQLSTATE `23514`, and exactly 30 MiB must be
**accepted**. Without the at-the-limit case, tightening the bound to `<` would keep the test green
while the endpoint's own ceiling started failing at the database.

The limit is written as a literal rather than imported from `maxCreativeStoredBytes`: the point of
the test is that the database agrees with that constant, and deriving it from the same constant
would make the test agree with itself if the constant moved.

**Mutation-verified, not asserted.** Dropping the constraint against the live database turns the
test red with its own message ("a byte_size of 30 MiB + 1 was accepted"); restoring it returns
green. Skip-count control proves the live tests really ran: **66 skips** without
`TEST_DATABASE_URL`, **0 skips and 277 passes** with it.

## The stale pointers this exposed

Two comments in `000028` promised things to "the follow-on PR", and the PR arrived:

- The per-row size bound — **landed**, in `000029`; `000028` now says so and points at it.
- The per-brief **CAP** — did **not** land. The cap value and global-vs-per-tier are product
  decisions, so it is tracked as `#171` rather than guessed. `000028`'s growth block now says the
  cap has not landed and that a per-row ceiling is not a per-brief cap, so the exposure it names
  still stands.

Both edits are comment-only; no SQL statement in the applied migration was touched. **Never edit
an applied migration's SQL** — a change is always a new version. Prose that has become false is
the one thing safe to correct in place, and leaving it false is worse: the next reader trusts it.
