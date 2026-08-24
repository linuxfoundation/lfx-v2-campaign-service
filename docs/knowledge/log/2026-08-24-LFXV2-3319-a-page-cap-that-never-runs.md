# 2026-08-24 — the cap cited as the safety net is on a path the defect never reaches

**Fix** — X Ads find-or-create: `nextCursorRaw`'s doc comment justified `findByName` not consulting
the cursor presence bits like this: "an absent cursor there costs at most a missed match on a
malformed body, which its own page cap already answers with an error."

Reading `findByName` shows the cap cannot answer it. The loop ends a page with:

```go
if resp.NextCursor == "" {
    return "", nil        // <- "not found"
}
cursor = resp.NextCursor
```

`maxListPages` is reached only by going round again, which requires a NON-EMPTY cursor. A body
whose `next_cursor` is absent (or null, or `""`) returns `("", nil)` on the **first** page and
never approaches the cap. `("", nil)` is what `findCampaignByName` documents as "no such
campaign", and its caller answers that with a create POST — so the malformed body produces a
DUPLICATE campaign, the precise outcome the surrounding code repeatedly says it exists to
prevent. **A cited safety net has to be traced to the path in question; this one guarded a
different exit.**

## The first fix was correct about the bug and wrong about the scope

Erroring whenever `!NextCursorPresent` broke 8 existing tests, because ~140 of this package's
fixtures write `{"data":[...]}` with no cursor key — terse stubs for unrelated flows. Rewriting
them all to satisfy the guard would have been large churn driven by a narrow finding.

X's own rule supplies the discriminator, and it is not the cursor field: *"If less than count
entities are returned in the current page of the result set, the next_cursor value will be
null."* So a **short** page is conclusively the last one on its own evidence, cursor or no
cursor. Only a **full** page (`len(items) >= listPageSize`) owes a cursor, and a full page
without one is the genuinely unknowable case. Scoping the guard that way fixes the defect with
**zero fixture changes**.

`count=1000` became the named `listPageSize` because the comparison and the two query strings
must not drift; three literals that must agree, spelled three times, eventually disagree.

## Both directions need pinning

- full page, no cursor key → must ERROR (killed by removing the guard)
- short page, no cursor key → must stay a clean not-found (killed by dropping `len(items) >=
  listPageSize` from the guard)

The second test is what stops the over-strict fix from looking correct. **A guard with a
boundary needs a test on each side of it, or the mutation that widens it survives.**
