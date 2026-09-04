---
type: "Architecture Doc"
title: "Local pre-PR review"
description: "Repo-owned review content for the local pre-PR review — the code-review rules and the empirical learnings knowledge base — and its known unresolved target-only pattern limitation; not the review lifecycle or its configuration."
resource: "docs/reviews/knowledge-base/README.md"
---

# Local pre-PR review

This concept documents **what this repo owns as review content**, and one known
limitation of that content. It does not define review timing, modes, ranges,
batching, checks, failure or rerun behaviour, Post-PR behaviour, or any
declaration value; none of that is duplicated here. The review lifecycle is
owned centrally, and this repo's exact live configuration lives solely in the
`## Review lifecycle configuration` section of `CLAUDE.md`.

## What the repo owns

- **Code-review content**: the written rules of this repository that its
  code-review role audits against and quotes verbatim.
- **Learnings-review content**: the empirical knowledge base under
  `docs/reviews/knowledge-base/` — patterns extracted from verified past review
  comments on this repo, each with a mechanical detect condition and its
  provenance — together with the repo's known-false-positive floor.

## Known limitation, deliberately unresolved

Ordinary knowledge-base pattern files are read at the target revision only. A
range that deletes or narrows the sole pattern that would catch a defect it also
introduces can therefore produce no candidate at all, and the known-false-positive
floor cannot compensate, because the floor only ever suppresses candidates. The
remedy — reading the union of the base and target pattern sets — was deferred as
out of scope for the rollout that introduced this subsystem and remains unsolved.
It is a recorded follow-up.
