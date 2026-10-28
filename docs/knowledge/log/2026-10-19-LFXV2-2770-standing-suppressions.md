# 2026-10-19 — Suppressions reuse one standard list per project; current registrants added

**Update** — every attach and compose built a new "<master> - Combined Suppression (<hash>)" list
that wrapped the same portfolio hygiene lists again, so the portal filled up with copies of one
union. This supersedes the per-selection combined attach list of
2026-10-16-LFXV2-2770-combined-attach-suppression-kernel (never released).

- `resolveSuppressions` (`internal/dispatch/audience_suppression.go`) is shared by `AttachExisting`
  and `ComposeMaster`. A COMPLETE set of resolved standard suppressions is replaced by the project's
  standing `LFX Standard Suppressions - <project>` list, found by name, created on first use and
  refreshed in place (`UpdateListFilters`) only when the resolved set changed. A partial standard
  selection is attached list by list. Brand, event-specific and Exclude-marked lists stay separate.
- Current registrants: a `current_registrants_event` (edition with year) on attach and compose finds
  or creates `<edition> - Current Registrants` from the registration custom event and records it as
  its own suppression. `GET suppression-lists` takes `registrants_event` and returns the row, with
  `will_create` when the list does not exist yet, plus `standard_list` when the standing list exists.
- Compose creates no suppression list of its own: the master excludes the standing lists directly
  (`MasterListWithSuppressionsFilter`), so only its own create can be partial.
- Merged with main: derived and versioned custom names are cut to `MaxListNameLength` (200, HubSpot's
  list-search limit) rather than refused, and the foundation-gate brand vote is unscoped because a
  project's lists are named for its events.
- Tests: `audience_suppression_test.go`, `builder_standard_test.go`, compose tests rewritten.
