// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// Microsoft Advertising reporting differs from every other platform client in this repo:
// every other platform client answers GetCampaignMetrics with ONE synchronous request, while Microsoft's
// Reporting service is a three-step asynchronous pipeline —
//
//	POST Reporting/v13/GenerateReport/Submit  -> a ReportRequestId
//	POST Reporting/v13/GenerateReport/Poll    -> Pending | Success | Error (+ a download URL)
//	GET  <download URL>                       -> a ZIP containing one CSV
//
// The synchronous MetricsReader contract is what the orchestrator's live-read endpoint
// calls, so this client CANNOT wait indefinitely: the platform ingress times out at 60s and
// a report can take longer than that to build. The poll loop is therefore bounded by
// reportPollBudget and gives up with ErrReportNotReady rather than hanging the caller. A
// report that is still building is a "ask again later" (with a fresh report — see below), not
// a failure of the campaign. The
// dispatcher deliberately does NOT map it to either metrics sentinel — both mean 400 ("this
// cannot work"), and a retryable timing condition is neither unsupported nor permanent — so
// it propagates as an ordinary wrapped error. There is no retryable sentinel in
// internal/domain/errors.go today; adding one is what this comment once assumed. Note that
// "retry" here means "submit a fresh report and hope it builds faster", NOT "collect the one
// already building" — see ErrReportNotReady, which spells out why the pending id cannot
// survive the call.
//
// UNVERIFIED CONTRACT: no Microsoft Advertising credentials were available when this was
// written, so the request/response shapes below follow Microsoft's published v13 Reporting
// API and have NOT been exercised against the live service. Every field this code reads is
// therefore treated as optional-and-checked rather than assumed present. Reads are gated
// behind MICROSOFT_METRICS_ENABLED (see the dispatcher) until someone runs them against a
// real account — the same gate reddit metrics shipped behind for the same reason.

const (
	// msReportingBaseURL is the Reporting service origin. Microsoft splits its API across
	// hosts by service: campaign.api for CampaignManagement, clientcenter.api for
	// CustomerManagement, and reporting.api for Reporting. apiVersion is shared — all
	// three services are versioned in lockstep at v13.
	msReportingBaseURL = "https://reporting.api.bingads.microsoft.com"

	// reportPollBudget bounds the ENTIRE submit+poll phase. The binding deadline is NOT
	// the 60s platform ingress: Orchestrator.ReadCampaignMetrics wraps every metrics call
	// in metricsCallTimeout (20s), so the budget must sit under THAT or the caller's
	// context cancels first and ErrReportNotReady can never be produced — the sentinel,
	// its message and the dispatcher's classification arm would all be dead code.
	// 15s leaves headroom for the download, which is deliberately NOT part of this budget:
	// once Success is reported the file exists and the transfer is a plain GET, so cutting
	// it off would discard a report we already paid to build.
	//
	// TestReportPollBudgetStaysUnderTheMetricsCallTimeout pins this relationship.
	reportPollBudget = 15 * time.Second

	// reportPollInterval is the delay between Poll calls. Microsoft documents a
	// recommended floor of ~1s; polling faster earns 429s that the shared retry policy
	// would then have to absorb.
	reportPollInterval = 1 * time.Second

	// reportDownloadCap bounds the ZIP read. A campaign-scoped, aggregate-over-window
	// report is a handful of rows, so anything approaching this is a contract surprise
	// (a per-day or per-keyword breakdown) and is refused rather than parsed.
	reportDownloadCap = 8 << 20 // 8 MiB
)

// msErrCodeInvalidScope / msErrNameInvalidScope are Microsoft's reporting error for a
// report Scope it will not accept, surfaced either as the numeric Code 2027 or as the
// symbolic ErrorCode enum — the same dual spelling errCodeDuplicateCampaign handles for the
// Campaign Management service, and the reason both are matched. submitReport names this pair
// when the campaign-only scope is rejected, so the first live run against a real ad account
// reads the cause instead of debugging it. See the Scope comment in submitReport.
const (
	msErrCodeInvalidScope = "2027"
	msErrNameInvalidScope = "InvalidAccountThruCampaignReportScope"
)

