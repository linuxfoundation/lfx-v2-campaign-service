---
type: "Dev Tooling"
title: "Local git hooks (DCO enforcement)"
description: "commit-msg hook rejecting commits without a DCO Signed-off-by trailer, installed via make setup."
resource: ".githooks"
---

# Local git hooks (DCO enforcement)

`.githooks/commit-msg` rejects a commit locally unless it carries a
`Signed-off-by:` trailer matching the committer's resolved identity (`git var
GIT_COMMITTER_IDENT` — the same GIT_COMMITTER_*/`committer.*`/`user.*`
precedence `git commit -s` itself uses to write the trailer). CI's DCO probot
check catches a missing sign-off too, but only after a push and a PR review
round; this hook catches it before the commit is even made.

The trailer is verified with `git interpret-trailers --parse` (an exact
trailer-block match), not a substring search over the whole message — text
that merely mentions "Signed-off-by: ..." in the commit body would otherwise
pass. Merge commits are exempted via `MERGE_HEAD` (an actual in-progress
merge — checking the message text would exempt an ordinary commit whose
subject happens to start with "Merge", and fail to exempt a real merge with a
custom subject).

`make setup` wires it in via `git config core.hooksPath .githooks` — a
repo-local config change, not a global one.

See [.githooks/commit-msg](../../../.githooks/commit-msg).
