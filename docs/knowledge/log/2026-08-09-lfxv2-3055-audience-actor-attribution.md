# 2026-08-09 — An audience edit records who made it (LFXV2-3055)

**Update** — `campaign_audiences` gained `updated_by` (migration `000017`), both inserts
now stamp it alongside `created_by`, and `UpdateAudience` stamps the editor.

**The gap.** `campaign_audiences` has carried `created_by` since `000005` and nothing else.
`000015` recorded why that was a gap rather than a design choice: `update-audience` is a
published PATCH backed by an in-place `UPDATE`, so an audience edit recorded no actor at
all — the row kept naming whoever created it however many times somebody else rewrote its
suppression lists or flipped its status. Audiences are built through **system accounts**;
HubSpot reports one shared LF-owned identity for everyone, so if this service does not
record who narrowed a list, the information exists nowhere.

**Where the stamp goes.** On the row the handler **loaded**, not on the incoming patch.
`UpdateAudience` is read-modify-write: the loaded row already carries the *previous*
editor, so writing it back unchanged would silently re-assert them as the author of
somebody else's edit. An audit trail that names the wrong person is worse than one that
names nobody, because it reads as evidence. A single-edit test cannot see this — a
fill-only-if-empty stamp passes it and is wrong from the second edit onwards, which is why
`TestAudienceActor_UpdateStampsTheEditorNotTheCreator` runs three actors through the row.

**Both inserts stamp, including the build path.** This follows the brief precedent
(`000015`): leaving `updated_by` NULL until the first edit makes "who touched this last"
unanswerable without also reading `created_by`. `CreateAudienceForApprovedBrief` is the
`BuildAudience` insert, and it is stamped too — that request creates real HubSpot lists and
spends money, and it runs under a human's HTTP request, so the initiator is a fact the
statement has. The build's later progress writes carry that actor forward rather than
restamping; if the build ever moves off the request goroutine, they must go NULL instead,
because a scheduled retry has no principal.

**What is deliberately NOT done.** Existing rows are not backfilled from `created_by`. The
migration cannot know whether anyone edited them, so a backfill would manufacture
attribution for edits it has no evidence of. NULL means *not recorded*, never *nobody*.

**Migration numbering is a merge-order constraint, not a formality.** This is `000017`
while `000016` is claimed by the campaigns actor columns on an unmerged branch.
golang-migrate records only the HIGHEST applied version and never applies a lower one, so
if this merges first, `000016` becomes unapplicable **forever** — and silently, since the
tool reports success. `TestMigrations_NoVersionGaps` caught the gap; `allowedVersionGaps`
records it as transitional, and `TestMigrations_AllowedVersionGapsAreStillOpen` stops that
entry outliving the branch that justified it.

**Three query constants.** `createAudienceQuery`, `createAudienceForApprovedBriefQuery` and
`updateAudienceQuery` moved to package level for the same reason the brief statements did:
the invariant is that the stamp happens in the *same statement* as the write, and that can
only be asserted without a live database if the SQL is reachable from a test. A follow-up
`UPDATE` compiles and passes everything else while leaving a committed window in which the
row had changed and the attribution had not.

## Round N+1: the merge-order obligation came due, and the number had to move

`#93` merged first, carrying `000017_index_disconnected_probe`. This branch's
`000017_audience_actor_columns` then became a duplicate version the moment `origin/main`
was merged in, and `TestMigrations_UniqueNumbering` caught it — which is the whole reason
that test exists. Renumbered to `000018`.

Worth stating plainly, because the earlier round in this file predicted the wrong failure:
the obligation recorded here was "do not merge before the PR that fills the gap." That is
the obligation for a GAP. A DUPLICATE is a different failure with a different remedy, and
which of the two you get is decided by the merge order you do not control. Both branches
picked the next free number against the `main` they could see; whichever merged second was
always going to collide. Choosing a migration version by reading `main` is choosing against
a snapshot that a sibling PR can invalidate between the choice and the merge — the number is
only settled at merge time, so the test, not the reasoning, is what holds.

The gap at `000016` (PR #95's campaigns actor columns) is now a live risk rather than a
scheduled one: `main` already carries `000017`, so the first environment to apply it records
`000017` as the highest applied version and `000016` is thereafter skipped silently and
forever. `#95` must merge before the next deploy, or be renumbered above `000017`. That is
`#95`'s obligation; nothing this branch does can discharge it, and the
`allowedVersionGaps[16]` entry now says so in those terms.
