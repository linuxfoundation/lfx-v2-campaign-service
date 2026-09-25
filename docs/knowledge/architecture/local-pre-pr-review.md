---
type: "Architecture Doc"
title: "Local pre-PR review"
description: "How the single pre-PR review block in CLAUDE.md runs the central general reviewer and the repo-owned learnings reviewer over the whole branch once, before a PR exists."
resource: "CLAUDE.md"
---

# Local pre-PR review

A review this repo runs from a working copy **once per branch, after the
implementation is complete and before a pull request exists**. The lifecycle is
the `## Pre-PR review` block in `CLAUDE.md`, pasted verbatim from the central
LFX template; that block is the only account of the lifecycle in this repo, and
this concept only explains the shape it produces. It stops at PR-open: it never
writes a GitHub label, status, check, review or approval, and it does not touch
`.github/**` or the PR-side pipeline.

## The two roles

| Role | Rulebook | What it may cite |
|---|---|---|
| general | central `/lfx-skills:lfx-general-code-review` | ordinary software quality **and** this repo's written rules (`CLAUDE.md`, README, docs, Makefile, checklists), quoted verbatim |
| learnings | [`campaign-service-learnings-reviewer`](#the-repo-owned-brain) | the **empirical** knowledge base, quoting the matched entry |

The lanes are deliberately disjoint. A written rule with no empirical entry
belongs to the general reviewer, which reads the rule surface from the repo at
the pinned revision and carries no rulebook of its own; a pattern with no written
rule belongs to the learnings reviewer. A finding the learnings role cannot tie
to a knowledge-base entry is dropped rather than emitted.

There is no longer a separate repo-owned conventions reviewer: the earlier
`campaign-service-code-reviewer` skill, the `local-code-review` and
`local-learnings-review` alias symlinks, and the `local-review-fallback` launch
table were retired when the repo adopted the single-round block. Do not
reintroduce them.

## The repo-owned brain

One physical skill, one copy: `.claude/skills/campaign-service-learnings-reviewer/SKILL.md`,
also exposed at `.agents/skills/campaign-service-learnings-reviewer` as a symlink
to that directory. The declared `name:` in its frontmatter is what a subagent
loads.

## What gets reviewed

The developer's session pins the revisions before either reviewer starts:

- `base_sha` — `git merge-base origin/main HEAD` after a fresh `git fetch origin`;
- `target_sha` — `HEAD`, the tip of the completed implementation.

The reviewed range is exactly `git diff <base_sha> <target_sha>` — the whole
branch, not one commit. **No reviewer derives or replaces the range**: the pins
come from the caller and are fixed. A reviewer may still make optional,
read-only GitHub calls to inform its judgement, as the skills permit; those never
change the range. Evidence is read at the pinned revisions — staged, unstaged,
untracked and later working-tree content are barred as evidence for the target.

## The knowledge base and its floor

`docs/reviews/knowledge-base/` holds patterns extracted from verified past review
comments on this repo, each with a mechanical detect condition and full provenance,
plus `known-false-positives.md` — the floor.

The floor is read at **both** `base_sha` and `target_sha`, and suppresses a
candidate only when both would suppress that same finding. This is what stops a
change silencing findings about itself by adding its own waiver, while letting a
deleted waiver take effect immediately. Because the reviewed range is now the
whole branch measured from `origin/main`, a waiver added anywhere on the branch
can never suppress a finding about that branch.

**Known limitation, deliberately unresolved:** ordinary pattern files are read at
the target only. A range that deletes or narrows the sole pattern catching a defect
it also introduces produces no candidate at all, and the floor cannot compensate
because a floor only ever removes candidates. The remedy — reading patterns from
the union of both revisions — was deferred as out of scope for the rollout that
introduced this subsystem. It is a recorded follow-up and is **not** solved.

## Harness

Both reviewers run as independent background Claude subagents
(`subagent_type: general-purpose`, `model: opus`) launched in parallel, each
told to load exactly one skill with the Skill tool and follow it. There is no
Pi harness and no fallback table in this repo any more.

## Failure semantics

A reviewer that cannot do its job makes `INCOMPLETE — <reason>` its first line and
never pairs that with a no-findings conclusion. A failed, empty or `INCOMPLETE`
report is not a clean review: the developer's session fixes the cause and
relaunches **that reviewer once**; if it fails again the session stops and tells
the developer.

## Boundaries

The developer's own session fixes what the reviewers find, in **exactly one**
signed, DCO-signed-off fix commit (or none), then runs the deterministic checks
named in the block and opens the PR. The reviewers are never rerun on the fix
commit, and no local review of any kind runs once the PR exists. Reviewers never
edit tracked files, commit, push, or write GitHub state. `local-agents/` — OAS
agent homes and their worktrees — is git-ignored and is not repository content.
