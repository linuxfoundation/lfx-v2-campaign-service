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

**Update** — Same day, second pass: the `## Pre-PR review` section of `CLAUDE.md` no longer
carries the lifecycle inline. It is now the short central pointer block — load
`/lfx-skills:lfx-pre-pr-review` and follow it; one fix commit for the round; no local reviews
once the PR is open — plus the two repo-owned values, `KB review skill:
/campaign-service-learnings-reviewer` and `Preflight: make check-fmt && make lint && make test
&& go run ./cmd/okfvalidate ./docs/knowledge`. The procedure has one authoritative home in the
central skill and is not restated anywhere in this repo; [Local pre-PR
review](../architecture/local-pre-pr-review.md) was rewritten again to describe that shape and
its index bullet updated. The retired `campaign-service-code-reviewer` skill was read once more
for expertise worth keeping: its known doc/code drift list (`project_id` is `TEXT`, provider
singletons are a partial unique index, `docs/build-summary.md` is a dated snapshot, the
Makefile's `GO_VERSION` pin is unused) and its `gen/`/`specs/`/`.specify/` formatting and
license-header exclusion became entries 9 and 10 of
`docs/reviews/knowledge-base/known-false-positives.md`, dated and marked as carried over without
a PR thread. Everything else in it — review method, report format, and rules already stated by
`CLAUDE.md`, `docs/`, the migrations, the chart or the knowledge base — was dropped as
redundant. No `.claude/rules/` entry was needed: every convention it named is documented
elsewhere in the tree.
