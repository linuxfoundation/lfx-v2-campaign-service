# 2026-11-25 — Newsletter PR review fixes: exact link matching, blank-value rules described

**Fix** — the first two rounds of PR review on #685 (Greptile, Copilot and lfx-one). Each finding was read
against the code and confirmed.

- **The newsletter migration is `000060`.** `main` merged its own `000059_creative_assets_pdf`
  (#683) while this was in review, and two migrations cannot share a version. The newsletter pair
  is renumbered and every doc, test message and comment that named it follows.
- **A newsletter link must match the notes exactly, slash, query and fragment included.**
  `normalizeLinkForMatch` dropped the fragment and trimmed every trailing "/" from the serialised
  address, which also cut one off a query value. Notes with `/report#results` vouched for
  `/report#invented`, `/report` for `/report/`, and `?file=reports/` for `?file=reports`. Only
  scheme and host case are folded now, and an empty path reads as "/", the one equivalence
  RFC 3986 makes. A link that differs loses its anchor and keeps its words, the safe way to be
  wrong.
- **Punctuation inside a delimited address is the address's own.** `sourceDocumentLinks` trimmed
  trailing punctuation everywhere, so notes with `<https://example.org/d?token=abc!>` vouched for
  `?token=abc`. Inside "<…>" or matching quotes it is now kept; open prose is trimmed as before.
- **An anchor with two `href`s loses its link.** The tokenizer reports only the first `href`,
  but a kept tag was written back from its raw bytes with both, and a mail client may follow
  either. An anchor whose raw tag carries more than one `href` is now dropped, even when both are
  vouched for.
- **A list rule that also admits blank values says so.** `describeOperation` ignored
  `includeObjectsWithNoValueSet`, so "country IS_EQUAL_TO Japan" with the flag on read as
  "Country/Region is “Japan”" and `complete: true`, though HubSpot also admits every contact
  with no country. The condition now ends ", or is blank".

The parity test comment now counts sixteen audience-builder leaves, six of them two segments deep.

Tests: `email_source_links_test.go` (`TestNormalizeLinkForMatch`, `TestSourceDocumentLinks`,
`TestUnlinkUnvouchedAnchors`, `TestSourceDocumentLinks_DelimitedKeepsPunctuation`, `TestUnlinkUnvouchedAnchors_DuplicateHref`) and `describe_test.go` (`TestDescribeRules_BlankValuesWiden`). With
either fix reverted, its tests fail.
