# 2026-08-18 — LFXV2-3260 Microsoft metrics: bot-round fixes

**Fix** — Seven findings from Cursor and Copilot on cs#140, including one defect in the
previous round's own fix.

**My redaction fix was itself broken.** The previous commit wrapped `context.Cause(ctx)` on
the cancellation arm. `http.Client.Timeout` and any custom RoundTripper surface
`context.DeadlineExceeded` while the caller's context is still live — and `Cause` is nil
there, so the error rendered `%!w(<nil>)` and stopped matching `errors.Is` entirely. Probed
and confirmed before fixing. Both arms now wrap the sentinel itself.
`TestDownloadReportPreservesContextSentinels` pins it.

**Report scope ids went out as JSON strings.** Microsoft types `AccountIds`, `AccountId` and
`CampaignId` as `long`, and `campaign.go` already sends `AccountId` as `json.Number` — with a
comment recording that mistyping it rejects every create. `metrics.go` disagreed with its own
package, so the first live submit would have failed deserialization. The existing
`TestGetCampaignMetrics_SubmitBodyShape` could not catch this: it asserted against the
DECODED map, and decoding into `any` turns both a quoted string and a bare number into
`float64`. The test now asserts the **raw wire bytes**.

**`ReportTimeZone` was omitted, so every window was silently off by a day.** Microsoft
defaults the report timezone to Pacific; the dates are computed in UTC. Now sent explicitly.
The wire assertion covers it.

**Numeric folding had none of the guards its siblings have.** `meta/metrics.go` and
`reddit/metrics.go` both reject NaN, Inf, negative and out-of-range spend before scaling;
this client folded them in, and `int64(spend*1e6 + 0.5)` also rounds the wrong way for
negatives. Now uses `math.Round` behind the same guards, and rejects negative impressions and
clicks.

**`Success` with no download URL no longer returns zeroes.** The old comment justified a
"confirmed zero" on the grounds that Microsoft omits the file rather than shipping a
header-only CSV — a claim about response SHAPE on a contract this file declares UNVERIFIED,
and the one assumption whose failure was silent. It also cannot distinguish "served nothing"
from "no such campaign in this account's scope". Now answers `ErrNoRowsInReport`, which the
dispatcher maps to `domain.ErrNoMetricsInWindow` exactly as `hubspot.go` does. The old test
asserted the unsafe outcome and has been rewritten.

**A comment promised a sentinel that does not exist.** It claimed the dispatcher maps
`ErrReportNotReady` to `domain.ErrMetricsUnavailable`; no such symbol is defined anywhere.
The dispatcher deliberately returns an ordinary wrapped error, and the comment now says so.

**The OKF concept description omitted the Reporting client** — updated in both the concept
frontmatter and, verbatim, the index entry, as `okfvalidate` requires.