// ErrReportNotReady means the report was still building when reportPollBudget ran out.
// It is NOT a failure of the campaign or the credentials: the same call may well succeed
// on a later attempt. Callers must not treat it as "this campaign has no metrics".
//
// A RETRY DOES NOT RESUME THE PENDING REPORT. reportID is a local in GetCampaignMetrics and
// is discarded with this error, and the synchronous MetricsReader contract has nowhere to
// return it — ReadMetrics answers (*model.CampaignMetrics, error) and the orchestrator never
// persists anything from a metrics read. So every retry calls submitReport afresh and starts
// a NEW Microsoft report job; it never polls the one that has since finished. The practical
// consequence: a report that RELIABLY takes longer than reportPollBudget to build is
// permanently unreadable through this path, however many times the caller retries, because
// each attempt restarts the clock. Retrying only helps when the build time varies around the
// budget.
//
// Resuming would need the pending ReportRequestId to outlive the call — a persisted
// report-job row plus an async completion path, i.e. a schema and orchestration change, not
// a change to this client. That gap is filed in
// docs/knowledge/log/2026-08-18-LFXV2-3260-scope-union-tradeoff.md. Until it is built, this
// sentinel means "the report was not ready in time", NOT "the same job will be waiting for
// you".
var ErrReportNotReady = errors.New("microsoft report not ready within the poll budget (a retry starts a NEW report; it does not resume this one)")

// ErrNoRowsInReport means the report completed but carried no data. It covers BOTH shapes
// that condition arrives in: a poll that reported Success while naming no file to download,
// and a downloaded CSV whose header is followed by zero data rows. The adapter cannot tell
// "the campaign served nothing" from "no such campaign in this account's scope", so it
// refuses to render either as a measured zero — and since the two shapes carry identically
// little information, they answer with the same sentinel.
var ErrNoRowsInReport = errors.New("microsoft report completed with no downloadable rows")

// ErrUnsupportedWindow is returned for a model.MetricsWindow this client does not map to a
// Microsoft date range. Mirrors the reddit client's sentinel so the dispatcher can classify
// an unsupported window separately from a genuine read failure.
var ErrUnsupportedWindow = errors.New("unsupported metrics window")

// GetCampaignMetrics returns campaign-scoped metrics for window, aggregated over the whole
// window (one row), by driving Microsoft's asynchronous report pipeline to completion.
//
// campaignID is the numeric Microsoft campaign id persisted by the create path. The window
// maps to an explicit start/end date pair rather than one of Microsoft's named
// predefined ranges, because the named ranges are defined in the ACCOUNT's timezone while
// this service's vocabulary is UTC — two different days near a month boundary.
//
// Returns ErrReportNotReady if the report is still building when the budget expires, and
// ErrUnsupportedWindow for a window with no mapping. Both are sentinels the caller is
// expected to classify; neither means the campaign has zero activity.
//
// reportID is deliberately NOT returned alongside ErrReportNotReady: there is no channel for
// it (see the sentinel's doc comment), so a caller cannot resume a pending report and a
// retry restarts one. Do not add a comment here claiming otherwise.
func (c *Client) GetCampaignMetrics(ctx context.Context, campaignID string, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	if strings.TrimSpace(campaignID) == "" {
		return nil, fmt.Errorf("microsoft campaign id is required")
	}
	if !accountIDRE.MatchString(campaignID) {
		// Same discipline as the account ids: this value reaches a request BODY, and a
		// non-numeric campaign id is a caller bug rather than something to forward.
		return nil, fmt.Errorf("invalid Microsoft campaign id %q: must be digits only", clipID(campaignID))
	}
	start, end, err := reportDateRange(window, c.now())
	if err != nil {
		return nil, err
	}

	reportID, err := c.submitReport(ctx, campaignID, start, end)
	if err != nil {
		return nil, err
	}
	downloadURL, err := c.pollReport(ctx, reportID)
	if err != nil {
		return nil, err
	}
	// A Success status with no download URL is reported as ErrNoRowsInReport, NOT as
	// zeroes. The earlier revision returned a zero here on the reasoning that Microsoft
	// omits the file rather than shipping a header-only CSV — but that is a claim about
	// response SHAPE on a contract this file declares UNVERIFIED, and it is the one
	// assumption whose failure is silent. This adapter also cannot distinguish "no
	// activity" from "no such campaign in this account's scope", which is exactly why
	// domain.ErrNoMetricsInWindow exists; the dispatcher maps this sentinel onto it, the
	// same way internal/dispatch/hubspot.go does.
	if downloadURL == "" {
		return nil, ErrNoRowsInReport
	}
	return c.downloadReport(ctx, downloadURL, campaignID, window)
}

