# 2026-08-19 — LFXV2-2641 keyword-action fault precedence and unconfirmed outcomes

**Fix** — three findings from the second reviewer sweep on #153. Each behavioural fix was
mutation-verified: the fix reverted with a COMPILING change, the new test observed to fail, then
restored.

**An ambiguous mutation collapsed into a retry-me 503.** `classifyKeywordActionError`
(`internal/service/brief_keyword_actions.go`) had NO branch for an UNCONFIRMED outcome — its own
`default`-arm comment admitted it swallowed them ("Includes the UNCONFIRMED outcomes the client
reports when a mutate may have been applied"), and `IsOutcomeUnconfirmed` appeared nowhere
outside the six dispatch files. So a keyword mutate that MAY already have been applied returned
the same `503 "the keyword actions could not be applied"` a definite failure gets. The two must
not answer the same thing: a 503 reads as "retry", and retrying a `REMOVE` that already ran is
precisely the wrong remedy, because Google cannot re-enable a removed criterion — only create a
new one with a new id. The client genuinely produces this state for keyword mutates
(`keywords_test.go` pins a short mutate response, a mismatched resource name and a foreign-customer
resource name as unconfirmed), and the dispatcher propagates the client error raw, so the arm was
live rather than theoretical.

UNCONFIRMED now has its own arm ABOVE the default, matched by BEHAVIOUR — `errors.As` on an
`Unconfirmed() bool` method, since the dispatch wrapper (`unconfirmedToggleError`) is unexported
and no sentinel crosses that package boundary. It keeps the **503 rather than inventing a status**:
the endpoint's declared error set (`commonBriefErrors`) is 400/404/409/500/503 and unchanged, so
no `gen/` churn rides along, and the ambiguity is carried by the MESSAGE, which tells the caller
to VERIFY before retrying. This mirrors the status toggle's unconfirmed arm exactly
(`brief.go`), which reached the same conclusion for the same reason. Because both arms are 503 by
design, the test asserts the MESSAGE — a test checking only the status code would have passed
against the defect.

**Two layers disagreed about which fault dominates.** The Google Ads adapter validates the batch
BEFORE it checks provisioning or resolves a connection, deliberately, "so a permanent input fault
masks any contingent connection fault rather than the other way round"
(`dispatch/googleads.go` — verified against the source, not assumed). But
`Orchestrator.ApplyKeywordActions` ran its own `ErrCampaignNotProvisioned` guard AHEAD of the
dispatcher, inverting it: a malformed batch against an unprovisioned campaign answered 409 ("try
later") instead of 400 ("your input is wrong"), so the caller retried forever on input only they
could fix. The orchestrator now refuses only a NIL campaign — not an input fault, and not a state
any `KeywordActioner` should have to nil-check around before it can validate anything — and
delegates the empty-`PlatformCampaignID` case to the adapter, which raises the same sentinel in
more detail (it also requires an ad group) and in the right order. Google Ads is the only
`KeywordActioner`, and it enforces provisioning itself, so nothing is left unguarded. A VALID
batch against an unprovisioned campaign still answers 409; only the malformed case moved.

**A test that agreed with the bug.** `TestOrchestratorApplyKeywordActions_UnprovisionedIsRefused`
asserted the OLD ordering — that the orchestrator refused both rows ahead of the dispatcher — and
passed only because its fake (`keywordActionDispatcher`) ignores the campaign argument entirely
and never raises the sentinel itself. With the guard delegated, the empty-id row returned a
success. It is rewritten against a dispatcher that actually implements the adapter's documented
validate-then-provisioning order, and now asserts WHICH LAYER refused (via a `called` flag) rather
than only the sentinel — the division of labour is the contract, and the sentinel alone cannot
distinguish the fixed code from the broken code. The `default`-arm row of
`TestApplyKeywordActions_ErrorMapping` was checked and left alone: its error is a plain
`errors.New("boom")` with no `Unconfirmed()` method, so it correctly still exercises the definite
arm and never asserted the swallowed case.

**One surviving "account-wide" claim.** `design/brief.go`'s `match_type` godoc still read "this is
an ACCOUNT-WIDE read returning keywords this service never created", contradicting the type godocs
and the `truncated` attribute, both already corrected to say project-scoped. Only the framing was
wrong — the UNKNOWN-enum rationale it supports remains valid, because an ADOPTED campaign's
criteria were authored in the Google Ads console rather than by any dispatch this service ran, so
an unrecognised match type is still reachable within a project-scoped read. The comment now says
that instead of deleting the reasoning.

**Contract surfaces.** `docs/api-catalog.md` and the design's Method Description both now record
that 503 carries two distinct outcomes separated by the message (so a client must not branch on
the status alone), and that a malformed batch is 400 even when the campaign is also unprovisioned.
No status code was added, so `commonBriefErrors`/`briefErrorResponses` are untouched; the design
Description change is a DSL string, so `gen/` and the `kodata` OpenAPI copies the deployed pod
serves were regenerated with `goa gen` and committed.
