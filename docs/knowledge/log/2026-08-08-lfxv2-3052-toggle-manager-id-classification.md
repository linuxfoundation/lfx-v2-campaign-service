# 2026-08-08 — A validity check that only ran on one of the two paths that needed it

**Fix** — `login_customer_id`'s stored-shape check moved out of
`resolveGoogleAdsDiscoveryClient` and into `validatedLoginCustomerID`, which BOTH Google Ads
resolvers now call. A malformed manager id on the toggle path used to answer `503`; it now
answers `409`.

## The defect

`internal/dispatch/googleads.go` has two resolvers over the same connection row:

| resolver | called by |
|---|---|
| `resolveGoogleAdsClient` | campaign dispatch, status toggle, metrics reads |
| `resolveGoogleAdsDiscoveryClient` | the account-discovery endpoint |

Both read `providerConfig["login_customer_id"]` and hand it to the same
`googleads.NewClient`. Only the discovery one inspected it first.

So the same stored value, with the same defect, produced two different answers depending on
which endpoint the caller reached:

- **Discovery** — caught at the dispatch boundary, tagged
  `ErrConnectionNotUsable` + `ErrProviderConfigInvalid`, mapped to **409**. Correct: a stored
  id with dashes in it needs a human to edit the connection.
- **Toggle** — passed through uninspected. It failed later, inside the client, at
  `validateLoginCustomerID` — by which point the error is indistinguishable at the
  orchestrator's boundary from a genuine upstream failure. It fell to the service layer's
  default arm: **503, "the provider call failed, retry later"**, for a value that no amount
  of retrying will repair.

## Why the check has to be where the value is READ

The client's own `validateLoginCustomerID` still exists and still runs. It is not a
duplicate of this check and it cannot replace it: it fires inside the same call that talks to
Google, so by then the information needed to tell a bad stored row from a bad upstream
response is gone. Classifiability is a property of WHERE a check runs, not of whether one
runs at all.

The two regexps (`storedCustomerIDRE` here, `customerIDRE` in the client) must stay in step —
widen one and you must widen the other.

## The shape of the mistake, which is the reusable part

Nothing about the original inline check was wrong. It was correct, well-commented, and had a
test. It was in the wrong PLACE — inside one caller of a shared resource, rather than beside
the read of that resource. That is invisible to every gate: it builds, it lints, its test
passes, and the endpoint it does cover behaves perfectly.

The tell is structural, not behavioural: **two functions reading the same stored field, and
only one of them validating it.** Worth grepping for whenever a helper acquires a second
caller — the second caller inherits the reads but not the guards.

## Note

The 503 was correctly described as an open gap in `internal/service/orchestrator.go` when
LFXV2-2023 shipped, rather than papered over; that comment now records the gap as closed.
Writing down "this is wrong and here is why" at the point you decline to fix it is what made
this a ticket instead of a surprise.
