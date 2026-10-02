# 2026-10-01 — LFXV2-2665: budget-writer pre-PR review fixes

**Fix** — the pre-PR review round of the budget-write branch returned one critical finding, two
important ones, and one knowledge-base match. All four are landed here.

## 1. The UNCONFIRMED arm was unreachable for the only platform that implements it

The critical one. `WriteBudget` returned the `campaignBudgets:mutate` error unwrapped, and Google
Ads carries "this may have applied" in the error's SHAPE — transport failure, 5xx, redirect — not
in an `Unconfirmed()` method. Nothing `UpdateCampaignBudget` returned satisfied the behavioural
interface, so `errors.As` in the service never matched and every ambiguous outcome fell to the
definite arm.

What that produced on a timeout (the orchestrator's own 45s budget-write timeout is enough to
reach it), a 5xx, or a dropped connection: the caller was told **"the campaign was not
modified"** — an affirmative false claim about a money-moving write — and the claim lock was
released inline instead of held through the cooldown, so the next caller could claim the same
unbumped version and write the platform again while the first outcome was unknown.

Fixed by classifying at the dispatcher boundary, the same wrap every other mutating Google Ads
path uses. Two details are deliberate. The wrap is scoped to the **mutate alone** — the settings
read changes nothing, so its failure is definite even when its status is a 5xx, and marking it
ambiguous would send an operator to verify a write that was never built. And the wrapper is a
sibling type rather than a reuse of `unconfirmedToggleError`: both satisfy the same interface,
but that one's message says "status change", and a budget write logged as a status change
misdirects whoever reads it during exactly the incident it exists for.

The gap was invisible because the service tests injected a hand-rolled unconfirmed fake and no
dispatcher test exercised a failing mutate — a fake standing in for a real shape nothing proved
the real code produced. Both halves are now pinned, including the negative cases: a definite 4xx
must NOT be classified unconfirmed, and neither must a failed settings read.

## 2. A sub-micro budget was refused three layers down, as a 503

Positive, so it cleared `budget <= 0`, but under half a micro — it rounds to zero at the
platform, where `ValidateBudgetMicros` refuses it with a bare error the service's switch can only
classify as 503. So a permanently invalid request took the write lock, cost a live settings
round-trip to Google, and came back inviting a retry that could never succeed.

The refusal now sits with the other validations, ahead of the load, the claim and the platform
read. It compares the ROUNDED value rather than a literal floor so it cannot drift from the
adapter's own `math.Round`.

## 3. A 2xx was treated as a confirmed write

`UpdateCampaignBudget` discarded the mutate response, so a success carrying an empty `results`
array counted as confirmation — while the capability's contract is that nil means the platform
APPLIED the change, which is what the service persists the new amount on. The response is now
decoded through the same `firstResourceName` the create path uses and the acknowledged id must
equal the budget addressed. A mismatch is classified **unconfirmed**, not failed: the request
reached Google and was accepted, so "nothing was modified" is the one claim that cannot be made.

## 4. The design contract admitted a value the service always rejects

Knowledge-base match `design-contract-looser-than-runtime`. Goa's `Minimum` is inclusive, so
`Minimum(0)` published an OpenAPI schema advertising `0` as valid while the service refused it
unconditionally — a generated client could learn the real floor only from a 400. Now
`Minimum(0.000001)`, one micro, which is the floor the stack actually enforces and the same
number fix 2 checks. NaN and Inf remain the only runtime rejections a Goa range cannot express.

Worth keeping: this is the *inverse* of the usual form of that pattern. The constraint was
present and looked like it covered the field; it was its inclusivity that admitted the impossible
value. A present constraint is not evidence of a correct one.

Refs: LFXV2-2665
