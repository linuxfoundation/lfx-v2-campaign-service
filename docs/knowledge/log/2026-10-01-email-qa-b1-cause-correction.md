# 2026-10-01 Email QA B1 — the cause named on 2026-09-30 was wrong

**Note** — Corrects `2026-09-30-email-qa-b1-list-section-boundary.md`, which
put the root cause of QA bug B1 (a list's last line running into the next bold
header) at "prompt-level, not code-level" in this service. It was not in this
service. This entry exists because the earlier file is not edited once written.

**What actually produces the run-on.** This service asks the model for each
`rich_text` section as inline HTML with no outer `<div>` (`composeEmailCopyPrompt`,
"inline HTML, no outer <div>/<style>"), and the urgency-fomo structure makes
each numbered part its own `rich_text` section. The consumer, lfx-self-serve's
BFF, flattened those sections with `.map((section) => section.html).join('')`,
so an inline fragment such as a closing `</ul>` or a plain sentence ended
directly against the next section's `<strong>` heading. `parseEmailCopyResponse`
returning each section untouched is correct; the sections are separate array
entries on purpose and it is the join that removed the boundary.

**Where it is fixed.** In lfx-self-serve, `CampaignServiceService.foldEmailSections`
(`apps/lfx-one/src/server/services/campaign-service.service.ts`) now wraps each
`rich_text` section in its own `<div>` and joins them with a newline. Nothing in
this service has to change for B1.

**What stays here, and what it is.** The `One idea per rich_text section` rule in
the shared system prompt, its restatement in the urgency-fomo block, and
`TestComposeEmailCopyPrompt_WarnsAgainstListRunningIntoNextSection` stay. They
ask the model to keep ideas in separate sections, which is consistent with how
the consumer now folds them, and they cost the 299 runes of prompt floor the
2026-09-30 entry recorded. They are defence in depth. They are not the fix, and
no measurement shows they change what the model emits either way, so do not cite
them as the reason B1 is gone.

**Not verified.** The fix in lfx-self-serve was verified against the BFF's unit
tests and by reading the join, not against a live generation from the model.
