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

## Draft rows are returned, not filtered

Draft emails come back with `state` on each row, mirroring the Meta account picker's handling of
disabled accounts. Dropping them would answer "your portal has no such email" about an email
sitting right there, and send someone looking for a permissions problem that does not exist. The
caller gets the state and decides.

ARCHIVED is NOT among them, and the first version of this section said otherwise — see "a
promised field that never arrives" below for how that was found and why the Meta analogy does not
transfer. The correction is folded in here rather than left to contradict itself further down;
the round below records what was learned.

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

## Round 3: a promised field that never arrives

The contract advertised `state` as "e.g. DRAFT, PUBLISHED, ARCHIVED" and the dispatcher comment
said archived rows are returned so a picker can warn before cloning one. Both were wrong, in two
different ways, and the wrongness was invisible from inside this change.

**`state` was never requested.** `SearchEmails` restricts the response with
`includedProperties`, and the list contained `name`, `subject`, `updatedAt` — not `state`.
`Email.State` therefore decodes to `""` on every row. The field existed on the struct, the model,
the design and the api-catalog row; only the wire request was missing it, so nothing failed and
the unit test passed because the MOCK supplied a value the real client never would. Requesting it
is the fix.

**ARCHIVED was never a value it could hold.** HubSpot models archival as a separate `archived`
boolean rather than a lifecycle state, and the walk does not ask for archived rows — so they are
absent from the result entirely. A `state` value cannot express an absence, and the Meta-account
analogy that motivated the promise ("known-bad rows are returned with the reason in the label")
does not transfer: Meta returns the disabled account, HubSpot omits the archived email.

Corrected in five places, because the claim had been restated in each: the design attribute
description, the model comment, the dispatcher comment, `docs/api-catalog.md`, and this bundle's
concept file. Restating a fact in five places is how a wrong one survives a review of any single
place.

## Round 4: the fix for the promised field was not pinned

Requesting `state` fixed the behaviour and pinned nothing. `TestSearchEmails_SortsMostRecentlyUpdatedFirst`
asserted only that `includedProperties` was NON-EMPTY, and the service-level test injected `State`
through a mock dispatcher that never sees a wire request — so deleting `"state"` from the query
would have recreated the exact bug with both tests green.

Two assertions now, because requesting a property and mapping it are separate failures with the
same symptom: the request test names `state` explicitly, and the fixture rows carry a `state`
value that is asserted after decoding. Revert-verified — dropping the property fails with
`includedProperties = [name subject updatedAt], want it to contain "state"`.

The general shape is worth keeping: a fix whose only test runs above the layer that broke cannot
bind. The mock that made the service test pass was the same mock that hid the bug for a round.

## Round 4: the cold-start 503 named the wrong operation

`resolveBackendWithOrch` hard-coded "account discovery service is unavailable" because account
discovery was its only caller. An email search hitting the pre-wiring window was therefore told
about an operation it never attempted — and a 503 is read by someone deciding whether to retry,
so naming the wrong operation sends them to check the wrong subsystem. It now takes the operation
name, exactly as `classifyDiscoveryError` already did one layer up.

That is the second place the same omission surfaced. Sharing a helper across two operations means
every caller-facing STRING inside it becomes a parameter, not just the branching logic — and the
strings are the part that gets missed, because they do not fail a compile.

## Round 5: a duplicated security guard with no test

`rejectSystemScope` is the first statement of `listAccounts`, and
`TestListAccounts_RejectsTheReservedSystemScope` covers both discovery endpoints through it —
its comment says so, and gives the reason: the guard is written once, so a third provider cannot
forget it.

Email search does not go through that helper. It calls `rejectSystemScope` itself, which made the
test's premise false the moment this endpoint landed: the guard was now DUPLICATED and the copy
had no test. A duplicated security check is the kind that is added without a test and later
removed without one failing, and this one gates a GET that would decrypt the LF system credential
and list the Linux Foundation's own marketing emails — subjects included — for whoever asked.

The case went into the existing table rather than beside the email tests, so the next endpoint
that copies the guard is added where the other copies already are. Revert-verified: deleting the
guard fails the hubspot subtest with "got 503, want the system-scope rejection", and the other two
still pass — which is what proves the case is specific to this endpoint rather than re-testing the
shared helper.

## Round 6: the log fragment is not the knowledge update

Every round so far appended to THIS file and called the bundle updated. It was not.
`CLAUDE.md` requires the relevant CONCEPTS to change alongside code and Helm, and three were
still describing the world before this endpoint:

- `docs/knowledge/code/internal-service.md` said the cold-start guard always emits "account
  discovery service is unavailable" — the exact string round 4 parameterized.
- `docs/knowledge/kubernetes/ruleset.md` called `/accounts` "today the only provider-specific
  `connection-*` sub-path", which `/emails` falsified.
- `docs/knowledge/kubernetes/httproute.md` described two alternation branches where there are
  now three, and framed the rule as "add each further provider to the DISCOVERY branch" — which
  is wrong advice for a provider whose extra sub-path is not `/accounts`. Branches group by
  SHAPE, not by recency.

The pattern is worth naming because it recurs: a dated log fragment is easy to write and feels
like the documentation step, but it is the CHANGELOG. The concepts are what a reader consults,
and they are the ones that go stale silently — `okfvalidate` checks frontmatter and the dated H1,
not whether a concept still describes the code. Nothing failed for six rounds.

Also corrected a paragraph in `internal-dispatch.md` that contradicted itself inside four lines:
it stated archived rows are absent, then said the picker can warn before cloning an archived
email. The second clause survived from the version written before round 3 established the first.

## Round 7: "the same timeout applies" hid what it was bounding

`SearchEmails` reuses `accountsCallTimeout`, and the comment justified it as "one bounded upstream
read on a request path" — copied from `ReadAccounts`, where it is true. It is not true here.
`Client.SearchEmails` follows `paging.next.after` and can issue up to `maxListPages` (200)
SEQUENTIAL HubSpot requests, so the 20s bounds a whole paginated walk.

Sharing the constant is still right, and for the reason the old comment gave badly: what the
deadline protects is a request path where the page cannot render until this answers, so the
ceiling has to be a human's patience rather than the walk's natural length. What was wrong was
describing the work, which matters because the consequence follows from it — a portal large
enough to need many pages hits the deadline MID-WALK and answers 503 rather than returning a
short list.

That is the correct direction and worth stating rather than eliding: a silently truncated picker
is worse than an error, because the caller cannot distinguish a template that is missing from one
that is merely past the cut. But it means the practical page ceiling is whatever fits in 20s,
well under 200 — so `maxListPages` is a runaway backstop here, not the operative bound. Recorded
in `docs/api-catalog.md` too, since it is the caller-visible half.

The reusable shape: a comment copied along with the code it justifies keeps the ORIGINAL's
reasoning. Reusing `accountsCallTimeout` was a decision; reusing its rationale was not.
