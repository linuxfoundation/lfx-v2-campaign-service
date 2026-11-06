# 2026-11-06 — #266 local review can reach the repo_code brain again

**Fix** — `/lfx-skills:lfx-local-review` stopped before starting any reviewer:
"no repo_code reviewer skill at `.claude/skills/local-code-review/SKILL.md` — this
repo does not own local review brains". The repo does have a code review method, but
it lives at `.github/skills/campaign-service-code-review/SKILL.md`. That is Copilot's
tree, and no Claude session registers a skill from it.

- **Added** `.claude/skills/campaign-service-code-reviewer/SKILL.md`
  (`name: campaign-service-code-reviewer`), plus its `.agents/skills/` symlink. It is
  a pointer, not a copy: it tells the reviewer to read the `.github/` method at the
  pinned `target_sha` and apply it, with the local scope, bar and report format in
  place of the PR-only parts. The method stays in one place because Copilot reads it
  from `.github/` and nothing was moved out of that tree.
- **Added** the tracked symlinks `.claude/skills/local-code-review` →
  `campaign-service-code-reviewer` and `.claude/skills/local-learnings-review` →
  `campaign-service-learnings-reviewer`. These are the paths the installed launcher
  probes. It then loads each brain by its frontmatter `name:`.
- **Verified** with the installed launcher's `run-pi.sh --readiness`. Before the
  change it printed the failure above with exit 1. After it, the probe got past
  skill resolution to the harness decision and printed the pins. A fresh
  `claude -p` session in the worktree lists both `campaign-service-code-reviewer`
  and `campaign-service-learnings-reviewer`, each once.
- **Kept**: the `## Pre-PR review` block in `CLAUDE.md` still names
  `/lfx-skills:lfx-pre-pr-review`. The issue said that skill does not exist, but
  that was because the installed plugin (`0.1.1`, `2f35e8e`, 2026-08-04) predates
  it. It is on `linuxfoundation/lfx-skills` main, and the current central
  `lfx-local-review` expects a `## Review lifecycle configuration` section, which
  `lfx-pre-pr-review` refuses to run beside. So pointing the block at
  `lfx-local-review` would break the round on an up-to-date plugin. Instead,
  `CLAUDE.md` now names the code review skill next to the existing KB line, as the
  issue asked. Moving to the declaration form stays with PR #202.
- **Docs**: [Local pre-PR review](../architecture/local-pre-pr-review.md) now
  describes both brains and the alias paths, and drops "do not reintroduce a
  conventions reviewer". Its description and index bullet are updated.
  `docs/reviews/knowledge-base/**` and `.github/**` are unchanged.
