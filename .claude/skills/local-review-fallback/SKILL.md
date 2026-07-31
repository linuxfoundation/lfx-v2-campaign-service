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

## Give each subagent its exact skill

A skill declares its `name:` in YAML frontmatter, which may differ from its alias directory. Resolve each selected `SKILL.md` to an absolute physical path and read only its frontmatter to obtain that declared name.

For each generic subagent:

1. If the harness has the declared skill registered, tell the subagent to load it by that declared name.
2. Otherwise, tell the subagent to read the exact absolute `SKILL.md` path in full and follow it as its entire rulebook.

The by-path arm is required for a subagent launched from a session where the plugin or another repo's project skills are not registered. Reading the one selected physical skill is not copying it: never paste its body into the prompt, never restate or summarize its rules, and never discover an ambient substitute.

Fail the role if the selected path is missing, unreadable or empty, or if the subagent cannot load/read that exact skill. Never continue with no rulebook or a different skill.

Forbid ambient instruction discovery, but not evidence reads directed by the selected skill.

Pass unchanged to every subagent: `target repo`, `target_sha`, `base_sha` (or literal `none`), the exact `review exactly:` range, and any `extra` hint. Use the pins from the single harness decision; never rerun the launcher to obtain them.

A subagent error, empty result, or non-review Markdown is a role-labelled all-Claude host failure. Never call it no findings and never synthesize reviewer `INCOMPLETE`. A reviewer-returned first-line `INCOMPLETE — <reason>` passes through. Any failure invalidates the cycle; rerun all three on Claude, never one role and never a mixed harness.
