---
type: "Architecture Doc"
title: "Local pre-PR review"
description: "How the repo-owned code and learnings reviewers, the empirical knowledge base, and the Claude fallback run a local review of the newest commit before a PR exists."
resource: ".claude/skills/local-review-fallback/SKILL.md"
---

# Local pre-PR review

A review cycle this repo owns, run from a working copy **after a commit and before
a pull request exists**. It reviews exactly one commit and returns ordinary Markdown.
It stops at PR-open: it never writes a GitHub label, status, check, review or
approval, and it does not touch `.github/**` or the PR-side pipeline.

Invoked from the repo as `/lfx-skills:lfx-local-review` after each signed commit.

## The three roles

| Role | Rulebook | What it may cite |
|---|---|---|
| `general` | central `lfx-general-code-review` | ordinary software quality |
| `repo_code` | [`campaign-service-code-reviewer`](#the-two-repo-owned-brains) | this repo's **written** rules, quoted verbatim |
| `repo_learnings` | [`campaign-service-learnings-reviewer`](#the-two-repo-owned-brains) | the **empirical** knowledge base, quoting the matched entry |

The lanes are deliberately disjoint. A written rule with no empirical entry belongs
to `repo_code`; a pattern with no written rule belongs to `repo_learnings`; a
generic defect with neither belongs to `general`. A finding the learnings role
cannot tie to a knowledge-base entry is dropped rather than emitted.

## The two repo-owned brains

Physical skills, one copy each:

- `.claude/skills/campaign-service-code-reviewer/SKILL.md`
- `.claude/skills/campaign-service-learnings-reviewer/SKILL.md`

`.claude/skills/local-code-review` and `local-learnings-review` are symlinks to
those directories, and `.agents/skills/` exposes the same two physical
directories. The generic names are what the host selects; the declared `name:` in
each file's frontmatter is what a subagent loads, and the two deliberately differ.

## What gets reviewed

The host pins the revisions before any reviewer starts:

- `target_sha` — the newest commit on the working branch;
- `base_sha` — normally its **first parent**, optionally a wider base the caller
  supplies; absent only for a root commit.

The reviewed range is exactly `git diff <base_sha> <target_sha>`. Nothing fetches,
nothing consults a remote, and no reviewer derives a base, so the cycle works
offline. Evidence is read at the pinned revisions — staged, unstaged, untracked and
later working-tree content are barred as evidence for the target.

## The knowledge base and its floor

`docs/reviews/knowledge-base/` holds patterns extracted from verified past review
comments on this repo, each with a mechanical detect condition and full provenance,
plus `known-false-positives.md` — the floor.

The floor is read at **both** `base_sha` and `target_sha`, and suppresses a
candidate only when both would suppress that same finding. This is what stops a
change silencing findings about itself by adding its own waiver, while letting a
deleted waiver take effect immediately.

**Known limitation, deliberately unresolved:** ordinary pattern files are read at
the target only. A range that deletes or narrows the sole pattern catching a defect
it also introduces produces no candidate at all, and the floor cannot compensate
because a floor only ever removes candidates. The remedy — reading patterns from
the union of both revisions — was deferred as out of scope for the rollout that
introduced this subsystem. It is a recorded follow-up and is **not** solved.

## Harness

Cross-model review runs on Pi (GitHub Copilot `gpt-5.6-sol`, thinking high) loading
the exact physical skills.

When Pi is unavailable the host falls back to `.claude/skills/local-review-fallback`
— a launch table that starts three generic Claude subagents on model `opus` in one
parallel batch, naming the three skills to load and passing the host's pins through
unchanged. It carries no review criteria of its own.

The Claude Opus fallback is **not** the cross-model check Pi provides and must not
be presented as one. The harness is chosen once per cycle: Pi and Claude roles are
never mixed in a single cycle.

## Failure semantics

A reviewer that cannot do its job makes `INCOMPLETE — <reason>` its first line and
never pairs that with a no-findings conclusion. A role the host reports as failed or
empty is a host failure, never "no findings". Either way the **whole cycle** is
incomplete — successful siblings do not rescue it, and the remedy is to rerun the
complete trio on the same harness.

## Boundaries

The developer's own session fixes what the reviewers find, landing fixes as separate
signed conventional commits. Reviewers never edit tracked files, commit, push, or
write GitHub state. `local-agents/` — OAS agent homes and their worktrees — is
git-ignored and is not repository content.
