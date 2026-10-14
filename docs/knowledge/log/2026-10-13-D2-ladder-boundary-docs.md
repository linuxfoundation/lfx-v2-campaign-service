# 2026-10-13 — D2 ladder docs state the exact boundaries

**Docs** — late PR #305 review threads. The decision record, the api-catalog brief-metrics row,
a test comment and the 2026-10-13 D2 log entry described the brief path's migration as
"90–100% … and 100–130% …", which overlaps at 100% and wrongly includes exactly 90%. The exact
change (pinned by `PacingLadder.Label` and the boundary tests) is: values above 90% through 100%
moved from `normal` to `constrained` (exactly 90% stays `normal`; exactly 100% is in this band),
and values above 100% through 130% moved from `constrained` to `overspending`. The catalog, the
`internal-service-rules` concept and the test comment now say so; the merged D2 log entry is left
as written (one file per entry) and is corrected by this one. `docs/architecture.md`'s five-band
table (with a `Severe` band above 130% that no code returns) is replaced by the four
`StandardLadder` bands.
