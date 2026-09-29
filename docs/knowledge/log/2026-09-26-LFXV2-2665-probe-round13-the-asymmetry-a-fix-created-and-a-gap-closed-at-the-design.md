# 2026-09-26 — LFXV2-2665: the asymmetry round 12 created, and a gap finally closed at the design

**Fix** — Round 13 of the connection-probe work. Three findings taken, one declined, and one
earlier decline reversed.

## Round 12's fix left LinkedIn behind

The mid-test-delete arm landed in `testConnUpstream`, which is the path for six of the seven test
endpoints. LinkedIn is the seventh and does not route through it: its second read of the row
happens behind `Orchestrator.VerifyAccountOrg`. So the endpoint that had the same race kept
answering **200** — "connection found, but linkedin ads verification could not be completed" —
while the other six had started answering 404.

That is worse than the original defect in one respect: before round 12 the seven endpoints were
consistently wrong, and after it they disagreed about the same row on the same race. The arm is
now mirrored in `TestLinkedinAds`, and `TestTestLinkedinAds_DeletedMidTestIs404` sits beside its
`testConnUpstream` twin. It was confirmed to fail against the pre-fix switch first; it reported
the 200 verbatim.

The lesson worth keeping is not about this race. A fix written into a shared helper silently
scopes itself to that helper's callers, and the one provider that has its own path is exactly the
one a "fixed everywhere" claim will miss.

## Microsoft's dispatch choke point held one id to half the rule

`validateAccountIDs` guards the values that become the `CustomerAccountId` and `CustomerId`
request headers. It held `AccountID` to the identity validator and `CustomerID` to the raw
digits-only regexp alone — so `0`, `007` and a value above `MaxInt64` reached the header while the
API, the bootstrap installer and the connection probe all refused them.

Both rules now run on both ids, there and in `doCustomerRequest`, and the review's own wording
would have been a regression: replacing the raw regexp with `ValidateCustomerID` alone drops the
header-safety guarantee, because the validators `TrimSpace` before parsing while the headers are
set from the RAW stored value. `"\n123"` passes the identity rule and must be caught by the
anchored regexp; `007` passes the regexp and must be caught by the identity rule. Neither
subsumes the other, and tightening the account half the same way closed a latent whitespace hole
this branch itself introduced. Seven subtests across the two new request-boundary tests were
confirmed to fail beforehand, including that one.

## Reversed: the Microsoft `account_id` int64 range gap

Declined in round 11 and again in round 12 on the grounds that closing it in the create/update
service path needs a shared non-platform validator, because `internal/service` does not import
`internal/platform/*`. Round 13 came back through `repo_learnings` against the knowledge base's
`design-contract-looser-than-runtime` pattern — and with a fix that has no layering problem at
all, because it is in the design rather than the service.

The design's `Pattern` still cannot express an int64 RANGE. It does not need to: it can express
LENGTH, and eighteen digits is the widest length every value of which is a valid int64. Both ids
are now `MaxLength(18)` with `^[1-9][0-9]{0,17}$`, so the design's rule is a SUBSET of the
runtime's instead of overlapping it, and the API can no longer store on an ACTIVE connection a
value every probe and dispatch deterministically rejects.

The cost is a 19-digit id at or below `MaxInt64` — which both runtime validators accept and the
pattern now refuses. Microsoft account ids are seven to nine digits, so that range names nothing
real, and the two apivalidation subtests that used to assert the gap now assert its closure and
the new deliberate mismatch, so nobody widens the pattern back on the grounds that it rejects
something the platform layer allows. The runtime validators stay: a pattern binds the HTTP
transport, and bootstrap, migrations and pre-pattern rows never meet it.

Three consecutive rounds raising the same gap is its own finding. Two of those rounds were spent
restating why the fix I had in mind was out of scope, when the question worth asking was whether a
different layer could take it — which is what the knowledge-base pattern match supplied.

## Declined: LinkedIn's `nil` verdict reporting `OK: true`

Review asked that a `nil` from `VerifyAccountOrgReference` stop meaning success, on the grounds
that the method returns `nil` when the account carries no comparable organization reference, so
"no organization check occurred".

It did occur. The walk returns `nil` only after `found == true` — the page carrying the configured
account was reached, and `!found` is a confirmed failure one branch above. So `nil` means the
configured account WAS found among the accounts this credential can see, which is exactly the
check the other six providers apply and exactly the conjunction `ok` reports. What is absent is
LinkedIn's EXTRA cross-check, which no other provider has at all.

Answering `OK: false` there would report a working connection as broken permanently, with no
remedy an operator could apply: a person-scoped ad account has no organization reference to supply
and never will, and it dispatches fine. That is the inverse of the defect this branch exists to
fix, and worse, because a false red has no path to green. The reasoning is now in the code at the
`nil` arm so the next round finds it before re-raising it.
