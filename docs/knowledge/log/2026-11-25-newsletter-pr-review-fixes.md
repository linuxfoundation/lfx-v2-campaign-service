# 2026-11-25 — Newsletter PR review fixes: exact link matching, blank-value rules described

**Fix** — the PR review of #685 (three rounds, plus the final Greptile and Copilot pass) (Greptile, Copilot and lfx-one). Each finding was read
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
- **Query order no longer breaks a newsletter link.** `parseEmailCopyResponse` sanitises every
  anchor through `httpURL`, which re-encodes the query and so sorts it, before the link check runs.
  Notes with `?year=2026&format=pdf` never matched the sanitised `?format=pdf&year=2026`. The
  check now compares queries in the same encoding: parameter order is forgiven, names and values
  are not.
- **A newsletter staged without copy is left as cloned.** It fell through to the full rebuild,
  and the clone's logo borrowed as the hero was enough to trigger it, dropping last issue's body
  and the brand's footer. Only the subject is set now, with a warning that the draft still holds
  the previous issue.
- **An active list with no rules in HubSpot's answer is unreadable.** `DescribeList` said "matches
  no contacts" with `complete: true` when the filter tree was absent; that is now `unreadable` and
  incomplete. An empty tree that did come back is unchanged.
- **Describing lists has one deadline.** `DescribeLists` made up to 25 sequential reads plus
  lookups with only a per-attempt timeout. `describeListsTimeout` (25 s) now bounds the request;
  an unread list is `unreadable`, and a batch with nothing read is still an error.
- **A list rule that also admits blank values says so.** `describeOperation` ignored
  `includeObjectsWithNoValueSet`, so "country IS_EQUAL_TO Japan" with the flag on read as
  "Country/Region is “Japan”" and `complete: true`, though HubSpot also admits every contact
  with no country. The condition now ends ", or is blank".

The parity test comment now counts sixteen audience-builder leaves, six of them two segments deep.

Tests: `email_source_links_test.go` (`TestNormalizeLinkForMatch`, `TestSourceDocumentLinks`,
`TestUnlinkUnvouchedAnchors`, `TestSourceDocumentLinks_DelimitedKeepsPunctuation`, `TestUnlinkUnvouchedAnchors_DuplicateHref`, `TestEnforceSourceLinks_UnsortedQuerySurvivesSanitising`), `hubspot_newsletter_test.go` (`TestHubSpot_NewsletterWithoutCopyLeavesTheCloneAlone`), `audience_describe_test.go` (`TestDescribeLists_ADeadlineBoundsTheWholeBatch`) and `describe_test.go` (`TestDescribeRules_BlankValuesWiden`, `TestDescribeList_DynamicWithoutRules`). With
either fix reverted, its tests fail.
