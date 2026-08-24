# 2026-08-24 — three docs that outlived the fix made on this branch

**Fix** — X Ads pagination prose and rosters: tonight's `findByName` fix added a full-page
cursor-presence guard, and three pieces of documentation still described the world before it.

`NextCursorPresent`'s comment said `findByName` "deliberately does NOT consult it", and
`NextCursorNull`'s said the cap "already answers" an absent cursor. Both are now false at the
same file: `findByName` consults `NextCursorPresent` precisely because an absent cursor
returns "not found" before the cap is ever reachable, and the caller acts on not-found with a
create POST. The rewritten prose names the real discriminator — PAGE FULLNESS — and states why
the cap cannot stand in for it: the cap is only reachable while a non-empty cursor keeps
arriving, which is exactly the case an absent cursor is not.

A test name carried the same falsified claim: `TestNextCursorPresentDoesNotChangeFindByName`.
Its body only ever asserted decode parity, so the name — not the coverage — was wrong; it is
now `TestCursorBitsDecodeWithoutChangingExistingValues`. The guard itself had no behavioural
test at all, which is why the prose could drift unchallenged, so one now pins both arms: a full
page with no cursor key aborts on the FIRST page rather than deferring to the cap, and a short
page with no cursor key stays a clean not-found. Disabling the guard fails the first arm.

Third, `connection.go` cited the `var _ service.AccountLister` block in
`account_discovery_test.go` as the "authoritative, self-updating list". It is not: it pins the
three providers that test exercises and omits the Google Ads and Meta implementations — five
dispatchers declare `ListAccounts`. The comment now points at `accountListerProviders`, which
derives membership by type-asserting every candidate, and says plainly what the assertion block
is not. An enumerating comment that nothing fails on is the recurring shape here.

Also noted, and worth acting on separately: `okfvalidate` does NOT validate kind markers.
`validateLogFragment` checks the filename, the absence of frontmatter, and the H1 date, then
returns as soon as it sees the heading — it never reads the marker line. The allowed set lives
only in CLAUDE.md prose, enforced by nothing, which is why four bad markers reached review
across tonight's branches.
