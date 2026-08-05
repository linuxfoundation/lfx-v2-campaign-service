---
type: "Dev Tooling"
title: "Local git hooks (DCO enforcement)"
description: "commit-msg hook rejecting commits without a DCO Signed-off-by trailer, installed via make setup."
resource: ".githooks"
---

# Local git hooks (DCO enforcement)

`.githooks/commit-msg` rejects a commit locally unless it carries a
`Signed-off-by:` trailer matching the committer's configured `user.name`/
`user.email` (i.e. `git commit -s`). CI's DCO probot check catches a missing
sign-off too, but only after a push and a PR review round; this hook catches
it before the commit is even made.

`make setup` wires it in via `git config core.hooksPath .githooks` — a
repo-local config change, not a global one. Merge commits are exempted
(their DCO trail comes from the commits they merge).

See [.githooks/commit-msg](../../../.githooks/commit-msg).