// submitReport posts the report definition and returns the ReportRequestId.
func (c *Client) submitReport(ctx context.Context, campaignID string, start, end time.Time) (string, error) {
	body := map[string]any{
		"ReportRequest": map[string]any{
			"Type":                   "CampaignPerformanceReportRequest",
			"Format":                 "Csv",
			"ReturnOnlyCompleteData": false,
			"Aggregation":            "Summary",
			"Columns": []string{
				"CampaignId", "Impressions", "Clicks", "Spend",
			},
			// Scope carries ONLY Campaigns. AccountThroughCampaignReportScope — the type of
			// CampaignPerformanceReportRequest.Scope — documents, on both of its elements,
			// that "the report scope includes a UNION of the AccountIds and Campaigns
			// elements", and the XSD agrees (both minOccurs="0" in an xs:sequence, not an
			// xs:choice). Sending AccountIds alongside Campaigns therefore widened this
			// campaign-scoped read to EVERY campaign in the account, which foldReportRows
			// then summed into an account-wide total reported as one campaign's metrics —
			// a valid request, no error raised, and a silently wrong number. That is the
			// failure-as-measurement class this file refuses everywhere else. The nested
			// AccountId inside Campaigns[] already scopes the request, so dropping
			// AccountIds loses nothing.
			//
			// KNOWN RISK, untestable here: community Q&A threads report error 2027
			// (InvalidAccountThruCampaignReportScope) when AccountIds is omitted. Those are
			// not normative — the docs require only "at least one of these elements" — and
			// no Microsoft credentials exist to settle it (see the UNVERIFIED CONTRACT
			// banner). We deliberately chose a correct-but-possibly-rejected request over
			// one that reliably returns the wrong number; submitReport names 2027 explicitly
			// in its error so the first live run diagnoses it in one read.
			//
			// Ids go out as json.Number, NOT as strings: Microsoft types these as `long`,
			// and campaign.go already sends AccountId that way — its comment records that
			// mistyping it rejects the request outright. A quoted id here would fail
			// deserialization the first time the gate is flipped on.
			"Scope": map[string]any{
				"Campaigns": []map[string]any{
					{
						"AccountId":  json.Number(c.account.AccountID),
						"CampaignId": json.Number(campaignID),
					},
				},
			},
			"Time": map[string]any{
				// toMSDate/msDate already exist in campaign.go for exactly this: Microsoft
				// rejects an ISO-8601 string for a date field and requires the
				// {Month,Day,Year} object.
				"CustomDateRangeStart": toMSDate(start),
				"CustomDateRangeEnd":   toMSDate(end),
				// ReportTimeZone is REQUIRED here even though it looks optional: Microsoft
				// defaults it to Pacific, so a UTC-computed window would aggregate a
				// different day than the dates above name. That is a silent off-by-one-day
				// on every window, rendered as a measurement.
				"ReportTimeZone": "GreenwichMeanTimeDublinEdinburghLisbonLondon",
			},
		},
	}
	// Submit is a POST that CREATES a report request, but it is safe to retry: a duplicate
	// submission costs an extra report build and returns a fresh id, it does not mutate
	// anything the caller can observe. idempotent=true buys the shared 429 policy.
	raw, err := c.doReportingRequest(ctx, http.MethodPost, "GenerateReport/Submit", body, true)
	if err != nil {
		// If Microsoft rejects the campaign-only scope, say so in the error itself rather
		// than leaving whoever first runs this against a live account to rediscover the
		// tradeoff recorded above. 2027 / InvalidAccountThruCampaignReportScope is the code
		// the community threads name; both spellings are matched because Microsoft returns
		// a numeric Code on some services and a string ErrorCode on others (see
		// parseErrorCodes/codeString).
		var ae *apiError
		if errors.As(err, &ae) && (ae.hasErrorCode(msErrCodeInvalidScope) || ae.hasErrorCode(msErrNameInvalidScope)) {
			return "", fmt.Errorf("submit microsoft report: the campaign-only report scope was REJECTED "+
				"(error %s/%s); Scope.AccountIds may be required after all — see "+
				"docs/knowledge/log/2026-08-18-LFXV2-3260-scope-union-tradeoff.md: %w",
				msErrCodeInvalidScope, msErrNameInvalidScope, err)
		}
		return "", fmt.Errorf("submit microsoft report: %w", err)
	}
	var resp struct {
		ReportRequestID *string `json:"ReportRequestId"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("decode microsoft report submit response: %w", err)
	}
	// POINTER, not a bare string: an ABSENT id and an id of "" must stay distinguishable.
	// Absent means the response did not answer the question (a contract change, or an
	// error body that decoded cleanly); empty means Microsoft answered with nothing. Both
	// are failures here, but conflating them would hide which one happened.
	if resp.ReportRequestID == nil || strings.TrimSpace(*resp.ReportRequestID) == "" {
		return "", fmt.Errorf("microsoft report submit returned no ReportRequestId")
	}
	return *resp.ReportRequestID, nil
}

// pollReport polls until the report succeeds, fails, or the budget expires. It returns the
// download URL, which is EMPTY when the report succeeded with no rows.
func (c *Client) pollReport(ctx context.Context, reportID string) (string, error) {
	deadline := c.now().Add(reportPollBudget)
	for attempt := 0; ; attempt++ {
		// Check the caller's context BEFORE the budget: a cancelled request should report
		// cancellation, not a budget expiry it never reached.
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("poll microsoft report: %w", err)
		}
		status, url, err := c.pollOnce(ctx, reportID)
		if err != nil {
			return "", err
		}
		switch status {
		case "Success":
			return url, nil
		case "Error":
			// Microsoft's own terminal failure. Retrying cannot help, so fail now rather
			// than burning the rest of the budget on a report that will never complete.
			return "", fmt.Errorf("microsoft report %s failed to build", clipID(reportID))
		case "Pending":
			// keep waiting
		default:
			// An unrecognized status is NOT treated as still-pending: looping on a value
			// we cannot interpret would spend the whole budget and then report a timeout,
			// hiding the contract change that actually happened.
			return "", fmt.Errorf("microsoft report %s returned unrecognized status %q", clipID(reportID), status)
		}
		if !c.now().Add(reportPollInterval).Before(deadline) {
			return "", fmt.Errorf("%w (waited %s)", ErrReportNotReady, reportPollBudget)
		}
		if err := sleepCtx(ctx, reportPollInterval); err != nil {
			return "", fmt.Errorf("poll microsoft report: %w", err)
		}
	}
}

// pollOnce issues a single Poll call and returns (status, downloadURL).
func (c *Client) pollOnce(ctx context.Context, reportID string) (string, string, error) {
	body := map[string]any{"ReportRequestId": reportID}
	raw, err := c.doReportingRequest(ctx, http.MethodPost, "GenerateReport/Poll", body, true)
	if err != nil {
		return "", "", fmt.Errorf("poll microsoft report: %w", err)
	}
	var resp struct {
		ReportRequestStatus *struct {
			Status            *string `json:"Status"`
			ReportDownloadURL *string `json:"ReportDownloadUrl"`
		} `json:"ReportRequestStatus"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", fmt.Errorf("decode microsoft report poll response: %w", err)
	}
	if resp.ReportRequestStatus == nil || resp.ReportRequestStatus.Status == nil {
		// An absent status is unreadable, not pending. Returning "Pending" here would
		// convert a contract change into a budget timeout.
		return "", "", fmt.Errorf("microsoft report poll returned no status")
	}
	var url string
	if resp.ReportRequestStatus.ReportDownloadURL != nil {
		url = *resp.ReportRequestStatus.ReportDownloadURL
	}
	return *resp.ReportRequestStatus.Status, url, nil
}

