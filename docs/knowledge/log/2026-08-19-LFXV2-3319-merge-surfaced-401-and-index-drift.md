# 2026-08-19 — LFXV2-3319 the merge surfaced a 500-instead-of-401 and index drift in both directions

**Fix** — two defects that neither `feat/LFXV2-3319-x-ads-discovery` nor `main` had on its
own. The branch was cut 14 commits behind; both failures exist only in the *combination* of
this branch's new code with rules `main` adopted while the branch sat behind, so neither
side's CI could have reported them. That is the part worth recording: a green branch merged
into a green main is not evidence of a green merge.

## `list-twitter-ads-accounts` encoded a refused token as a 500

The branch added `list-twitter-ads-accounts` with its errors hand-listed —
`Error("BadRequest", ...)` written out inline — and no `Error("Unauthorized", ...)`. On the
branch alone that was merely inconsistent with its siblings. Meanwhile `main` merged PR #149,
which added `TestEverySecuredMethodEncodesAuthErrors`: every method carrying `bearerToken()`
must declare `Unauthorized`, because Goa builds a method's error encoder from the errors that
method DECLARES. A method omitting the declaration has no `case "Unauthorized"` in its
generated encoder, so `JWTAuth`'s typed rejection falls through to the generic encoder and the
caller is told **500**.

Nothing about that is visible from the Go types: it compiles, the handler returns the correct
error, and only the wire status is wrong. The new method predated the rule, so the rule and
its violation arrived in the same merge and not before.

The fix is the shape every sibling already uses — `authErrors()` in the method block and
`connectionAuthErrorResponses()` in the `HTTP` block. Both halves are required and only
correct together: a declared error with no `Response` mapping encodes exactly like an
undeclared one. Two traps come with that shape, and both were hit on PR #153 fixing the same
defect on `get-google-ads-keywords` / `get-google-ads-audience`: the hand-listed
`Error("BadRequest", ...)` must be REMOVED rather than kept beside `authErrors()`, or Goa
sees a duplicate case and the build breaks; and the matching `Response("BadRequest",
StatusBadRequest)` line becomes redundant once `connectionAuthErrorResponses()` maps it.

A design-wide sweep of every `bearerToken()` method confirmed `list-twitter-ads-accounts` was
the last one missing the declaration — the other X/Twitter methods (`get-`, `create-`,
`update-`, `delete-`, `test-`, `set-credential-twitter-ads`) all reach it through the shared
`connectionMethods` helper, which already calls `authErrors()`.

No new test was added. `TestEverySecuredMethodEncodesAuthErrors` IS the regression guard: it
parses the generated encoders rather than a hand-maintained list of method names, precisely so
that a NEW provider method added without the declaration fails it. A test asserting the same
property for this one method would restate what that guard already enforces for all of them.

## Four index bullets drifted against their concept frontmatter

`TestValidateRealBundle` failed with four errors: the `docs/knowledge/code/index.md` bullets
for `internal-platform-reddit.md`, `internal-platform-meta.md`, `internal-platform-googleads.md`
and `internal-platform-microsoft.md` no longer matched their concept files' frontmatter
`description` verbatim.

Two causes, and they are worth separating. The meta and googleads bullets went stale through
the merge's own conflict resolution, which took the index as a three-way union (meta and
googleads from `main`, twitter from this branch) — the right call for the *conflict*, wrong for
the *rule*, since a union of bullets is not the same as agreement with the concept files.
Reddit and microsoft were never in the conflict at all: `main` advanced those concept
descriptions (keyword targeting, geo targeting, Reporting v13 metrics) while this branch sat
behind, so the bullets this branch carried were simply older.

The correct repair is mechanical and one-directional: the concept file's frontmatter is the
source of truth and the index mirrors it, so each description was copied verbatim into its
bullet. Editing the concept frontmatter to match the index would have rewritten other
tickets' shipped descriptions. The comparison is exact and untrimmed on both sides, so the copy
must be byte-for-byte rather than reflowed.

The sweep ran over every bullet in the file, not only the four the validator named — a stale
bullet is only reported once the file it points at is read, and the four were what the
validator reached first. No fifth bullet was stale. The twitter bullet retains this branch's
own "ad-account discovery" addition, which is 3319's contribution and had to survive the sync.

### That sweep had the wrong shape: it never asked which bullets were MISSING

"No fifth bullet was stale" is true and was the wrong question. A sweep over the bullets
*present* in the file can only ever find drift among them; it cannot report a bullet that is
not there to be swept. Two were not: the merge silently dropped the `internal/infrastructure/
metrics` and `internal/service/rules` bullets, whose concept files both exist. `main` added
them (`2ff11794`, `0e50a34a`) during the 14 commits this branch sat behind, the branch parent
`48f7bde8` never carried them, and resolving the index conflict as a three-way union of the
*conflicted lines* kept only what the two sides disagreed about. A line present on one side
and absent on the other produces no conflict marker, so nothing in the resolution surfaced it.
That is the general trap: **conflict markers show disagreement, not absence.**

Nor could CI report it. `internal/okfvalidate/validate.go`'s `validateIndex` walks the `* `
lines of `index.md` and checks each one — format, then `checkBulletDescription` against the
linked concept. `validateConcept` validates a concept file in isolation and never consults the
index. There is no pass in either direction that asks whether every concept HAS a bullet, so
`TestValidateRealBundle` passed clean with two concepts unindexed. The index is the surface an
agent reads first to decide which file to open, so an unindexed concept is effectively
invisible — a worse failure than the stale descriptions the validator does catch.

The repair restores both bullets in `main`'s ordering (metrics after `internal/infrastructure/
config`, rules after `internal/service/email_copy`), with each description read out of its
concept file's frontmatter by `okf.ParseFrontmatter` rather than retyped, since the comparison
is exact and untrimmed. The sweep was then re-run in BOTH directions over the whole file: 30
concept files against 30 bullets, zero unindexed concepts, zero bullets naming a missing
concept, zero description mismatches. No third concept was unindexed.

The durable lesson is the direction, not the two bullets: after resolving a conflict in an
index, a manifest, a registry or any file whose job is to enumerate something else, verify the
enumeration against what it enumerates in BOTH directions. Checking only the entries you can
see confirms the entries you kept and says nothing about the ones you lost.
