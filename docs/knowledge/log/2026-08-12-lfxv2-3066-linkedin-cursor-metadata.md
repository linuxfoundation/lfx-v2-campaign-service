# 2026-08-12 — the two older LinkedIn cursor walks, and what a missing envelope costs

**Update** — `internal/platform/linkedin/client.go` (LFXV2-3066). `findByName` and
`listCreativeURNs` now reject a response whose `metadata` block is absent, instead of reading it
as an exhausted cursor. 45 single-page fixtures across five test files gained `"metadata":{}`.

## This was a known hole, left open on purpose

LFXV2-3063 made `linkedInMetadata` a pointer and made `ListAdAccounts` reject nil. The same
exposure lived in two older walks, and the log for that round said so plainly: closing them meant
touching roughly fifty fixtures, which did not belong in a review round on ad-account discovery.

So this is not a newly discovered defect. It is the deferred half, and the estimate was right —
45, not "roughly fifty" by luck but because the count was checked before the deferral.

## The two walks do not cost the same thing

Worth separating, because the shared mechanism hides a large difference in blast radius.

`listCreativeURNs` truncating means a partial creative list reported as complete. The status
cascade then reports success and the service persists ACTIVE while the creatives it never
discovered stay DRAFT and never serve. Bad, recoverable, and visible to anyone who looks at the
campaign.

`findByName` truncating is worse in kind, not just degree. It is the find half of find-or-create,
so its absence value is the LICENCE TO CREATE. A dropped cursor envelope on an intermediate page
means "searched everything, found nothing" about a name that may sit on a page never fetched —
and the caller answers by creating a **duplicate paid campaign**. Real money, and nothing about
the outcome looks wrong at the time.

That asymmetry is why the `findByName` guard's message borrows the wording the repeated-token arm
right below it already uses: both mean *the search could not be completed*, and neither may be
reported as an absence. The file had already reasoned its way to the right answer for a looping
cursor; a missing envelope is the same failure arriving through a different door.

## The fixture sweep had one trap in it

A regex that adds `"metadata":{}` to every JSON literal containing `"elements"` also patches
`TestListAdAccounts_AbsentMetadataIsNotExhaustion` — the LFXV2-3063 guard whose entire purpose is
to omit the block. The test then passes for the wrong reason, and the guard it protects is
unprotected.

It was caught because that test failed loudly rather than silently: the fixture no longer matched
the behaviour the test asserted. Restoring the one fixture fixed it. The general shape is worth
naming — **a mechanical sweep over test data will happily delete the negative case**, and the
negative case is usually the one the fix exists for. Sweep, then read the diff for tests whose
NAME says they assert an absence.

## Verification

Both guards are revert-verified: removing the nil check makes
`TestFindByName_AbsentMetadataOnLaterPageIsNotAbsence` and
`TestListCreativeURNs_AbsentMetadataIsNotExhaustion` fail, and the first of those asserts on the
INTERMEDIATE page — page one advertises a cursor, page two drops its envelope — because a
first-page-only test would not distinguish this bug from an empty result set.

The full service suite passes; `internal/dispatch` needed 7 of the 45 fixtures, which is the
reminder that this client has callers whose own fixtures encode the same assumption.
