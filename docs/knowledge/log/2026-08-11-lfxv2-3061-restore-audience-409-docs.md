# 2026-08-11 — LFXV2-3061: restore the audience-build 409 contract to the API catalog

**Fix** — a conflict resolution taken at FILE granularity silently reverted an unrelated
section of `docs/api-catalog.md`.

Merging `main` into this branch conflicted on `docs/api-catalog.md`. The conflict was real and
the branch side was the right answer for it: `main`'s Meta rows described the
`Required("account_id")` behaviour that this ticket supersedes. Resolving with
`git checkout --ours` settled that conflict and, in the same move, restored the branch's
older copy of every OTHER part of the file — including the audience-build row and the ~20
lines after it that `main` had added, which document the endpoint's two 409s.

Those two 409s carry OPPOSITE remedies, so the deletion is not cosmetic: a client keying on
the status code alone does the wrong thing for one of them. `mapAudienceErr`
(`internal/service/audience.go`) still returns `stale_approval`,
`audience_build_in_flight` and `already_exists`, and tests still pin them — the CONTRACT was
never touched, only its documentation, which is exactly why nothing failed.

Found by Copilot on PR #116 as a suppressed comment.

## The rule

`--ours` and `--theirs` resolve a FILE; a conflict is a HUNK. Where a conflicted file has
sections the branch never meant to touch, whole-file resolution reverts them to the merge
base and reports success. Check the post-merge diff of the resolved file against `main`, not
just that the conflict markers are gone: a hunk the branch has no reason to own is the tell.
