# 2026-09-02 — Adopt the central review lifecycle configuration

**Update** — The repo replaced its own local pre-PR review lifecycle with the
central declaration. `CLAUDE.md` drops the `## Local work cycle — post-commit
and pre-PR review` section and instead carries the single
`## Review lifecycle configuration` section that `/lfx-skills:lfx-local-review`
reads; that section is now the repo's only local-review trigger and reviewer
configuration surface, and it is not repeated anywhere else in the repo.

The old lifecycle/configuration concept at
`docs/knowledge/architecture/local-pre-pr-review.md` is replaced in place by a
narrow concept covering only the repo-owned review content and its known
unresolved target-only pattern limitation, so historical log links to that path
stay valid; it restates no configuration or lifecycle rule, and its index bullet
is updated to match.

Deleted as obsolete: the repo-owned fallback launcher
`.claude/skills/local-review-fallback` with its `.agents/skills/` link, and the
generic `local-code-review` and `local-learnings-review` symlinks in
`.claude/skills/`. The deleted launcher was
this repo's own table for starting reviewer subagents; it is unrelated to the
central lifecycle's own derived same-name `SKILL.md` fallback, for which the repo
adds nothing.

Retained: the two repo-owned reviewer skills
`.claude/skills/campaign-service-code-reviewer` and
`.claude/skills/campaign-service-learnings-reviewer`, with their bodies unchanged
and only their frontmatter `description` corrected to state how they are loaded,
plus their same-name `.agents/skills/` links.
