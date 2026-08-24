# 2026-08-19 — LFXV2-2641 permanent keyword faults were answered as retryable

**Fix** — three follow-ups on the keyword-actions guard. Two of them are the same defect
wearing different clothes: a request this code should have refused locally reached Google,
whose PERMANENT rejection was then classified onto the retryable 503 path.

## `keyword_view` returns REMOVED criteria, and the type-resolution query admitted them

The type-resolution query added with the criterion-type guard carried no status predicate:

    SELECT ad_group_criterion.criterion_id, ad_group.id, ad_group_criterion.negative
    FROM keyword_view
    WHERE ad_group.id IN (...) AND ad_group_criterion.criterion_id IN (...)

`GetKeywordPerformance`, three hundred lines above, does carry one, and its comment already
explains why it is an ALLOW-LIST (`ENABLED`, `PAUSED`) rather than an exclusion of `REMOVED`:
the enum also carries `UNSPECIFIED`/`UNKNOWN` and an omitted proto field decodes to `""`, all
of which survive `!= 'REMOVED'`. The mutating path needed it for that reason and for a
sharper one. Google rejects a pause or removal of an ALREADY-REMOVED criterion as permanently
unmutable. So a removed row resolved as an ordinary positive keyword, the mutate went out, and
the permanent rejection came back through the transport-error arm as a **retryable 503** —
this endpoint telling an operator to try again on a handle that can never work.

With the allow-list the row is simply absent from the resolution result, so the existing
fail-closed `!ok` arm answers `ErrKeywordCriterionNotPositiveKeyword` — a permanent 400 —
before anything is mutated, and the mutate endpoint is never called.

**Class sweep.** The review asked whether any other query added tonight had the same gap,
since this is the same class as the GAQL-REMOVED finding fixed on the settings readback. Every
GAQL query added across tonight's commits was enumerated from the diffs rather than from
memory: exactly two exist — the settings readback's `FROM campaign` (fixed in `9ede21dd`,
which named `ENABLED`, `PAUSED` and `REMOVED` because that endpoint's whole purpose is to
REPORT a removal) and this one. The pre-existing queries in `campaign_lookup.go`,
`metrics.go` and `client.go` already carry status predicates. No third instance.

The two fixes point opposite ways for the same reason, which is worth stating: a READ that
exists to report the platform's state must INCLUDE `REMOVED`; a MUTATE must EXCLUDE it. What
they share is that neither may leave the predicate off and inherit GAQL's silent default.

The test has to assert the allow-list POSITIVELY. Checking that the query lacks
`!= 'REMOVED'` passes against a predicate-free query — the exact trap the settings readback's
first REMOVED test fell into.

## The runtime backstop mirrored the character class but not the length

`design/brief.go` declares both ids `Pattern(^[0-9]+$)` **and** `MaxLength(20)`.
`ValidateKeywordActions`, the backstop for callers that never touch the generated decoder,
checked only the pattern. A 21-digit id is digits-only and injection-safe, and cannot name a
real criterion — Google Ads ids are `int64`. It was interpolated into the type-resolution
request, and Google's permanent rejection was classified as a retryable 503 again.

`maxKeywordIDLen = 20` now mirrors the design on both ids. The test pins both sides of the
boundary: 21 digits refused, 20 still accepted — a one-sided check could be satisfied by a cap
that rejects ids the contract declares valid.

The general rule: **a non-HTTP validation backstop must mirror EVERY design constraint, not
just the interesting one.** Whatever it lets through becomes an upstream error this code then
has to classify, and a permanent upstream fault reached by a request we should have refused
reads as transient.

## Widening a guard from "all" to "any" changed who the remedy was for

`googleAdsScopeForCustomer` fails closed on ANY provenance mismatch, not only when every
campaign mismatches — deliberately, because returning the matching subset would be a silent
partial result no response field could disclose. The 409's message was not re-read against
that widened condition:

    this project's campaigns belong to a different ad account than its current connection
    — reconnect the original account to read their keywords

Both halves are wrong for the mixed case, which is now the common one. It asserts that ALL the
project's campaigns are elsewhere, and it prescribes a remedy that would only swap which
subset mismatches — breaking the campaigns that currently match. The message now names
reconciling or re-dispatching the mismatched ROWS, and offers the reconnect only for the case
where one account owns every campaign in scope. The account ids stay server-side, as on every
sibling arm, which is also why it cannot say WHICH campaigns mismatch.

The single-campaign paths (`brief.go`'s metrics and toggle arms,
`brief_keyword_actions.go`) keep "reconnect the original account": exactly one account is in
play there, so it is the correct instruction. Only the scope-wide arm had the mixed case.

The lesson is narrow and repeatable: **when a guard is widened from "all" to "any", the remedy
text is part of the change.** The sentinel and the status code stay right while the
instruction attached to them silently stops applying, and no compiler or test notices.

An existing test comment asserted the old "refuses every campaign" reading; it was rewritten
rather than left to contradict the code, and the remedy itself is now pinned by a test that
fails against the previous wording.
