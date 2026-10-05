# 2026-10-05 — Correcting "every supported platform bills in micros"

**Fix** — `microsPerCurrencyUnit`'s comment in `internal/service/brief_budget.go` and the
budget-validation paragraph of `docs/knowledge/code/internal-service.md` both said every
supported ad platform bills in micros. It is not true: Google Ads bills in micros, LinkedIn
settles on whole cents, and Meta on the account currency's minor unit.

The design's `budget` attribute comment was already corrected when the LinkedIn and Meta
budget writers landed (2026-10-01 log entry); these copies were missed, and so were the
client-facing 400 ("the smallest amount an ad platform accepts is 0.000001") and the
`api-catalog.md` budget row ("the smallest amount an ad platform bills"). All now say what is
true: one micro is the LOOSEST floor any supported platform has, so it is the only floor the
contract can state for every platform at once. LinkedIn and Meta enforce stricter floors of
their own and already answer them 400 with a reason; Google's floor IS this one, and its adapter
would refuse a sub-micro amount with a bare error the service could only call 503 — which is
why the service checks it first. The design comment's "each adapter enforces a stricter floor"
is narrowed the same way. The only behaviour change is the 400's wording.

It matters now because the budget UI (Optimize) is the next consumer of this endpoint, and
the false sentence would have suggested micro-precision amounts are settable everywhere.
