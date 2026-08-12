# 2026-08-12 — HubSpot email search, and why it is not account discovery

**Update** — `internal/dispatch/hubspot.go`, `internal/service/{connection,orchestrator}.go`,
`design/connection.go` (LFXV2-3197). Adds `GET /projects/{project_id}/connection-hubspot/emails`,
exposing `Client.SearchEmails` so a caller can choose the marketing email an email campaign will
clone.

## The gap was one layer, not the feature

`Client.SearchEmails` already existed and was already the right shape — name-or-subject match,
case-insensitive, most-recently-updated first, following `paging.next.after` across every page so
a match beyond the first is not missed, bounded by `maxListPages`. `HubSpotDispatcher.Dispatch`
already staged emails. What did not exist was a route: nothing in `design/` reached the client
method, so `hubspotConfig.SourceEmailID` — required, no default — had no way to be chosen, and the
email channel could not be driven from a UI at all.

Worth stating because the feature LOOKED unbuilt from the outside. The Campaigns page renders
"Email campaigns are coming soon", which reads as an unimplemented channel rather than an
implemented one missing a single endpoint.

## Not modelled as account discovery, on purpose

The obvious move is to reuse `AccountLister`. It is wrong, and the reason is not stylistic.

A HubSpot connection is already scoped to the portal its private-app token authenticates against
(`AuthenticatedPortalID`, read from the token rather than from the optional operator-supplied
`portal_id` a credential swap leaves stale). So "which account may this credential act as?" has no
answer to look up. The choice that DOES exist is which email to clone, and it has a different
lifetime: an account id is stored on the connection row, a source email id travels per campaign in
the dispatch config. `MarketingEmail` is therefore its own model type. Sharing `AccessibleAccount`
would have made two unrelated things look interchangeable to the next reader, which is the kind of
saving that costs more later than it returns now.

`EmailSearcher` sits beside `AccountLister`, `StatusToggler` and `MetricsReader` as a fourth
optional capability, discovered by type assertion. `ErrEmailSearchUnsupported` is a separate
sentinel from `ErrAccountsUnsupported` for the same reason the interfaces are separate: the
capabilities are independent in both directions — HubSpot searches emails and has no ad accounts,
every ad platform is the reverse — so one sentinel would leave "this platform cannot do X"
ambiguous about which X.

## The status mapping is shared by extraction, not by copy

`listAccounts` carries a switch encoding several judgements that are easy to get subtly wrong:
404 rather than 503 for a connection that does not exist, a 500 that LOGS but never echoes a
decryption failure, 400 rather than 503 for a defect no amount of waiting fixes. Its own doc
comment says a second copy is where one of them quietly diverges, and that every provider gaining
discovery gets all of those arms or none.

That applies to this endpoint even though it is not discovery: a HubSpot connection can be
missing, unusable as configured, undecryptable, or the platform can be down, and every one of
those answers should be identical. So the switch was lifted into `classifyDiscoveryError`
UNCHANGED and both callers share it. The only thing parameterized is the operation noun
(`accountDiscovery.operation`, empty meaning "account discovery") — because telling a caller who
searched for an email template that "account discovery could not be completed" describes an
operation they did not perform.

The extraction is behaviour-preserving by construction: the switch body was moved verbatim, with
`return nil, X` rewritten to `return X`, and the existing discovery tests pass unchanged. The new
tests exercise the same arms through the new endpoint anyway — a mapping shared by reference is
still a mapping this endpoint could be wired away from.

## Known-bad rows are returned, not filtered

Archived and draft emails come back with `state` on each row, mirroring the Meta account picker's
handling of disabled accounts. Dropping them would answer "your portal has no such email" about an
email sitting right there, and send someone looking for a permissions problem that does not exist.
The caller gets the state and decides — including warning before cloning something archived, which
it cannot do about a row it never receives.

`(nil, nil)` from a searcher is rejected as a contract violation rather than reported as an empty
portal, and the empty result is built with `make(..., 0, n)` so it marshals as `[]` and not `null`.
Both are the same property: an empty picker must mean "the portal authoritatively has nothing",
never "something fell through a branch".

## Round 2: the gateway did not know the path existed

Two findings, both about a claim rather than the mechanism.

**The endpoint was unreachable.** A Goa route is not a routed path: the HTTPRoute regex and the
Heimdall RuleSet each have to admit it, and neither did. The service would have answered
correctly and no caller could have arrived. `parity_test.go` pins both sides against each other,
so the gap was one revert away from being provable — reverting the regex now fails with
`PARITY VIOLATION ... HTTPRoute match=false but RuleSet match=true`.

HubSpot gets its OWN branch in the alternation rather than joining the discovery one. The
comment there already explains why google-ads and meta-ads are spelled out separately — folding
`/accounts` into the shared branch would rule it for providers that do not serve it — and the
same argument runs in both directions here: `/emails` for google-ads is as wrong as `/accounts`
for hubspot. Two negative parity rows pin exactly that, because a widened alternation passes
every positive test.

**The doc comment named the wrong status.** It said the tagged defects "answer 409 rather than
503", copied from the campaign endpoints, while this function's caller is a CONNECTIONS endpoint
and `classifyDiscoveryError` maps `ErrConnectionNotUsable` to 400. The tests already asserted
400, so the code was right and only the prose was wrong — which is the more dangerous ordering,
since nothing fails. A status is chosen by the caller, not by the sentinel, and a comment that
names one the function cannot produce sends an integrator to handle a case that never arrives.
