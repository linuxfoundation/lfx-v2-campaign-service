# 2026-10-27 — the API contract no longer says Google Ads failures are unclassified

**Docs** — `PlatformResult.error`'s description in `design/brief.go` ended with "Google Ads
dispatch failures are not yet classified beyond the connection classes." That stopped being
true when `domain.UpstreamCreateOutcomeCarrier` landed (`7058e3be`, #373): a retained
google-ads failure that `googleads.CreateOutcome` classified now reaches the operator as one of
five fixed sentences naming what is in the ad account, not as the generic could-not-be-confirmed
line. Refs #377.

**Why it mattered beyond tidiness.** This description is not a code comment — Goa publishes it
into `gen/http/openapi*.yaml|json`, the `kodata` copies the image embeds, and the Go doc comment
on `PlatformResult.Error`. A client author reading the contract was told the richest sentences
the service can send do not exist, and the enumeration of classes they could expect was missing
five of them. Of the two directions a stale contract can be wrong, this was the worse one: it
asserted an absence rather than merely omitting a presence.

**What the description now says.** The class list is unchanged, and the final sentence is
replaced by one naming the google-ads states — a budget of the composed name already exists and
nothing new was created; a campaign of that name already exists; a budget was created or reused
and the campaign was then refused or left unconfirmed; the campaign was created and its setup
could not be confirmed — followed by the property that makes them safe to publish: each is a
fixed sentence chosen by a service-authored token and never built from it, so #208's rule that
no platform text reaches the operator holds for them too. `create_unconfirmed` is deliberately
not listed: it has no sentence of its own and falls through to the could-not-be-confirmed
wording already described.

**Scope.** Description text only. Regenerated with `make apigen`; the diff is the one string in
`design/brief.go` and its copies in `gen/**` and `cmd/campaign-service/kodata/gen/**`. No
behaviour, no schema, no field changes — `PlatformResult.error` is still an optional string.

**The rest of #377 was already done.** Both of its root causes landed in #373: `*apiError`
carries `UpstreamStatus()` (`client.go:439`), so the status arm of `DispatchFailureMessage` is
reachable for google-ads; and all five of the issue's partial-state rows map to a token in
`CreateOutcome`. Its third ask — that the message point at a place the operator can actually
look for a non-shared, unattached budget — is in the `budget_name_taken` sentence, which names
the campaign that carries it or the platform's change history. This description was the one
piece of the issue still outstanding.
