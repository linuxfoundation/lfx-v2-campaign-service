# 2026-09-08 — LFXV2-3040: a late 401/403 says which row's token was refused

**Fix** — `SearchCampaigns` and `CreateCampaign` now carry the credential's ORIGIN on a permission
failure, so an expired or under-scoped LF token is not reported to every fallback foundation as
their own configuration fault.

`credsSource.resolve` already tags the defects it finds itself, and `res.systemScoped` is how they
reach the caller marked system-owned. A permission failure is different in kind: resolution returns
CLEANLY, and the 401/403 only appears on the first real platform call. Both of these methods used
the narrow `resolveHubSpotClient` wrapper, which discards the `resolved` — so by the time the status
was visible there was nothing left to attribute it to.

This was unreachable until `credsSource.systemConn` stopped refusing the email channel. Before it,
a HubSpot call always ran on the project's own connection, so "the project's token" was true by
construction. Now a project with no connection of its own runs on the shared LF row, and one bad LF
token produces the same message in as many places as there are fallback projects — each telling an
operator to check a connection they do not have, while the one person who can repair it hears from
nobody.

The two paths carry it by DIFFERENT mechanisms, and the difference is not cosmetic:

- `SearchCampaigns` emits `ErrConnectionNotUsable`, which is exactly what `systemScoped` is gated
  on, so `res.systemScoped(...)` upgrades it to `ErrSystemConnectionNotUsable` and it lands in
  `classifyDiscoveryError`'s existing system arm — a 500 plus an ERROR log naming the operator,
  ordered ABOVE the generic `ErrConnectionNotUsable` 400.
- `CreateCampaign` emits the platform-rejection taxonomy (`ErrPlatformPermission` joined with
  `ErrPlatformRejected`). `systemScoped` is gated against that and passes it through UNCHANGED —
  verified with a probe, not assumed, after a first attempt wrapped it in `systemScoped` and shipped
  a no-op with a comment claiming otherwise. It joins `domain.ErrSystemConnectionOrigin` directly
  instead, via the new nil-safe `resolved.isFromSystem`.

The origin is ADDITIVE in both cases: every existing `errors.Is` on the original sentinel keeps its
answer, because a consumer that switches on `ErrPlatformRejected` must not stop seeing this error
just because the token came from elsewhere. `TestHubSpot_LatePermissionFailureCarriesTheCredentialOrigin`
asserts the original tag in the system-owned row precisely to catch a "fix" that REPLACES rather
than adds — mutation-verified in both directions, including the false-positive one where every
failure is marked system-owned.

The service-side message splits to match, in `ConnectionService`'s create arm: the project-owned
wording ("check that the connection's private app token…") is kept, and the system-owned case says
the shared LF connection was used and that this is an operator fault. Naming the right system is
the whole point of the tag; leaving one message for both would have made the sentinel invisible
where a human actually reads it.

Sibling knowledge surfaces corrected in the same pass, all of which still described the pre-fallback
world: `internal-bootstrap.md`'s frontmatter and opening (ad-account-only, with the index bullet
updated verbatim to match), its tri-state paragraph (which listed only the discovery providers and
never mentioned that the email channel is exempt outright rather than admitted to that map),
`cmd-campaign-service.md`'s description of the subcommand, `InstallSystemCredentials`'s own doc
comment, and `internal-audience.md`'s claim that `audienceBuildErr` always says "failed upstream".

Stacked on [[2026-09-08-LFXV2-3040-hubspot-system-fallback]], which is what makes all of this
reachable.