// downloadReport fetches the ZIP, extracts its single CSV, and folds the rows into metrics.
//
// The download URL is a pre-signed Microsoft storage URL, NOT an API endpoint: it carries
// its own authorization in the query string, so this request deliberately does NOT attach
// the OAuth bearer token or the account headers. Sending them would leak our credentials to
// a storage host that neither needs nor expects them.
func (c *Client) downloadReport(ctx context.Context, downloadURL, campaignID string, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		// The cause is NOT wrapped: net/http builds a *url.Error carrying the full URL,
		// and this URL's query string IS the credential. Same reasoning as do()'s
		// fullURL/path split — a URL in an error is a disclosure surface.
		return nil, errors.New("build microsoft report download request")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Report only whether the transfer is retryable. The *url.Error the transport
		// returns renders the pre-signed URL, sig= included, in its Error() string.
		// Preserve the sentinel itself, NOT context.Cause: http.Client.Timeout and a
		// custom RoundTripper both surface these while the caller's context is still
		// live, and Cause is nil there — wrapping it renders %!w(<nil>) and stops
		// matching errors.Is entirely.
		if errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("download microsoft report: %w", context.Canceled)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("download microsoft report: %w", context.DeadlineExceeded)
		}
		return nil, errors.New("download microsoft report: transport error")
	}
	defer func() {
		// Drain before close so the connection returns to the idle pool: a body closed
		// unread forces the next request to reopen TCP and TLS.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, reportDownloadCap))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Status only — the body is a storage-service error page and may echo the
		// pre-signed URL's credentials.
		return nil, fmt.Errorf("download microsoft report: unexpected status %d", resp.StatusCode)
	}
	// cap+1 so a file AT the cap is distinguishable from one that exceeds it.
	data, err := io.ReadAll(io.LimitReader(resp.Body, reportDownloadCap+1))
	if err != nil {
		return nil, fmt.Errorf("read microsoft report: %w", err)
	}
	if len(data) > reportDownloadCap {
		return nil, fmt.Errorf("microsoft report exceeds %d bytes", reportDownloadCap)
	}
	return parseReportZip(data, campaignID, window)
}

