# 2026-08-07 — Actor attribution on campaign briefs

**Update** — `campaign_briefs` now records WHO made each write, via migration `000015`
adding `created_by` / `updated_by` JSONB columns and all four brief write statements
stamping an actor from the request's bearer token.

This closes the biggest gap between the service and its stated end state. Campaigns
execute under **system accounts** — shared, LF-owned platform credentials — so Google
Ads, Meta, LinkedIn, Reddit and X all report the SAME identity regardless of which
person acted. There is no upstream to ask later. Either this service captures the actor
at write time or the information does not exist anywhere.

**What was built.** `model.CampaignBrief` gained `CreatedBy` / `UpdatedBy *Actor`,
reusing the `model.Actor` + `marshalActor`/`unmarshalActor` JSONB pair already carrying
connection actors. The handlers pass `actorFromCtx(ctx)` on create, update and delete;
`Approve` stamps `updated_by` alongside the `approved_by` it already wrote.

**Three decisions worth keeping.**

1. *The stamp goes in the same statement as the write.* The four SQL strings were
   promoted to package constants (`createBriefQuery`, `replaceBriefQuery`,
   `approveBriefQuery`, `archiveBriefQuery`) specifically so a test can assert this —
   a follow-up `UPDATE` would compile, pass everything else, and leave a committed
   window in which the content had changed and the attribution had not.
2. *A missing actor does not fail the write.* NULL means "not recorded", never
   "nobody". Rejecting the write would escalate a token-decoding regression into a
   total outage of brief creation.
3. *Nothing is exposed on the API surface.* `approved_by` set that precedent — it is
   persisted but absent from both the Goa result type and the index payload — so
   following it needed no Goa regeneration and kept the change small.

**A test that looked binding and was not.** The first version of
`TestBriefWrites_StampTheActorInTheSameStatement` matched the actor column name against
the whole SQL string. Deleting `created_by, updated_by` from the INSERT left it GREEN:
every brief statement ends in `RETURNING ` + `briefCols`, and `briefCols` names both
columns for the read-back. The assertion was satisfied by what the statement READS.
Stripping the `RETURNING` clause first made the revert fail as it should.

The generalisable form: when asserting what a statement WRITES, first remove the part
that describes what it READS — otherwise the select list quietly satisfies the
assertion. Every guard here was checked by disabling it one at a time and confirming
the right subtests failed while the rest stayed green.

**Not in this change.** `campaigns` gets the same columns in a follow-up migration. Its
write paths (dispatch claim, upsert, status toggle) are a distinct change with distinct
failure modes — the claim in particular is a contended `ON CONFLICT` statement, not a
plain update — and splitting keeps both changes reviewable.
