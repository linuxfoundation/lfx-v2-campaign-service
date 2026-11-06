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
- Migration `000056` swaps `campaigns`' `brief_id`-only FK for the composite
  `(brief_id, project_id) -> campaign_briefs (id, project_id)`. It fails loudly on any existing
  mismatched row and never rewrites data. The id-only key was why the replace lock could not
  carry `project_id` before: a two-column predicate could have silently missed.
- `campaign_jobs` keeps its id-only FK: it has no `project_id` column, and a composite key would
  need a new column plus a backfill.
- `TestBriefRowLocks_AreProjectScoped` pins every named brief `FOR UPDATE` statement to
  `(id, project_id)`. Live fixtures that seeded campaigns under another project's brief now
  create the brief under the campaign's project (`insertBrief` takes the project).

See [internal-infrastructure-postgres](../code/internal-infrastructure-postgres.md).
