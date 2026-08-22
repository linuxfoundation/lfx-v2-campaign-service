# 2026-08-22 — the consequence, not the insert, is what carries the decision

**Note** — `createCreativeAssetQuery`'s unlocked parent-brief gate has now been raised six
times in review. It is not an oversight, the decision to leave it unlocked stands, and this
entry records why re-arguing it kept happening rather than re-arguing it again.

## The position

The insert gates its parent with `INSERT ... SELECT ... WHERE EXISTS (... status <> 'archived')`
and takes no lock, so under READ COMMITTED an `ArchiveBrief` can commit between the statement's
snapshot and its insert, leaving an asset stored under a brief that is archived by the time the
row lands. That is real, and it is the SAME position the plain `createAudienceQuery` holds with
a byte-identical predicate. `SELECT ... FOR UPDATE` is reserved for paths where losing the race
produces an EXTERNAL side effect — `CreateAudienceForApprovedBrief` builds real HubSpot lists —
and no such effect exists here. Locking this insert would make it stricter than its sibling for
a strictly smaller consequence, so it stays as it is.

## Why it kept being re-litigated

Because the justification is not a property of the insert. The insert's comment says the
consequence "is only a stored blob nothing can reach" — and whether anything can reach it is
decided entirely by the READ path. A reviewer reading `createCreativeAssetQuery` sees an
unlocked gate and a claim about the rest of the system, with nothing in view that would fail if
the claim stopped being true. The argument was re-derivable but never checked, so it was
re-derived, six times.

## What changed

Nothing about the SQL. The claim is now anchored to the thing that enforces it.

`getCreativeAssetQuery`'s `EXISTS` on a non-archived parent is what makes "nothing can reach it"
true, and `TestCreativeAssetRepo_GetAsset_ReturnsBytesScopedToTenant`'s "an archived parent
brief makes the asset unreadable" subtest already failed when that `EXISTS` was removed — this
was verified by deleting it and watching the subtest fail. So the property was ALREADY guarded;
what was missing was that the guard did not know what it was guarding. That subtest justified
itself purely on lifecycle consistency, so weakening it would have silently removed the unlocked
insert's entire justification, and the insert's comment pointed at no test at all.

Both now name each other. Adding a second, concurrent test was considered and REJECTED: it
would have driven the interleaving with two transactions and asserted the same GetAsset
outcome the sequential subtest already asserts, killing no mutation the existing subtest does
not kill, while carrying a `t.Skipf` arm that can pass vacuously when the race does not
reproduce. A redundant test with a silent-pass path is a liability, not coverage.

## The rule

When a trade-off's justification lives in ANOTHER function, the test that enforces it there
must say so. Otherwise the decision is only as durable as the next reader's willingness to
re-derive it — and the author's own escape clause ("a path that reads assets without
re-checking the parent") names a trigger that a reviewer of the insert cannot see and a
reviewer of the read has no reason to connect.
