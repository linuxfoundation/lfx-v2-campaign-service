# 2026-09-30 Email QA B1 — list-to-header run-on

**Fix** — production QA (`LFX-Campaigns-Email-QA-Report.md`, bug B1) found
generated campaign emails where a bulleted/emoji list's last line ran directly
into the next bold header with zero separation
(`"...premier cloud native event.Why attend KubeCon + CloudNativeCon:"`),
reproduced across three CNCF events including a real staged HubSpot draft.

Root cause is prompt-level, not code-level: `GenerateEmailCopy`'s path
(`internal/service/email_copy.go`) has no markdown/HTML join step of its own
— `parseEmailCopyResponse` passes each `rich_text` section's HTML straight
through unmodified, so a missing boundary between a list and the following
section is the model's own raw output, not something this repo assembles.
The shared stage-aware system prompt (`composeEmailCopyPrompt`) never told
the model to keep each idea in its own `rich_text` section or to fully close
a `<ul>`/`<ol>` before the next heading; the urgency-fomo variant block made
this worse by explicitly asking for adjacent list-heavy numbered sections
("Why attend" then "What you'll experience") without saying they must be
separate array entries.

Added one rule to the shared system prompt ("One idea per rich_text section
-- never run a closed `</ul>` straight into the next heading with no space or
tag between them") and a matching restatement in the urgency-fomo variant
block naming the exact two sections the report reproduced. This is caller-
facing prompt text, not a code branch, so it is validated the same way the
rest of this file's prompt rules are: `TestComposeEmailCopyPrompt_WarnsAgainstListRunningIntoNextSection`
(`internal/service/email_copy_test.go`) asserts both strings are present.

This grew the worst-case composed prompt (Post-Event + urgency-fomo + the
alumni segment) from an 11689-rune floor to **11988** — 299 runes, not the
full first draft's 753, because the wording was trimmed twice specifically to
fit under `maxComposedPromptSize` (14600) without raising the bound itself.
The worst valid composition is now 14388 against the 2400-rune input bound,
leaving 212 runes of headroom (down from 511).
`docs/knowledge/code/internal-service-email-copy.md` was updated to match,
including the historical "At 6500" narrative's remaining-allowance arithmetic
against the new floor.
