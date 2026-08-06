# 2026-08-05 — Fix stale migration-number references in campaign-delete PR

**Fix** — Addressed a blocking review finding (dealako). The two new migrations
in this PR are `000013_campaigns_partial_unique_platform` and
`000014_drop_campaigns_full_unique_platform`, but their own comments, the
operator-facing `RAISE EXCEPTION` message in 000014's up migration, and the
cross-referencing prose in `charts/lfx-v2-campaign-service/templates/deployment.yaml`
and `parity_test.go` all mistakenly called them `000010`/`000011` — the numbers
of this repo's pre-existing, unrelated `index_outbox` migrations. An on-call
engineer following the failed-migration error message during a real incident
would have been pointed at the wrong migration entirely. Corrected every
self-referential `000010`→`000013` and `000011`→`000014` across the two
migration files (up and down), the deployment chart comments, and
`parity_test.go`, leaving the genuine `000010_index_outbox`/
`000011_index_outbox_lease` references elsewhere in the repo untouched.
