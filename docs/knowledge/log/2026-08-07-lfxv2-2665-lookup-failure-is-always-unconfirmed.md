# 2026-08-07 — LFXV2-2665: every failed campaign-name lookup is UNCONFIRMED

**Update** — `CreateCampaign`'s name-lookup failure path no longer branches on
`createOutcomeAmbiguous`. All lookup errors now return the name-carrying partial result marked
UNCONFIRMED, and the returned error joins `errLookupAmbiguous` so the classification survives for
`IsOutcomeUnconfirmed`.

**Fix** — Review found that a pre-send dial error or a definite 4xx on the lookup GET returned a
bare `(nil, err)`. The gate was answering the wrong question. `createOutcomeAmbiguous` asks *could
this request have created something* — and a GET creates nothing, so on the lookup path it reduces
to *was the transport ambiguous*. The lookup is not there to report what it created. It is there to
establish that the campaign NAME IS ABSENT, so a prior ambiguous attempt can be adopted rather than
duplicated. A dial error establishes nothing about absence. A 4xx establishes nothing about absence.
Both leave the question exactly as open as a timeout does.

The consequence is asymmetric, which is what settles it. `(nil, err)` makes `IsOutcomeUnconfirmed`
false, so the dispatcher records a clean failure, releases the retained partial, and the next
dispatch POSTs the same deterministic name into an account where Meta enforces no name uniqueness —
a duplicate PAID campaign. Over-reporting UNCONFIRMED costs one look in Ads Manager.

The cancelled-context branch immediately above had already reached this conclusion and was written
as a special case. It was not a special case; it was the general rule found early. The two branches
now differ only in step wording.

`TestCreateCampaignLookup4xxIsStillUnconfirmed` replaces
`TestCreateCampaignLookup4xxReturnsCleanFailure`, which pinned the defect. It asserts the partial is
returned, that it carries the campaign name, and — separately — that `createOutcomeAmbiguous(err)`
is true, because the message alone does not steer the dispatcher.

**Fix** — Three comments and one durable log still described an automatic next-run reconciliation:
"the next run's `findCampaignByName` / `findAdSetByName` either adopts the node Meta did commit or
creates it once." Nothing in `internal/dispatch` sets `ReconcileByName`, so that does not happen —
an UNCONFIRMED create is resolved by a person in Ads Manager. The sites in `doCreate`, the two
create-failure branches, the throttle test, and
`2026-08-07-lfxv2-2665-throttle-retry-vs-ambiguity.md` now say so and name the gate. A previous pass
recorded this as fixed while the first and most-read paragraph still carried it.

**Fix** — `TestCreateCampaign_ThrottledNameLookupIsUnconfirmed` incremented a plain `int` from the
`httptest` handler goroutine and read it from the test goroutine. Returning from `CreateCampaign` is
not a happens-before edge for that variable, so `-race` can report it. It is an `atomic.Int64` now,
matching the neighbouring call-count tests.
