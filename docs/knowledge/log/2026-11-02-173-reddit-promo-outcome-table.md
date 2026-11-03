# 2026-11-02 — 173: the Reddit promoted-post outcomes are one table

**Update** — the Reddit authoring and ad-create outcome handling is now one table
(`internal/platform/reddit/promo_outcome.go`). Each outcome class maps to the result shape and
the persisted status it produces, the exact `AdWarning`, the runbook step, and whether Step 4
appends the deferred campaign-context sentence. `CreateCampaign` classifies the error with
`classifyAuthorFailure` / `classifyAdFailure`, which share one ordered rule, and renders the row.
It no longer carries the AdWarning or runbook strings inline.

Behaviour is unchanged. `TestReddit_PromoOutcomeTable` (22 scenarios covering all 14 rows)
passes against the pre-refactor `client.go` as well as the new one. It drives every row through
`RedditDispatcher.Dispatch` against a fake Reddit, covering 5xx, 429, mutating 3xx, transport
errors, malformed 2xx, 4xx, refused dials and caller cancellation both in flight and before
send. It also fails if a row has no scenario. Reordering the classifier, dropping the AdWarning
seed, or rendering the campaign context for the wrong rows each fail named scenarios.
