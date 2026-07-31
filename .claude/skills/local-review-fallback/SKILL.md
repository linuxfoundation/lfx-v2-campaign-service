---
name: local-review-fallback
description: Launch the three local pre-PR reviewers as Claude subagents when the lfx-local-review host reports that Pi is unavailable. A launch table only — it carries no review criteria of its own.
---
<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Local review — Claude fallback

The `lfx-local-review` host has already decided the harness and printed its pins. Launch three reviewers and nothing else. This is a launch table: review criteria, severities, floor rules and KB knowledge stay in the selected skills.

## Launch exactly three generic subagents in one parallel batch

| Role | Selected skill file |
|---|---|
| `general` | the absolute central general-skill path supplied by the caller |
| `repo_code` | `.claude/skills/local-code-review/SKILL.md` |
| `repo_learnings` | `.claude/skills/local-learnings-review/SKILL.md` |

The caller resolves central general. Resolve only the two repo paths. For each selected file, read only its YAML frontmatter to obtain the declared `name:`. Tell that subagent to load the registered skill by that declared name and follow it as its entire rulebook. Pass the absolute physical path to identify and verify the selection; do not read, paste or restate the skill body in the prompt. If the declared skill is not registered or cannot be loaded, fail that role rather than silently reading the file as ordinary text.

Forbid ambient instruction discovery, but not evidence reads directed by the loaded skill.

Pass unchanged to every subagent: `target repo`, `target_sha`, `base_sha` (or literal `none`), the exact `review exactly:` range, and any `extra` hint. Use the pins from the single harness decision; never rerun the launcher to obtain them.

A subagent error, empty result, or non-review Markdown is a role-labelled all-Claude host failure. Never call it no findings and never synthesize reviewer `INCOMPLETE`. A reviewer-returned first-line `INCOMPLETE — <reason>` passes through. Any failure invalidates the cycle; rerun all three on Claude, never one role and never a mixed harness.
