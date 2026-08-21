# 2026-08-21 — LinkedIn connection test now verifies the org/account pairing upstream

**Creation** — `CreateLinkedinAds`/`UpdateLinkedinAds` persist a caller-supplied `org_id` with
no upstream check at write time, so a manually mistyped org id was previously undetectable
until it silently broke a campaign create. LinkedIn's `adAccounts` API carries an optional
`reference` field (`urn:li:organization:{id}` or `urn:li:person:{id}`) identifying which
organization actually sponsors an ad account, decoded but previously unused by this package.

`AdAccount.OrgID` and `Client.VerifyAccountOrgReference` (`internal/platform/linkedin/accounts.go`)
use that field to cross-check a connection's stored `org_id` against LinkedIn's own record,
walking the existing `ListAdAccounts` enumeration (there is no single-resource `GET
/adAccounts/{id}` — `doRequest`'s GET path requires an `elements` envelope). It fails closed
only on a CONFIRMED disagreement (`account.OrgID != configuredOrgID`); the account not
appearing in the walk, an empty or person-scoped reference, or a missing configured org id are
all inconclusive (`nil`), while an enumeration/transport failure still propagates as an error —
a failure to check is not the same outcome as checking and finding nothing wrong.

A new optional dispatcher interface, `OrgReferenceVerifier` (`internal/service/orchestrator.go`),
and `Orchestrator.VerifyAccountOrg` wire this through the same type-asserted optional-capability
pattern as `StatusToggler`/`MetricsReader`/`AccountLister`/`CampaignAdopter` — but it is the one
capability where absence is a deliberate silent no-op rather than an error, since only LinkedIn
has an upstream signal to check. `LinkedInDispatcher.VerifyAccountOrg`
(`internal/dispatch/linkedin.go`) is the only implementation, resolving credentials the same way
`Dispatch` does (needs both `accountID` and `org_id`).

`ConnectionService.TestLinkedinAds` (`internal/service/connection.go`) is the only caller: it
runs the shared `testConn` baseline first and short-circuits on failure, then calls
`VerifyAccountOrg` and folds any error — confirmed mismatch or verification failure alike — into
an ordinary `ConnectionTestResult{OK: false}` rather than a 5xx, since a connection test failing
is an expected outcome, not a service outage. `CreateCampaign` and every other LinkedIn path,
and the shared `testConn` baseline used by the other 5 platforms (LFXV2-2556), are untouched.

See `docs/knowledge/code/internal-platform-linkedin.md` ("Org/account reference verification")
and `docs/knowledge/code/internal-service.md` ("LinkedIn org/account pairing verification") for
the full mechanics.
