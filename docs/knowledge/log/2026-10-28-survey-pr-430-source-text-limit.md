# 2026-10-28 — A survey source document is refused past its limit, never cut

**Fix** — follow-ups from the #430 review.

- **`event_details.sourceText` over `maxFactSourceTextRunes` (3000) is a 400**, naming the field
  and the limit, when a brief is created or replaced and again at `generate-email-copy` and the
  wizard's generate step (for a brief stored before the save-time check). Both generators used to
  cut it to the bound silently, and it is a survey's only factual basis: a finding, closing date
  or incentive past the bound was simply missing from the copy. Measured as the prompt renders it
  (whitespace runs collapsed, trimmed). The constant is the one limit; the truncation that remains
  in the facts block is only a backstop.
- **A reverted body behind identical widget keys is caught.** `verifyContentSaved` now compares the
  `staging_body` html it reads back with what it wrote. A template cloned from an earlier generated
  email already carries a placed `staging_body`, so a reverted PATCH used to pass the key check and
  dispatch recorded `contentStaged` for a draft holding the earlier body.
- **The wizard's reference variant follows the survey specification.** For a survey stage,
  `composeWizardReferencePrompt` appends the stage's `ContentPrompt` (word cap, disclosure, share
  line, what a reminder or closing may claim); only the stage variant carried it before. Event
  stages' reference prompt is unchanged.

See [internal/service](../code/internal-service.md) and
[internal/platform/hubspot](../code/internal-platform-hubspot.md).
