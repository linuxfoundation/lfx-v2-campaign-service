# 2026-09-02 — LFXV2-1940: the composed margin was down to 24 runes

**Fix** — a reviewer flagged a fifth stale number in the sizing section: "5503 under a 6500 bound
leaves ~1459 runes", when 6500 - 5503 is 997. Correcting it surfaced something larger.

**The floor was not 5503 either.** Measured, it is **5976** — this session's template edits added
473 runes. With the 2400-rune input allowance the worst composition was 8376 against an 8400
bound: **24 runes of margin**, from 497 at the start of the night. One more sentence in any
template would have pushed valid caller input into a 503 blaming the service.

The bound is now **9000**, restoring ~624 runes — roughly one template section, which is the unit
the margin should be measured in: not "some slack" but "one more section can be written".

`TestConceptDocSizingArithmetic` derives the input bound, composed bound, worst composition and
headroom from the real constants and fails when the prose contradicts them. Presence alone was too
weak — 8376 appears twice, so one instance could go stale while the other satisfied the check — so
it also FORBIDS the specific values that have shipped and been superseded (5503, 7903, 5041, 7600,
997), while leaving the historical bullets that deliberately narrate 6500 and 7700 as past
mistakes.

Five figures in this one section have gone stale, each caught by a reviewer rather than the repo.
The section's own thesis is that these numbers go stale; it now has a test instead of a warning.
