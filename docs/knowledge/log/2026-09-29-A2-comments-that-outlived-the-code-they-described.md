# 2026-09-29 — A2: comments that outlived the code they described

**Docs** — no behaviour change. Raised by the third pre-PR local review round over the whole A2
range; comments and one concept-doc claim only.

Three corrections, all of the same kind: prose that was accurate when written and was not
carried forward when the code under it moved.

`TestGooglePacingBoundaries`' comment said it pinned "the three LOCAL literals … NOT this
package's shared Thresholds" and existed to catch a future edit that repointed them at the
shared constants. That is now exactly backwards — Google routes through `pacingLabelFor` by
design (`linuxfoundation/lfx-self-serve#3019`). The test itself is still worth having, for a
different reason: it pins the inclusive/exclusive boundaries a refactor can silently flip. The
warning it carries is now the accurate one — do not repoint this at `pacing.go`'s `Thresholds`,
which run 50/100/130 for the single-campaign brief path and would move operator-facing
alerting bands.

`monitor_shared.go`'s `fetchFailedRow` doc block had come loose from its function. As helpers
moved into the file it ended up at the top, above the pacing constants, reading as though it
described them. It also still said Reddit's empty-`StartDate` branch "reuses this same
builder", which stopped being true when that branch moved to `unknownPacingRow`. Moved back
onto `fetchFailedRow` and the Reddit sentence removed.

The account-monitor concept said the budget-less defect had all four platforms reporting
`underspending`. Only Google, Meta and Reddit did. LinkedIn's `hasBudget` guard really did hold
those rows off the ladder — it just did not say so, emitting `normal` with `PacingUnknown`
false, which a consumer reads as "on plan". The log entry for that fix drew the distinction and
the concept summarising it flattened it. Both outcomes are now stated separately.

## The pattern

A comment stating what the code does **not** do — "these are local, not the shared ones" — is
the kind most likely to invert rather than merely go stale, because the change it warns against
is precisely the change someone eventually makes on purpose. It is still worth writing; it just
has to be re-read by whoever makes that change, and a negative claim in a doc comment is a
reason to grep for the thing it names.
