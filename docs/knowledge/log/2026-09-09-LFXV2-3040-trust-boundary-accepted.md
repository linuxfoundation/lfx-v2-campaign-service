# 2026-09-09 — LFXV2-3040: cross-foundation portal visibility is an accepted trust boundary

**Note** — Misha accepted, on 2026-09-09, that contact lists built for a foundation with no HubSpot
connection of its own land in the shared LF portal and are visible to everyone operating there. This
records the decision and its scope; it changes no code.

The question was raised by David Deal in review of the change that let `credsSource.systemConn` serve
the email channel, and it is the one part of that change not settled by reading the diff.

## What was accepted

Every LF foundation shares ONE HubSpot portal under one org-wide token. Before the fallback widened,
HubSpot never resolved the LF system row, so a foundation without its own connection simply could not
build an audience. It can now, and its contact lists are created in the shared portal.

The remaining isolation is list NAMING — `Plan.listName` composes an event name with a build ref, and
it is portal-global by design. That is collision avoidance, not an access boundary, and it should
never be cited as one: `hubspot.AccountConfig` carries only an optional `PortalID`, and nothing
scopes one foundation's lists away from another's.

The shared-portal model itself is not new — paid ads already fell back to the same system row. What
changed is that contact PII from unconnected foundations now lands there instead of the build
failing.

## What bounds it

- Reachable ONLY for a foundation with no HubSpot connection of its own. One that connects its own
  portal resolves there and never touches the LF row.
- The cross-portal dispatch guard (`assertAudiencePortal`) REFUSES a send whose audience was built in
  a different portal, so a foundation that connects mid-flight gets a clean refusal rather than a
  cross-portal send. The two changes were reviewed separately and the boundary depends on both.

## Why the decision was needed before bootstrap, not before merge

Merging does not create the exposure. `bootstrap-system-account -provider hubspot` does: until the
system row exists there is nothing for the fallback to resolve, so the code ships inert with respect
to THIS question. That is why the sign-off gates the bootstrap step and not the merge.

(It does not ship inert in every respect — see
[[2026-09-09-LFXV2-3040-provenance-is-immutable]] for the separate rollout consequence, where
audiences predating migration `000032` are refused at dispatch on deploy.)

## If it is revisited

The remedy named in review is the right shape: a per-foundation portal or child account, or
list-level ACLs — not naming alone. A foundation connecting its own HubSpot portal removes itself
from this path entirely, which is also the per-project escape hatch.

Jira was unreachable from the session that recorded this (expired token), so the LFXV2-3040 entry was
added separately; this file is the in-repo record.
