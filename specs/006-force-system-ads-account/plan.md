# Implementation Plan: Force the System (Marketing-Ops) Account for Paid-Ads Dispatch

**Spec**: [`spec.md`](./spec.md) · **Branch**: `feat/force-system-ads-account`

## Design

One policy on the shared credential resolver flips every paid-ads platform at
once, but it must apply to **creation and account discovery only** — never to an
operation on a campaign that already exists.

Every paid-ads dispatcher constructs its `credsSource` via
`newCredsSource(repo, enc)`. Creation and discovery call `credsSource.resolve(ctx,
projectID, provider)`, and adding a `forceSystemPaidAds bool` to `credsSource`
with a guard at the top of `resolve` covers all six with no per-dispatcher logic:

```go
func (s *credsSource) resolve(ctx, projectID, provider) (*resolved, error) {
    if s.forceSystemPaidAds && provider.IsPaidAds() && projectID != model.SystemProjectID {
        return s.resolveForcedSystem(ctx, provider)
    }
    // ...unchanged...
}
```

Toggle-status and read-metrics do **not** go through `resolve`. They call
`credsSource.resolveExisting(ctx, projectID, provider, creationAccountID)`, which
consults the flag nowhere and instead resolves the account the campaign RECORDS
having been created under (`metaCreationAccountID` and its four siblings). Forcing
one of those operations is unrecoverable in both directions: a pre-cutover campaign
forced onto the system account, or a cutover-created campaign resolved back to the
project's, trips the adapter's account-provenance guard and leaves a live campaign
the service cannot stop. Keying on the recorded account handles both without a flag
test, and keeps cutover-created campaigns pausable after the flag is retired.

When neither scope resolves — or the project resolves a different account and the
system row is broken — the error reported keys on that same recorded account
(`systemCreated`), not on which resolution happened to fail. A system-created
campaign surfaces the operator-owned system fault; anything else, including
provenance that cannot be established, keeps the project's own actionable error.
Reporting the wrong one sends the wrong operator to repair a healthy row.

`resolveForcedSystem` loads the system row directly (`Get(SystemProjectID,
provider)` → `resolveConn`), stamps `fromSystem = true`, and wraps failures in
`systemOrigin`. It deliberately does **not** call `systemConn` (the fallback's
`Disconnected`/`IsPaidAds` guards are fallback semantics; forcing is
unconditional). A missing/unusable system row fails closed as a `notCreated`
system-origin error — never a fall-through to the project connection.

`forceSystemPaidAds` is read once in `newCredsSource` from
`LFX_FORCE_SYSTEM_ADS_ACCOUNT`, mirroring the existing `REDDIT_METRICS_ENABLED`
dispatch flag. This avoids changing the seven `New*Dispatcher` signatures (191
call sites) and keeps the policy where the only consumer lives. The flag is off by
default; per-env enablement is an ArgoCD overlay flip, like the cutover flags.
HubSpot is excluded structurally by `IsPaidAds()`. Adoption (`resolveOwned`) is
untouched.

## Commits

- **A1 — spec + plan** (this commit). No code.
- **A2 — env flag + resolver force path.** `EnvForceSystemAdsAccount =
  "LFX_FORCE_SYSTEM_ADS_ACCOUNT"` in `pkg/constants`; `forceSystemPaidAds bool` on
  `credsSource`, read once in `newCredsSource`; the `resolve` guard +
  `resolveForcedSystem`. No constructor or container change (the six paid-ads
  dispatchers inherit the policy through their shared `newCredsSource`).
- **A3 — resolver tests.** Table-driven `resolve` tests per FR-002..FR-006
  (system used; not-installed fail-closed; HubSpot never forced; SystemProjectID
  short-circuit; flag-off unchanged) + a `newCredsSource` env-parse test.
- **A4 — OKF docs.** Update `docs/knowledge/code/internal-dispatch.md`
  credential-resolution section (add the forced-primary mode beside the fallback);
  extend frontmatter description + mirror verbatim into `code/index.md` if it
  changes; new `docs/knowledge/log/2026-08-18-force-system-ads-account.md`
  (**Creation**). `go run ./cmd/okfvalidate ./docs/knowledge`.

## Review

Review follows the repository's current review guidance and configuration in
`CLAUDE.md`; this plan carries no review lifecycle instructions of its own.
No push / PR without explicit authorization.

## Verification

`make test` (full suite, `-race`) green; `go vet`; `gofmt`; `okfvalidate`
conformant. No `gen/**` hand-edits (this change adds no API surface, so no
`make apigen`).
