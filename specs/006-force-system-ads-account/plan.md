# Implementation Plan: Force the System (Marketing-Ops) Account for Paid-Ads Dispatch

**Spec**: [`spec.md`](./spec.md) · **Branch**: `feat/force-system-ads-account`

## Design

One policy on the shared credential resolver flips every paid-ads platform at
once. Every paid-ads dispatcher constructs its `credsSource` via
`newCredsSource(repo, enc)` and calls `credsSource.resolve(ctx, projectID,
provider)` on create / toggle / metrics / discovery. Adding a `forceSystemPaidAds
bool` to `credsSource` and a guard at the top of `resolve` therefore covers all
six with no per-dispatcher logic:

```go
func (s *credsSource) resolve(ctx, projectID, provider) (*resolved, error) {
    if s.forceSystemPaidAds && provider.IsPaidAds() && projectID != model.SystemProjectID {
        return s.resolveForcedSystem(ctx, provider)
    }
    // ...unchanged...
}
```

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

Run `/lfx-skills:lfx-local-review` after each signed commit (Claude fallback trio
if Pi is unavailable); fix findings as `fix(<scope>):` commits and rerun the full
trio. No push / PR without explicit authorization.

## Verification

`make test` (full suite, `-race`) green; `go vet`; `gofmt`; `okfvalidate`
conformant. No `gen/**` hand-edits (this change adds no API surface, so no
`make apigen`).
