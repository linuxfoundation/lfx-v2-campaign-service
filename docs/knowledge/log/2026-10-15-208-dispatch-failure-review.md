# 2026-10-15 — #208 review: doc alignment and provider token-refresh tests

**Fix** — Two older knowledge paragraphs still said the async create job collapses every
dispatcher error into `platform campaign creation failed`, contradicting "Dispatch failure
reasons in the job result" in [internal/service](../code/internal-service.md). The campaign-create
paragraph in `internal-service.md` now describes `DispatchFailureMessage`: a fixed sentence per
connection class completed with the `unusableConnectionReason` token, a `domain.PreflightRefusal`
echoed verbatim, fixed token-refresh and upstream-status sentences, the generic string as the
fallback, and Google Ads still reaching only the generic classes. The HubSpot A/B pre-check
rationale in [internal/dispatch](../code/internal-dispatch.md) keeps its reasoning (dispatch runs
after the `202` and never renders a dispatcher error's text) without claiming the job error is
always the fixed string.

New dispatch-depth tests in `internal/dispatch/preflight_refusal_test.go`:

- `TestDispatch_ProviderTokenRefreshFailureIsClassified` runs real LinkedIn and Microsoft
  dispatchers against an httptest token endpoint answering 503 with a secret-bearing body, and
  asserts the job message is the token-refresh sentence with no body text and only the token
  request sent. LinkedIn's token URL override is unexported, so its client reaches the server
  through a host-rewriting transport. The Microsoft case sets `geoTargets` so the geo resolve,
  which runs before the find-existing lookup, is the first networked step. A failed lookup is
  deliberately UNCONFIRMED, because it cannot prove that a campaign from a previous attempt is absent.
- `TestDispatch_CancelledTokenRefreshIsNotReportedAsARefreshFailure` cancels the caller's context
  while a real LinkedIn token exchange is blocked in flight, and asserts the dispatch stays
  pre-create and the job message is the generic fallback, not the token-refresh sentence.

No production code changed.
