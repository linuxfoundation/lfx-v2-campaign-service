# 2026-08-20 — "present" is not "changed", and the enumeration that would have found it

**Fix** — `rejectForcedSystemAccountWrite` rejected every config update carrying a non-empty
`account_id`, not just a changed one. It now takes the row's current selection and fires on a
CHANGE.

Extends, and does not supersede,
[2026-08-20-a-guard-that-reads-a-shared-field](2026-08-20-a-guard-that-reads-a-shared-field.md),
which records the provider-scoping fix to the same helper. That fix is correct as written; this
fragment records a dimension it did not consider.

## What broke

`account_id` is `Required` on LinkedIn, Reddit, X and Microsoft (`design/connection.go`, generated
as a non-pointer `string`), and PUT is a full replace on every provider in this API. A caller
renaming a connection therefore has no way to omit the id — the schema will not decode a body
without it. A guard keyed on the id being PRESENT returned 400 for every update those four
providers can express: **four update endpoints dead** while the flag was on, not degraded.

Google Ads and Meta, whose `account_id` is optional, could satisfy the presence check — but only
by omitting the field, which (PUT being a full replace) CLEARS the column. The guard's single
permitted way to rename a Google or Meta connection was to destroy the account selection. That
selection is the project's own, pre-flag choice: **the thing a rollback needs, and the thing the
guard was added to protect.** Obeying the guard caused the loss it exists to prevent.

## The general shape

A write guard has two questions available to it and they are not the same question:

- *Is this field present / non-empty?*
- *Does this write CHANGE what is stored?*

An invariant phrased as "X must never be persisted onto this row" is about the second. Re-sending
a value the row already holds persists nothing — the column ends byte-identical — so it cannot
violate such an invariant, and rejecting it only breaks the callers obliged by the schema to send
it. **A full-replace API makes every unchanged field arrive on every write.** Under PUT semantics
the two questions diverge for the entire normal traffic of the endpoint, not for an edge case.

Two sub-decisions were not free:

- **Compare against the stored value, not provenance.** `model.Connection` records no provenance
  for the account selection, so provenance was not an available answer without a schema change.
  The stored value answers the question actually being asked.
- **Compare exactly past a trim.** `internal/dispatch`'s `matchesAccount` was tempting and wrong:
  it returns `true` for an empty creation id (the newly-set case, which must be refused) and folds
  Meta's `act_` prefix (Meta's `account_id` is `Pattern`-pinned to `act_<digits>`, so treating the
  bare form as "unchanged" would admit a write that changes the column into an invalid shape). A
  helper that answers "may this credential act here" is not a helper that answers "does this write
  move the row".

## Why this is the FIFTH variant, and what the enumeration would have caught

Every round on this PR has been the same defect wearing different clothes — *a guard that fixes one
path and breaks the adjacent one*:

1. pre-cutover → post-cutover campaigns
2. the success arm → the error arm
3. paid-ads → HubSpot (a SHARED field read without asking whose it was)
4. the reported exits → the sibling exit
5. **present → changed** (this one)

Rounds 3 and 5 are the same helper, and the pattern across all five is that the fix was validated
against the case that was reported and not against the case adjacent to it.

The cheap check that would have ended this earlier is a one-line classification applied to every
guard the change adds, before writing any of them:

> **Does this guard distinguish a CHANGED value from a merely PRESENT one — and if it fires on
> presence, does a stored counterpart exist that it could have compared against?**

Applied across this PR's guards, exactly one answers badly. The Meta IGSID format check and the
DSA beneficiary/payor XOR are create-only with no stored counterpart ("N/A", correctly). The
`creds.go` provenance comparisons (`matchesAccount`, `systemCreated`) already compare against a
stored value ("YES") — the correct answer was *in the same diff* as the defect. Only
`rejectForcedSystemAccountWrite` as called from `updateConn` fired on presence while a stored
`account_id` for the same `(project, provider)` sat one `repo.Get` away.

The lesson is not "read the design file", though that would also have worked — `Required("account_id")`
on four providers is what made the blast radius total rather than partial. It is that **the
enumeration is cheap and mechanical and finds the adjacent case without needing anyone to guess
which one it is.** Enumerating four guards costs a minute; the alternative has now cost five review
rounds.

## A survivor the mutation run caught

The first shape of the fix wrote the guard's scope twice: once inside the guard, once as the gate
deciding whether to read the current row. Mutating the gate to drop `IsPaidAds()` **survived** —
it is behaviour-preserving, buying only a pointless read on HubSpot updates, so no outcome-based
test could ever see it. That is the signature of a duplicated predicate rather than a missing
test: the two copies can disagree in a way that is invisible until the day the disagreement is the
other one. Extracting `forcedSystemGuardApplies` removed the survivor by construction rather than
by writing a test that asserts how many database reads happened.

## Also pinned

The current-row read sits between `parseIfMatch` and `repo.Update`, and both boundaries are
asserted rather than assumed: a missing `If-Match` must still answer 428 without paying for a read,
and a rejected write must not reach the database. A read FAILURE returns as-is rather than
defaulting the current value to `""` — that default is the reverse of fail-closed, since it makes
every resubmission look newly set and reports a database fault as a 400 about the caller's body.
