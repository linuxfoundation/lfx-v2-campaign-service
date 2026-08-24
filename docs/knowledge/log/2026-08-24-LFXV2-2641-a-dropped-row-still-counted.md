# 2026-08-24 — LFXV2-2641 a dropped row still counted against the cap

**Fix** — `GetKeywordPerformance` filters negative criteria out of the result but kept deriving
both the row cap and `Truncated` from the RAW response. One negative row ahead of a full page of
positives returned 49 rows with `truncated: true`, from a response that carried exactly 50
publishable keywords and nothing more.

## The same coupling, broken from the other direction

Earlier on this branch the truncation probe row — the 51st, fetched only to prove "there is
more" — was sliced off BEFORE `assertCampaignInScope`, so the one row proving the campaign
filter was unhonoured was the only row exempt from validation. The fix was to stop slicing and
let the cap govern the APPEND instead of the validation.

The polarity fix then added `continue` for negative rows. The cap and `Truncated` still counted
the raw response, so rows were now dropped AFTER being counted — the same cap/validation
coupling, inverted. A guard added at one layer silently re-broke an invariant another layer had
just been fixed to hold.

## Three counts, not one

The body was conflating quantities that only coincide when nothing is dropped:

| quantity | meaning |
| --- | --- |
| `len(rows)` | rows the server returned, including ones the read discards |
| `matched` | rows that are keywords this endpoint publishes (in scope, positive) |
| `len(out)` | matched rows actually appended, capped at `maxKeywordRows` |

`Truncated` must answer **"are there more MATCHING keywords than we returned"** — a caller uses
it to decide whether to page. Derived from `len(rows)` it answers a different question, "did the
server send rows we did not keep", and a negative criterion makes those two diverge. The cap has
the identical requirement, so both now key off `matched`; the index `i` no longer governs
anything.

This is the failure-as-a-confident-value shape once more: an incomplete-looking answer
manufactured from a complete one. The caller is not shown an error, it is shown `truncated: true`
and pages for data that does not exist.

## The probe row can still answer the question

Worth stating because a polarity filter looks like it should invalidate the probe. It does not,
and the reason is that **the polarity check is enforcement of a filter already REQUESTED**, not a
second narrower filter applied after the fact. The GAQL query carries
`AND ad_group_criterion.negative = FALSE`, so `LIMIT maxKeywordRows+1` probes the MATCHING set,
and a matched count past the cap is exactly the signal the extra row was fetched to produce.

A page cannot come back entirely negative unless the server ignored its own predicate — and a
row proving that is dropped rather than counted, so `truncated` stays honest in that case too
rather than reporting truncation off rows the caller can never reach. The check is the same
request/enforce split `assertCampaignInScope` draws: both re-verify a filter the query asked for.

Had the polarity restriction existed ONLY response-side, the probe genuinely would have been
uninformative and the fetch size would have had to change.

## Mutation results

Five compiling reverts, all killed, no survivors. Each revert was re-verified by READING the
restored line, not by a `grep -c`, and each mutant was confirmed to build (`go build` / `go vet`
exit 0) so that no "kill" was really a compile error:

| revert | killed by |
| --- | --- |
| `Truncated: len(rows) > maxKeywordRows` | reviewer's case + 4 sweep cases |
| cap back to `if i >= maxKeywordRows` | reviewer's case + 4 sweep cases |
| polarity check to `if false` | `NegativeKeywordIsNotReturned` + 5 sweep cases |
| scope check skipped past the cap | `OutOfScopeProbeRowFailsTheRead`, `NegativeProbeRowIsStillScopeChecked` |
| `Truncated: false` hardcoded | `TruncatesAndReportsIt` + 2 sweep cases |

The last revert is the one that answers "did the sweep only prove `truncated` can be false".
It cannot be hardcoded: two cases keep `truncated: true` reachable.

The polarity and scope reverts are the pair that matters most: they prove this fix did not buy
its correctness by trading away either of the two guards already on this branch. `NegativeProbeRowIsStillScopeChecked`
covers the combination neither earlier test reached — a row that is BOTH negative AND past the
cap, i.e. carrying both conditions that independently cause a row to be dropped.

The sweep asserts the returned count and `Truncated` TOGETHER across six polarity/boundary
combinations, because either half can be right while the pair is wrong. Two cases keep
`truncated: true` reachable; a fix that made it count publishable rows could otherwise have
hardcoded it false and passed.

## Fixture convention unchanged

Positive rows still OMIT `negative` entirely, which is what Google sends — protobuf JSON does not
serialise a field at its default. Absence means POSITIVE and is kept. No fail-closed-on-absence
guard was introduced; that defect has shipped once on the mutate path already and its guard tests
(`OmittedNegativeIsPositive`, `ExplicitNegativeFalseIsPositive`) still pass.

**The rule this leaves behind:** when a filter is added to a loop, every counter the loop feeds
must be re-derived from what SURVIVES the filter. A count taken before a drop and used after it
is wrong by construction, and it stays wrong quietly — the value still looks like a number.
