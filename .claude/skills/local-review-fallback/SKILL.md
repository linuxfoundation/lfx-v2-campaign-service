---
name: local-review-fallback
description: Launch the three local reviewers as Claude subagents when lfx-local-review selects the Claude fallback.
---
<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Local review — Claude fallback

Launch exactly three generic subagents in one parallel batch:

| Role | Skill to load |
|---|---|
| `general` | `lfx-general-code-review` |
| `repo_code` | `campaign-service-code-reviewer` |
| `repo_learnings` | `campaign-service-learnings-reviewer` |

Give every subagent the same `target repo`, `target_sha`, `base_sha` (or `none`), exact `review exactly:` range and optional `extra` hint supplied by the host.

Tell each subagent: **Load the named skill and follow it exactly. Review only the supplied range and return an ordinary Markdown review.**

If any subagent errors, returns nothing or does not return a review, report a role-labelled Claude fallback failure and rerun all three. Never combine Pi and Claude roles in one cycle.
