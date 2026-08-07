# 2026-08-07 — LFXV2-2023: the keyword plan leaked Phase 2 into Phase 1

**Update** — Closed eight suppressed Copilot findings on PR #81
(`docs/plans/keyword-surface.md`). Six of them are the same defect wearing different
clothes: **the plan described a shape it had already decided against.**

**Fix** — `change-bid` was pre-implemented inside PR 1. `BidMicros`, `ErrInvalidBidChange`,
the orchestrator's bid-validation loop, and the handler's 400 arm were all specified as PR 1
"groundwork" while the Phase 1 Goa enum is `Enum("pause", "remove")`. The generated request
validation rejects `change-bid` before any of that runs, so none of it is reachable by any
input a test could supply — a sentinel nothing returns and a guard no test can reach, which
is exactly the defect class this repo's reviewers flag. All of it moved into a single
**Phase 2 surface** table, and the rule is now stated once: a phase ships the enum widening
and the code that serves it in the same commit, or ships neither. Open question 7's
sub-question ("should Phase 1 pre-implement enable/change-bid?") is answered rather than
left hanging.

**Fix** — The capability assertion was placed in `internal/dispatch/googleads.go`. That
creates a production `internal/dispatch` → `internal/service` import, which
`status_toggler_guard_test.go:25-27` exists to prevent — it says so in its own comment, and
the assertion would have created the edge silently because the import already resolves. It
moves into that guard's `var` block, beside the six `StatusToggler` assertions, for the same
reason those are there: the orchestrator discovers the capability by RUNTIME type assertion,
so a drifting signature would not fail the build, it would silently return 400 forever.

**Fix** — The non-`*PartialMutateError` fallback marked the whole batch `unconfirmed`. Two
large classes of error are provably "nothing applied": pre-send failures (credential
resolution, request construction, an already-cancelled context) and definite 4xx rejections
(`adGroupCriteria:mutate` is atomic without `partial_failure`, which this client does not
set). Reporting those as `unconfirmed` is not conservative — it tells the operator to go
check Google Ads for a change that certainly did not happen, and an `unconfirmed` that fires
on every expired credential is noise that gets ignored, including the once it was real. The
fallback now defers to `googleads.IsOutcomeUnconfirmed`, the same helper the create and
toggle paths use. The asymmetry with the partial branch is deliberate and documented: there,
an in-flight chunk stays unconfirmed even under a definite 4xx, because the 4xx describes
only the last chunk. Four separate tests replace the one that would have passed either way.

**Fix** — The 409 guard's rationale rested on a false invariant: "`platform_campaign_id`
stays empty until the platform call succeeds." It does not — `internal-service.md:37-41`
records that a retained `pending` partial can carry an upstream id, kept deliberately so the
id is not lost. The real invariant is that the id is empty until the upstream campaign
EXISTS, which is what the guard needs. The plan now also DECIDES the question that false
invariant was hiding: keyword writes are allowed whenever the id is non-empty, including for
pending and reconciliation-required campaigns, because that is the state where unsupervised
spend is most likely and `AuthorizeKeywordCriteria` still scopes every criterion to this
campaign.

**Fix** — Two documentation-only corrections of the same kind. The goal section listed
`enable` under Phase 1 and omitted `remove`; the response echoed the reporting window as an
unconstrained `String` while the request had already constrained it to `Enum(7, 14, 30)`,
which would have generated a schema permitting any string and forced the UI to parse the
value back to compare it against what it asked for. It is now `Int32 window_days` carrying
the same closed enum.

**Fix** — Two stale-prose corrections. PR 3 was assigned "the OpenAPI snapshot" although PR 1
changes `design/brief.go` and therefore runs `make apigen`, which writes both generated trees
at once — deferring one leaves the binary serving a document that does not describe its own
endpoints. PR 3 is tests-only. And the open-questions section claimed more open decisions than
remain: four of the eight are resolved inside the plan, and the section now says which four.
