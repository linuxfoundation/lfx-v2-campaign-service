# 2026-08-24 — LFXV2-2641: a type-scoped view still carries both polarities

**Fix** — `GetKeywordPerformance` returned NEGATIVE keywords as ordinary, actionable rows.

The read selects `FROM keyword_view` and filtered only on status. The reasoning recorded in
this repo for why it needed no criterion-TYPE predicate was correct — `keyword_view` is
Google's type-scoped resource, it holds keywords and nothing else — but it was carried one
step too far, into "therefore every row it returns is actionable". TYPE and POLARITY are
different axes. The view holds keywords of BOTH polarities, so exclusions came back through
the same query.

The same file already knew this. `resolveKeywordCriteria`, on the mutate path, selects
`ad_group_criterion.negative` precisely because "keyword_view carries BOTH polarities and only
the positive ones are the keywords this endpoint may act on" — and it queries the same view
specifically so that "if the keywords endpoint would hand it back, this endpoint accepts it"
is true by construction. That sentence was false in the one direction nobody checked: the read
handed back rows the action endpoint refuses. **A cross-reference between two paths asserts a
property of BOTH; verify it from each end, not just the one being written.**

The consequence is not cosmetic. Every row is published as the `criterion_id` + `ad_group_id`
handle `keyword-actions` takes, and that endpoint refuses a negative criterion outright,
because pausing or removing an exclusion WIDENS delivery and spend and `REMOVE` is
irreversible. A published exclusion is therefore a handle whose only advertised use is
guaranteed to fail — and it also consumes one of the 50 capped rows, making `truncated`
describe a set containing rows the caller can do nothing with.

This is the SAME defect class this branch already fixed once on the mutate side: an endpoint
that establishes a criterion is a keyword by its container rather than by its polarity. It
recurred on the read path because the fix was applied where the finding was, not across the
surface that shares the assumption.

The fix is in three parts, and the middle one is the one a WHERE clause alone would miss:
`ad_group_criterion.negative` is SELECTed, the query restricts to `negative = FALSE`, and the
decoded field is RE-CHECKED before a row is published. That mirrors `assertCampaignInScope` in
the same function — the GAQL predicate is the filter REQUESTED, the code is the filter
ENFORCED. A negative row is DROPPED rather than failing the whole response, unlike a foreign
campaign: a foreign campaign proves the filter was not honoured and invalidates every row on
offer, whereas an exclusion is the view answering honestly about a criterion this endpoint
does not publish.

**Absence of `negative` still means POSITIVE.** It is a proto bool, protobuf JSON omits a false
scalar, so the ordinary keyword arrives with no `negative` key at all. A fail-closed-on-absence
guard here would refuse the entire happy path — which is exactly what shipped once on the
mutate path and had to be reverted. The read-side fixture helper was audited for the same trap:
`keywordRowJSON` OMITS the field, so the positive-path tests run against the body Google really
sends rather than one no conformant serialiser emits.

Related, found in the same sweep: a failed PRE-MUTATION read was being reported as an ambiguous
MUTATION outcome. `createOutcomeAmbiguous` INFERS ambiguity from an error's shape — any
`transportError`, 5xx or exhausted 429 — which is the right default for an error returned by a
mutate and the wrong answer for `resolveKeywordCriteria`, whose GAQL read fails with those same
shapes before `adGroupCriteria:mutate` is ever built. The arm even said "no keyword was changed"
in its own message while classifying as "the changes may have been applied"; the prose and the
classification contradicted each other, which is how it survived. A `notAttemptedError` marker,
checked ahead of the inference, states the fact at the call site that KNOWS it. **When a
classification is inferred from shape, the call site that knows better must say so structurally
— prose in the message is not detection.** It changes only the CONFIRMED/UNCONFIRMED axis: a
failed read remains a retryable 503.
