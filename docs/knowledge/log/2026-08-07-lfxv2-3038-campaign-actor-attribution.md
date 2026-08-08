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

**The claim INSERT is the row's only INSERT.** Every subsequent write for a (brief, platform)
pair takes the upsert's conflict arm, so `created_by` is stamped on the claim and is deliberately
absent from that arm's SET list — otherwise a retry would rewrite the original author with
whoever triggered the latest run. `updated_by` moves there via `COALESCE(EXCLUDED.updated_by,
campaigns.updated_by)`: the recovery sweeper re-persists with no originating request, and letting
its NULL land would turn "we know who" into "we do not". Neither property is reachable from a
service-level test, so both are asserted against the SQL text.

**Ordering constraint.** golang-migrate tracks a single version integer, so `000016` must reach
`main` after `000015`; merged first, `000015` would never be applied at all.

Still open: `campaign_audiences` carries `created_by` only, so an audience edit records no actor.
