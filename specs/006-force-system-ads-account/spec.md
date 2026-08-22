# Feature Specification: Force the System (Marketing-Ops) Account for Paid-Ads Dispatch

**Feature Branch**: `feat/force-system-ads-account`

**Created**: 2026-08-18

**Status**: Draft

**Input**: The existing LF-owned system-account scope (`model.SystemProjectID`,
`internal/bootstrap/sysacct.go`, and the `credsSource` fallback in
`internal/dispatch/creds.go`). Marketing Ops wants every paid-ads campaign to
authenticate as the single LF marketing-ops account (`marketingops_lfx@linuxfoundation.org`)
regardless of any per-project connection, rather than only when a project has
connected no account of its own.

## Background

The service already models one LF-owned credential set per provider under the
reserved scope `system:linuxfoundation`, installed out-of-band by the
`bootstrap-system-account` CLI and encrypted at rest exactly like a project
connection. Today `credsSource.resolve` uses that system row **only as a
fallback** — a project with its own connection dispatches on its own credentials.
This feature adds a policy that makes the system row the **primary** credential
source for all paid-ads dispatch, so the marketing-ops account is the account of
record for every campaign.

## Scope

This feature changes only the **dispatch credential-resolution path** shared by
the six paid-ads dispatchers (create / toggle-status / read-metrics / account
discovery). It is gated by a single config flag, **off by default**, enabled
per-environment (the ArgoCD overlay pattern used by the cutover flags), so the
behavior change is opt-in and reversible without a code change.

Two properties are what make "reversible" true rather than aspirational, and both
are enforced in code rather than left to operator discipline:

- The flag governs **creation and account discovery only** (FR-002; both run
  through `credsSource.resolve`). Toggle-status and read-metrics resolve the
  account the campaign was actually created under (`credsSource.resolveExisting`),
  so campaigns that pre-date the cutover stay pausable and readable while it is on.
  Forcing them onto the system account would trip each adapter's account-provenance
  guard and leave a live campaign that the service cannot stop — a state no later
  rollback repairs, because the spend has already happened.
- While the flag is on, **no ad account id can be persisted onto a project's
  connection row** (`rejectForcedSystemAccountWrite`). Discovery resolves the system
  credential while forcing is active, so every id it can offer names an LF-owned
  account; storing one would outlive the flag and leave the project's row pointing at
  an LF account it has no credentials for. Clearing a selection stays allowed.

## Requirements *(mandatory)*

- **FR-001**: Add a boolean env flag `LFX_FORCE_SYSTEM_ADS_ACCOUNT`
  (`constants.EnvForceSystemAdsAccount`), **default false** (on iff the value is
  exactly `"true"`). It is read once when `credsSource` is constructed
  (`newCredsSource`), mirroring the existing dispatch-layer `REDDIT_METRICS_ENABLED`
  flag rather than threading a new field through the seven `New*Dispatcher`
  constructors (191 call sites) and `Config`.
- **FR-002**: When the flag is **on**, `credsSource.resolve` MUST resolve
  credentials from the system scope (`model.SystemProjectID`) for any
  **paid-ads** provider, ignoring the project's own connection entirely. When the
  flag is **off**, resolution is unchanged (project connection, then the existing
  fallback).
- **FR-003**: The forced path applies **only to paid-ads providers**
  (`Provider.IsPaidAds()`). `ProviderHubSpot` (email) MUST NEVER be forced to the
  system account — forcing it would write a project's contacts into the LF portal.
  This holds even when the flag is on.
- **FR-004**: A request already scoped to `model.SystemProjectID` MUST
  short-circuit (no second identical lookup), and the forced path MUST NOT run the
  fallback's `Disconnected`/`systemConn` guards: forcing is unconditional, so a
  project that explicitly disconnected its own account is still dispatched on the
  system account.
- **FR-005**: The forced resolution MUST reuse `resolveConn` against the system
  row and mark the result `fromSystem = true`, and MUST wrap any failure with
  `systemOrigin` so error attribution points at the LF row, not the project.
- **FR-006**: When the flag is on but the system row for the provider is not
  installed (or is unusable), the dispatch MUST **fail closed** with a
  not-created, system-origin error — never silently fall through to a project
  connection.
- **FR-007**: Adoption (`credsSource.resolveOwned`) is **unchanged** — it remains
  project-scoped with no fallback and no forcing.

## Out of Scope

- **Account discovery UX under the flag.** Discovery uses the same `resolve`, so
  with the flag on it reports the *system* account's accounts for every project.
  That is the correct reflection of what dispatch will use; any UI treatment of it
  is a follow-up.
- **Adoption semantics under the flag** (see FR-007) — a follow-up if adopting
  system-created campaigns per-project is ever needed.
- **Generating and installing the six marketing-ops credential sets.** That is an
  operational task (each platform's OAuth tokens authorized by the marketing-ops
  identity, installed via `bootstrap-system-account`), not a code change.
- Any change to HubSpot/email resolution.

## Testing Notes

- Unit tests on `credsSource.resolve` (no DB — fake `connReader`): flag on + a
  paid-ads provider resolves the system row and never reads the project row; the
  system row not installed → fail-closed with `NoUpstreamCreate` + system origin;
  `ProviderHubSpot` is never forced even with the flag on; a `SystemProjectID`
  input short-circuits; flag off leaves the existing fallback behavior intact.
- A test that `newCredsSource` sets `forceSystemPaidAds` from
  `LFX_FORCE_SYSTEM_ADS_ACCOUNT` (present `"true"` → on; absent / other → off).
