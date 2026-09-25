---
type: "Architecture Doc"
title: "Local pre-PR review"
description: "How the short pre-PR review block in CLAUDE.md points at the central /lfx-skills:lfx-pre-pr-review skill and carries the two repo-owned values: the knowledge-base review skill the central skill reads, and the deterministic preflight the block's own second step runs."
resource: "CLAUDE.md"
---

# Local pre-PR review

A review this repo runs from a working copy **once per branch, after the
implementation is complete and before a pull request exists**. The procedure
is **not written in this repo**: it lives in the central LFX skill
`/lfx-skills:lfx-pre-pr-review`, and the `## Pre-PR review` section of
`CLAUDE.md` is a short pointer block pasted verbatim from the central template.
The block states the three steps between "implementation committed" and "PR
open" with the rules that must not drift out of mind, and carries the two values
that belong to the repo. This concept explains that shape; it does not restate
the procedure — read the block and the skill for that.

## The two values

| Value in the block | This repo's setting | What it is |
|---|---|---|
| KB review skill | `/campaign-service-learnings-reviewer` | the repo-owned knowledge-base reviewer, launched by the central skill as the knowledge-base role of the round; the only value the central skill reads |
| Preflight | `make check-fmt && make lint && make test && go run ./cmd/okfvalidate ./docs/knowledge` | the deterministic checks CI also runs; step 2 of the block runs it, the review skill does not |

These two lines are the only facts about the lifecycle that belong to this
repo. Everything else about the review round — how it is launched, how the
range is pinned, what a failed reviewer means, when the fix commit happens — is
the central skill's business, and a fix to it is one change there rather than
one per repo.

## The repo-owned brain

One physical skill, one copy: `.claude/skills/campaign-service-learnings-reviewer/SKILL.md`,
also exposed at `.agents/skills/campaign-service-learnings-reviewer` as a symlink
to that directory. The declared `name:` in its frontmatter is what the central
skill launches. It matches the reviewed range against
`docs/reviews/knowledge-base/` — patterns extracted from verified past review
comments on this repo, each with a mechanical detect condition — and cites the
matched entry in every finding. A finding it cannot tie to an entry is dropped.

The repo's **written** conventions (`CLAUDE.md`, README, docs, Makefile,
checklists) are the central general reviewer's surface; it reads them from the
repo at review time and carries no rulebook of its own. There is no repo-owned
conventions reviewer any more: the earlier `campaign-service-code-reviewer`
skill, the `local-code-review` and `local-learnings-review` alias symlinks, and
the `local-review-fallback` launch table were retired when the repo adopted the
block. What that skill knew beyond a restatement of the repo's docs was
salvaged into `docs/reviews/knowledge-base/known-false-positives.md` (entries
9 and 10). Do not reintroduce a conventions reviewer.

## The knowledge base and its floor

`docs/reviews/knowledge-base/` holds the empirical patterns plus
`known-false-positives.md` — the floor. The learnings reviewer applies the
floor last and reads it at **both** ends of the reviewed range, suppressing a
candidate only when both revisions would suppress that same finding. This is
what stops a change silencing findings about itself by adding its own waiver,
while letting a deleted waiver take effect immediately. Because the round
reviews the whole branch from the pinned `base_sha`, which predates every
commit on it, a waiver added anywhere on the branch can never suppress a
finding about that branch.

**Known limitation, deliberately unresolved:** ordinary pattern files are read at
the target only. A range that deletes or narrows the sole pattern catching a defect
it also introduces produces no candidate at all, and the floor cannot compensate
because a floor only ever removes candidates. The remedy — reading patterns from
the union of both revisions — was deferred as out of scope for the rollout that
introduced this subsystem. It is a recorded follow-up and is **not** solved.

## Boundaries

The round stops at PR-open: it never writes a GitHub label, status, check,
review or approval, and it does not touch `.github/**` or the PR-side pipeline
(`.github/copilot-instructions.md` and `.github/skills/**` are a different
reviewer with its own lifecycle). Reviewers never edit tracked files, commit,
push, or write GitHub state; the developer's own session acts on what they
report, as the central skill directs. When a fix commit changes what the code
does, the knowledge-bundle upkeep rules in `CLAUDE.md` apply to it.
`local-agents/` — OAS agent homes and their worktrees — is git-ignored and is
not repository content.
