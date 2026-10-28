# 2026-10-28 — A survey send workflow runs the send-time audience checks

**Fix** — `POST /projects/{project_id}/send-workflows` enrolled a list in the cloned workflow
without any of the checks every other send path runs before it applies recipients, and could
enrol the wrong people.

Enrolling a list in a workflow IS the send on this path: a workflow's send step does not use the
draft's own recipient or suppression settings, so the checks staging ran on the draft never reach
the people the workflow sends to.

- **Standing suppressions (#396) now apply.** `CreateSendWorkflow` runs
  `wizardStandingCoversSendList` on the series' send lists and suppressions before cloning, the
  same check `SetWizardSendList` and HubSpot dispatch run before `SetSendList`. A standing list
  that has grown to cover a send list is a 409 and nothing is created.
- **The portal is proven.** `assertWizardPortal` refuses an audience built in a different portal
  than the credential reaches, as dispatch and the wizard do.
- **Every send list is enrolled.** The audience is read through `resolveWizardAudience`, so an
  attach-existing audience (several include lists, from the custom audience builder merged from
  main) enrols all of them as an OR of IN_LIST branches, not only its first list.
- **The audience's suppressions reach the workflow.** They are added to the source workflow's own
  `suppressionListIds`, which are kept as portal policy.
- **No silent fallback.** A brief with no built audience, an unreadable one, or no audience store
  used to keep the SOURCE workflow's enrollment — another campaign's audience. It is now a 409
  (503 for the store), with the send and brief named.
- **The briefs must agree** on send lists, suppressions and portal, not only the master list id.
- The result's `enrollment_list_id` (declared but never set) is replaced by `enrollment_list_ids`
  and `suppression_list_ids`, both populated.
- `SURVEY_SEQUENCE_WORKFLOW_ID` is wired in the chart (empty selects the in-code default), which
  `TestEveryConfiguredEnvVarIsWiredInTheChart` required.

The survey migrations from the 2026-10-16 and 2026-10-20 entries were renumbered on the merge
from main, which claimed `000047`–`000051` first: `000047` is now `000052` and `000048` is now
`000053`.

See [internal/service](../code/internal-service.md).
