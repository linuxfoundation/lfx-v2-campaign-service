# 2026-08-31 — LFXV2-2775: a guard that stopped at the first match

**Fix** — the guard added hours earlier, to stop stage templates asserting facts nothing supplies,
had the same defect it was written to catch.

`claimSentence` returned the FIRST sentence containing a commercial word. The OMIT rule drops one
sentence at a time, so a guarded sentence says nothing about the one after it:

```
"Early bird pricing ends [DEADLINE]. Standard pricing applies after that date."
```

The first half has a placeholder, so the guard stopped there and passed. The second asserted a
standard-pricing schedule with nothing to omit it — for a pipeline that supplies neither a standard
price nor a registration deadline. `claimSentences` now returns every matching sentence, and the
footer carries `[REGULAR_PRICE]`.

**Proven by mutation in both directions:** with the first-match guard the bad footer passes (`ok`);
with the widened guard the same footer fails. The narrow version would have shipped it.

**Both bots found this independently**, which is the signal worth acting on immediately — cursor
("Test skips later unguarded claims") and Copilot ("only validates the first sentence containing
each keyword") described the same mechanism from different angles.

**The general shape:** a guard that scans for a condition must scan the WHOLE input, not stop at
the first hit. Returning early makes it agree with itself: it reports on what it chose to look at.
This is the second time in one PR that the granularity of a check was wrong — first the string
where the rule acts on sentences, then the first sentence where the rule acts on all of them.

Related: `docs/knowledge/log/2026-08-31-LFXV2-2775-a-claim-with-no-placeholder.md`.
