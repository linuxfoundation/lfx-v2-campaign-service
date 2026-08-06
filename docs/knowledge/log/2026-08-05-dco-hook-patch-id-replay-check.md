# 2026-08-05 — DCO hook: patch-id-based replay content check + bats coverage

**Fix** — Addressed a blocking review question (dealako) on the `.githooks/commit-msg`
DCO hook. The rebase/cherry-pick replay exemption compared only the commit
MESSAGE text before skipping the fresh-sign-off check, which missed a
tree-only amend: `git commit --amend --no-edit` at an interactive-rebase
`edit` stop preserves the message (and its Signed-off-by trailer) while
changing the committed content underneath. Added an equal-weight content
check via `git patch-id --stable`, which hashes the diff rather than the
tree/SHA — this keeps the comparison valid across a rebase onto a different
base commit (unlike a raw tree-hash comparison, which a prior revision used
and was reverted for breaking legitimate cross-base rebases). Also added a
symmetric empty-identity guard on the author-identity path (mirroring the
existing guard on the committer-identity path), so a resolvable-but-empty
`GIT_AUTHOR_IDENT` fails closed with a clear error instead of silently
producing a `Signed-off-by: ` trailer with no name/email.

Added `.githooks/commit-msg.bats` covering the hook's full branching logic:
unsigned rejected, signed accepted, merge exempted via `MERGE_HEAD`,
unmodified replay exempted, edited-message replay requires fresh sign-off,
and the new tree-only-amend case (message/trailer unchanged, content
changed via patch-id) requires fresh sign-off. Verified the new test is
binding by running it against the pre-fix hook: it fails there and passes
against the fix.
