# 2026-09-25 Adopt the single-round pre-PR review block; retire the repo conventions reviewer

**Update** — The `## Local work cycle — post-commit and pre-PR review` section of `CLAUDE.md`,
which launched `/lfx-skills:lfx-local-review` after every commit and reran a three-reviewer
trio after each fix, is replaced by the central LFX `## Pre-PR review` block. The block points
at `/lfx-skills:lfx-pre-pr-review`, which runs one review round of the whole branch with three
reviewers in parallel — general, security and knowledge-base — right before the PR. The
procedure itself is not restated in this repo; the block carries only the two repo-owned values,
`KB review skill: /campaign-service-learnings-reviewer` (the one value the central skill reads) and
`Preflight: make check-fmt && make lint && make build && make test && go run ./cmd/okfvalidate ./docs/knowledge`
(run by the block's own second step, not by the skill).

Deleted, because the central general reviewer now reads this repo's written conventions from
the repo itself: the repo-owned conventions brain `.claude/skills/campaign-service-code-reviewer`,
the `local-review-fallback` launch table, and the `local-code-review` / `local-learnings-review`
alias symlinks (with their `.agents/skills/` mirrors).

Changed: `/campaign-service-learnings-reviewer` is the one remaining repo-owned review skill and
is the knowledge-base role of the three-reviewer round. Its frontmatter now says it is launched by
the central skill per the block rather than by "the trio", and its body was edited on the same
points: the two siblings are named as the central general and security reviewers (the retired
"code reviewer" role is folded into the general one wherever it was cited — sibling roles,
exclusions, report rules), and `base_sha` is described as the base the central skill pins rather
than the target's first parent. [Local pre-PR
review](../architecture/local-pre-pr-review.md) was rewritten to describe the new shape and its
index bullet updated, and the knowledge-base README's two references to the trio and the retired
code reviewer were reworded. Historical log entries that mention the post-commit cycle are left
as written. Part of linuxfoundation/lfx-self-serve#2945.

**Update** — Same day, second pass: the retired `campaign-service-code-reviewer` skill was read
once more for expertise worth keeping. Its known doc/code drift list (`project_id` is `TEXT`,
provider singletons are a partial unique index, `docs/build-summary.md` is a dated snapshot, the
Makefile's `GO_VERSION` pin is unused) and the `gen/`/`specs/`/`.specify/` exclusions the repo's
own tooling applies became entries 9 and 10 of
`docs/reviews/knowledge-base/known-false-positives.md`, dated and marked as carried over without
a PR thread. Everything else in it — review method, report format, and rules already stated by
`CLAUDE.md`, `docs/`, the migrations, the chart or the knowledge base — was dropped as
redundant. No `.claude/rules/` entry was needed: every convention it named is documented
elsewhere in the tree.

**Update** — PR review round 4: the `Preflight` value gains `make build`, in the position CI runs
it (`.github/workflows/lfx-v2-campaign-service-build.yaml`: check-fmt, lint, build, test), so the
block's step 2 catches a compilation failure before the PR; the value is now
`make check-fmt && make lint && make build && make test && go run ./cmd/okfvalidate ./docs/knowledge`
in `CLAUDE.md`, [Local pre-PR review](../architecture/local-pre-pr-review.md) and this log. Not
added, and carried as a CI-parity follow-up for the repo owners: `make apigen` (it regenerates
`gen/` and copies specs into `kodata/`, so it mutates the tree rather than checking it), and the
MegaLinter and license-header gates, which are separate CI-only workflows. The retained
`/campaign-service-learnings-reviewer` skill no longer mentions a "wider base the caller supplied"
or "a later range whose supplied base" carries a waiver: `base_sha` is the merge-base the central
skill computes against the PR target and pins once for the branch's single round, and the
later-waiver case is now stated as a later branch whose merge-base already carries it.
