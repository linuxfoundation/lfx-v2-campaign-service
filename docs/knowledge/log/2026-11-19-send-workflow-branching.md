# 2026-11-19 — A send series is cloned from a workflow that branches

**Fix** — `POST /projects/{project_id}/send-workflows` refused the survey sequence workflow it
exists for. Building a series from workflow `1841232183` failed with `workflow shape is not
supported for a send series: ... action 5 branches; a send series must be one straight sequence`,
so a 2- or 3-email survey series could be staged but never wired into a workflow.

That workflow gates a later send on a branch. The walk refused any action carrying
`staticBranches`, `listBranches` or `defaultBranch`, so it stopped at the first fork and nothing
was created.

- **Branches are walked through.** `flowSeriesSteps` (was `flowMainPath`) follows every edge out of
  each action and ranks the sends, and separately the wait-until-date steps, by how many of their
  own kind each can reach.
- **What is still refused is what has no order.** Two sends, or two dated steps, on parallel arms
  share a rank and the clone is refused naming both actions. So are a loop, a plain edge of a type
  other than STANDARD or GOTO, and a send or dated step the walk cannot reach -- the last would
  keep the source campaign's email or date.
- **GOTO edges are followed.** Found by running the fix against the real workflow: its action 11
  continues by a GOTO edge, which the walk first refused as "not a standard one". A GOTO is a jump
  to an action elsewhere, the way a branch rejoins the main line, so it is followed like a STANDARD
  edge; a GOTO that jumps back is a loop and is still refused.
- **An unrecognised branch spelling fails closed.** An arm the walk does not read is not followed,
  so the steps behind it are unreachable and the clone is refused, never mapped to the wrong send.
- Unchanged: the count pairing (`ErrFlowStepCountMismatch`), the enrollment replacement, and the
  always-disabled create.
