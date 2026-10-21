# 2026-10-21 — A staged series becomes one HubSpot workflow

**Update** — a survey promoted by an opening email, a reminder and a closing push produced three
independent drafts, each needing to be scheduled by hand, in order, on the right dates. The
sequence existed in the briefs and nowhere in HubSpot.

`POST /projects/{projectId}/send-workflows` now creates ONE workflow that owns the schedule and
sends each staged draft itself.

- It is a READ-THEN-RECREATE clone of a workflow the OPERATOR pastes. The v4 Flows API has no
  clone endpoint, so `internal/platform/hubspot/flows.go` fetches the definition, rewrites the
  parts that differ in memory, and POSTs the result as a new flow. The source is only ever read.
- Cloning rather than building a flow from scratch is the point: enrollment criteria, suppression,
  sending windows and blocked dates are marketing-operations policy that already exists in the
  portal. Rebuilding it in code would mean re-deciding all of it here.
- Steps are found by `actionTypeId` — `0-4` for a send, `0-35` for a wait-until-date — and ordered
  by NUMERIC action id. Not by position and not by hard-coded ids: a hand-built workflow numbers
  its actions however the editor happened to. Numeric ordering matters at ten steps, where a
  string sort puts action 10 between 1 and 2 and maps the emails to the wrong sends while still
  producing a complete-looking workflow.
- Only the DATE of a dated step is replaced; its `time_of_day` stays the source's, because that is
  a sending-hours decision its author made and a calendar date carries no time.
- Counts must match exactly and are checked BEFORE anything is created. A source with three send
  steps and two staged drafts would otherwise leave the third send pointing at the SOURCE
  campaign's email — which reads as a filled-in step rather than a missing one.
- The caller names BRIEFS, not HubSpot email ids: which draft a brief produced is this service's
  own record (the campaign row's platform id), and a client that had to look that up could pair a
  brief with another campaign's email.
- Two briefs is the floor. A single email is staged, scheduled and sent on its own, so a workflow
  around it only adds an enrollment step between the draft and its send. The prototype this
  replaces had the same rule.
- The new workflow is ALWAYS created disabled. Enabling it is the one irreversible step on this
  path and is left to the human who reviews the drafts.

Ported from `LF-Marketing-Ops/marketing/survey_workflow/hubspot_workflow_clone.py`, whose
generic `actionTypeId` approach replaced an earlier prototype that hard-coded one portal's
workflow id, three template email ids and six action ids.

Still open: splitting one pasted source document into N sends automatically, which that prototype
did with `ai_split_drafts`. Today the operator plans each send as its own brief.

See [internal/service](../code/internal-service.md).
