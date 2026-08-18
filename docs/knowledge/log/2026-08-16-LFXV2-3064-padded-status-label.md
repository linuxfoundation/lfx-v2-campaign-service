# 2026-08-16 — LFXV2-3064 padded status label

**Fix** — `microsoftAccountLabel`'s fallback arm tested `strings.TrimSpace(a.Status) != "Active"`
while `Usable()` gates on the exact `a.Status != "Active"`. For `Status: " Active "` the two
disagree, and the label loses: the exact gate denies the account (padding is not `"Active"`), the
trimmed test reads it as Active and selects the else branch, and the row is labelled
`"not writable with this credential"` — blaming the ROLE for what is a status problem.

That is precisely the mislabel this arm was added to remove, reintroduced for one input class.
Raised by dealako on #132 as a nit, with the failing case worked out in full; padding is unlikely
because `Status` is unmarshalled from `AccountLifeCycleStatus` with no normalization, but nothing
prevents it.

The arm now makes the SAME comparison the gate made. That is the general rule this case
illustrates: an arm whose only job is to EXPLAIN a decision another predicate already took cannot
use a different predicate. Any divergence means it explains a decision that was never taken, and
the explanation will be confidently wrong rather than absent — the worse of the two failures for
an operator deciding where to look. The alternative fix, trimming inside `Usable()`, was not taken
because it changes what counts as a usable account rather than what the label says about one.

The test case pins the exact scenario from the review. Reverting to `TrimSpace` fails it with
`label = "… — not writable with this credential"`, which names the defect in the assertion output
rather than only in prose.
