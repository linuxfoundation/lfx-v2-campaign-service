# 2026-10-26 — attach-existing documents that it rewrites suppression_list_ids (LFXV2-2770)

**Docs** — `attach-existing-audience` records and returns `suppression_list_ids` that are not the
caller's: a full standard set becomes the standing list's id, and the current-registrants list's
id is appended. The Goa method and result descriptions (OpenAPI regenerated) and
`docs/api-catalog.md` now say so, and that an attach may create up to two lists, grows the
standing list only, and answers `409` naming a list that squats a standing name. The catalog also
records that `current_registrants_event` is checked only for a year.
