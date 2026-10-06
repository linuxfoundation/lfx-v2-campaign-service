# 2026-10-05 — Microsoft keyword insights: pre-PR review fixes

**Fix** — Three findings on the new `GET /projects/{project_id}/microsoft-ads/keywords`:

- The window check ran the Google reads' helper first, whose 400 lists `yesterday` and
  `last_14_days`, then refused both. The Microsoft read now validates against its own window set
  and builds the message from it.
- The campaign scope was not de-duplicated. One Microsoft campaign can be held by two live rows
  (the scope query's DISTINCT includes the result blob; Microsoft has no live-row uniqueness
  index), so it was sent twice and counted twice against the 300 ceiling. Ids are now
  de-duplicated and the ceiling counts distinct campaigns; provenance is still checked per row.
- A malformed stored campaign id, and Microsoft rejecting the campaign-only scope (2027), were
  logged as transient submit failures on every read, leaving an empty 200 forever. A malformed
  id is now refused locally before any upstream call (`ErrKeywordReportScopeInvalid`, 409); a
  2027 is tagged `ErrServiceDefect` (500). Both fail the read.

Also: the response gains `data_incomplete`, the served report's "Potential Incomplete Data"
flag. See [Microsoft Keyword Insights](../architecture/microsoft-keyword-insights.md).
