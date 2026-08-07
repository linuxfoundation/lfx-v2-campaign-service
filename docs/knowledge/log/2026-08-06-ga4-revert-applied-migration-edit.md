# 2026-08-06 — GA-4: revert the edit to applied migration 000009

**Update** — Cursor flagged that this branch had edited `000009_drop_invalid_stuck_claim_index.up.sql`,
which is already merged and applied, moving its index rebuild out into a new
`000015_rebuild_stuck_claim_index`. golang-migrate never re-runs a version a database has
already applied, so the source stopped matching applied history — and `000008`'s comment
("Migration 000009 drops an INVALID copy AND rebuilds it in the same step") became false.

The change is now fully reverted: `000009` is byte-identical to `main` again and the `000015`
pair is deleted. The branch's migration directory matches `main` exactly.

Two things were confused when the edit was made. The Cursor finding it originally answered
was about `CREATE INDEX CONCURRENTLY` inside an implicit transaction — but `000009` as merged
does a **plain** `CREATE INDEX` inside its `DO` block, deliberately and with a comment saying
so. It was commit `47123b39` on this branch that switched it to `CONCURRENTLY` and thereby
CREATED the transaction problem; `e3b3db9a` then split the file to fix a defect that only
existed on this branch. Reverting both removes the problem at the root.

The motivation behind `47123b39` — a plain `CREATE INDEX` blocks writes during a rolling
deploy — is real but does not survive the constraint. Making that rebuild non-blocking
REQUIRES removing it from `000009`, and `000009` cannot be edited. A purely additive `000015`
would be a no-op on every database (`IF NOT EXISTS`, after `000009` already rebuilt), so it
buys nothing. And the blocking build is only reachable on the recovery path, where the index
is already absent and the scan already degraded — which is exactly the trade-off `000009`'s
own comment argues for.

Migration changes were also out of scope for GA-4 (ad-group targeting); they arrived through
stack-sync work rather than this ticket.
