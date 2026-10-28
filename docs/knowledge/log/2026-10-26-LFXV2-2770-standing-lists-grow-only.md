# 2026-10-26 — Standing suppression lists only grow, survive create races, and are verified before reuse (LFXV2-2770)

**Fix** — review findings on the standing suppression lists of
2026-10-19-LFXV2-2770-standing-suppressions.

- `ensureStandardList` (`internal/dispatch/audience_suppression.go`) no longer replaces the
  standing list's filter with the resolved set. The new filter is the union of what it already
  covers and the resolved set, written only when that adds an id. Before, a quarter rotation, a term
  that stopped resolving, search lag or a hand-added list shrank the one list every audience in the
  project excludes at send time.
- `findOrCreateStanding`: a refused (name taken) or unconfirmed create of either standing list looks
  the name up again, three times over under two seconds, and reuses what it finds; it surfaced as a
  500 before.
- A list found under a standing name is verified before reuse: the standard list must be `DYNAMIC`
  (and a pure union of lists when it must grow), the registrants list `DYNAMIC` and selecting
  exactly that edition. Otherwise `audience.StandingListConflictError`, a 409 naming the list.
- `CurrentRegistrantsName` tags an edition too long to fit with a short hash of the full edition,
  so two long editions sharing a prefix no longer share (and cross-reuse) a list.
- Documented, not fixed: an edition with a year is not checked against real registration event
  names (no existing source can confirm a CURRENT edition), and the standing list is named from
  `project_id` as given (no UUID-to-slug resolver exists). See
  [internal/dispatch](../code/internal-dispatch.md).
- Tests: rotation, unresolved term, hand-added list, unchanged set, create races and a create that
  never appears, non-dynamic and wrong-edition lists, the 409 mapping, and the name hash.
