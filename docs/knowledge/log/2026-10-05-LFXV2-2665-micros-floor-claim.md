# 2026-10-05 — Correcting "every supported platform bills in micros"

**Fix** — `microsPerCurrencyUnit`'s comment in `internal/service/brief_budget.go` and the
budget-validation paragraph of `docs/knowledge/code/internal-service.md` both said every
supported ad platform bills in micros. It is not true: Google Ads bills in micros, LinkedIn
settles on whole cents, and Meta on the account currency's minor unit.

The design's `budget` attribute comment was already corrected when the LinkedIn and Meta
budget writers landed (2026-10-01 log entry); these two copies of the claim were missed. Both
now say what the design says: one micro is the LOOSEST floor any supported platform has, so it
is the only floor the contract can state for every platform at once, and each adapter
enforces its own stricter floor. No behaviour changes — the check itself was already correct
for that reason; only the stated reason was wrong.

It matters now because the budget UI (Optimize) is the next consumer of this endpoint, and
the false sentence would have suggested micro-precision amounts are settable everywhere.
