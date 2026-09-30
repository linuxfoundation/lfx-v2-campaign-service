# 2026-10-01 — LFXV2-2665: LinkedIn/Meta budget-writer review fixes

**Fix** — the pre-PR review round of the LinkedIn and Meta budget slices returned two important
findings, two minor ones, and two knowledge-base matches. All are landed here, in one commit.

## 1. A platform's own minimum was answered 503, not 400

The load-bearing one. The service layer validates only what is true for EVERY platform at once —
finite, `> 0`, `<= 1e9`, `>= half a micro` — and deliberately holds no per-platform floor, because
a floor that belongs to one platform does not belong in a layer that serves seven. But both new
adapters have one: LinkedIn refuses below `$10` daily / `$100` lifetime, Meta below one minor unit
of the account currency. Those refusals came back as bare errors, so the dispatcher switch could
only fall to its default arm — **503, "unconfirmed upstream", with a retry invitation, for a
request that can never succeed**. Google needed this least and that is why it went unnoticed: its
adapter's floor is the one the service already mirrors, so its validator is unreachable through
this endpoint.

Fixed with the pattern the repo already had rather than a new one. `unconfirmedBudgetWriteError`
carries `Unconfirmed() bool` and the service detects it with `errors.As` on an anonymous
interface; `rejectedBudgetAmountError` now carries `BudgetAmountReason() string` and is detected
the same way. Each adapter's validator wraps a package-local `ErrBudgetAmountInvalid` and exposes
`BudgetAmountReason(err)`; the dispatcher maps exactly those to the new
`domain.ErrBudgetAmountRejected`, which the service answers **400** with the adapter's own
sentence appended.

Two details are deliberate:

- **The sentinel is attached by an `Unwrap` method, not by `%w` in the message.** `%w` would
  append the sentinel's text to every message, which is fine until the message is the thing being
  shown to a client — and here it is.
- **The rendered chain never reaches the client.** `safeErrSummary` strips non-graphic runes and
  bounds length; it does **not** redact. Only the validator's own sentence is interpolated, and a
  service test asserts the 400 does not contain `write linkedin campaign budget`.

## 2. Meta's currency failure was about to be blamed on the caller's amount

`ResolveBudgetMinorUnits` can fail three ways and they are not the same fault. Classifying the
whole call as "amount rejected" would have told an operator to change a number that was perfectly
valid. The three are now separated: a refused amount → `ErrBudgetAmountRejected` (400); an ad
account whose currency has no known minor-unit scale → the new
`meta.ErrAccountCurrencyUnresolvable`, mapped to `ErrBudgetUnwritable` (409), because the remedy
is in Meta Ads Manager or in this service's currency map; a failed account preflight → the default
503, since nothing about the currency was established at all.

The dispatch table gained a `wantNotErr` field to pin this: the currency case must **not** be
classified `ErrBudgetAmountRejected`. An assertion that a refusal carries the right sentinel is
only half the claim.

`ResolveBudgetMinorUnits`' texts are rewritten rather than reused from `resolveCurrencyOffset`.
Those are written for the CREATE path, where an explicit `AccountConfig.CurrencyOffset` is a real
remedy. **This path has no such fallback** — its client is built from the connection row alone and
the offset is carried on neither the row nor the campaign — so a project whose account uses an
unmapped currency can be created with an explicit offset and never edited here. That is the
fail-closed side of the trade: a budget encoded at the wrong scale is off by a factor of a
hundred.

## 3. LinkedIn's empty-currency escape hatch was aimed at a case another guard already covers

The currency guard sat ABOVE the pacing guard and had to permit an empty `currencyCode`, on the
grounds that a campaign with no budget yet reports none. But the currency is read off whichever
budget field the platform reported, so empty had two meanings, and only one of them was harmless.
The other — a campaign that HAS a budget whose currency LinkedIn did not report — went through,
and the write then sends `"USD"` unconditionally over an amount denominated in something the
platform declined to name.

The guard moved BELOW the pacing guard, and **that order is now itself the guard**: by then the
"NEITHER budget present" refusal has already removed the harmless case, so the only empty currency
left is the dangerous one and it is refused (`ErrBudgetUnwritable`). A dispatch case pins it,
asserting both the sentinel and that the platform received no write.

## 4. Two knowledge-base matches

- **Captured-variable races in `httptest` handlers.** Three test sites read variables a handler
  goroutine wrote, with no synchronisation — clean today only because `Client.Do` happens to
  return after the handler. Each now writes under a mutex and snapshots under the same mutex before
  asserting. The assertions are untouched: the handler-side names changed and the snapshot keeps
  the original name.
- **A doc claim stated more than the code guarantees.** `UpdateCampaignBudget`'s godoc said the
  service "never reports a budget the platform does not have". The dispatcher confirms the platform
  ACCEPTED the write; it does not re-read what the platform then holds, and the two can differ by
  less than the platform's smallest settable unit. Qualified rather than widened — widening
  `BudgetWriter` to return a readback would put a second round-trip on every write to close a
  sub-unit gap the settings readback already exists to surface.

## Documentation

`ErrBudgetWriteUnsupported`'s godoc still named Google as the only implementer, and
`docs/api-catalog.md` still described one budget model. Both now name all three and state that the
refusal list is the UNION of the three models. `ErrBudgetShared`'s godoc says plainly that the
sentinel is **Google-only and deliberately so**: LinkedIn's budget is a pair of fields on the
campaign, so there is nothing to share and the guard has no subject, while Meta's analogue exists
but is a different shape — Campaign Budget Optimization — and is refused with `ErrBudgetUnwritable`
instead.

Refs: LFXV2-2665
