# 2026-10-01 — LFXV2-2665: budget writer PR review fixes

**Fix** — PR #233's review round. All CI checks green, `dealako` approved with one nit, and
Copilot returned six findings. Every one was verified against the code before it was accepted;
all six are landed here. The two load-bearing ones are the same defect in two adapters: a
money-moving write trusting an identifier it had the means to check.

## 1. Meta wrote to an ad set it never established was the campaign's

The highest-value finding. Every guard on the Meta path established that the ACCOUNT was right;
none established that the AD SET was. The ad set id is read from this service's own persisted
row and the write is addressed to it directly, so a stale or corrupted `AdSetID` reaches a real,
writable ad set — and inside one account, the shared LF system account most of all, that ad set
belongs to another campaign, with every account check passing on the way there.

`GetAdSetBudget` already reports the owner, on the read this path makes anyway. It is now
checked (`ErrCampaignAccountMismatch`, refused before any mutate). An **unreported** owner is
refused on the same terms as every other unreported fact on this path: "we could not establish
that this ad set belongs to the named campaign" and "it does" are opposite facts, and only the
second justifies moving money.

## 2. Google trusted an acknowledgement it had only half-checked

`firstResourceName` extracts the TRAILING path segment. The budget mutate compared only that id,
so `customers/OTHER/campaignBudgets/555` and `customers/{ours}/adGroups/555` both carried the id
that was addressed and both passed — and the service persists the new amount on the strength of
that acknowledgement. The campaign create path has validated the whole resource name since it was
written (`validateCampaignResource` → `validateResourceKind`); the budget mutate now does too,
BEFORE the id comparison, and its failure is **unconfirmed** rather than definite because Google
accepted the request.

## 3. Meta's "no ad account selected" claimed unknown provenance

The campaign DID record its creating account — the guard above had already refused the case where
it did not — so what was missing was a selection on the CONNECTION. Joining
`ErrCampaignProvenanceUnknown` sent an operator to re-dispatch a row that was correct, and
skipped the system-origin scoping every other Meta account check goes through. Replaced with the
shared `requireMetaAccountID`, which wraps `ErrAccountNotSelected` under `ErrConnectionNotUsable`
and passes it through `res.systemScoped`.

The test that pinned the old classification was updated rather than deleted: it now asserts both
correct sentinels AND that `ErrCampaignProvenanceUnknown` is **absent**, which is the half a
status assertion cannot see — the previous and corrected answers are both 409.

## 4. LinkedIn's construction guard documented an intention it did not enforce

`UpdateCampaignBudget` is EXPORTED, and its guard against an amount that bypassed
`ValidateBudgetAmount` was a bare `ParseFloat` — which admits `NaN`, `1e2`, `100`, and a `"5.00"`
daily budget against a published $10 minimum. Those are exactly the amounts the guard exists to
stop. It now re-runs the validator and requires the canonical two-decimal wire form it returns to
equal the supplied string. A second test asserts the validator's own output round-trips through
the guard on both pacing models — a refusal test alone passes just as well for a guard that
refuses everything.

## 5. The published description promised a symmetric guarantee

`design/brief.go` said the row is written only after the platform confirms, "so a failure never
leaves this service reporting a budget the platform does not have". The forward half is true; the
converse is not, and cannot be — after the platform confirms, `ReplaceCampaign` can still fail,
which answers 500 with the platform holding the new amount and the row the old one. The code
already logs that case loudly as a divergence; only the prose overclaimed. Rewritten as the
one-way invariant it actually is, with the reconciliation path named. Regenerated with
`make apigen`.

## 6. The published floor and the runtime floor differ, and the direction is the safe one

Goa's `Minimum(0.000001)` rejects the half-open sliver `[0.0000005, 0.000001)` that the service's
own check admits — it compares against the adapter's `math.Round` rather than a literal, so it
cannot drift from it, and those values round UP to one micro. **No threshold moved.** Publishing
`0.0000005` would state a minimum that is not a whole unit in any platform's billing; publishing
one micro states the real floor and closes the sliver to HTTP callers before the handler sees it.
Nothing a generated client can send is accepted at runtime and refused by the contract — the
published contract is never the looser of the two. The design comment now says this explicitly,
which is what was actually missing.

## 7. The nit: no success log on a money-moving write

`dealako` offered the out — skip it if the twin `ToggleCampaignStatus` stays silent on success,
which it does — and it was declined on purpose. Every other arm of this endpoint logs, so the one
operation that moved money was the only one leaving no trace beside the platform-call warnings. A
status is reconstructable from the campaign's current state; an amount's history is not. The
ACTOR is deliberately not a field: it is already persisted as `UpdatedBy`, the durable queryable
place for it, and a principal identifier is a different category of data from the resource ids
these logs carry.

## 8. The sibling-parity sweep these findings prompted

Every one of findings 1–4 had the same shape: **the correct thing already existed in the repo and
was not used.** The owner field was already fetched. `validateResourceKind`, `requireMetaAccountID`
and `ValidateBudgetAmount` all already existed, and the Google create path already called the
first. None of these were platform-knowledge gaps, and a diff-scoped review cannot see them —
each new function is internally coherent; the defect is only visible against code outside the diff.

So all three `WriteBudget` paths were then read against their nearest siblings (`ToggleStatus` per
platform, `ReadSettings` for Google) and every guard, helper and already-populated response field
accounted for. Result: parity holds. Two things worth recording.

**Google carries no `rejectedBudgetAmountError` mapping where LinkedIn and Meta both do**, and
that asymmetry is a fact about the platforms. Those two enforce floors the service layer cannot
know — LinkedIn's $10/$100, Meta's one minor unit in an account currency only Meta reports — so an
amount the service accepted can still be refused by the adapter, and unmapped that refusal falls to
503 and invites a doomed retry. Google's three failure modes are the service's own bounds: the same
1e9 ceiling, the same `math.Round` comparison, and a non-finite float that cannot survive JSON
decoding. Nothing reaching that line can fail there. Now stated in the code, with the condition
under which the mapping would have to be added.

**Meta's ad-set echo was already checked** one layer down — `GetAdSetBudget` refuses an answer whose
echoed id is not the one requested. A dispatcher-level guard written for it during the sweep was
redundant and was removed; the premise is noted where the guards rely on it. The sweep caught the
same not-checking-what-exists mistake it was created to prevent, one layer before the bots would
have.

**The service-layer switch is a strict superset of the twin toggle's arms**, and the ordering
differences between them are safe: no sentinel reaching either switch matches two arms
(`ErrCredentialDecryptionFailed` and `ErrConnectionNotUsable` are alternatives at the one
construction site, never joined).

Refs: LFXV2-2665
