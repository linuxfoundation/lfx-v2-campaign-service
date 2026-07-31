---
name: local-review-fallback
description: Launch the three local pre-PR reviewers as Claude subagents when the lfx-local-review host reports that Pi is unavailable. A launch table only — it carries no review criteria of its own.
---
<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Local review — Claude fallback

The `lfx-local-review` host has already decided the harness and printed its pins. Launch three reviewers and nothing else. This is a launch table: review criteria, severities, floor rules and KB knowledge stay in the selected skills.

## Launch exactly three generic subagents in one parallel batch

| Role | Skill name it loads |
|---|---|
| `general` | `lfx-general-code-review` |
| `repo_code` | `campaign-service-code-reviewer` |
| `repo_learnings` | `campaign-service-learnings-reviewer` |

## Give each subagent its skill by name

Tell each subagent to load its one registered skill by the name above and follow it as its entire rulebook. Those three names are fixed here; do not derive them at runtime.

Pass no skill path. Do not resolve a `SKILL.md` location, do not parse frontmatter, do not read a skill file as ordinary text, do not paste or restate its rules, and never adopt an ambient substitute.

If a named skill is unavailable, that role **fails loudly** and the whole Claude cycle is invalid. The remedy is to relaunch Claude from this repo with the project skills and the `lfx-skills` plugin registered — never to bypass skill loading by reading a path. A developer running this normally starts Claude from the repo with that plugin loaded, so all four skills are registered.

Forbid ambient instruction discovery, but not evidence reads directed by the loaded skill.

Pass unchanged to every subagent: `target repo`, `target_sha`, `base_sha` (or literal `none`), the exact `review exactly:` range, and any `extra` hint. Use the pins from the single harness decision; never rerun the launcher to obtain them.

A subagent error, empty result, or non-review Markdown is a role-labelled all-Claude host failure. Never call it no findings and never synthesize reviewer `INCOMPLETE`. A reviewer-returned first-line `INCOMPLETE — <reason>` passes through. Any failure invalidates the cycle; rerun all three on Claude, never one role and never a mixed harness.
