# 2026-09-09 — LFXV2-3040: the portal stamp is immutable, and late refusals keep their origin

**Fix** — Three defects David found in the merged provenance work, plus its twin on the dispatch
path. The first is the one that mattered: the guarantee this feature advertises was bypassable by a
plain PATCH.

`built_in_portal_id` names the portal the EXISTING list ids were built in. `applyAudiencePatch` wrote
`platform_master_list_id` and `suppression_list_ids` straight through and never touched the stamp,
and `Validate()` checked neither against the other. A `campaign_manager` could PATCH a built row's
ids to different values while the stamp went on naming the portal the OLD ids came from.

That is not caught downstream, which is what makes it the defect rather than an untidiness:
`assertAudiencePortal` compares the stamp to the **currently resolved** portal, not to the ids. With
the connection still resolving to the original portal, the stale stamp matches, the guard PASSES, and
the send goes out against list ids no lookup ever verified — the exact state the column exists to
make impossible.

`refuseProvenanceBreakingPatch` refuses the write with `ErrAudienceProvenanceImmutable` (409). A
PATCH carries ids, not a credential, so nothing in the request can prove where the new ids live;
refusing is the only answer that does not leave the guard vouching for something it never saw. It
runs BEFORE `applyAudiencePatch`, because once the merge has happened `cur` already holds the new
value and "did this change the ids?" can no longer be asked.

Refusing beats clearing the stamp, which was the other option. Both fail closed, but clearing does it
LATER and silently — the PATCH succeeds and the operator finds out at send time. Refusing says so
while the caller still holds the request that caused it, and matches the documented model: provenance
is set at build time only.

Scoped narrowly, and the scoping is tested: only the two id-carrying fields refuse, a re-send of the
SAME id is not a change, status/summary stay patchable, and a row with no stamp is untouched — which
matters because the pre-provenance rows were deliberately not backfilled and must stay editable.

The 409's `reason` is deliberately OMITTED rather than reusing one of the enum's three members. None
describes this, and `design/connection.go` documents absence as meaningful ("present only where an
endpoint returns more than one kind of conflict"). Sending `already_exists` would publish a false
discriminator to every client switching on it; adding a fourth member is a contract change and
belongs in its own PR.

Two diagnostic fixes alongside it:

- **No connection ANYWHERE** was misrouted to the retryable 500. `noOwnConnection` wraps
  `ErrNotFound` alone, so it missed the `ErrConnectionNotUsable` arm and an operator with no HubSpot
  on either the project or the LF row was told to "retry" a condition that cannot clear itself. Its
  own arm now answers 400 and names the remedy. Kept local rather than widening `noOwnConnection` to
  carry `ErrConnectionNotUsable`, which would reach `classifyDiscoveryError` — whose 404 "connect
  your project" arm is ordered above its 400 and would start answering the wrong one.
- **A late 401/403 lost its origin.** `systemScoped` tags what RESOLUTION can see; a revoked or
  under-scoped token builds a client cleanly and is refused on the first real call. Bare-wrapped, it
  reached `audienceBuildErr` with no tag at all — so a revoked LF token told every unconnected
  foundation its own config was broken while nothing paged the operator. `buildScope` now caches the
  origin beside the client (the client holds a token, not its provenance, and `res` is long gone by
  then), and the same fix is applied to `assertAudiencePortal` on the dispatch path.

All four mutation-verified in both directions, including the false-positive one where everything is
marked system-owned.

Follows [[2026-09-09-LFXV2-3040-audience-portal-provenance]].
