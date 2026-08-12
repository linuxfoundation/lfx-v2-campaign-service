# 2026-08-12 — the two older LinkedIn cursor walks, and what a missing envelope costs

**Update** — `internal/platform/linkedin/client.go` (LFXV2-3066). `findMatch` and
`listCreativeURNs` now reject a response whose `metadata` block is absent, instead of reading it
as an exhausted cursor. 44 single-page fixtures across five test files gained `"metadata":{}`.

`findMatch`, not `findByName`: the walk is shared, and naming the wrapper undersells the fix.
`findByName` and `findCampaignByNameInGroup` both delegate to it, so ONE guard closes TWO
find-or-create entry points — and `findCampaignByNameInGroup`'s own comment already says an
absence there "would allow a duplicate create".

## This was a known hole, left open on purpose

LFXV2-3063 made `linkedInMetadata` a pointer and made `ListAdAccounts` reject nil. The same
exposure lived in two older walks, and the log for that round said so plainly: closing them meant
touching roughly fifty fixtures, which did not belong in a review round on ad-account discovery.

So this is not a newly discovered defect. It is the deferred half, and the estimate held up: 45
against "roughly fifty". Whether that was a counted estimate or a good guess is not recorded, so
no credit is claimed for it here — what matters is that the deferral named its own cost and the
cost turned out to be the one named.

## The two walks do not cost the same thing

Worth separating, because the shared mechanism hides a large difference in blast radius.

`listCreativeURNs` truncating means a partial creative list reported as complete. The status
cascade then reports success and the service persists ACTIVE while the creatives it never
discovered stay DRAFT and never serve. Bad, recoverable, and visible to anyone who looks at the
campaign.

`findMatch` truncating is worse in kind, not just degree. It is the find half of find-or-create,
so its absence value is the LICENCE TO CREATE. A dropped cursor envelope on an intermediate page
means "searched everything, found nothing" about a name that may sit on a page never fetched —
and the caller answers by creating a **duplicate paid campaign**. Real money, and nothing about
the outcome looks wrong at the time. Both of its callers are such a path, which is why the guard
belongs in the shared walk rather than in either wrapper.

That asymmetry is why the `findMatch` guard's message borrows the wording the repeated-token arm
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

## The sibling clients were checked, and LinkedIn was the outlier

The obvious follow-up question is whether the other platform clients share the shape. They do
not, and the reasons differ enough to be worth recording rather than re-derived next time:

- **Meta** decodes `Paging` as a VALUE, which would normally be exactly this bug — but its
  exhaustion test is `resp.Paging.Next == ""`, and `paging.next` is a URL the Graph API omits
  only on the genuine last page. It then errors when `next` is present but the cursor is empty
  (`accounts.go`), so "more pages, no way to ask for them" is already refused.
- **HubSpot** nil-checks explicitly: `resp.Paging == nil || resp.Paging.Next == nil ||
  resp.Paging.Next.After == ""` (`email.go`).

So there is no fourth site to fix. LinkedIn was the only client where an absent envelope and an
exhausted cursor decoded to the same value, and all three of its walks now reject it.

## Two things checked and deliberately left alone

**A whitespace-only `nextPageToken` is still treated as a live cursor.** `{"nextPageToken":"   "}`
decodes to a non-empty string, so the walk echoes it back rather than reading it as exhaustion.
That fails CLOSED — the `seenTokens` guard catches the repeat on the next page and the page cap
backstops it, so the outcome is an abort, never a false absence. It is also the choice
`accounts.go` already made explicitly: its comment says a cursor is "an opaque server token
echoed back verbatim", and trimming one could request a DIFFERENT page than the one offered.
Unchanged here for the same reason.

**The three guards word their errors differently, and that is the point.** `accounts.go`,
`listCreativeURNs` and `findMatch` encode one rule but name three different caller-facing
consequences — an unenumerated account list, creatives that never serve, a duplicate create. A
shared helper would flatten exactly the distinction that makes each message useful at its call
site. Noted in a comment on `findMatch` so the next reader does not "fix" it into one.

## Verification

Both guards are revert-verified: removing the nil check makes
`TestFindByName_AbsentMetadataOnLaterPageIsNotAbsence` and
`TestListCreativeURNs_AbsentMetadataIsNotExhaustion` fail, and the first of those asserts on the
INTERMEDIATE page — page one advertises a cursor, page two drops its envelope — because a
first-page-only test would not distinguish this bug from an empty result set.

The full service suite passes; `internal/dispatch` needed 7 of the 44 fixtures, which is the
reminder that this client has callers whose own fixtures encode the same assumption.

The count is 44 rather than 45 because the first sweep also patched an `accounts_test.go`
fixture whose ENVELOPE already carried `"metadata":{}` — the regex matched an element object and
injected the key one nesting level too deep, where `responseElement` has no such field and it
decoded to nothing. A silent no-op that misrepresented the LinkedIn contract to the next reader.
Reverted. Second lesson from the same sweep, and the more general one: a regex over JSON string
literals does not know the difference between an envelope and an element.
