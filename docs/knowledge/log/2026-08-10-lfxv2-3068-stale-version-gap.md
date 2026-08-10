# 2026-08-10 — Retiring a version-gap exemption is part of merging the migration

**Fix** — `internal/infrastructure/postgres/outbox_repo_test.go` (LFXV2-3068). Deletes
`allowedVersionGaps[16]`, the exemption that excused a missing `000016` while PR #95 held it.
#95 merged, `000016_campaign_actor_columns` is in the tree, and the entry outlived its gap.

## Why a stale exemption is worse than no exemption

`TestMigrations_NoVersionGaps` exists because golang-migrate records only the HIGHEST version
it has applied. A tree that deploys carrying a gap will thereafter skip anything that fills
that gap — silently, permanently, with `Up()` reporting success.

`allowedVersionGaps` suspends that guard at one specific version, deliberately, so a branch
can stay green while a sibling PR that claims the number is still open. The suspension is a
merge-ORDERING obligation, not a numbering opinion.

Once the sibling lands, the obligation is discharged and the entry becomes a hole in the
guard at exactly the version most likely to be reused next. `TestMigrations_AllowedVersionGapsAreStillOpen`
is the second half of the mechanism: it fails when an entry names a version that now exists,
which is what surfaced this. Every open PR on the repo was red on this package until the
entry came out — the failure is inherited from `main`, not caused by whichever branch is
being reviewed.

## The map stays declared and empty

Deleting the whole variable would have been the smaller diff, and the wrong one. A gap is a
legitimate transitional state; the next sibling PR to need one should find the mechanism and
add an entry, not re-derive from scratch why it exists. The retired entry's rationale stays
as a comment in the empty body, so the record of what was excused and why survives the
exemption itself.

## Related

- `docs/knowledge/code/internal-infrastructure-postgres.md` — migrations, `000015`/`000016`
