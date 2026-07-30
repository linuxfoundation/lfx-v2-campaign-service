# Context and cancellation

Both patterns exist because every client in this repo bounds each attempt with
its **own** `context.WithTimeout` in addition to an `http.Client.Timeout`. That
makes the standard context sentinels ambiguous: they fire for the client's own
per-attempt deadline while the caller's context is still perfectly alive.

---

## caller-cancel-gate-uses-ctx-err

**Severity:** `high`.

**Detect:** A gate that decides "did the *caller* cancel?" and implements it as
`errors.Is(err, context.Canceled)` or `errors.Is(err, context.DeadlineExceeded)`.
The repo's gate is `ctx.Err() != nil`, tested on the caller's context. Flag the
sentinel form wherever the decision it drives is caller-cancellation: aborting a
flow, classifying a failure as clean, or converting a non-fatal per-item failure
into a fatal one.

**Why it matters:** the sentinel also matches the client's own 30-second
per-attempt deadline. An ordinary per-creative timeout is then misread as a
caller abort, so a documented non-fatal variant failure becomes a fatal campaign
abort — or, in the other direction, a real caller cancellation is swallowed and
`CreateCampaign` returns a validation error instead of `context.Canceled`.

**Evidence:**

- [`r3563548449`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3563548449)
  (PR #20) is the clearest statement: "`errors.Is(verr,
  context.DeadlineExceeded)` also matches the client's own 30-second
  `http.Client.Timeout` while the caller context is still live. That turns an
  ordinary per-creative timeout into a fatal campaign abort, contradicting the
  documented non-fatal per-variant behavior. Use `ctx.Err()` for …". Fixed in
  `7e11bdb`.
- [`r3563548438`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3563548438)
  (PR #20) — the mirror-image defect: a caller cancellation swallowed as a
  warning, so the caller's `context.Canceled` is lost. Same fix `7e11bdb`.
- [`r3563383941`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3563383941)
  (PR #20) — a cancellation during a variant request treated as an ordinary
  non-fatal ad failure, so `CreateCampaign` "can return a successful result after
  its context has been canceled". Fixed in `85aa542`.
- Developer fixing commits on merged PRs: `6e2fb0c2e` and then `92ad879d8`
  ("distinguish caller-cancel from client HTTP timeout") on **#20** — note the
  second commit fixes the first attempt at the fix; `d73ae70` ("ctx-cancel during
  /ads is fatal") on **#21**; `c639374c1` ("gate lookup abort on caller ctx") and
  `1d1920967` ("align OKF knowledge with the ctx.Err() abort gate") on the
  Microsoft client work.

**Status on main:** held across the clients — `ctx.Err()` gates appear in
googleads, hubspot, reddit, meta, linkedin and twitter, and the orchestrator uses
`gctx.Err() != nil` to distinguish "dispatch cancelled" from "queue timed out".
`docs/knowledge/code/internal-platform-googleads.md:146` records the pattern, and
the most explicit prose statement is in the Microsoft client's concept doc,
`docs/knowledge/code/internal-platform-microsoft.md`: "`(nil, err)` abort ONLY
when the CALLER's context is done — the gate is `ctx.Err() != nil`, not
`errors.Is(err, context.DeadlineExceeded)`", with the reason spelled out —
"Because the client wraps each attempt in its own `context.WithTimeout`, a
per-attempt `DeadlineExceeded` can surface while the caller's context is still
live."

**Not a finding when:** the sentinel is used for something other than a
caller-cancellation decision — for example distinguishing a deadline from a dial
error when classifying whether a request was sent, or a test asserting the
sentinel it deliberately injected. `refreshToken` returning `ctx.Err()` directly
is the correct form, not a violation.

---

## presend-clean-inflight-unconfirmed

**Severity:** `critical` when the in-flight half is wrong on a mutating create;
`high` otherwise.

**Detect:** For a mutating request, both halves must be present.

*Pre-send:* a context error observed **before** `Do` is a clean, definite
not-created failure — nothing was sent, so the claim may be released and
`(nil, err)` is correct.

*In-flight:* a context error observed **during** an in-flight mutating `Do` stays
**UNCONFIRMED** — it must produce a partial result and retain the claim.

Flag code that collapses the two: a single `if ctx.Err() != nil { return nil, err
}` wrapped around the whole attempt, or a `Do` error on a mutating POST treated as
a definite failure. Also flag a definite-failure classification that discards a
known HTTP status on a mutating POST, since a 5xx can still follow a committed
create.

**Why it matters:** collapsing in-flight into "definitely failed" makes a retry
duplicate a paid campaign. Collapsing pre-send into "unconfirmed" is the opposite
error and is also real: it tells an operator to reconcile something that was never
created.

**Evidence:**

- [`r3582327667`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/22#discussion_r3582327667)
  (PR #22): "`http.Client.Do` errors are not necessarily pre-send failures: a
  timeout or EOF can occur after LinkedIn accepted a POST. Collapsing that into an
  ordinary error makes every create caller report a definite failure, so a retry
  can duplicate the group/campaign and especially dark posts/creatives". Fixed in
  `3db43ef`.
- [`r3583213833`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/22#discussion_r3583213833)
  (PR #22) — the status-discarding variant: "For a mutating POST, a 5xx response
  can still follow a committed create; if its body read fails,
  `createOutcomeAmbiguous` will now return false and a retry can duplicate the
  resource."
- Developer fixing commits on merged PRs: `2f2fdb0e5` ("preserve
  partial+UNCONFIRMED on in-flight cancel") on **#22**; `b24cc05` ("clean pre-send
  ctx failure") on **#33**; `0dd5ccf9f` ("pre-send guard on already-cancelled ctx
  (not unconfirmed)") on **#35**; and the reddit transport-error narrowing merged
  as `8d22492` on **#27**.

**Status on main:** the classification is a named, cross-client contract —
`preSendError`, `transportError` and `apiError` with the shared predicates
`isPreSendDialError`, `isMutatingMethod` and `createOutcomeAmbiguous`.
`docs/knowledge/code/internal-platform-reddit.md:45-50` records the deliberate
boundary, including that **no** TLS error is ever classified pre-send.

**Not a finding when:** the request is a read. A GET is never UNCONFIRMED — a
malformed read is a plain, safely retryable error. Do not extend the pre-send
class to TLS errors; that is settled in the opposite direction.
