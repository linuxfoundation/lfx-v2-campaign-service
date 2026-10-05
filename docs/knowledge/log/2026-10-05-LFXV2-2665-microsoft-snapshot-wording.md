# 2026-10-05 — Microsoft keyword snapshot: wording and test name

**Docs** — Review follow-up to #259. The code comment, the snapshot test and both concepts said
Microsoft receives every keyword "exactly as written". It receives it un-redacted, but the
client's own validation still applies (`validateKeywords` trims text, canonicalizes the match
type and drops case-insensitive duplicates), so the wording now says "not snapshot-redacted".
`TestMicrosoft_ConfigSnapshotScrubsTimeZoneKeepsKeywordsVerbatim` is renamed
`TestMicrosoft_ConfigSnapshotScrubsTimeZoneAndKeywords`, since it asserts keyword redaction. The
#259 log entry's "exactly as written" phrasing is superseded by this entry. No behaviour change.
