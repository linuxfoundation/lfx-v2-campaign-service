# 2026-11-25 — Newsletter PR review fixes: exact link matching, blank-value rules described

**Fix** — the first round of PR review on #685 (Greptile, Copilot and lfx-one). Each finding was read
against the code and confirmed.

- **A newsletter link must match the notes exactly, slash, query and fragment included.**
  `normalizeLinkForMatch` dropped the fragment and trimmed every trailing "/" from the serialised
  address, which also cut one off a query value. Notes with `/report#results` vouched for
  `/report#invented`, `/report` for `/report/`, and `?file=reports/` for `?file=reports`. Only
  scheme and host case are folded now, and an empty path reads as "/", the one equivalence
  RFC 3986 makes. A link that differs loses its anchor and keeps its words, the safe way to be
  wrong.
- **A list rule that also admits blank values says so.** `describeOperation` ignored
  `includeObjectsWithNoValueSet`, so "country IS_EQUAL_TO Japan" with the flag on read as
  "Country/Region is “Japan”" and `complete: true`, though HubSpot also admits every contact
  with no country. The condition now ends ", or is blank".

The parity test comment now counts sixteen audience-builder leaves, six of them two segments deep.

Tests: `email_source_links_test.go` (`TestNormalizeLinkForMatch`, `TestSourceDocumentLinks`,
`TestUnlinkUnvouchedAnchors`) and `describe_test.go` (`TestDescribeRules_BlankValuesWiden`). With
either fix reverted, its tests fail.
