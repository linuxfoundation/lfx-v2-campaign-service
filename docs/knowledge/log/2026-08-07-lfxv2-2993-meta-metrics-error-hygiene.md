# 2026-08-07 — LFXV2-2993 meta metrics error hygiene

**Update** — review found that the Meta metrics parser's own error messages were the
leak the surrounding code was built to prevent.

`safeErrSummary` in `internal/service/brief.go` exists because a platform error can carry
unvalidated upstream response text into a log record. It bounds the length and replaces
non-graphic runes — and that is all it can do, because it does not know whose text it is
holding. A short printable secret sitting in an upstream field passes through it intact.

Six branches in `internal/platform/meta/metrics.go` handed it exactly that. Every
malformed-row error interpolated the offending value with `%q`: both counters and all four
spend guards. `parseMetricInt` was worse than it looked, because even the branch that
returned `strconv`'s error unmodified was leaking — `ParseInt` renders the input inside its
own message (`parsing "...": invalid syntax`), so returning the error verbatim published
the value without a single `%q` in this package.

All six now report the field name, the reason, and the value's byte LENGTH. That is enough
to tell a truncated response from a wrong-typed one from an adversarially long one; the
bytes were never what made the diagnosis. Same discipline the LinkedIn parser's
`costInUsdToMicros` already followed — this file simply had not adopted it.

`TestGetCampaignMetrics_MalformedValuesAreNotEchoed` plants a marker string in each
malformed field and asserts it appears nowhere in the error chain, while still asserting a
diagnostic substring survives, so the fix cannot regress into an uninformative error either.
Reverting `parseMetricInt` to the raw `strconv` error fails the four counter cases and
leaves the four spend cases green, which is the localisation the test should have.

Two smaller corrections in the same pass:

- The concept doc said the window and the campaign id were "both validated against an
  allow-list". Only the window is. The id is matched against `numericIDRE`, because there is
  no enumerable set of valid campaign ids to allow-list. Describing a regex as an allow-list
  overstates the guarantee to the next reader deciding whether a new interpolated value needs
  its own check.
- The spend path rounded twice. `scaled` is already `math.Round(spend * 1e6)`, so the
  `int64(math.Round(spend))` at the construction site was a no-op on an integral float. The
  intermediate now stays an `int64` from the point it is known to fit, which also removes the
  float variable that made the double round easy to miss.
