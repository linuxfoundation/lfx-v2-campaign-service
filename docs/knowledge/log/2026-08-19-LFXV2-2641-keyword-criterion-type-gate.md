# 2026-08-19 — LFXV2-2641 keyword actions gate on criterion TYPE

**Fix** — six suppressed reviewer findings on the keyword/audience PR, none previously
triaged, all reproduced at head `9ec15be2` before being fixed.

The serious one: `ApplyKeywordActions` is a SPEND-REDUCTION endpoint that could WIDEN
spend. The ownership loop before the atomic mutate checked one thing —

    if a.AdGroupID != adGroupID { ... refuse }

— and an ad group holds much more than its positive keywords. This client itself creates
`userList` criteria in the same ad group (`targeting.go`, GA-4), and NEGATIVE keywords
share the `adGroupCriteria` resource family and the identical `adGroupId~criterionId`
handle. So a caller could PAUSE or REMOVE an EXCLUSION through this endpoint. Removing a
negative keyword or an audience exclusion widens what serves and what is spent — the
opposite of the endpoint's guarantee — and `REMOVE` is irreversible.

The read side already had this right, which is what made the mutate path an asymmetry
rather than an oversight: `GetKeywordPerformance` selects `FROM keyword_view`, Google's
type-scoped resource, and its comment says "Only ad-group criteria of type KEYWORD are
returned". The fix reuses that mechanism instead of inventing a second one —
`resolveKeywordCriteria` queries the same view before the mutate is built, selecting
`ad_group_criterion.negative` on top because `keyword_view` carries both polarities. That
makes the guard's claim true by construction: if the keywords endpoint would hand a
criterion back, this endpoint accepts it.

Unresolvable ids FAIL CLOSED, deliberately, on the asymmetry the dispatcher's provenance
guard already rests on. Wrongly refusing a real keyword costs a re-read; wrongly admitting
an unresolved criterion risks an irreversible removal of an exclusion. Absence is also the
exact shape of every case the guard exists to catch — a userList criterion returns no
`keyword_view` row at all — so treating it as permission would defeat the guard with the
very input that triggers it. A row OMITTING `negative` is likewise treated as negative:
Google omits proto fields at their default, so a positive keyword can legitimately arrive
that way, but assuming the benign default is precisely how an exclusion gets removed.

Finding 3 was the same class one layer up. `keywords.go:675,685` returned bare
`fmt.Errorf` with the word UNCONFIRMED in the TEXT and no `%w`, while
`classifyKeywordActionError` detects ambiguity STRUCTURALLY (`var unconfirmed interface{
Unconfirmed() bool }`, matched by behaviour). `errors.As` found nothing, so both 2xx arms
fell through to the definite-failure 503 "could not be applied" — telling a caller to retry
a batch Google may already have applied, including an irreversible REMOVE. These two arms
carry NO underlying error for `createOutcomeAmbiguous` to classify, so the structural
`unconfirmedKeywordError` wrapper is the only thing that can reach the right arm. The
dispatcher also returned the client error raw, unlike the toggle path at `:897`; it now
preserves the marker. Finding 4 is the same defect in `orchestrator.go`: by the time the
outcome count can be short the mutate has ISSUED, so that error now carries `Unconfirmed()`
too.

Finding 2 was a CONTRACT choice. `googleAdsScopeForCustomer` dropped mismatched entries and
only errored when NOTHING survived, so a project with campaigns under both its old and
current customer got the current-account subset reported as its whole picture. Both
remedies were available; this takes FAIL-CLOSED. Neither response has any omitted-campaign
signal — no count, no flag — so a filtered subset is indistinguishable from a complete
answer, and an audience distribution over half a project's campaigns looks exactly like a
full one. `ErrCampaignAccountMismatch` already maps to a 409 whose remedy (reconnect the
original account) is the right one, so no design/gen change rides along. A partial-coverage
field remains the richer long-term answer and was NOT landed unilaterally: it is only as
good as the consumers that read it, and every one that ignores it silently regains today's
defect.

Findings 5 and 6 were documentation that named a broader boundary than the code. The Goa
descriptions still said "this project's **account**" and "the **account** holds more" while
the implementation queries only project-owned campaign ids; on a Google customer shared
across every foundation, "account" is a materially wider security boundary. Reworded and
`make apigen` run, propagating to `gen/`, the OpenAPI and the `kodata` copies. The API
catalog claimed a scope its SQL does not implement — `listProjectPlatformCampaignIDsQuery`
has NO dispatch-origin predicate, so ADOPTED campaigns (which `AdoptCampaign` persists a
`platform_campaign_id` for) ARE in scope. Corrected to describe what the SQL returns.

A seventh claim was checked and REJECTED rather than actioned: a reviewer twice said
`campaign_repo.go:~311`'s comment claims only the platform id is selected while the query
also selects `result`. The comment block explains why `result` travels with each id; the
claim is false and nothing was changed there.

**Verification** — four mutations, each compiling, each reverted:

- Disabling the type gate (`_ = c.resolveKeywordCriteria`) fails 5 client tests —
  negative keyword, userList, unresolvable, omitted-`negative`, and the keyword_view
  assertion — plus both dispatch sub-cases. The positive-keyword test still passes, so
  the guard is not merely refusing everything.
- `if false && skipped > 0` fails `MixedProvenanceScopeFailsClosed` on BOTH the keywords
  and audience reads: "a partial result was reported as complete".
- Renaming `Unconfirmed()` off `unconfirmedKeywordError` (message text untouched) fails
  4 client tests and the dispatcher test — proving the assertions are structural, not
  string matches on "UNCONFIRMED".
- The same rename on `unconfirmedOutcomeCountError` fails the short-outcome service test
  with the definite-failure message, "inviting a retry of a batch that may already have
  run". The pre-existing test asserted only the 503 TYPE, which both arms share, so it
  passed against the bug; it now asserts the message that separates verify from retry.
