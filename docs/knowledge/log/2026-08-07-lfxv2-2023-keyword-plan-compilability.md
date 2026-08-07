# 2026-08-07 — LFXV2-2023: a plan's code has to compile too

**Update** — Review found two places where the keyword-surface plan's illustrative code
would not build, both introduced by a correction made elsewhere in the same document.

`KeywordActionOutcome` was reworked from a stored `Success bool` into a three-state
`State KeywordActionState`, with `Applied()` derived so the two cannot drift. Two early-exit
branches in the dispatch outline were missed by that rework and still assigned
`outcome.Success = false`. Beyond not compiling, the shape of the mistake matters: had they
been translated mechanically to a zero-valued `State`, the outcome would have been neither
applied, failed, nor unconfirmed, and the documented invariant
`total == succeeded + failed + unconfirmed` would silently not hold. Both now set
`KeywordActionFailed`.

`PartialMutateError` is returned through `error` and matched with `errors.As`, but had no
`Error()` method, so it did not implement `error` at all. It now has one, plus `Unwrap` —
the caller distinguishes a context cancellation from an upstream rejection with
`errors.Is(err, context.Canceled)` on the same value it matched with `errors.As`, and
wrapping the cause without `Unwrap` would hide it. The message names the boundary indices
rather than counts, because the type does not know `len(ops)` and so cannot count the
never-sent tail.

The general point: code in a plan document gets read as a specification and copied. It is
not exempt from compiling, and the reviewer who checks it is doing the same work as the one
who checks the implementation.