// parseReportZip extracts the first CSV entry and totals its rows.
func parseReportZip(data []byte, campaignID string, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open microsoft report zip: %w", err)
	}
	var file *zip.File
	for _, f := range zr.File {
		if strings.HasSuffix(strings.ToLower(f.Name), ".csv") {
			file = f
			break
		}
	}
	if file == nil {
		return nil, fmt.Errorf("microsoft report zip contains no CSV")
	}
	rc, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("open microsoft report csv: %w", err)
	}
	defer func() { _ = rc.Close() }()

	// The decompressed size is bounded independently of the ZIP's compressed size, so a
	// small archive cannot expand into an unbounded read.
	//
	// The buffer is read to completion and size-checked BEFORE parsing, rather than being
	// streamed into csv.Reader through a bare io.LimitReader. io.LimitReader reports EOF at
	// its limit — it does not error — so a truncated-but-syntactically-complete PREFIX of an
	// oversized CSV parses cleanly and yields FEWER rows, which then fold into a total that
	// reads as authoritative. That is the failure this file refuses everywhere else: a
	// number that is wrong rather than an error. cap+1 so a file AT the cap is
	// distinguishable from one that exceeds it, mirroring the compressed-side check in
	// downloadReport.
	decompressed, err := io.ReadAll(io.LimitReader(rc, reportDownloadCap+1))
	if err != nil {
		return nil, fmt.Errorf("read microsoft report csv: %w", err)
	}
	if len(decompressed) > reportDownloadCap {
		return nil, fmt.Errorf("microsoft report csv exceeds %d bytes", reportDownloadCap)
	}
	cr := csv.NewReader(bytes.NewReader(decompressed))
	// FieldsPerRecord = -1 permits a RAGGED file, which Microsoft's report writer always
	// produces: the CSV opens with a two-column metadata preamble ("Report Name:", …), then
	// the four-column header and data, then a one-column copyright trailer. The default
	// (lock to the first record's width) rejects the whole file at the header row with
	// "wrong number of fields" — so every real report would fail to parse. Row width is
	// still enforced, per row, by the column lookups in foldReportRows.
	cr.FieldsPerRecord = -1
	records, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse microsoft report csv: %w", err)
	}
	return foldReportRows(records, campaignID, window)
}

