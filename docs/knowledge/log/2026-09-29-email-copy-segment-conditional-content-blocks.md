# 2026-09-29 Email copy segment-conditional content blocks

**Update** — `generate-email-copy` gained an optional `segment` query
parameter (`design/brief.go`), same free-text/lenient-fallback shape as the
existing `stage` and `variant` parameters: absent or unrecognised means "no
segment requested" rather than an error. Four values are recognised —
`developer`, `business-decision-maker`, `alumni`, `prospect` — each appending
its own fixed guidance block in `composeEmailCopyPrompt` (`internal/service/email_copy.go`)
that narrows which of the stage's content blocks are relevant to that
audience, without adding new facts or changing the stage's purpose. Reaches
the stage-aware prompt only, same LFXV2-1940 restriction as `registrationURL`
and `variant`: an absent stage never sees `segment` at all.

`segment` composes ADDITIVELY alongside `variant` rather than replacing it —
`variant` restyles the whole draft's framing, `segment` narrows which blocks
within that draft matter to a named audience. Both may be set together,
either alone, or neither.

`maxComposedPromptSize` moved from 14000 to 14600 runes to accommodate the
new segment blocks as floor contributors (fixed prompt text, not caller
input): `worstStageFloorNamed` (`internal/service/email_copy_test.go`) now
composes each stage across variant-on/off AND every recognised segment (plus
none), which moved the worst case from Post-Event + urgency-fomo (11055) to
**Post-Event + urgency-fomo + the alumni segment (11689)**, 634 runes higher.
The worst valid composition is now 14089 against the 2400-rune input bound,
so 14600 clears it with 511 runes of headroom.
`docs/knowledge/code/internal-service-email-copy.md` and `docs/api-catalog.md`
were updated to match, including the historical "At 6500" narrative's
remaining-allowance arithmetic against the new floor.
