# 2026-09-08 — LFXV2-3040: the reserved-scope fallback now serves the email channel

**Update** — `credsSource.systemConn` no longer refuses the LF system account for HubSpot, and
`bootstrap.InstallSystemCredentials` no longer refuses to install a HubSpot system row.

LFXV2-3040 added both guards on 2026-08-08, and the hazard it named was real for the topology it
assumed: `AudienceBuilder` resolves HubSpot through the same `resolve`, so an unconnected project
falling back would have written its contact lists into another tenant's CRM portal. Spending LF ad
budget on an LF-run campaign is a trade this service makes deliberately; mixing tenants' contacts
is not.

That reasoning does not describe this deployment. Every LF foundation shares **one** LF HubSpot
portal and one org-wide private app token, so there is no second tenant for a list to land in —
and the code already assumed exactly that: `internal/audience` `Plan.listName` disambiguates list
names PORTAL-GLOBALLY, by event name plus build ref, never by tenancy. The guard therefore
protected nothing while leaving the email channel unable to resolve any credential at all, since
bootstrap also refused to install the row the fallback refused to read.

What still holds, and is now pinned separately, is the rule the guard was entangled with: **only a
genuine absence falls back.** A project with its own connection is served that connection; a
project that disconnected is not overridden by the LF row.

Unchanged: `LFX_FORCE_SYSTEM_ADS_ACCOUNT` (FR-003) still gates on `IsPaidAds()`, so forcing
remains ad-account-only and cannot capture HubSpot. Fallback and forcing now reach the same row by
different routes, and the tests tell them apart by how many scopes were asked — forcing consults
the system scope alone, the fallback asks the project first.

Also in this change:

- `cmd/campaign-service` offers every valid provider in the `-provider` usage error
  (`installableProviders`, derived from `Valid()`), so the CLI names the value `api-catalog.md`
  now sends operators to.
- Migration `000031` indexes the disconnect probe on `hubspot_connections`. `000017` skipped that
  table and stated the trigger for revisiting it — "if that gate ever widens, this migration widens
  with it". It widened.
- `audienceBuildErr` gained a system-origin arm: a defect in the shared LF row names that row and
  its operator remedy, instead of telling every foundation its own configuration is broken.
- `TestHubSpot_UnusableConnectionIsTaggedOnEveryPath` covers the email channel in the
  connection-defect suite, whose `lf system fallback` half was previously unreachable for HubSpot.

**Rollout ordering.** The change is inert until `bootstrap-system-account -provider hubspot` runs:
it enables resolution of a row that does not exist yet. Install in dev, verify, then prod. A revert
while a HubSpot system row exists re-instates the gate and that row stops resolving, so a revert
should be paired with removing it.

**Between merge and install, the change is genuinely inert — verified, not assumed.** An
unconnected project keeps receiving the same `domain.ErrNotFound` it received before
("no hubspot connection configured for project X"), because the fallback's missing-row path
returns `(nil, nil)` from `systemConn` and the caller falls through to `noOwnConnection`.
`ErrSystemConnectionMissing` is NOT produced here: it is wrapped only inside `resolveForcedSystem`,
which HubSpot never enters. So merging without installing changes nothing observable, which is what
makes the ordering above safe rather than merely recommended.

That also names the one rough edge worth knowing: while the row is absent, the message tells a
foundation to connect a HubSpot portal, when the LF row is the actual missing thing.
`ErrSystemConnectionMissing` exists for exactly that shape of unfollowable advice but is not
wrapped on the fallback's miss. Marking it there would improve the message for all seven providers
and is deliberately left out of this change, which does not otherwise touch that classification.
Installing the row closes the window.
