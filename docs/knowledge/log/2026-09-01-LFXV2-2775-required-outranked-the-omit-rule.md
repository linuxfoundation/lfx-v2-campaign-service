# 2026-09-01 — LFXV2-2775: a REQUIRED marker outranked the OMIT rule

**Fix** — the third instance of the same class, and the first two fixes could not reach it.

The prompt says to OMIT any sentence or section whose `[BRACKETED]` placeholder has no supplied
value. The stage briefs also carry `REQUIRED INFORMATION HIERARCHY`, and Registration Push's first
required item is `1. HEADLINE: "Early Bird Pricing Ends [DEADLINE]"` — required, with a placeholder
nothing fills. Two contradictory instructions, and the emphatic one wins: the model invents a
deadline rather than dropping the section.

The earlier fixes placeholdered `SubjectPattern`/`PreviewPattern`/`FooterNote`. Neither touched
`ContentPrompt`, where the requirement lives, so the guard I wrote for those fields could not see
this at all.

The prompt now states precedence explicitly — the OMIT rule outranks the stage brief, quoting this
exact conflicting line — and a test asserts the composed SYSTEM prompt carries it. Also removed the
CFP mentorship claims: they assert a programme exists, carry no placeholder, and so were unreachable
by the OMIT rule in either field.

**The general shape:** when two instructions in one prompt can conflict, the prompt must say which
wins. A rule that is merely present loses to a rule that is marked REQUIRED. And a guard scoped to
some fields says nothing about the field that actually carries the instruction — the same
granularity error as the previous two entries, one level out.

Related: `docs/knowledge/log/2026-08-31-LFXV2-2775-a-guard-that-stopped-at-the-first-match.md`.
