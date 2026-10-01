# 2026-10-01 — LFXV2-2665: budget writer regains parity with its stated twin

**Fix** — the rerun of the pre-PR review found no correctness regression in the previous fix
commit, but five omissions measured against the endpoint's own declared contract. `orchestrator.go`
says `WriteCampaignBudget` is *"structured deliberately as `ToggleCampaignStatus`'s twin: same
pre-platform guards, same classification of what the returned error can mean … an operator hitting
one failure mode should not find it reported differently by the other."* Three of the toggle's
classification arms had not been carried over. All five are landed here.

## 1. Three connection faults were answered 503 with a retry invitation

The same defect in three places, and in every one of them the answer was *"the campaign budget
could not be changed on the ad platform; the campaign was not modified"* — retryable — for a
condition no retry clears.

- **`LinkedInDispatcher.WriteBudget` ran neither of its platform calls through `linkedinExpiry`
  or `res.systemScoped`.** Every other LinkedIn capability does. An expired member credential or
  a rejected application credential therefore reached the service untagged, lost to every
  `errors.Is` arm, and fell to the default — while the toggle answers the identical failure 409.
  The missing `systemScoped` additionally attributed a failure on the LF **system** row to the
  caller's project, which left both the `ErrSystemConnectionNotUsable` arm and the
  credential-attribution branch dead on this path. `linkedinConnectionDefect`'s own doc names this
  shape: *"A sentinel added there and missed here is re-tagged correctly and then never reached."*
  Here the guard was absent one level up.
- **`ErrServiceDefect` had no arm**, so it was swallowed by `ErrConnectionNotUsable` and answered
  409 "repair the connection" — for OUR defect. Its sentinel doc mandates matching it above the
  general arm wherever both can appear; toggle and metrics both comply.
- **`ErrAccountNotSelected` had no arm.** It is *always* wrapped alongside `ErrConnectionNotUsable`
  on all three budget-writing platforms, so the generic arm always won and told an operator to
  repair credentials that are fine when the real remedy is selecting an ad account.

**The ordering inside the LinkedIn dispatcher differs between its two calls, and that is the
contract rather than an inconsistency.** The current-budget read built no mutate, so there is
nothing ambiguous to protect and the defect tag is applied directly. The `PARTIAL_UPDATE` checks
`IsOutcomeUnconfirmed` FIRST, because a 401 there may still have applied — "nothing was modified"
is then the one claim that cannot be made. Nothing is lost either way:
`unconfirmedBudgetWriteError` wraps the tagged error, so the originating cause survives. A test
pins both paths, including the negative halves (a read must NOT come back unconfirmed; a mutate
must).

The service-layer tests wrap each new sentinel the way production wraps it — alongside
`ErrConnectionNotUsable` — because a bare sentinel passes even with the arm missing, there being
nothing else to claim it. And because `ErrAccountNotSelected` and `ErrConnectionNotUsable` are
both 409, a status assertion cannot see the difference at all: a second test asserts the message
names the ad account and does **not** blame credentials.

## 2. A comment in the previous fix commit contradicted its own code

`meta_budget.go` still carried the pre-split sentence — "a failed account preflight, an
unresolvable currency — are upstream and keep the 503 the default arm gives them" — two lines
above the code that maps an unresolvable currency to `ErrBudgetUnwritable`. Narrowed to the
preflight case, which is the only one of the three that is still genuinely upstream.

## 3. The published contract omitted the new 400

`design/brief.go`'s endpoint description is what reaches `gen/http/openapi*.yaml`, and its 400
enumeration listed only the amount bounds, the unknown budget type and an unwired platform. A
platform's own minimum — the whole subject of the previous fix commit — was absent, so a
generated client still read those as 503. The Go comment on the `budget` attribute mentioned it;
Go comments do not reach OpenAPI. Extended and regenerated with `make apigen`.

Refs: LFXV2-2665
