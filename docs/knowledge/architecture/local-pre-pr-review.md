---
type: "Architecture Doc"
title: "Local pre-PR review"
description: "How the short pre-PR review block in CLAUDE.md points at the central /lfx-skills:lfx-pre-pr-review skill and carries the two repo-owned values: the knowledge-base review skill the central skill reads, and the deterministic preflight the block's own second step runs. Also where the repo's two review brains live, and how /lfx-skills:lfx-local-review reaches them."
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
| Preflight | `make check-fmt && make lint && make build && make test && go run ./cmd/okfvalidate ./docs/knowledge` | the deterministic checks CI also runs; step 2 of the block runs it, the review skill does not |

These two lines are the only facts about the lifecycle that belong to this
repo. Everything else about the review round — how it is launched, how the
range is pinned, what a failed reviewer means, when the fix commit happens — is
the central skill's business, and a fix to it is one change there rather than
one per repo.

## The repo-owned brains

Two repo-owned review skills, one physical copy of each, each also exposed under
`.agents/skills/<name>` as a symlink to its directory:

| Skill (declared `name:`) | File | Role |
|---|---|---|
| `campaign-service-learnings-reviewer` | `.claude/skills/campaign-service-learnings-reviewer/SKILL.md` | the knowledge-base role: the `KB review skill` of the pre-PR round, and `repo_learnings` for `/lfx-skills:lfx-local-review` |
| `campaign-service-code-reviewer` | `.claude/skills/campaign-service-code-reviewer/SKILL.md` | `repo_code` for `/lfx-skills:lfx-local-review`; not part of the pre-PR round |

The learnings brain matches the reviewed range against
`docs/reviews/knowledge-base/` — patterns extracted from verified past review
comments on this repo, each with a mechanical detect condition — and cites the
matched entry in every finding. A finding it cannot tie to an entry is dropped.

The code brain carries **no rules of its own**. The line-level method is
`.github/skills/campaign-service-code-review/SKILL.md`, which the PR-side
Copilot reviewer also uses; the code brain tells its reviewer to read that file
at the pinned `target_sha` and apply it, with the local scope, bar and report
format in place of the PR-only parts. That keeps one source for the method.
Copilot needs the file under `.github/`, and a Claude session registers skills
only from `.claude/skills/`, which is why the brain is a pointer and not a copy.
Do not paste the method into it: two copies drift.

`/lfx-skills:lfx-local-review` (the launcher in the installed plugin) looks for
the two brains at fixed paths, `.claude/skills/local-code-review/SKILL.md` and
`.claude/skills/local-learnings-review/SKILL.md`, and then loads each one by the
`name:` in its frontmatter. Both paths are tracked symlinks to the directories
above, not copies. Without them its probe fails with "no repo_code reviewer
skill … this repo does not own local review brains" before any reviewer starts
(#266). The pre-PR round in `CLAUDE.md` does not use them.

The repo's **written** conventions (`CLAUDE.md`, README, docs, Makefile,
checklists) are also the central general reviewer's surface; it reads them
from the repo at review time. When the repo adopted the pre-PR block it retired
an earlier conventions reviewer of the same name, the alias symlinks and the
`local-review-fallback` launch table (2026-09-25). That left
`/lfx-skills:lfx-local-review` with no `repo_code` brain to load, so the name
and the two aliases came back for #266 (2026-11-06), this time as a pointer to
the shared method. What the retired skill knew beyond the repo's docs is still
entries 9 and 10 of `docs/reviews/knowledge-base/known-false-positives.md`. The
`local-review-fallback` table stays retired: the launcher falls back to its own
table when a repo has none.

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

## Sibling claims in comments

A uniqueness or counting claim in a comment — "the one platform", "the only
probe", "the strongest of the six" — is a claim about every sibling in the repo.
It goes stale when any sibling changes, in a file the change never touched, so no
sweep keyed on "what did this commit touch" will find it. LFXV2-2665 hit this
squarely: `reddit.Client.VerifyAccount` said Reddit was the only probe to read its
configured account directly, which was true when written and was made false by
adding X's probe three files away.

The convention that follows: state a cross-cutting fact **once**, in
`docs/api-catalog.md`, which is the roster of record. A comment says what is true
of the code it sits on; where it must place that against siblings, it names **the
reason the relationship holds** rather than re-enumerating the roster, so the claim
is falsifiable where it stands. Before the pre-PR review round, sweep the branch's
added comments for this class — it is mechanical, and it front-runs the findings
this repo generates most.

## Boundaries

The round stops at PR-open: it never writes a GitHub label, status, check,
review or approval, and it does not touch `.github/**` or the PR-side pipeline
(`.github/copilot-instructions.md` and `.github/skills/**` are a different
reviewer with its own lifecycle; the local code brain only reads one file there,
at a pinned revision). Reviewers never edit tracked files, commit,
push, or write GitHub state; the developer's own session acts on what they
report, as the central skill directs. When a fix commit changes what the code
does, the knowledge-bundle upkeep rules in `CLAUDE.md` apply to it.
`local-agents/` — OAS agent homes and their worktrees — is git-ignored and is
not repository content.
