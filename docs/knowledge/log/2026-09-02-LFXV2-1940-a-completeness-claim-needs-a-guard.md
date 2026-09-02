# 2026-09-02 — LFXV2-1940: the inventory claimed completeness it did not have

**Fix** — the concept doc's `## Testing` section states its inventory "names every test rather
than counting them", and it named 25 of 27. `TestComposedBoundClearsEveryStageFloor` and
`TestComposeEmailCopyPrompt_OmitOutranksRequired` were both missing — added by this branch and
never listed. A reviewer found it, not the repo.

Both are now listed, and `TestConceptDocNamesEveryTest` makes the claim checkable: it parses every
`func Test...` in `email_copy_test.go` and fails when one has no `**Name**` entry in the doc. The
guard caught itself on first run, which is the right behaviour.

This is the same failure shape as the composed-bound comment, which went stale three times while
carrying its own "re-measure" warning: a prose claim that nothing verifies drifts silently. Where
a claim has a computable form, assert it.

Also corrects a fourth stale figure in that same comment block. Line 382 said "Post-Event is 5041
runes on its own" while three lines below said 5503 — and NEITHER is the template's own size. The
ContentPrompt is **3637 runes** (measured); **5503** is the COMPOSED floor: system and user framing
plus the template at zero caller input, which is what `TestComposedBoundClearsEveryStageFloor`
computes. All three "on its own" phrasings now say which quantity they mean.
