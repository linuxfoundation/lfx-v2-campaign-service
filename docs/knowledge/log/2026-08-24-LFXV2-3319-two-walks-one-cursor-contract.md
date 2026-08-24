# 2026-08-24 — two walks, one cursor contract, and the gap between them

**Fix** — `findByName` reported a confident "not found" for a full page carrying
`"next_cursor":""`, and the caller answers a not-found with a create POST. The result is a
duplicated live campaign spending a second budget.

The guard was there and it was tested. It simply asked the wrong question:

```go
if len(items) >= listPageSize && !resp.NextCursorPresent {
```

An empty STRING is present. So a full page with `"next_cursor":""` skipped the guard, fell
through to `if resp.NextCursor == "" { return "", nil }` one line below, and terminated as a
clean not-found. The bit the guard consulted answered "did the key appear", when the question the
walk actually needed answered was "did this body say the results are exhausted" — and X only ever
says that with an explicit `null`.

## The sibling already knew

`ListAdAccounts` walks cursors too, and it had all three shapes right: absent is an error, empty
is an error, only the documented null terminates. It had been fixed for the empty-cursor case in
its own right, and the reasoning never crossed to `findByName` — the two walks read the same
`apiResponse`, with the same two extra bits on it, and disagreed about what those bits meant.

Worse, the divergence was written down as intentional. `NextCursorNull`'s doc comment said
"findByName does not need this bit", and the concept doc said it "deliberately does not consult
it". Both sentences described a real decision that had been correct when made and was falsified
by a later change to the same branch — so the code and its documentation asserted the gap was a
design choice rather than an oversight, which is the state in which a defect survives review.

## The fix is a shared classifier, not a second guard

Adding the missing `""` case to `findByName` would have fixed this instance and left the shape
that produced it: two walks, each reading raw bits, each free to draw a different conclusion.
`cursorVerdict` now owns the classification and returns one of three outcomes — usable,
exhausted, unknowable. Both walks call it.

What the walks do NOT share is the policy, and that separation is the point. `ListAdAccounts`
owes every account or an error, so unknowable is always fatal. `findByName` may still conclude
from a SHORT page, because X documents a page below `count` as conclusively the last one — only a
FULL page owes a cursor and is therefore unknowable without one. Folding that into the classifier
would either make the accounts walk too permissive or break find-or-create for every ordinary
small account. The classifier says what the body SAID; each caller applies its own contract.

The mutation check is what shows this is structural rather than cosmetic: flipping either arm of
`cursorVerdict` now fails tests in BOTH walks at once. Before, the same flip could only ever
break one of them.

## The rule

When two code paths read the same wire field, the thing to share is the READING, not the
conclusion. A comment asserting that one of them "deliberately does not" consult a bit is a claim
about behaviour and ages exactly like any other — re-derive it against the code before trusting
it, especially when the change that falsified it is on the branch you are already reviewing.
