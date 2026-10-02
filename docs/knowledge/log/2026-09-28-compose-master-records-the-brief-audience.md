# 2026-09-28 — compose-master records the brief's audience

**Update** — `compose-audience-master` gained an optional `brief_id`. When it is present the
service records the composed master as that brief's built audience in the same call, and the
response carries `recorded: true` plus a slim `audience` object (`id`, `status`, `version`,
`platform_master_list_id`). Without it the endpoint is unchanged in every observable way — no
portal lookup, no database write, `recorded: false` — so the exploratory use of the builder
([[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]]) pays nothing for the new path.

The change exists because the audience builder composed real HubSpot lists that nothing
consumed. The HubSpot dispatcher resolves recipients from the brief's newest `campaign_audiences`
row, which only `POST .../audiences/build` ever wrote, so an operator could assemble an audience
by hand and still have the email sent to a list the service derived on its own.

Recording had to happen **inside compose**, not through a follow-up `create-audience`:

- `assertAudiencePortal` refuses to dispatch an audience with no recorded `built_in_portal_id`,
  and `audienceFromInput` never stamps one — so a composed row created through the existing
  endpoint is permanently undispatchable.
- `refuseProvenanceBreakingPatch` (409 `audience_provenance_immutable`) then blocks repairing it.
- The portal must be read from the **same build-scoped client** that creates the lists
  (`BeginBuild` + `cachedClient`). Resolved from a second credential it would vouch for list ids
  it never saw — provenance that is plausible rather than true, which is worse than none. This is
  the same portal-provenance concern as
  [[2026-09-22-wizard-cross-turn-portal-provenance]], one layer down.

So `ComposeMaster` resolves the portal after `cachedClient` and **before the first create**, and
returns it on `ComposeOutcome.PortalID`; a failure there is an ordinary error because nothing has
been created yet. `ComposeInput.RecordUnderBriefID` only asks for that resolution — the
orchestration still records nothing itself.

Ordering in the service layer follows from compose being non-idempotent. Everything that can
refuse a recording compose is checked before the orchestration is entered: a missing audience or
brief repository is a typed `503` (never a silent downgrade to composing-unattached, which would
hand back a master the operator believes is wired to the send), and an unknown brief is a `404`.
Both leave **zero** HubSpot lists behind. The brief read needed its own error mapping rather than
`audienceExploreErr`, where `domain.ErrNotFound` means "no usable HubSpot connection" — a mistyped
brief id reported as a connection outage sends the operator to reconnect HubSpot over something no
reconnection can fix.

The insert uses plain `CreateAudience`, not `CreateAudienceForApprovedBrief`: the approval gate
exists to stop a build from *creating* platform state, and recording a pointer to lists that
already exist creates none. The partial unique index from migration 000018 is partial on
`status='building'`, so inserting a `built` row never collides with a build lease, and
`built_in_portal_id` already exists (migration 000032) — no migration was needed.

An insert failure after the lists exist is reported as a **fifth** `ComposePartial` shape,
carrying a confirmed `master`. It is a partial rather than a 500 because a 500 invites the retry
that mints a second master list for the same send; the message tells the operator to attach the
existing list by hand and not to compose again. One bounded retry precedes it, since the
expensive, irreversible half has already succeeded and a transient database blip should not cost
a duplicate contact list.
