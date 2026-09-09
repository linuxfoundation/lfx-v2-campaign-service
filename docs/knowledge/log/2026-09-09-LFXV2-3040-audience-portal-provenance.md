# 2026-09-09 — LFXV2-3040: an audience records which portal built it

**Update** — `campaign_audiences` gained `built_in_portal_id` (migration `000032`), stamped at
build time from the TOKEN's own portal, and `HubSpotDispatcher.Dispatch` now refuses a send whose
audience cannot be proven to belong to the portal it authenticates against.

`campaigns` already recorded its creating tenant, and the HubSpot dispatcher already verified it
for campaigns (`hubSpotCreationPortalID` plus the mismatch guard). Audiences recorded nothing —
so a row holding HubSpot list ids, which are bare numerics meaningless outside their portal, could
not say what its own ids referred to.

That gap was unreachable while `credsSource.systemConn` refused the email channel: a build and the
dispatch consuming it always resolved the same project connection. Lifting that guard made it
reachable. A project with no HubSpot connection builds against the LF portal, connects its own
portal, and dispatch — which resolves credentials afresh and prefers the new connection — clones
the email THERE while `SetSendList` receives ids from the LF portal. HubSpot answers about ids it
cannot see: a partial send, or a hard failure neither row explains.

Two refusals, mirroring the campaign-side split:

- **No portal recorded** → `ErrCampaignProvenanceUnknown`. Every audience built before this column
  existed is in this state, and it is deliberately NOT backfilled: inventing a value would assert
  provenance nobody verified. The remedy is a rebuild, because there is no portal to reconnect to.
- **A different portal recorded** → `ErrCampaignAccountMismatch`, naming both portals so an
  operator can see which way the connection moved.

Both refuse BEFORE any HubSpot mutation, so the dispatch claim is released and nothing is created.
The guard sits AFTER the master/suppression pre-flight, which is pure local validation — putting a
network call ahead of it made `TestHubSpot_MasterInSuppressionRefusedBeforeClone` fail, correctly.

The stamp is best-effort by contract (`AudienceBuilder.BuiltInPortalID` returns `("", nil)` on a
failed lookup). It runs after the lists already exist upstream, so failing a build to record a
field would orphan real HubSpot lists. It reads the portal the token authenticates against rather
than the operator-supplied `portal_id` config, which a credential swap leaves stale — the same
choice the campaigns path makes.

`built_in_portal_id` is appended LAST in `audienceCols` rather than placed beside
`platform_master_list_id` where it belongs by meaning: `scanAudience` reads positionally, so a
mid-list insert would shift every later column into the wrong destination — a defect that
compiles, runs, and surfaces as a status parsed from a timestamp.
`TestAudienceCols_ColumnOrderMatchesScanAudience` caught exactly that during this change.

Not exposed in the Goa design: this is provenance an operator reads, not a field a caller sets, so
the published contract is unchanged.

Stacked on the branch for [[2026-09-08-LFXV2-3040-hubspot-system-fallback]], which is what makes
the hazard reachable.
