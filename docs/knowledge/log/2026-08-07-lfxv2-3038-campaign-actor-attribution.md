# 2026-08-07 — LFXV2-3038: campaigns record who authorized the spend

**Update** — Migration `000016` adds `created_by` / `updated_by` JSONB to `campaigns`, the
follow-up `000015` forecast. These rows are the ones that SPEND MONEY, and campaigns execute
under shared LF-owned system accounts, so no ad platform can report which person launched one.

**Why it needed its own change rather than a second ALTER in 000015.** A brief write happens on
the request goroutine, so the actor is read from the request context at the point of the write.
Dispatch does not: `Orchestrator.Start` returns once the job row exists and the work continues on
`o.rootCtx` in a goroutine that outlives the request. An `actorFromCtx` at the INSERT would have
returned nil for every campaign ever created — no error, no log line, just a permanently empty
column. `Start` now captures the DECODED actor (never the bearer token, which may be expired by
the time async work runs, with no retry) and threads it through `run` → `dispatchPlatform` →
`ClaimCampaignDispatch`. That is a signature change across the dispatch path, which is what
earned it a separate review.

**The claim INSERT is the row's first INSERT.** Every subsequent write for a (brief, platform)
pair normally takes the upsert's conflict arm, so `created_by` is stamped on the claim and is
deliberately absent from that arm's SET list — otherwise a retry would rewrite the original
author with whoever triggered the latest run. It stamps `updated_by` from the same placeholder,
as `createBriefQuery` does: at creation the author IS the last mover, and a NULL there would be
indistinguishable from a lost attribution. `updated_by` then moves on the conflict arm via
`COALESCE(EXCLUDED.updated_by, campaigns.updated_by)`, because an unattributed re-dispatch must
not erase the actor an attributed one recorded. None of this is reachable from a service-level
test, so it is asserted against the SQL text.

**The nil actor has one source, and it is not a sweeper.** An earlier draft of these comments
justified the NULL path with "the recovery sweeper re-dispatches with no originating request".
No such component exists: `StartRecoverySweeper` calls `jobs.FailStuckJobs` and nothing else —
it fails stale JOBS and never writes a campaign row, and a dispatch claim is explicitly not
reclaimed on a timer. The real and entirely ordinary source is `attributedActor`, which returns
nil (after logging a warning) when the request carries no authenticated principal;
`Orchestrator.Start` captures that value and threads it down. The distinction matters because a
comment naming a component that does not exist invites the next reader to preserve a behaviour
for a caller that will never arrive.

**The soft delete had no actor at all.** It is the one mutation where "who did this" is asked
after the fact — the row survives a delete precisely because it may still point at a campaign
spending real money upstream — and it was leaving `updated_by` naming whoever last edited the
campaign. That is worse than NULL: it reads as knowledge and it is wrong. `DeleteCampaign` now
takes the actor as a parameter (the same reason `ClaimCampaignDispatch` does not read it from
ctx does not apply here — the delete IS on the request goroutine — but the parameter keeps the
whole port consistent about where attribution enters).

**`scanCampaign` is now driven directly.** Both actor columns are JSONB and both timestamps are
`time.Time`, so a swapped destination list cannot fail at the type level; `created_by` and
`updated_by` would trade places and every existing test would stay green while every campaign in
the database attributed to the wrong person. Distinct per-column values are what make it visible.
NULL actors decode to nil; wrong-shaped actor JSON fails the scan with the column named.

**Ordering constraint.** golang-migrate tracks a single version integer, so `000016` must reach
`main` after `000015`; merged first, `000015` would never be applied at all.

Still open: `campaign_audiences` carries `created_by` only, so an audience edit records no actor.

## Follow-on (review round 3)

**The migration's own comment still named a recovery sweeper.** The prose correction above
landed in the knowledge bundle; the SQL file it was describing did not get the same edit, so
`000016_campaign_actor_columns.up.sql` was still telling the next reader that a sweeper
re-persists campaign rows with a nil actor. A comment inside the migration is the version
someone reads when they are deciding what the columns mean, so it is the version that had to
be right. It now names the single live nil source — `attributedActor` on a request with no
decodable principal — and says explicitly that `StartRecoverySweeper` writes no campaign row,
so the `COALESCE` on `updated_by` is not there to accommodate one.

**000016 had no migration test.** `TestMigration000016_AddsCampaignActorColumns` mirrors
000015's, plus two assertions the sibling did not need: that the file does NOT say
`ALTER TABLE campaign_briefs` (000015 and 000016 are near-identical files against different
tables — a copy-paste that keeps the wrong target applies cleanly as a no-op and leaves
campaigns without the columns the repository writes to), and that neither column is declared
`NOT NULL` (which is unrunnable, not merely strict: existing rows have no actor to backfill).

`TestLiveCampaignActorColumnsExistAndAreNullable` covers what source text cannot: it asks
`information_schema` whether the columns are actually present as `jsonb` and nullable in the
MIGRATED schema, and then inserts a campaign with both actors NULL to show the table as a
whole — triggers and constraints included — still accepts an unattributed dispatch.

