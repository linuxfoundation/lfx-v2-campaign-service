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
