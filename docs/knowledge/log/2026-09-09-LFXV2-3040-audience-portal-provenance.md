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

The stamp is REQUIRED, and resolving it first is what makes that affordable. `BuildAudience`
calls `AudienceBuilder.BuiltInPortalID` before `createPlanLists`, and refuses the build if it
returns empty or errors.

The first cut had it the other way — best-effort, after the lists existed, on the reasoning that
failing a build to record a field would orphan real HubSpot lists. That reasoning was right about
the cost and wrong about the remedy. A transient token-info failure or a cancelled request then
produced a `built` audience with empty provenance, which the dispatch guard above refuses forever:
a retry re-reads the stored empty value, and a rebuild mints a SECOND set of real lists beside the
first. The best-effort stamp did not avoid the orphaning, it deferred it to a path with no repair.

Resolving first inverts that. Nothing upstream exists yet, so the refusal costs nothing and the
retry is clean — the build either has provable provenance or it does not happen, which is what
lets the dispatch guard stay strict. The refusal releases its build claim like every other
pre-upstream exit (`releaseUnstartedClaim`); leaking it would wedge the brief against the very
retry the refusal promises. It carries its own `errPortalUnconfirmed` sentinel so `audienceBuildErr`
does not label it "failed upstream" — nothing was sent to HubSpot, and that message would send an
operator to check a platform this path never contacted.

It reads the portal the token authenticates against rather than the operator-supplied `portal_id`
config, which a credential swap leaves stale — the same choice the campaigns path makes.

The public POST/PATCH still accept `status=built` without a portal, and that is deliberate: the
API has no `built_in_portal_id` field for a caller to supply, so enforcing it in
`CampaignAudience.Validate()` would reject every API-created built audience with no way to comply —
removing a documented capability (`TestAudienceService_Create_PreservesExplicitStatus`) rather than
tightening one. Such a row is refused at dispatch by the no-portal arm above, which is the
fail-closed answer; giving the API a way to record verified provenance is a separate change.

`built_in_portal_id` is appended LAST in `audienceCols` rather than placed beside
`platform_master_list_id` where it belongs by meaning: `scanAudience` reads positionally, so a
mid-list insert would shift every later column into the wrong destination — a defect that
compiles, runs, and surfaces as a status parsed from a timestamp.
`TestAudienceCols_ColumnOrderMatchesScanAudience` caught exactly that during this change.

Not exposed in the Goa design: this is provenance an operator reads, not a field a caller sets, so
the published contract is unchanged.

Stacked on the branch for [[2026-09-08-LFXV2-3040-hubspot-system-fallback]], which is what makes
the hazard reachable.
