# 2026-09-25 Adopt the single-round pre-PR review block; retire the repo conventions reviewer

**Update** — The `## Local work cycle — post-commit and pre-PR review` section of `CLAUDE.md`,
which launched `/lfx-skills:lfx-local-review` after every commit and reran a three-reviewer
trio after each fix, is replaced by the central LFX `## Pre-PR review` block (the
with-knowledge-base variant): one full-branch review pinned at `git merge-base origin/main
HEAD`, two parallel background reviewers — `/lfx-skills:lfx-general-code-review` and
`/campaign-service-learnings-reviewer` — at most one fix commit, the deterministic checks
(`make check-fmt && make lint && make test && go run ./cmd/okfvalidate ./docs/knowledge`),
then the PR. No reviewer is rerun on the fix commit and nothing local runs once the PR exists.

The general reviewer now reads this repo's written conventions from the repo itself, so the
repo-owned conventions brain `.claude/skills/campaign-service-code-reviewer` is deleted, along
with the `local-review-fallback` launch table and the `local-code-review` /
`local-learnings-review` alias symlinks (and their `.agents/skills/` mirrors). The one
remaining review skill is `campaign-service-learnings-reviewer`; only its frontmatter changed,
to say it is launched by the pre-PR review block rather than "the trio". [Local pre-PR
review](../architecture/local-pre-pr-review.md) was rewritten to describe the new shape, and
the knowledge-base README's two references to the trio and the retired code reviewer were
reworded. Historical log entries that mention the post-commit cycle are left as written.
Part of linuxfoundation/lfx-self-serve#2945.
