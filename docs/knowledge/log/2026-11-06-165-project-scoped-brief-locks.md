# 2026-11-06 — Brief row locks and the campaigns brief FK are project-scoped

**Update** — the optional hardening from the #165 audit (#534). Neither item was a live gap: both
locks ran after a project-scoped read, and every insert path already paired a campaign with its
brief's project. The datastore now enforces it, so a future caller that skips the scoped read
cannot cross projects.

- `JobRepo.CreateJobForApprovedBrief` takes `projectID` and locks through
  `lockApprovedBriefForJobQuery` (`WHERE id = $1 AND project_id = $2 FOR UPDATE`). Another
  project's brief reads as absent and the job is refused with `ErrStaleApproval`.
- `ReplaceCampaign`'s opening brief lock is now `lockBriefRowForReplaceQuery` (renamed from
  `lockBriefRowByIDQuery`), keyed on `(id, project_id)`. A mismatched project is `ErrNotFound`
  at once, without waiting on the brief's row lock.
- Migration `000056` adds the composite `(brief_id, project_id) -> campaign_briefs (id,
  project_id)` key to `campaigns`. 000002's id-only key stays, redundant: dropping it would need
  ACCESS EXCLUSIVE (blocking reads) and an ADD -> DROP lock upgrade that can deadlock. The id-only
  key was why the replace lock could not carry `project_id` before: a two-column predicate could
  have silently missed.
- It fails loudly on any existing mismatched row and never rewrites data. That failure leaves
  version 56 dirty, so a restart does not retry it: resolve the rows, `migrate force 55`, restart.
- Lock waits and deadlocks do not fail it. The ADD retries with a 2s `lock_timeout` per attempt,
  catching `55P03` and `40P01` and giving up its queue place between attempts, for at most 15
  attempts. Only exhausting all 15 fails it, which dirties the version like the precheck (same
  recovery). The 2s bounds lock ACQUISITION only: failed attempts add up to about 45s, and a
  queued attempt holds writes for at most 2s, but the attempt that succeeds then holds SHARE ROW
  EXCLUSIVE on both tables while it validates every existing `campaigns` row, so writes also wait
  for that scan. Neither the scan nor the precheck has a time limit; both scale with the table,
  which is small. Reads never queue.
- `campaign_jobs` keeps its id-only FK: it has no `project_id` column, and a composite key would
  need a new column plus a backfill.
- `TestBriefRowLocks_AreProjectScoped` pins every named brief `FOR UPDATE` statement to
  `(id, project_id)`. Live fixtures that seeded campaigns under another project's brief now
  create the brief under the campaign's project (`insertBrief` takes the project).

See [internal-infrastructure-postgres](../code/internal-infrastructure-postgres.md).
