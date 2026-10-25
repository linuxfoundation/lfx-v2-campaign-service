# 2026-10-25 — Guard account-monitor scope revalidation (ops#897)

**Fix** — Address PR #391's defensive type-assertion nit without changing the normal scoped-monitor flow.

- `revalidateAccountMonitorScope` checks that the dispatcher implements
  `AccountMonitorScoper` and returns an error with no authorized ids if it does
  not, rather than relying on the caller's `scoped` guard to avoid a panic.
- A focused regression calls revalidation directly with a missing dispatcher and
  with one that lacks the scoper capability. Both cases must fail closed.