// foldReportRows totals the metric columns across every data row.
//
// Columns are resolved BY HEADER NAME rather than by position: Microsoft's report writer
// emits the columns in its own order, and a positional read would silently swap Clicks and
// Spend if that order ever changed — producing plausible numbers that are simply wrong.
func foldReportRows(records [][]string, campaignID string, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	header, rows, err := reportHeaderAndRows(records)
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i, name := range header {
		idx[strings.ToLower(strings.TrimSpace(name))] = i
	}
	impCol, impOK := idx["impressions"]
	clkCol, clkOK := idx["clicks"]
	spendCol, spendOK := idx["spend"]
	if !impOK || !clkOK || !spendOK {
		// Refuse rather than defaulting the missing column to zero: a zero that came from
		// an absent column is indistinguishable, to every consumer, from a measured zero.
		return nil, fmt.Errorf("microsoft report csv missing required columns (have %v)", header)
	}
	if len(rows) == 0 {
		// A header with no data rows is the SAME condition as a Success carrying no
		// download URL, arriving through the other door — and it must answer the same
		// sentinel. GetCampaignMetrics refuses the no-URL form explicitly because this
		// adapter cannot tell "the campaign served nothing" from "no such campaign in this
		// account's scope"; a header-only file carries exactly as little information. The
		// column guard directly above does NOT already cover it: the header names all
		// three columns, so the lookups succeed and the fold below runs zero times,
		// returning a zero-valued CampaignMetrics with a nil error — a measured zero
		// synthesized from an empty file. Returning the sentinel keeps the two shapes
		// indistinguishable to the caller, which is correct, because they are.
		return nil, ErrNoRowsInReport
	}

	out := &model.CampaignMetrics{CampaignID: campaignID, Window: window}
	for _, row := range rows {
		imp, err := parseReportInt(row, impCol)
		if err != nil {
			return nil, fmt.Errorf("impressions: %w", err)
		}
		clk, err := parseReportInt(row, clkCol)
		if err != nil {
			return nil, fmt.Errorf("clicks: %w", err)
		}
		spend, err := parseReportFloat(row, spendCol)
		if err != nil {
			return nil, fmt.Errorf("spend: %w", err)
		}
		// Reject malformed magnitudes rather than folding them in. meta/metrics.go and
		// reddit/metrics.go both guard this way, and for the same reason: a NaN, a
		// negative, or a value that wraps int64 becomes a number the dashboard renders
		// as a measurement. An error here is the honest answer.
		if imp < 0 {
			return nil, fmt.Errorf("impressions: negative value %d", imp)
		}
		if clk < 0 {
			return nil, fmt.Errorf("clicks: negative value %d", clk)
		}
		if math.IsNaN(spend) || math.IsInf(spend, 0) || spend < 0 {
			return nil, fmt.Errorf("spend: non-finite or negative value %v", spend)
		}
		// Spend is a decimal in the ACCOUNT's currency; CostMicros is micros of that same
		// currency, matching what the other clients store. math.Round (not +0.5, which
		// rounds the wrong way for negatives and is why the guard above comes first).
		scaled := math.Round(spend * 1e6)
		if scaled >= float64(math.MaxInt64) {
			return nil, fmt.Errorf("spend: %v exceeds the representable micros range", spend)
		}
		// Checked accumulation. The per-row guards above bound each VALUE; they say nothing
		// about the running TOTAL, which is what the dashboard renders. Without these, many
		// individually-valid rows wrap int64 into a negative — a number, not an error.
		// Mirrors the same three checked additions in reddit/metrics.go. Note each guard
		// needs TWO rows to trip: the total starts at zero, so a single row can never
		// exercise it.
		scaledMicros := int64(scaled)
		if imp > 0 && out.Impressions > math.MaxInt64-imp {
			return nil, fmt.Errorf("impressions: total would overflow")
		}
		out.Impressions += imp
		if clk > 0 && out.Clicks > math.MaxInt64-clk {
			return nil, fmt.Errorf("clicks: total would overflow")
		}
		out.Clicks += clk
		if scaledMicros > 0 && out.CostMicros > math.MaxInt64-scaledMicros {
			return nil, fmt.Errorf("spend: cost total would overflow")
		}
		out.CostMicros += scaledMicros
	}
	if out.Impressions > 0 {
		out.Ctr = float64(out.Clicks) / float64(out.Impressions)
	}
	return out, nil
}

