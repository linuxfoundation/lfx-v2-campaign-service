# 2026-08-24 — a leaked permit is never recovered, so the release cannot be per-return

**Fix** — `resolveVariantAssets` leaked semaphore permits on every failure arm. Two independent
bugs, compounding:

1. The two `GetAssetSize` error arms returned `func() {}` instead of the accumulated releaser, so
   reservations from iterations `0..i-1` were discarded.
2. `Dispatch`'s `defer releaseAssets()` sat **below** its error check, so on a failed resolve it
   never ran at all — leaking whatever the other seven arms handed back.

Either alone leaks. Together they leak on every error path in the loop.

## Why this is worse than it looks

A leak here fails nothing immediately. The dispatch reports its real error, the caller sees the
right message, and the only casualty is a budget that is quietly smaller **forever**. The bill is
paid by a *later* dispatch, shed for capacity nothing is using, and it never recovers without a
process restart. That is why the tests assert on the budget rather than on the returned error —
asserting the error would have passed throughout.

## The shape, not the instances

This was the third round on this same bound. Round one charged the reservation after loading the
bytes; round two left the encoded string resident; round three leaked on failure. Patching the two
named arms would have been a fourth partial fix, because the defect is not any one return — it is
that a correctness property was spread across every exit and re-established by hand at each one.

So the unwind moved into the function that takes the reservations:

```go
succeeded := false
defer func() {
    if !succeeded {
        releaseAll()
    }
}()
```

Every error arm now hands back a no-op and the defer does the work. A return added later cannot
leak, because it does not have to remember anything.

**The flag is not `err != nil`, and the difference is the panic path.** The obvious shape uses a
named error return and `if err != nil`. On a PANIC no return statement runs, so `err` is nil and
the release is skipped — on the one exit a `defer` exists to cover. Keying on "did this hand the
reservation to the caller", set immediately before the single successful return, covers the panic
and every future return as a side effect.

## Why the caller still releases on success

The bytes outlive the call: `resolveVariantAssets` returns them in the variant slice and the Meta
client POSTs them to `/adimages` later in the same dispatch. Releasing at the resolve's return
would free budget for memory that is still resident — the bound inverted. So the contract is
asymmetric on purpose: **the callee owns failure, the caller owns success.** `Dispatch`'s defer
also moved above its error check, which is now belt-and-braces rather than the bound itself, and
`releaseAll` nils its slice so a double call cannot over-release (a weighted semaphore *panics* on
over-release).

## What pins it, and what one test does not

| test | proves | fails on shipped code? |
| --- | --- | --- |
| `ReleasesReservationsOnMidLoopFailure` | budget whole after one failure | **yes** |
| `RepeatedFailuresDoNotExhaustTheBudget` | budget whole after 32 failures | **yes** |
| `ReleasesReservationsOnPanic` | budget whole after a panic | n/a — guards the new shape |
| `ReleaseIsIdempotent` | double release does not over-release | n/a — guards the new shape |
| `SuccessPathReleasesExactlyOnce` | success does NOT release early | n/a |

The exhaustion test matters on its own: a single leak of a few MiB against a 240 MiB budget still
admits the next dispatch, so a single-shot test can pass while the leak is real. Thirty-two
iterations put the leaked total past the whole budget.

**One test was written vacuous and rewritten.** The first `ReleaseIsIdempotent` called the
releaser twice on the *error* path — where it is already a no-op, so it could never fail. Removing
`releases = nil` did not break it. Rewritten against the SUCCESS path, where the returned func is
the real accumulated one, the same mutation now panics with `semaphore: released more than held`.
A test that cannot fail is worse than no test: it is counted.

## The rule

**A resource acquired inside a loop must be released by a construct that cannot be forgotten, not
by each return statement.** Enumerate every exit — including the ceiling arm, context
cancellation, and panic — before writing the fix, and prefer a shape where the leak is
structurally impossible over one that is merely correct today.
