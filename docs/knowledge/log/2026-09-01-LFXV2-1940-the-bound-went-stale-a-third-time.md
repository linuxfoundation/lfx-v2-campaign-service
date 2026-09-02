# 2026-09-01 — LFXV2-1940: the composed bound went stale a third time, by my own hand

**Fix** — the composed-prompt bound was 7600 with a comment claiming "worst valid composition is
7441". Measured: **Post-Event floors at 5503**, so with the 2400-rune input allowance the worst
valid composition is **7903** — 303 runes above the bound. A caller sending perfectly valid event
details would be refused by the SECOND check after the first accepted it, with a 503 blaming the
service.

The comment already carried a warning: *"Re-measure both whenever the shared prompt or any
template changes: this comment has now been wrong twice from exactly that."* It was wrong a third
time within hours, because the precedence paragraph I added to the shared system prompt earlier
today grew every stage by ~130 runes and I did not re-measure.

**A prose instruction to re-measure does not survive contact.**
`TestComposedBoundClearsEveryStageFloor` now computes every stage floor and fails if
`worst + inputBound > composedBound`, naming the stage and the arithmetic. Reverting the bound to
7600 fails it with the exact numbers. Bound raised to 8400 (~497 runes headroom).

Also fixed, from the same hidden block: three stage patterns asserting facts nothing supplies —
"First-time speakers welcome", "Recordings available. Survey takes 3 minutes", and a required
WHAT'S NEW section demanding "1-2 changes since they last attended" from a pipeline that has no
attendance history. None carried a placeholder, so the OMIT rule could not reach any of them.

**How these were found:** Copilot's `<details>Suppressed comments (6)` block, inside the review
BODY. They are not review threads, so the thread query reported **0 open** while six real findings
sat unread. Sweep bodies too:

```bash
gh api repos/<org>/<repo>/pulls/<n>/reviews --paginate \
  --jq '.[]|select(.body|test("Suppressed";"i"))|.body'
```

**The general shape:** a constant justified by a comment is a constant that will go stale. If the
number has a computable relationship to something else, assert the relationship.
