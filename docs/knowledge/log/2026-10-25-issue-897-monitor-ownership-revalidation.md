# 2026-10-25 — Account-monitor ownership revalidation (ops#897)

**Fix** — Address PR #391 feedback about in-flight ownership transfers and historical binding lookup cost.

- Both scoped monitor paths revalidate campaign bindings after all upstream reads,
  using one database snapshot for live and eligible soft-deleted rows. Intersecting
  with the initial scope removes transferred campaigns without admitting campaigns
  acquired during the read; lookup failures return no campaign data.
- Account provenance is checked again before a captured id can remain authorized.
  Transfers committed after the final snapshot remain outside this guarantee;
  no database lock is held through provider calls or response delivery.
- Migration `000049` concurrently adds a non-unique, all-status
  `(platform, platform_campaign_id)` index, supporting the takeover lookup without
  changing live binding uniqueness or soft-delete behavior.
- Focused regression coverage exercises takeovers between the initial scope queries,
  during blocked live and report-backed reads, and during the report-backed currency
  read, plus failed revalidation, changed provenance, and newly acquired ids.
  PostgreSQL coverage checks the combined scope after a takeover and the index catalog.
