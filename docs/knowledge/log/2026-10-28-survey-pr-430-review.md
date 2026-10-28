# 2026-10-28 — Survey PR #430 review: shared-portal source and wizard survey stages

**Fix** — two gaps from the review of the survey PR.

- **The shared LF connection copies only the deployment's workflow.** `CreateSendWorkflow`
  discarded `fromSystem`, so on the shared portal any campaign manager could name any workflow
  id in it: its definition was read, its name echoed back, and its other steps copied into a
  workflow that would run against this series' audience. A project on the shared connection may
  now copy only `SURVEY_SEQUENCE_WORKFLOW_ID` (or the in-code default); anything else is a 409
  that says to connect the project's own portal.
- **The email wizard serves survey stages.** Plan, generate and clone resolved the stage through
  the event library, where a survey stage falls back to Registration Push: survey briefs got
  registration copy and a draft named after it. One helper, `resolveStageTemplate`, now picks the
  library by stage for every wizard turn and for `generate-email-copy`. The wizard also reads
  `event_details.sourceText` and renders it in its prompt facts as a fenced `Source document`.
- **Survey CTAs keep their link.** Every survey stage now declares `LinksToRegistration`: a survey
  brief's url IS the promoted survey or report, and each survey prompt asks for a button pointing
  at "the supplied URL", which was being withheld.
- **The facts block leaves out whole lines.** With the source document first, a later line that
  would pass `maxEventFactsBlockRunes` is omitted, not cut, so no URL reaches the prompt truncated.
- **Only drafts carrying their own copy go into a workflow.** HubSpot dispatch's content write is
  best-effort, so it now records `contentStaged` in the campaign result (true only when the
  generated subject AND body both landed), and `CreateSendWorkflow` refuses a draft without it.
- **The workflow edit link uses the verified portal**, not the client's configured one, which is
  empty on a connection that never recorded its portal id.
- The published examples for `is_enabled` (`false`) and a `send_dates` element (`2026-11-09`) now
  match their descriptions.

See [internal/service](../code/internal-service.md).
