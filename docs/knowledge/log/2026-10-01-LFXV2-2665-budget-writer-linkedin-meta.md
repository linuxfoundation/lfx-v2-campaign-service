# 2026-10-01 — LFXV2-2665 budget writer: LinkedIn and Meta slices

**Update** — `update-campaign-budget` now supports three platforms, not one. The
`BudgetWriter` capability gained `LinkedInDispatcher.WriteBudget` and
`MetaDispatcher.WriteBudget` alongside the Google Ads implementation that landed
earlier today, and the published API contract was corrected to match.

## Why each slice's guard set differs

`BudgetWriter` is an OPTIONAL type-asserted capability and the service layer holds no
platform allowlist, so adding a platform is purely additive — adapter plus dispatcher
method, nothing else. What is NOT shared is the refusal set: each platform's budget
model decides which guards even have a subject.

- **Google Ads.** The budget is a SEPARATE RESOURCE, so budget-id resolution, the
  `explicitly_shared` guard and period→type mapping all exist, and amounts are micros.
- **LinkedIn.** The budget is a pair of FIELDS ON THE CAMPAIGN (`dailyBudget` /
  `totalBudget`, `{amount, currencyCode}` with a two-decimal string amount). A campaign
  id fully addresses its budget, and **there is nothing to share** — the shared-budget
  refusal has no analogue here and is deliberately absent rather than forgotten. In its
  place is a CURRENCY guard: minimums are USD-specific and the client only ever SENDS
  `currencyCode "USD"`, so a campaign denominated in anything else would be silently
  redenominated by writing over it. An EMPTY currency is permitted — that is the shape a
  campaign with no budget yet returns.
- **Meta.** The budget is on the AD SET, in the account currency's MINOR UNITS. A row
  recording no ad set is `ErrCampaignNotProvisioned`. **Campaign Budget Optimization is
  Meta's form of Google's shared budget**: the campaign holds one amount and distributes
  it across every ad set beneath it, so an ad-set write there either fails or converts
  the campaign off CBO, and in both cases changes spend this request never named.

`GetAdSetBudget` therefore fetches the ad set's budget fields **and the parent
campaign's in ONE request** via field expansion. Two separate calls would let a CBO
budget appear between them and slip past the guard that exists to catch it. CBO is also
checked BEFORE pacing: an ad set under a CBO campaign has no budget fields of its own, so
a pacing answer there would explain the wrong thing.

## Create and edit share one definition of a valid budget

The same extraction the Google slice made (`ValidateBudgetMicros`) was made on both new
platforms, so an amount this service would refuse to CREATE with cannot be reached by
EDITING a live campaign:

- `linkedin.ValidateBudgetAmount` — now called by the create path as well. Its minimums
  (`$10` daily, `$100` lifetime) are checked against the ROUNDED value, which is
  load-bearing: `9.999` is SENT as `"10.00"` and therefore meets the $10 minimum, where
  checking the raw float would refuse an amount the platform accepts. The wire string it
  returns is what is sent verbatim — nothing downstream re-formats the float.
- `meta.resolveCurrencyOffset` and `meta.budgetToMinorUnits` — lifted out of
  `CreateCampaign` with their rules and error texts intact. The account currency stays
  authoritative and a conflicting explicit `AccountConfig.CurrencyOffset` stays REJECTED.

`budgetToMinorUnits` fixed a latent hole while it was being extracted: the inline code
relied on `scaled >= float64(math.MaxInt64)` to catch bad values, and **NaN fails every
ordered comparison**, so a NaN budget reached the `int64` conversion. It is now rejected
explicitly. The create path's behaviour is otherwise byte-for-byte unchanged.

## Provenance is stricter than the sibling paths, deliberately

Both new dispatchers enforce the account-identity invariant more strictly than the shared
helper they still call:

- `verifyLinkedInAccountMatch` returns `nil` when the campaign records no creating
  account — the pre-existing-row case `ToggleStatus` and `ReadMetrics` tolerate.
- `verifyMetaAccountMatch` returns `nil` when EITHER the recorded account OR the current
  one is absent, because Meta's toggle and metrics address the campaign node by id and
  need no account at all.

Both tolerances are correct for those callers and wrong for a budget write, whose contract
requires the invariant to be enforced at least as strictly as `ReadSettings` enforces it.
So each dispatcher refuses the absence(s) ITSELF, before calling the helper, which is then
still used for the mismatch case so the wording stays common across adapters. Meta's
current account id is additionally load-bearing for a reason no other Meta path has: the
account's CURRENCY decides the minor-unit scale, and with no account there is no scale
that could be assumed without risking a budget encoded 100× wrong.

**Reusing the helper alone would have silently inherited a contract these paths cannot
accept** — which is how a shared helper reopens the bug it was written to prevent.

## One deliberate asymmetry with the create path

Meta's budget write does NOT gate on `account_status`, though `CreateCampaign` does.
Lowering the budget of an account under review is precisely the action that reduces
exposure; refusing it would leave the spend running. The reasoning is written into the
code at the call site so it does not read as an omission.

## Published contract corrected

`design/brief.go` asserted two things that became false with these slices, and both were
fixed in the design and regenerated with `make apigen` (never by editing `gen/**`):

- The endpoint description said **"Google Ads only today"**. It now names all three and
  states that the refusal list is the UNION of the three models — a platform whose model
  has no analogue of a given refusal simply never raises it.
- The `budget` attribute's comment claimed one micro is the true floor because *"every
  supported platform bills in micros"*. It is now stated as the LOOSEST floor any
  supported platform has (Google in micros, LinkedIn in whole cents, Meta in its account
  currency's minor unit) — the only floor the contract can state for every platform at
  once, with each adapter enforcing its own stricter one.

## Tests

Four new files, all green: `internal/platform/linkedin/budget_update_test.go` (43
subtests), `internal/platform/meta/budget_update_test.go` (57),
`internal/dispatch/linkedin_budget_test.go` (17) and
`internal/dispatch/meta_budget_test.go` (20). Each dispatch file opens with a
`var _ service.BudgetWriter = (*…Dispatcher)(nil)` compile-time assertion — the capability
is reached by TYPE ASSERTION, so a signature drift would otherwise downgrade the
dispatcher to "budget writes unsupported" rather than failing the build — and asserts on
every refusal that the platform received **no write at all**, since a refusal that mutated
anything tells the caller nothing changed while the platform was changed.
