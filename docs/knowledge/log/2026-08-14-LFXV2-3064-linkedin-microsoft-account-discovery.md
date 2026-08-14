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
path calls the DISPATCH resolver and tolerates exactly one error —
`ErrAccountNotSelected` — propagating every other sentinel intact. Duplicating
the validation would let the two drift, so a credential rejected at dispatch
could be accepted here, which makes a discovery endpoint actively misleading
rather than merely permissive. Both resolvers now return the validated credential
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

Verified by mutation: making either discovery path demand an account id again
fails `TestListAccountsWorksWithoutASelectedAccount`, and removing either chart
side fails the parity test. A first attempt at the Microsoft mutation broke the
BUILD (an unused import) rather than failing a test, which proves nothing about
coverage — it was redone as a change that compiles.
