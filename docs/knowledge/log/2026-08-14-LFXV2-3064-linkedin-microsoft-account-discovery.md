# 2026-08-14 — LFXV2-3064 account discovery for LinkedIn and Microsoft

**Update** — `GET /projects/{id}/connection-{linkedin-ads,microsoft-ads}/accounts`
now enumerates the ad accounts each connection's credential reaches, joining the
Google Ads and Meta endpoints that already existed.

The platform clients needed no work: `linkedin.ListAdAccounts` and
`microsoft.ListAdAccounts` were already built and tested, including pagination,
de-duplication and the empty-answer case. Only Meta's dispatcher was wired to the
service layer, so both were dead code reachable from nothing.

The load-bearing part is what discovery does NOT require. Both dispatchers'
credential resolvers hard-fail on a missing account id — correctly, because a
client constructed without one cannot reach the platform at all — but discovery
exists to answer *which account should this connection use?*, so demanding one
makes the endpoint reachable only by connections that no longer need it. The
account-less connection it is meant to rescue is exactly the one it would refuse.
Meta's discovery path already drew that distinction; both new paths follow it.

Rather than duplicate the status / decode / completeness checks, each discovery
path calls a SHARED resolver and tolerates exactly one error —
`ErrAccountNotSelected` — propagating every other sentinel intact. Duplicating
the validation would let the paths drift, so a credential rejected on one could
be accepted here, which makes a discovery endpoint actively misleading rather
than merely permissive.

**How much that shares differs by provider, and the difference is load-bearing.**
Microsoft's `validateMicrosoftConnection` is called by `Dispatch` ITSELF, so
discovery and create genuinely cannot diverge. LinkedIn's
`resolveLinkedInCredentials` is shared by TOGGLE, METRICS and discovery —
`Dispatch` validates inline and does not call it. So for LinkedIn the invariant
is narrower than "cannot drift", and it has already drifted once in practice:
`Dispatch` sent an untrimmed access token while the resolver trimmed it, so a
padded credential listed accounts successfully and failed on create. That is
fixed by a regression test rather than by shared code, which is the weaker
guarantee and should be recorded as such. Both resolvers now return the validated credential
ALONGSIDE the account-not-selected error so the discovery path can reuse it; every
dispatch caller checks `err` first and cannot observe the value.

**Note** — The chart is the half that is easy to miss: a route with no matching
Heimdall rule is denied at the gateway, and a rule with no route is unreachable.
Both new paths are added to the HTTPRoute regex AND the `project-api` rule list,
and `parity_test.go` pins them from both directions. The two rows asserting
LinkedIn had no discovery were retargeted onto reddit-ads and twitter-ads, whose
clients still have no `ListAdAccounts` — that is what keeps the discovery branch
from being collapsed back into the shared alternation before those platforms can
answer.

**Fix** — Review found three defects in the above, all in the first cut.

Microsoft discovery passed the connection's stored `customer_id` into the client.
That looked like harmless scoping and was not: `discoveryCustomerIDs` treats a
configured customer as the COMPLETE answer and returns early without enumerating
any other, so an ordinary configured connection listed only that one customer's
accounts while the endpoint's own description promises every customer the
credential reaches. The endpoint contradicted itself for exactly the connections
most likely to use it. `AccountConfig` is now fully zero.

Both labels discarded the information that says whether an account can be USED.
The clients carry purpose-built renderings — LinkedIn's `StatusLabel`,
`ServingHolds`, `Test` and `Currency`; Microsoft's `StatusLabel`, `PauseLabel`
and `Usable` — and the first cut rendered a bare name. An account that is ACTIVE
but on `BILLING_HOLD`, a TEST account that never serves, or one the credential
can only READ all looked exactly as selectable as a writable active account, with
the refusal arriving later at dispatch and no way back to the list.

The tests reached the REAL LinkedIn and Microsoft endpoints and treated any
network failure except `ErrAccountNotSelected` as success — so an unrelated
preflight error passed, and CI depended on DNS. They now run against `httptest`
servers and assert the OUTBOUND REQUEST, which is the only thing that can catch a
discovery client scoped to one account: a scoped client still returns a plausible
non-empty list.

Verified by mutation: making either discovery path demand an account id again
fails `TestListAccountsWorksWithoutASelectedAccount`, and removing either chart
side fails the parity test. A first attempt at the Microsoft mutation broke the
BUILD (an unused import) rather than failing a test, which proves nothing about
coverage — it was redone as a change that compiles.

The customer-scoping test needed the same correction twice over. Its first
assertion checked for a `CustomerAccountId` request header and PASSED with the
bug reintroduced: the scoped path does not send a differently-headed request, it
skips the `User/Query` call entirely. Asserting on that call's ABSENCE is what
binds. A test that cannot fail is worse than no test, because it reads as
coverage.
