# 2026-10-15 — Custom audience builder: gate property-only audiences, skip missing ambassador programmes

**Fix** — two of the design questions left open on #337:

- **A property-only audience must be gated.** Criteria made only of `property_filters` (for example
  `email is_known`) passed validation with no `gate_list_id`, and create built a list of most of the
  portal. `ValidateCustomCriteria` now refuses that, and with a gate the single roles/properties
  branch carries it, so `AllBranchesGated` and preview's upper bound hold. A role restriction
  (speakers, ambassadors) stays allowed without a gate.
- **A missing ambassador programme is skipped, not refused.** All three programme properties had to
  exist or the whole request failed with a 400. Preview now reads them as it reads the speaker
  signals: one the portal lacks is left out with a warning naming it, and only a portal with none is
  refused.

Create idempotency (a retry after a lost 201 makes a `(v2)` list) is unchanged: it needs a product
decision on an idempotency key.
