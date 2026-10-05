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
would refuse an amount that rounds to zero micros with a bare error the service could only call
503 — which is why the service checks it first. The design comment's "each adapter enforces a
stricter floor" is narrowed the same way. The only behaviour change is the 400's wording.

**Two bounds, kept apart.** Review of this fix caught a second overstatement: "every sub-micro
amount rounds to nothing" is false. The CONTRACT floor is 0.000001 (the design's `Minimum`, which
Goa's generated decoder enforces, so an HTTP caller below it never reaches the service). The
service's RUNTIME cutoff is half a micro, because it compares the rounded value: amounts in
[0.0000005, 0.000001) round up to one micro and Google accepts them. The concept, the catalog
row and the 400 now state the two separately. The 400's wording is visible only to direct Go
callers — over HTTP, Goa's validation error answers first — so the reason this matters is the
docs a budget-UI author reads, not text the UI will display.