// reportHeaderAndRows finds the header row and returns it with the data rows that follow.
//
// Microsoft's CSV is not a bare table: it is prefixed with report-metadata lines (name,
// date range, account) before the real header, and suffixed with a "©" copyright line.
// Scanning for the header rather than assuming records[0] is what keeps this from reading
// a metadata line as column names.
func reportHeaderAndRows(records [][]string) (header []string, rows [][]string, err error) {
	for i, row := range records {
		for _, cell := range row {
			if strings.EqualFold(strings.TrimSpace(cell), "CampaignId") {
				return row, dropTrailerRows(records[i+1:]), nil
			}
		}
	}
	return nil, nil, fmt.Errorf("microsoft report csv has no header row")
}

// dropTrailerRows removes the copyright/blank trailer lines that follow the data.
//
// The trailer is identified POSITIVELY — blank, or a first cell beginning with "©" — rather
// than by being narrower than the header. Width is not evidence of a trailer: an earlier
// revision dropped every row shorter than the header on the reasoning that "a DATA row
// always carries the full column set", which is an assumption about response SHAPE on a
// contract this file declares UNVERIFIED. A real data row missing a trailing field was
// therefore discarded silently, and its impressions/clicks/spend vanished into a total that
// still looked clean — the same indistinguishable-zero failure the missing-column guard in
// foldReportRows refuses by name. A short row now survives this filter and reaches
// parseReportInt/parseReportFloat, whose existing short-row error reports it.
func dropTrailerRows(rows [][]string) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		blank := true
		for _, cell := range row {
			if strings.TrimSpace(cell) != "" {
				blank = false
				break
			}
		}
		if blank {
			continue
		}
		if len(row) > 0 && strings.HasPrefix(strings.TrimSpace(row[0]), "©") {
			continue
		}
		out = append(out, row)
	}
	return out
}

// parseReportInt reads an integer cell, treating an empty cell as zero.
func parseReportInt(row []string, col int) (int64, error) {
	if col >= len(row) {
		return 0, fmt.Errorf("row has %d columns, wanted column %d", len(row), col)
	}
	cell := strings.TrimSpace(row[col])
	if cell == "" {
		return 0, nil
	}
	// Thousands separators appear in some locales' report output.
	cell = strings.ReplaceAll(cell, ",", "")
	v, err := strconv.ParseInt(cell, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable value %q", cell)
	}
	return v, nil
}

// parseReportFloat reads a decimal cell, treating an empty cell as zero.
func parseReportFloat(row []string, col int) (float64, error) {
	if col >= len(row) {
		return 0, fmt.Errorf("row has %d columns, wanted column %d", len(row), col)
	}
	cell := strings.TrimSpace(row[col])
	if cell == "" {
		return 0, nil
	}
	cell = strings.ReplaceAll(cell, ",", "")
	cell = strings.TrimPrefix(cell, "$")
	v, err := strconv.ParseFloat(cell, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable value %q", cell)
	}
	return v, nil
}

// reportDateRange maps the shared window vocabulary to a start/end date pair in UTC.
// Mirrors the reddit client's dateRangeForWindow so the two platforms answer the same
// question about the same days.
func reportDateRange(window model.MetricsWindow, now time.Time) (start, end time.Time, err error) {
	now = now.UTC()
	e := now
	var s time.Time
	switch window {
	case model.MetricsWindowToday:
		s = now
	case model.MetricsWindowLast7Days:
		s = now.AddDate(0, 0, -6)
	case model.MetricsWindowLast30Days:
		s = now.AddDate(0, 0, -29)
	case model.MetricsWindowThisMonth:
		s = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	case model.MetricsWindowLastMonth:
		firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		e = firstOfThisMonth.AddDate(0, 0, -1)
		s = time.Date(e.Year(), e.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("%w: %q", ErrUnsupportedWindow, window)
	}
	return s, e, nil
}
