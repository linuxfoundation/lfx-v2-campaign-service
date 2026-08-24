# 2026-08-24 — LFXV2-2641: the truncation probe row escaped the tenant check

**Fix** — `GetKeywordPerformance` requests `maxKeywordRows+1` rows so a full page can be told
from a truncated one, and discarded the extra row with `rows = rows[:maxKeywordRows]` BEFORE the
loop that calls `assertCampaignInScope`. The probe row therefore never reached the tenant check
that every other returned row passes through. A response whose 51st row named a campaign outside
the requested scope returned 50 clean rows and `truncated: true` with no error — reproduced
exactly, against a fixture where ONLY the probe row was out of scope.

This contradicted the fail-whole-response rule the same file had just adopted for scoped reads:
a row outside the requested set means the GAQL WHERE clause was not honoured, so the whole
response is refused rather than the row skipped. The probe was the single row still exempt from
it, and it is the row a caller's "there is more data" conclusion is sourced from.

The cap now governs the APPEND rather than the validation. Every one of the `maxKeywordRows+1`
rows is decoded, metric-parsed and tenant-checked; rows at index `>= maxKeywordRows` are skipped
only when building the output slice, and `Truncated` is still computed from the raw row count.

The class was swept rather than the named line. `GetAudienceInsights` issues no LIMIT and holds
no probe row — it already iterates and checks every row — so it was not a sibling. The only
other N+1 probe in the repo, `internal/platform/snowflake/client.go`'s `(maxEventRows+1)*2`
fetch, fails closed when the raw limit is reached and validates every fetched row before any
trim, so it does not share the defect either. `GetKeywordPerformance` was the sole site.

Verified by mutation: restoring the pre-loop slice, dropping the append cap, an `i >` off-by-one,
turning the scope error into a `continue`, and the subtler variant that loops over all rows but
guards the ASSERT instead of the APPEND were each re-applied as compiling reverts. All five were
caught; no mutation survived.

Three suppressed review findings on the same PR were verdicted in the same pass, all REAL:

- `internal/service/orchestrator.go` — the `KeywordInsightsReader` godoc told implementers the
  adapter "must drop entries whose recorded customer disagrees" with the client's, while
  `googleAdsScopeForCustomer` deliberately refuses the WHOLE read on any mismatch, and documents
  at length why dropping is the worse option. The interface was publishing the unsafe contract a
  future adapter would have followed. The godoc now states the refusal and the reason.
- `design/brief.go` — the published `apply-keyword-actions` description promised every action is
  checked "BEFORE Google is contacted at all". Criterion type/polarity validation happens in
  `resolveKeywordCriteria`, which issues a GAQL read first. The description now scopes the
  pre-contact claim to the local syntax/provisioning/account checks and describes the criterion
  check as pre-MUTATE. Regenerated through Goa so the OpenAPI and clients carry the corrected
  text. The matching log line in `brief_keyword_actions.go` said "rejected before contacting the
  platform" for a sentinel that also carries the post-read refusals; it now says "before the
  mutate; no keyword was changed".
- `internal/service/orchestrator_metrics_test.go` — `upstreamCapableDispatcher.ApplyKeywordActions`
  returned an EMPTY outcome slice, so the orchestrator's one-outcome-per-action guard made every
  "success" call fail with `unconfirmedOutcomeCountError`. The test asserted errors only in the
  `platformErr != nil` arm, so the success subtest passed while the operation it claims to
  instrument had actually failed — instrumentation asserted over a failed call. The fake now
  returns one outcome per action, and the success arm asserts `err == nil`. Confirmed by
  restoring the empty-slice fake: the new assertion catches it.
