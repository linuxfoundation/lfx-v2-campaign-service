# 2026-08-18 — Force the system marketing-ops account for paid-ads dispatch

**Creation** — A single dispatch-layer flag makes the LF-owned system account
the PRIMARY credential source for every paid-ads campaign, so all six ad
platforms authenticate as the one marketing-ops account
(`marketingops_lfx@linuxfoundation.org`) regardless of any per-project
connection. Spec/plan in
[`specs/006-force-system-ads-account`](../../../specs/006-force-system-ads-account/spec.md).

The system-account scope already existed as a FALLBACK — a project with no
connection of its own dispatches through the reserved `model.SystemProjectID`
row. This change adds `LFX_FORCE_SYSTEM_ADS_ACCOUNT`
(`constants.EnvForceSystemAdsAccount`, exactly `"true"` to enable, default off),
which inverts that: when on, the project's own connection is not consulted at
all for paid ads. It is read once in `newCredsSource` from the environment,
mirroring the existing `REDDIT_METRICS_ENABLED` dispatch flag rather than
threading a field through the seven `New*Dispatcher` constructors (191 call
sites) and `Config` for a value only `credsSource` reads.

A guard at the top of `credsSource.resolve` routes to the new
`resolveForcedSystem` when the flag is on, `Provider.IsPaidAds()`, and the
request is not already at `SystemProjectID`. The three conditions are the whole
policy:

- **`IsPaidAds()` gate** — HubSpot/email is never forced, the same trade the
  fallback's `systemConn` refuses. Forcing an audience build onto the system
  HubSpot row would write a project's contacts into the LF portal, so email
  keeps the default resolution even with the flag on.
- **`SystemProjectID` short-circuit** — a request already in the reserved scope
  drops to the ordinary path; forcing it would re-issue the identical lookup and
  there is no project connection to override.
- **Unconditional** — unlike the fallback, `resolveForcedSystem` does NOT
  consult `Disconnected`. Forcing overrides a project's own choice by design, so
  a project that explicitly disconnected its account is still dispatched on the
  system account.

`resolveForcedSystem` loads the system row directly, validates and decrypts it
through the shared `resolveConn`, marks the result `fromSystem = true`, and
`systemOrigin`-tags every failure — so a missing or unusable system row FAILS
CLOSED with a not-created, system-origin error rather than falling through to
the project connection the flag means to ignore. `resolveOwned` (adoption) is
untouched.

**Note** — Generating and installing the six marketing-ops credential sets is an
OPERATIONAL task, not part of this change: each platform's OAuth tokens are
authorized by the marketing-ops identity and installed via the existing
`bootstrap-system-account` CLI. Until they are installed for a provider, turning
the flag on fails that provider's dispatch closed by construction.

Tests: `internal/dispatch/creds_force_system_test.go` covers the flag parse and
`resolve` under the flag (system row used and project row never read; disconnect
overridden; missing row fails closed with `NoUpstreamCreate` +
`ErrSystemConnectionOrigin`; unusable row keeps its origin; HubSpot never forced;
every paid-ads provider forced and no other; `SystemProjectID` short-circuit;
flag-off unchanged). The concept file
[`code/internal-dispatch.md`](../code/internal-dispatch.md) gained a
"Forced-primary mode" section beside the system-account fallback.