**The toggle stamp's coverage arrived from #96.** Round 3 flagged that every test in the
toggle suite passed `context.Background()`, so deleting
`existing.UpdatedBy = attributedActor(ctx, "toggle campaign status")` left them all green
while the only record of who paused or resumed a spending campaign disappeared. That is
accurate against the commit the review read; LFXV2-3044 (#96) landed
`TestCampaignActor_ToggleStampsUpdatedByOnly` and `TestCampaignActor_SystemToggleRecordsNoActor`
on `main` shortly after, and merging `main` here brings them in. They seed the row with a
prior mover so a toggle that merely carries the previous actor forward is distinguishable
from one that reads the request actor, and they pin that an unauthenticated toggle stamps
nil rather than inventing a principal. No further test was added here — a second pair
asserting the same property would only be one more thing to keep in step.

## Round N: the delete case named in three places was the wrong case

Three comments — `orchestrator.dispatchPlatform`, `ClaimCampaignDispatch`'s godoc, and
`docs/channel-connections-schema.md` — justified passing `CreatedBy` into the upsert by
naming a soft-deleted row as the case that reaches its INSERT arm. The code is right and
stays; the justification is not.

Trace it. `dispatchPlatform` reaches the upsert only after it OWNS a claim, and
`claimCampaignDispatchQuery` and `upsertCampaignQuery` share one conflict target
(`ON CONFLICT (brief_id, platform) WHERE status <> 'deleted'`). So after a delete the
deleted row is outside the index, the CLAIM's INSERT wins and stamps `created_by` on the
fresh campaign, and the upsert conflicts with the row the claim just wrote. The delete case
is an ordinary conflict-arm case. It is also doubly unreachable as stated: a `pending` row
cannot be soft-deleted at all, because `CampaignStatusDeletable` is a whitelist of settled
statuses (`campaign_test.go` pins `CampaignStatusPending: false`), so a claim never enters
the deleted state.

What DOES reach the INSERT arm is the claim row disappearing between the two statements: an
operator clearing what looked like a stuck claim (`StuckDispatchClaims` exists to surface
them), or a concurrent `DeleteDispatchClaim`. Narrower, real, and the only path that creates
a campaign row with no live claim in front of it.

Doc-only, but not cosmetic. **A comment that names the wrong case invites the reader to
delete the code.** The next person to notice that the soft-delete case takes the conflict arm
has been handed a proof that `campaign.CreatedBy = by` is dead — and it is not. The general
rule: when a defensive assignment is justified by a scenario, the scenario has to be one that
actually reaches it, or the justification argues for removal.

Pinned by extending the comment on `CampaignStatusPending: false` in
`TestCampaignStatusDeletable` to record what else depends on that row: it is the fact that
makes the claim, not the upsert, the statement that preserves attribution after a delete.

## Round N+1: the arm's justification described a caller that cannot reach it

Copilot, in a suppressed comment. Real, and it invalidates the reasoning recorded near the top
of this log ("otherwise a retry would rewrite the original author with whoever triggered the
latest run") — which stays where it is, because this file is history; the correction lives here.

The claim was that `upsertCampaignQuery`'s conflict arm is taken by "every later dispatch of
the same (brief, platform) — a retry, a re-dispatch after a brief edit". It is not. Tracing
`dispatchPlatform`: the upsert is reached only when `ClaimCampaignDispatch` returned
`claimed=true`, and every `!claimed` branch returns before it — a reusable campaign is reported
as a reuse, a retained partial as a reconcile-required failure, a bare pending claim as a skip.
So the conflict arm always finalizes the claim THIS invocation just inserted. The two cases
that look like exceptions are not: a retry re-claims first (the released row is gone, the
INSERT wins, and `created_by` is re-stamped with the retrying actor anyway), and a re-dispatch
after a soft delete inserts a fresh row outside the partial unique index.

What survives is the omission itself, on a different and better footing. `UpsertCampaign` is a
general-purpose repository method, not a private half of `dispatchPlatform`, and its conflict
arm has to be safe for a caller that reaches it WITHOUT a claim in front of it. For that caller
`created_by` in the SET list would rewrite the original author, and under shared system accounts
no ad platform can supply it again. So the SQL is unchanged and
`TestUpsertCampaignDoesNotRewriteCreatedBy` keeps its assertion — only the stated reason moves
from "prevents a thing that happens" to "keeps a contract the caller could otherwise break".

The `updated_by` COALESCE is the interesting contrast, and its old justification was closer to
right than `created_by`'s: it IS reachable today, because the claim and the upsert are separate
statements and only the second can run against a row the first already stamped.

Two things worth keeping from this. First, a defensive guard justified by a scenario that
cannot occur is *more* fragile than one justified as a contract, not less: the next reader who
traces the reachability finds the justification false, and the natural conclusion is that the
guard is unnecessary. Second, the correct account was already sitting in the neighbouring
`ClaimCampaignDispatch` godoc ("A retry claims again first, so the row is back and the conflict
arm takes it") — two comments about one mechanism, written at different times, and only one of
them re-derived. Same shape as this round's `pool.go` finding on #106.
