# 2026-08-07 — LFXV2-3038: audiences DO have an update path, so their missing `updated_by` is a gap

**Update** — Four documents justified `campaign_audiences` carrying `created_by` only by asserting
that an audience is built once and never edited. That assertion is false, and stating it as a design
invariant hid a real attribution gap behind a reason that does not exist.

**Fix** — `update-audience` is a published `PATCH /projects/{project_id}/briefs/{brief_id}/audiences/{audience_id}`
(`design/audience.go`), handled by `AudienceService.UpdateAudience` and backed by
`AudienceRepo.UpdateAudience`, which performs an in-place `UPDATE ... RETURNING` under an
optimistic-concurrency guard. An audience is edited in place, and today that edit records *when* but
not *who*. Architecture D5, `docs/channel-connections-schema.md`, the `000015` migration header, and
the prior log fragment now all describe the missing `updated_by` as an outstanding gap
(LFXV2-3038 follow-up) instead of explaining it away.

**Fix** — The lesson is the reason this is worth a fragment of its own rather than a quiet edit. The
false claim entered one document and was then copied into three more as supporting context, so a
single unverified sentence became four mutually-corroborating ones. A rationale that says a code path
does not exist is checkable in one grep, and it is exactly the kind of claim that should be checked
before it is repeated: the cost of getting it wrong is that the gap it conceals stops being tracked.
