# 2026-09-28 — LFXV2-2665: two more prose sites outlived the change under them

**Docs** — round 21 of review, raised by the repo-code reviewer. No behaviour change.

Two passages round 19's sweep missed, each the second copy of a sentence that round already
corrected elsewhere.

`design/connection.go`'s discovery-capability comment still said of an `active` connection that
"nothing verifies them". Round 19 fixed that exact claim in `internal-dispatch.md` and left this
one standing. The conclusion the passage is making is unchanged and still right — `active` says
the connection is ENABLED for credential-based operations, not that anything checked the
credential — but the `test-<platform>` endpoints this branch adds do verify, on demand. It now
says nothing on the WRITE path verifies them, names the test endpoints as the thing that does,
and keeps the point: nothing runs those endpoints for you.

`docs/knowledge/code/internal-dispatch.md` still described flat mode's second leg as a
`customer_client` read "scoped to the configured customer and narrowed to its own row by id".
That is the query round 19 REMOVED, and removed because it was wrong — `customer_client` is
documented as a link resource belonging to manager customers, so asking for it in flat mode,
where there is no manager, rested on behaviour the contract does not promise and made a working
direct connection test amber. The passage now describes the `FROM customer` self-record read that
replaced it, with no `WHERE` and no hierarchy walk, and points at
`internal-platform-googleads.md` for the full reasoning rather than restating it.

Both are the same failure mode and worth naming: a sentence fixed in one file is not fixed in the
repo. The corrected wording existed in the bundle for a full round while these two copies went on
telling the opposite story, and a reader who met one of them first had no way to know it was the
stale one.
