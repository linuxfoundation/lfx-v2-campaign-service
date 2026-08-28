# 2026-08-28 — LFXV2-2641 hubspot campaign utm lookup

**Creation** — Added `SearchCampaigns` and `CreateCampaign` to the HubSpot
client, with the routes
`GET/POST /projects/{projectId}/connection-hubspot/campaigns`, so a caller can
read back an existing HubSpot campaign's `hs_utm` token.

This closes the gap `internal/utm/resolve.go` names explicitly: the
`SourceHubSpotCampaign` provenance was reserved but never emitted because
"following the source email to its campaign needs endpoints this client does
not have." It is a DIFFERENT feature from the utm generation already in
`internal/utm` — that package tags links this service creates; these read a
token an upstream campaign already carries.

**Note** — The credential was already present. HubSpot has been a first-class
provider in the connection store with its own `hubspot_connections` table and
a `PrivateAppToken`, and the client already implemented fourteen operations.
Only the campaign object (`0-35`) was untouched.

The namespace is LF-GLOBAL and every layer says so. HubSpot campaigns are not
scoped to a project: `projectID` selects the credential, not the visibility.
A campaign created here appears for every foundation's campaign managers, so
the create path documents an operator warning rather than pretending to scope.

Three contract decisions, each pinned by a test:

- An empty result is a 200 with `[]`, never null and never an error. The
  caller branches on empty-vs-found to decide whether to offer a create, so a
  malformed 2xx is refused rather than reported as "nothing matched" — a
  broken response answering empty would prompt a duplicate in a shared
  namespace.
- A campaign with an EMPTY `hs_utm` is kept and its wire field is OMITTED
  rather than sent as "". A campaign with no token is a real campaign, and a
  consumer must be able to tell "no token" from "the producer sent an empty
  string" without guessing.
- Create performs NO duplicate check. A search-then-create inside one call
  still races a concurrent caller and cannot prevent a duplicate, so the check
  belongs with the human reading the candidate names. An id-less 2xx is an
  error: the campaign may or may not exist, so the caller must check HubSpot
  rather than retry into a second copy.

**Verification** — Nine mutations verified to fail their tests, including a
malformed 2xx read as empty, an id-less create reported as success, an
untrimmed query, and dropping a campaign whose token is empty. The Heimdall
RuleSet entry was verified to bind by removing it alone and watching
`TestRouteRuleSetParityWitnesses` fail.
