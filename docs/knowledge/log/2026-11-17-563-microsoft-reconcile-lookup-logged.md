# 2026-11-17 — #563 Microsoft bid reconcile logs its lookup failures

**Fix** — From the Copilot review of PR #623. When the bid write reconciles an unconfirmed ad group by name and `FindAdGroupByName` fails, `reconcileMicrosoftUnaddressedAdGroups` used to drop the error. That left a blocked bid update with no diagnostic. It now logs a warning with the project, campaign and entry label and the cause. The client-safe 409 reason still carries only the entry label.
