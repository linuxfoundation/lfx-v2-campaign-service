# 2026-08-29 — a second instruction is not a stronger one

**Update** — Bot review on the stage-aware copy PR found a prompt that told the model two
incompatible things, and the cause was that I added an instruction without reading the one already
there.

## The contradiction

A reviewer noted the templates demand facts nothing supplies — `[PROMO_CODE]`, `[REGULAR_PRICE]`,
`[SESSION_COUNT]` and a dozen more — while marking those sections required, so the model invents
values. That is a real problem, and I added a constraint telling it to KEEP an unfillable
placeholder verbatim rather than invent one.

The system prompt already said the opposite, thirty lines above: **omit** any sentence or section
whose placeholder has no supplied value, and do not emit the bracket itself. Both instructions
shipped in the same prompt. A model can follow either, so the output became less predictable than
before the "fix" — the added rule made things worse while looking like a safeguard.

**The lesson is narrow and mechanical: read the whole prompt before adding to it.** A prompt is one
document, not a list of independent rules, and two rules in it can conflict in a way two functions
cannot. The original instruction was already correct; the correct action was to add nothing.

## Two more of the same shape, in adjacent files

- A Post-Event `FooterNote` asserted **"Session slides ready now"** as a fact. Nothing supplies it,
  so it contradicts the no-invention constraint the same prompt states — now `[SLIDES_DATE]`.
- A Final Countdown validation rule forbade **"CTA info"** while the same stage requires a primary
  "View Full Schedule" CTA. The model cannot satisfy both. Narrowed to sponsorship and pricing.

## And one in the docs

The concept doc claimed that sharing one size constant between the pre- and post-composition checks
would make the post-check "unreachable". It is the reverse: the smallest composed prompt is 4923
runes because the stage template alone exceeds 3000, so a shared 3000 REJECTS every valid request.
Being wrong in the safe-sounding direction is what let it read as plausible.

**How to apply.** When a review says "X is unsatisfiable", check whether the fix ADDS a rule. If it
does, find the rule it duplicates or contradicts first — in this case the existing one was right
and the whole fix was unnecessary.
