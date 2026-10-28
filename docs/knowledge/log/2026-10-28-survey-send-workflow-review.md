# 2026-10-28 — Survey send workflow: pre-PR review fixes

**Fix** — the pre-PR review of the survey branch found the send-workflow endpoint unreachable
and several ways the cloned workflow could email the wrong people, the wrong copy, or at the
wrong time.

- **Routed and authorized.** `send-workflows` joins the HTTPRoute alternation and the
  `campaign_manager` RuleSet as a sibling of `briefs`; `TestRouteRuleSetParity` pins both.
- **Canvas order, not id order.** Steps are walked from `startActionId` along STANDARD
  connections, superseding the numeric action-id ordering in the 2026-10-21 entry. A branching
  source, a send or dated step off that path, a non-`LIST_BASED` enrollment, or a source still
  referring to its own emails elsewhere is refused (`hubspot.ErrFlowShapeUnsupported`, 409).
- **Dates are required, future and in order.** The source's own dates are no longer kept; a past
  or non-increasing date is a 400.
- **Only fully staged drafts, in the audience's portal.** A pending or unconfirmed campaign row,
  or one whose recorded `portalId` differs from the audience's, is a 409.
- **Checks see the workflow's real suppressions.** The overlap and standing-suppression checks run
  in `BeforeCreate`, after the source's own `suppressionListIds` are merged.
- **Errors by sentinel, not text.** `mapFlowCloneErr` uses `errors.Is`; a 404 on the deployment's
  default workflow is a 409 naming `SURVEY_SEQUENCE_WORKFLOW_ID`.
- **The survey source document is rendered first** in the event-facts block, so the block backstop
  cuts event facts rather than the document.
- `send_dates` elements carry a `YYYY-MM-DD` pattern in the design.

See [internal/platform/hubspot](../code/internal-platform-hubspot.md) and
[internal/service](../code/internal-service.md).
