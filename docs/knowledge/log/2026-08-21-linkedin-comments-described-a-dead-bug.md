# 2026-08-21 — LinkedIn's dedup comments described a dead bug

**Docs** — four comments in `internal/platform/linkedin/client.go` and two in
`internal/dispatch/linkedin.go` still described the retry-duplicates-creatives risk as
"PLANNED but NOT provided" and pointed at LFXV2-2665 as the fix. The orchestrator's
per-`(brief, platform, variant)` single-flight claim (`internal/service/orchestrator.go`,
commit 55276007, 2026-07-10) already closes that window in the real dispatch path: a retry
reuses a `created_degraded` campaign or returns "reconciliation required" for a true partial
orphan, and never re-invokes `CreateCampaign`/`Dispatch` for the same tuple. The comments
predated that guard's wording settling and were never updated afterward.

LFXV2-2665 is an umbrella ticket bundling two distinct pieces: the single-flight dedup guard
(done, 2026-07-10) and automatic reconciliation of a true partial orphan — auto-discovering
and completing a stranded `group_created`/`unconfirmed` row without a human (still open). The
comments cited the ticket number for both, which reads as "still open" for a piece that has
been done for weeks.

Corrected each comment to state plainly that the single-flight guard is live and to scope the
remaining LFXV2-2665 work to reconciliation only. No behavior changed — `internal-dispatch.md`
and `internal-platform-linkedin.md` already described the live guard correctly and needed no
edit; only the LinkedIn-specific comments had drifted.

**The generalisation.** A comment that cites a ticket number as "still open" is a claim about
the present, not a snapshot of when it was written. Verify it against the code and commit
history it's near before trusting it — an umbrella ticket spanning multiple sub-scopes is
exactly the case where "done" and "open" get silently conflated under one number.
