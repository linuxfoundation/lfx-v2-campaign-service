# 2026-08-28 — LFXV2-2641 hubspot campaign utm lookup

**Creation** — Added `SearchCampaigns` and `CreateCampaign` to the HubSpot
client, with the routes
`GET/POST /projects/{projectId}/connection-hubspot/campaigns`, so a caller can
read back an existing HubSpot campaign's `hs_utm` token.

This NARROWS, but does not close, the gap `internal/utm/resolve.go` names: the
`SourceHubSpotCampaign` provenance was reserved but never emitted because
"following the source email to its campaign needs endpoints this client does
not have." The client can now reach campaigns — by NAME, and by creating one —
but resolution there starts from a source EMAIL, and nothing reads the
association from an email to the campaign it belongs to. Searching by the
email's name would be a guess rather than a lookup: campaign and email names
need not match, and a wrong hit attributes this email's traffic to somebody
else's campaign. `SourceHubSpotCampaign` therefore remains un-emitted, and
closing it needs an association read rather than another search endpoint.
`resolve.go`'s comment was rewritten to state that narrower gap.

It is a DIFFERENT feature from the utm generation already in
`internal/utm` — that package tags links this service creates; these read a
token an upstream campaign already carries.

**Note** — The credential was already present. HubSpot has been a first-class
provider in the connection store with its own `hubspot_connections` table and
a `PrivateAppToken`, and the client already implemented fourteen operations.
Only the campaign object (`0-35`) was untouched.

The namespace is PORTAL-WIDE and every layer says so. HubSpot campaigns are
not scoped to a project or a sub-account, so every campaign in a portal is
visible to any caller holding that portal's token. `projectID` selects the
credential — and therefore WHICH portal is visible, not merely whether the
caller may look: connections are stored per project with their own token and
`portal_id`, and `credsSource` refuses the LF system fallback for HubSpot, so
two projects share the namespace only when configured against the same portal.
A campaign created here appears for everyone working in that portal, so the
create path documents an operator warning rather than pretending to scope.

(An earlier draft of this note said LF-GLOBAL / "every foundation". That
overstated it: it is true only where projects share a portal, which is common
under the LF umbrella but is not what the code guarantees.)

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

**Update** — Review found five defects in the first cut, two blocking.

`CreateCampaign` posted only `hs_name` and never asked for the properties BACK.
The CRM create endpoint returns system fields unless they are named, so the
"read back the assigned token" the doc comment promised was describing a
request that never asked for it.

That fix was WRONG, and review caught it: `?properties=` is documented on the
READ endpoints, not on the create — and the create is not even on the same
surface. HubSpot documents campaign creation at `/marketing/v3/campaigns`,
while the CRM object endpoint `0-35` has no create operation at all, so the
original code would have failed every real call. The legacy UI path running in
production uses exactly the split now shipped: CRM search, marketing create,
NO query parameters, and a follow-up search when the token is wanted.

So the create sends no `?properties=`, and an absent `hs_utm` on the response
is a documented real state rather than an error — which is what `Campaign.UTM`
already said.

Create failures were routed through the READ classifier, so a timeout or 5xx
became a retryable "campaign search could not be completed" 503. That names the
wrong operation and, far worse, invites a retry of a NON-IDEMPOTENT write into
a namespace shared by everyone on that portal — which is how a duplicate gets
made. They
are now reported as UNCONFIRMED, with a message sending the operator to HubSpot.

Three more: an id-less hit was silently dropped, turning a malformed payload
into the clean empty answer that licenses a create; a whitespace-only `q` passed
Goa's rune-counting MinLength(1) and was classified as a retryable 503 rather
than a 400; and the orchestrator forwarded a `(nil, nil)` contract violation
that the service layer would have rendered as an authoritative empty list.

The search cap was also raised from 10 to HubSpot's maximum of 200 (raised
from 100 on their side in September 2024) and
documented as a contract fact rather than a tuning detail: it is a cap with no
paging, so a campaign below it reads as absent, and absent is what prompts a
duplicate.
