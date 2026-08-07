# 2026-08-07 — LFXV2-2023: the keyword plan trusted client-reported indices and validated only one of three ids

**Update** — Review raised three hardening items and one reconciliation question on the keyword
surface plan. None were blocking; all three were places where the plan's own stated behaviour was
not enforced by anything the implementer would be made to write.

**Fix** — `PartialMutateError`'s `ConfirmedThrough`/`UnsentFrom`/`Results` were consumed straight
off the platform client with no bound check. These are a CLIENT CONTRACT, not data from Google, so
a violation is our own bug — and each way it manifests is silent in a different direction:
`len(Results) > len(pending)` panics on the index; `UnsentFrom > len(pending)` runs the
in-flight-chunk loop past the slice; and `ConfirmedThrough > UnsentFrom` makes that loop no-op, so
operations that may have committed upstream keep their zero-value state instead of being marked
UNCONFIRMED. The guard rejects all three and marks the WHOLE batch unconfirmed — the only honest
answer, since a client that cannot say how far it got has provided nothing attributable
per-operation, and the failure that produced bad indices is the kind that leaves upstream unknown.

**Fix** — The GAQL-injection section documented digits-only validation for `campaignID` alone.
`campaignID` is the one id the service reads off a stored row; `adGroupID` and `criterionID` come
from the request body and reach two interpolation sites — `AuthorizeKeywordCriteria`'s query, and
the composite resource name that tells Google which row to PAUSE or REMOVE. Both now carry an
explicit `Pattern("^[0-9]+$")` in the design AND an independent check in the platform client. Two
layers on purpose: the design constrains generated clients and the OpenAPI document, the client
check covers callers that never pass through generated validation.

**Fix** — The dispatcher's mutate path returns `outcomes, nil` for every failure, which read as a
contradiction of the endpoint's declared 503. It is not: 503 is fed by PRE-FLIGHT failures —
credential resolution, the account-mismatch guard, `AuthorizeKeywordCriteria` — all of which run
before a mutate is sent. That is now stated at the return site as an invariant PR 2 must keep,
rather than left to be re-derived from the call graph, which is what produced the review thread.
It also explains why the new boundary guard marks the batch unconfirmed instead of erroring: a
mutate had already been sent by that point.

**Note** — The fourth item, "assert zero platform requests in the account-mismatch tests", was
already in the tree under review; the same reasoning had been applied one push earlier. Two
reviewers arriving independently at the same assertion is the signal that it belongs in the
reviewer checklist, not just in this plan.
