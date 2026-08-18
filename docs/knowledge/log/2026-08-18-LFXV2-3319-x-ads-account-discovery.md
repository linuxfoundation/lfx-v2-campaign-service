# 2026-08-18 — LFXV2-3319 X Ads account discovery

**Update** — `GET /projects/{id}/connection-twitter-ads/accounts` now enumerates the X Ads
accounts the connection's credential reaches, joining the Google Ads, Meta, LinkedIn and
Microsoft endpoints. X was the last provider excluded for the FIRST half alone.

## X had the second half already, and that is the load-bearing finding

The bar for a provider is two halves: a discovery endpoint, AND a create path that fails in a
way NAMING the missing choice. `internal/bootstrap/sysacct.go` listed Reddit and X together as
lacking the first. For X that was the whole story — it already had the second and had had it
all along. `validateTwitterConnection` tags an empty account id with BOTH
`ErrConnectionNotUsable` and `ErrAccountNotSelected`, and `TwitterDispatcher.Dispatch` calls
that validator ITSELF rather than validating inline, so `unusableConnectionReason` reports
`account_not_selected`.

That is the MICROSOFT shape, not the LinkedIn one, and the distinction is exactly what
stating-which-half-is-missing buys: LinkedIn's `resolveLinkedInCredentials` tags the sentinel
but `Dispatch` never calls it, so LinkedIn still needs create-path work. X needed none.
Supplying `ListAdAccounts` completed the pair with no change to the create path at all. X's
toggle and metrics paths share the same validator and answer synchronously, so unlike Meta the
naming is not log-only there. Like Microsoft, X is now eligible for
`accountDiscoveryProviders` and has deliberately NOT been added — that changes what the
bootstrap CLI accepts and belongs in its own commit.

## What X's documentation settles, and what it does not

Three claims about a sibling platform's API decided fixes on other PRs and were false until
checked, so each of these was taken from X's own reference rather than assumed:

* **Endpoint.** `GET {base}/{version}/accounts` — "a listing of advertising accounts that the
  current user has access to". The COLLECTION form of the `/accounts/{id}` resource the client
  already builds. Version stays the client's configured `12`.
* **Pagination is cursor-based, and confirmed rather than assumed.** `count` (min 1, max 1000,
  default 200) and `cursor` are the request parameters; `next_cursor` is the response field.
  Termination is documented as an EXPLICIT null: "If less than `count` entities are returned in
  the current page of the result set, the `next_cursor` value will be `null`."
* **Narrowing parameters exist and are all optional**: `account_ids`, `q` (case-insensitive
  prefix match on name), `sort_by`, `with_deleted` (default false), `with_total_count`. None
  are sent — each would silently narrow the picker, and a caller cannot tell a narrowed list
  from a complete one.

**UNVERIFIED, recorded as such:**

* **The zero-account case is not documented.** X does not say whether a credential reaching no
  accounts gets an empty collection or an error. The fail-loud option was taken: an empty,
  non-nil slice with a nil error is returned ONLY when X sent `"data":[]` together with an
  explicit `next_cursor` — a body that affirmatively says "here is the set, and it is empty".
  Every weaker body is an error, so an empty answer always means X said the set was empty,
  never that the call failed.
* **The `approval_status` enum is not published.** The reference shows only `ACCEPTED`.
  `approvalStatusLabels` is therefore an ALLOW-LIST of known-bad values, so an unrecognized or
  absent status yields `""` — not a claim the account is fine, only that this package has
  nothing to say. A deny-list would have had to guess the good values, and guessing wrong
  either hides a usable account or labels a healthy one broken. The raw value still travels to
  the caller in `Status`.

## Two absence guards, both subtler than they look

* **`"data":null` is NOT a nil `json.RawMessage`.** `encoding/json` stores the four bytes
  `null` in it, so an absent `data` and an explicit null cannot be distinguished by a nil check
  on the raw field — and `null` then unmarshals into a nil element slice and reports a healthy
  zero accounts. The first cut had exactly that bug; a test caught it. The guard now checks the
  DECODED slice for nil, which a present `[]` leaves non-nil and both absent and null leave nil.
* **An absent `next_cursor` is not exhaustion.** A plain `string` field collapses null and
  absent onto `""`, which would let a malformed page terminate the walk and report a partial
  list as complete. `apiResponse` gained a `NextCursorPresent` bit via a custom
  `UnmarshalJSON`; `NextCursor`'s type and value are unchanged, so `findByName` — the package's
  other cursor walk — is untouched, and deliberately does not consult the bit, since it already
  errors rather than reporting "not found" when it runs out of pages.

The walk is the one call that must NOT go through `doRequest`, whose path is rooted at
`accountURL()` and would ask about a single account while returning a plausible list. It uses
`doRequestAbs`, as the stats endpoint does, keeping identical OAuth1 signing, redirect policy,
bounded read and three-way error classification. `logPath` is the bare `accounts` label, never
the request URL, which carries the cursor query into strings persisted to Steps.

**Note** — The chart is the half that is easy to miss. `twitter-ads` moved from the shared
`connection-*` alternation into the discovery branch of the HTTPRoute regex AND gained its
RuleSet path; `parity_test.go` pins it from both directions, and its negative row was retargeted
onto reddit-ads. Reddit is now the shared branch's ONLY member, so that branch is one ticket
from disappearing — collapsing it early would admit `/accounts` for a provider the service does
not serve.

**Verification** — Seventeen mutations were run, each a COMPILING change. Fourteen were killed
immediately. Three are worth recording:

* **A cursor-trim mutation SURVIVED** because the fixture cursor was `"c1 c2"`, whose trimmed
  form is identical — the fixture shared the code's assumption. The fixture now uses
  `" c1 c2 "`, and the mutation dies. A cursor is a server-minted opaque token: trimming can
  request a different page, and a whitespace-only token would trim to `""` and read as
  exhaustion.
* **A filter-unusable-accounts mutation SURVIVED** at the dispatcher, because every fixture
  there returned only ACCEPTED, non-deleted rows, so dropping unusable ones removed nothing.
  A test covering under-review, rejected and deleted rows was added; the mutation now dies. The
  label test alone could not catch this — a filtered row never reaches the labeller.
* **A mutation passing the stored account id into the discovery client SURVIVES, and is left
  standing as a real gap rather than papered over.** `twitter.ListAdAccounts` never reads
  `AccountConfig`, so populating it changes nothing observable and no test CAN distinguish the
  two. The zero `AccountConfig` in the dispatcher is therefore a convention, not an enforced
  invariant. What does bind is one layer down, where the client test asserts the outbound path
  and query against a client built WITH an account id — so the moment the client starts
  consulting `AccountConfig`, that test fails. LinkedIn's discovery path carries the same shape
  and the same caveat.

One further attempt broke the BUILD (an unused `errors` import) rather than failing a test,
which proves nothing about coverage; it was redone as a change that compiles — tolerating the
WRONG sentinel — and killed three tests.
