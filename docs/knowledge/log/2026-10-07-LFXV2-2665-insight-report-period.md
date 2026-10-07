# 2026-10-07 — LFXV2-2665 insight reports served only for their own period

**Fix** — Three late review threads on #289:

- **Calendar rollover served the wrong period** (both the Microsoft keyword AND audience reads —
  the freshness step is shared). A saved report was fresh by submission age and campaign scope
  alone, so a `this_month` report requested at 23:50 on 31 October was served at 00:10 on
  1 November as November's data (likewise `today` across midnight). Both reader interfaces now
  embed `InsightReportPeriod` (`ReportWindowDates`, the client's own UTC date rule), and
  `discardOtherPeriod` treats a finished report whose saved dates differ from the window's dates
  NOW as absent: a replacement is submitted and the read answers like the no-report case. The
  dates were already stored by 000038/000041, so no migration.
- **Impossible audience-builder examples**: `AudienceListBrief` and `AudiencePreviewCount` gained
  type-level `Example()`s; a generated-spec test checks both invariants in all four specs.
- **Label whitespace**: the age/gender fold checks the RAW cell for control/format characters
  before trimming ordinary spaces, so a leading tab or trailing CR/LF is refused rather than
  trimmed into a clean label.

See [Microsoft keyword insights](../architecture/microsoft-keyword-insights.md).
