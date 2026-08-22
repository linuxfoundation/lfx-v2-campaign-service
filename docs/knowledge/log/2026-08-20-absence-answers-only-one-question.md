# 2026-08-20 — absence answers only one question, and a guard is a whole-interface obligation

**Fix** — three review findings, two of which are regressions from
[2026-08-21-absent-is-not-unprovable](2026-08-21-absent-is-not-unprovable.md). That fragment's
`(nil, nil)` guidance stands; its **provenance** conclusion was wrong and is corrected here.

## Reading absence as provenance inverted a working behaviour

That fragment split `systemCreated`'s `known=false` into "cannot determine" vs "determined
absent" and used the second to return the operator-owned fault when the recorded creation
account matched neither scope. The reasoning was: no reachable credential can address this
campaign, so page an operator.

That answered the wrong question. Two are in play, and only one decides who is paged:

- *"can anything address this campaign right now?"* — absence is evidence for this.
- *"who CREATED this campaign?"* — absence is evidence for **nothing**.

A missing row proves only that the row is missing **now**. The counterexample was sitting in a
comment directly above the new arm: a PROJECT-created campaign whose project later re-pointed
its connection reaches the same state — recorded account differs from the current one, no system
row installed. Indistinguishable, by absence alone, from a system-created campaign whose row was
deleted.

Verified empirically rather than by reading: the same fixture against pre-branch `creds.go`
returned the project's resolution (`act_111`, the actionable 409); against the new code it
returned `ErrSystemConnectionMissing` — a 500 paging the platform operator for a reconnect the
project performs itself.

**The test I had added asserted the wrong behaviour**, and its fixture is why: project resolves
`act_111`, campaign records `act_999`, no system row. That *is* an ordinary re-pointed project.
It has been replaced by `TestAbsentSystemRowKeepsTheProjectFault`, plus
`TestSystemCreatedCampaignWithAbsentRowSurfacesTheOperatorFault` for the case the old test
*meant* to cover — where the system row **positively matches** the recorded account and is
unusable, so provenance is established rather than inferred from a gap.

Provenance now keeps ONE discriminator: a positive match. The `absent` third value had no
remaining consumer and was removed rather than left as a misleading affordance.

**The general rule: absence is evidence about reachability, never about history.** Before acting
on "the row is gone", name the state that produces the same observation without the conclusion.

## A guard on an interface contract is owed at every call site

`ConnectionReader.Get` may return `(nil, nil)`. The previous round guarded that in `updateConn`
and `systemCreated` — and Bugbot found a third site, `resolveForcedSystem`, passing the nil
straight to `resolveConn`, which reads `conn.ID` immediately and panics.

Rather than fix the reported line, all **nine** call sites were enumerated and audited:

| Site | Handling |
|---|---|
| `creds.go` `systemCreated` | guarded (`err != nil \|\| conn == nil`) |
| `creds.go` `resolve` (project) | **fixed** — nil now takes the same system fallback `ErrNotFound` does |
| `creds.go` `resolveForcedSystem` | **fixed** — same `ErrSystemConnectionMissing` + `ErrNotFound` as the absent arm |
| `creds.go` `resolveOwned` | **fixed** — `noOwnConnection` |
| `creds.go` `systemConn` | **fixed** — returns `(nil, nil)`; also stops logging a resolution never found |
| `connection_handler.go` `getConn` | **fixed** — was returning a nil row with a nil error, which adapters marshal as a 200 |
| `connection_handler.go` `updateConn` | guarded |
| `connection_handler.go` `testConn` | **fixed** — `HasCredentials()` reads a field on the nil row |
| `bootstrap/sysacct.go` | **fixed** — normalised to `ErrNotFound` so the CLI creates the row |

The `resolve` case is the one worth remembering: rendering the nil as a plain error would have
been "safe" and still wrong, because a genuinely absent project row **earns the LF system
fallback**. A nil-guard has to reproduce its arm's whole behaviour, not just avoid the panic.

## A validation ordering invariant broke when the first mutating call moved

Step 1.5 authors a promoted post before the campaign create. Two deterministic checks — the
effective-window check and the two composed-name length checks — stayed where they had been,
just above the campaign POST, which was correct while that POST was the first mutating call.

With an `ImageURL` set they could return `(nil, error)` with a post **already authored
upstream**. `CreateCampaign`'s contract makes `(nil, err)` mean "nothing was or may have been
created" and `Dispatch` releases the claim on `result == nil` alone — so the claim was released
on a post that definitely existed, and a retry with the same bad input authors another duplicate
every time. Deterministically, forever.

Both checks were **verified post-independent before being moved**: their inputs are the caller's
`StartDate`/`EndDate`/`EventName`/`GeoTargets`, plus `objective` and `geos` (both settled well
above Step 1.5) and the clock. Neither reads `postID`, `validatedPostID` or `postWarning`. So
they move ahead of Step 1.5 rather than being converted to partial returns — restoring the
invariant the ordering was always meant to have: **a bad input costs nothing upstream.**

This is a small structural step toward #173: the fix is a change to the step *sequence*, not
another string patch on its output.

## Verification notes

- The finding-3 test asserts the **claim outcome plus the upstream effect** — `result == nil`
  AND no mutating request reached Reddit. Asserting only the error would have passed on the
  broken code, which also returned an error; what the defect left behind is the whole point.
- Mutations moving each check back independently fail only their own subtest, showing the two
  are separately pinned. Removing either nil guard panics rather than failing an assertion.
- The dispatch fixture could not express `(nil, nil)` — same gap as `fakeRepo` last round. A
  fixture that can only report absence one way makes a guard against the other way read as
  covered.
