// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Account monitor primitives — the Microsoft counterpart of linkedin/monitor.go's
// ListAccountCampaigns, split into STATELESS steps because Microsoft cannot answer the
// account-wide read inside one request.
//
// LinkedIn (and Meta) answer an account-wide, per-campaign metrics read synchronously, so
// their monitor is one call. Microsoft's Reporting service is the asynchronous
// Submit -> Poll -> Download pipeline described at the top of metrics.go, and Microsoft's own
// guidance is that a report takes MINUTES to build while the monitor request has a 20s budget.
// GetCampaignMetrics' bounded poll loop therefore cannot be reused here: it would answer
// ErrReportNotReady on essentially every call and discard the pending report each time (see
// ErrReportNotReady). Microsoft's prescription for exactly this case — "consider saving the
// report identifier, exiting the loop, and trying again later" — is what this file enables:
//
//	SubmitAccountCampaignReport -> a ReportRequestId the CALLER persists
//	CheckAccountCampaignReport  -> exactly ONE Poll (no loop, no sleep); on Success, the rows
//
// This layer holds no state. Persisting the report id between monitor requests, deciding when
// a saved report is too old to show, and when to submit a fresh one all belong to the caller.
// The campaign list (ListAccountCampaigns) is a synchronous Campaign Management read and needs
// no such split.
//
// The same UNVERIFIED CONTRACT caveat as metrics.go applies: no Microsoft credentials were
// available, so every field read here is optional-and-checked, and decoding fails closed.

// ErrInvalidMonitorAccountID marks an account id the account monitor will not act on. It is
// new rather than reused because this package has no invalid-ACCOUNT-id sentinel:
// ValidateAccountID returns an unwrapped error, and ErrInvalidCustomerID names the other id on
// the connection row, which would misdirect a caller that branches on it.
var ErrInvalidMonitorAccountID = errors.New("microsoft-ads: invalid account id for the account monitor")

// monitorAccountIDRE is design/connection.go's MicrosoftAdsConnectionConfig account_id Pattern,
// verbatim: a positive integer with no leading zero and at most 18 digits, so that every value
// it admits is a valid int64.
var monitorAccountIDRE = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// ValidateMonitorAccountID reports whether id is an account id the account monitor may use.
//
// STRICTER than ValidateAccountID on purpose: that function trims whitespace before judging
// the id, which is right for a stored connection field that a header check (accountIDRE) also
// guards, but wrong for a value the monitor layer is about to persist alongside a report id and
// compare on a later request. " 1" and "1" would be two keys naming one account. So nothing is
// trimmed here, and the rule is exactly the HTTP boundary's — a value the API itself would have
// refused cannot reach Microsoft by any other route through the monitor.
func ValidateMonitorAccountID(id string) error {
	if !monitorAccountIDRE.MatchString(id) {
		return fmt.Errorf("%w: %q must match ^[1-9][0-9]{0,17}$", ErrInvalidMonitorAccountID, clipID(id))
	}
	return nil
}

// AccountCampaign is one live campaign on the client's ad account, as Campaign Management
// reports it. It carries no metrics: those arrive separately, from the account report.
type AccountCampaign struct {
	ID           string
	Name         string
	Status       string
	CampaignType string
	// DailyBudget is the campaign's DailyBudget in the ACCOUNT's currency (no micros, no FX —
	// the same unit rule CreateCampaign writes it in). It is 0 whenever BudgetUnparseable is
	// set; read the flag before the number.
	DailyBudget float64
	// SharedBudget is true when the campaign draws on a shared budget (a non-null, non-zero
	// BudgetId). A shared budget is spent by EVERY campaign attached to it, so DailyBudget is
	// not this campaign's own allowance and per-campaign pacing against it would be wrong.
	SharedBudget bool
	// BudgetUnparseable is true when the campaign has no shared budget AND its DailyBudget was
	// null, non-numeric, negative or non-finite. DailyBudget is then 0 — a placeholder, never a
	// measured "no budget" — and the caller must treat the budget as unknown.
	BudgetUnparseable bool
}

// allCampaignTypes is every CampaignType value the v13 CampaignType value set documents
// (App, Audience, DynamicSearchAds, Hotel, ObjectiveBased, PerformanceMax, Search, Shopping),
// as the space-separated flags string GetCampaignsByAccountId documents for multiple values
// ("a string that contains a space-delimited list of values for example ... Search Shopping").
//
// ALL of them, because the operation's own default is "Search" ONLY: omitting the field, or
// naming fewer types, silently drops every other campaign from the monitor while the account
// report — scoped to the whole account — still carries their spend. That is a list that looks
// complete and is not.
//
// Verified against learn.microsoft.com (CampaignType value set and GetCampaignsByAccountId,
// read 2026-10-05), which lists eight values — three more than the brief this was written
// from ("Search Shopping DynamicSearchAds Audience PerformanceMax"): Hotel, App and
// ObjectiveBased. UNVERIFIED LIVE: whether Microsoft rejects a type the account is not enabled
// for (App and Hotel are pilot-gated in some markets) has not been exercised. If it does, the
// read fails LOUDLY with an apiError rather than returning a short list, which is the
// direction this file prefers; trimming the list would trade that error for a silent omission.
// DynamicSearchAds is documented as "no longer supported" as a campaign TYPE but remains in the
// value set, so legacy campaigns of that type are still asked for.
//
// SHARED with the adoption lookup (campaign_lookup.go, GetCampaign), which asks for every type
// for the mirror-image reason: filtered to Search, a live campaign of another type would not come
// back at all, and its absence could read as "no such campaign" and invite a duplicate. One
// constant, so the two reads cannot drift onto different lists.
const allCampaignTypes = "Search Shopping DynamicSearchAds Audience Hotel PerformanceMax App ObjectiveBased"

// campaignStatusDeleted is the one CampaignStatus the monitor drops. Microsoft documents it as
// "for internal use only ... all Get operations do not return deleted objects", so this filter
// should never fire; it is kept so a contract change cannot put deleted campaigns in front of
// an operator as live ones.
const campaignStatusDeleted = "Deleted"

// ListAccountCampaigns returns every non-deleted campaign of every type on the client's ad
// account, via the same Campaigns/QueryByAccountId read findCampaignByName uses (one response,
// not paged — see findCampaignByName). It returns a non-nil, empty slice for an account with
// no campaigns.
//
// Statuses other than Deleted are kept VERBATIM, including ones this code does not know: a
// paused-for-budget or suspended campaign is exactly what an operator looking at a monitor
// needs to see, and a status Microsoft adds later is better shown than hidden.
func (c *Client) ListAccountCampaigns(ctx context.Context) ([]AccountCampaign, error) {
	if err := ValidateMonitorAccountID(c.account.AccountID); err != nil {
		return nil, err
	}
	req := queryCampaignsRequest{
		AccountId:    json.Number(c.account.AccountID),
		CampaignType: allCampaignTypes,
	}
	body, err := c.doRequest(ctx, http.MethodPost, "Campaigns/QueryByAccountId", req, true)
	if err != nil {
		return nil, fmt.Errorf("list microsoft account campaigns: %w", err)
	}
	out, err := decodeAccountCampaigns(body)
	if err != nil {
		return nil, fmt.Errorf("decode QueryByAccountId response: %w", err)
	}
	return out, nil
}

// accountCampaignElement is the subset of a v13 Campaign the monitor reads. DailyBudget and
// BudgetId are RAW because both have more than one legitimate wire form: Microsoft's JSON
// reference renders a `long` (BudgetId) as a quoted string and a `double` (DailyBudget) bare,
// either can be null, and a typed field would turn one unexpected-but-harmless form into a
// whole-list decode failure.
type accountCampaignElement struct {
	ID           *json.Number    `json:"Id"`
	Name         string          `json:"Name"`
	Status       string          `json:"Status"`
	CampaignType string          `json:"CampaignType"`
	BudgetType   string          `json:"BudgetType"`
	DailyBudget  json.RawMessage `json:"DailyBudget"`
	BudgetID     json.RawMessage `json:"BudgetId"`
}

// decodeAccountCampaigns STREAMS the Campaigns array, element by element, with the same
// fail-closed structure as lookupNamedEntity (adgroup_ad.go): an omitted or null Campaigns
// field is an error, not an empty account; an unterminated array or object is an error; and
// trailing bytes after the object are an error. An empty account must arrive as a PRESENT,
// empty array — the only shape that actually says "no campaigns".
//
// Unlike lookupNamedEntity it keeps every live element, so its memory is O(live campaigns);
// that is inherent to returning the list, and the 8 MiB response cap bounds it.
func decodeAccountCampaigns(body []byte) ([]AccountCampaign, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected a JSON object")
	}
	for dec.More() {
		keyTok, kerr := dec.Token()
		if kerr != nil {
			return nil, kerr
		}
		if key, _ := keyTok.(string); key != "Campaigns" {
			if serr := skipJSONValue(dec); serr != nil {
				return nil, serr
			}
			continue
		}
		vTok, verr := dec.Token()
		if verr != nil {
			return nil, verr
		}
		if vTok == nil {
			// null is not "no campaigns": the documented empty answer is an empty array.
			return nil, fmt.Errorf("response has a null Campaigns field; cannot confirm the account's campaign list")
		}
		if d, ok := vTok.(json.Delim); !ok || d != '[' {
			return nil, fmt.Errorf("expected a JSON array for Campaigns")
		}
		out := make([]AccountCampaign, 0)
		for i := 0; dec.More(); i++ {
			var e accountCampaignElement
			if derr := dec.Decode(&e); derr != nil {
				return nil, derr
			}
			id := numberID(e.ID)
			if id == "" {
				// A campaign with no usable id cannot be joined to its report row, and dropping
				// it would show the account as having one campaign fewer than it has. Reject
				// the whole list — the same rule linkedin/monitor.go applies to a pivot row
				// that resolves to no campaign.
				return nil, fmt.Errorf("campaign element %d has no usable Id", i)
			}
			if strings.EqualFold(strings.TrimSpace(e.Status), campaignStatusDeleted) {
				continue
			}
			row, rerr := accountCampaignFromElement(id, e)
			if rerr != nil {
				return nil, fmt.Errorf("campaign element %d (id %s): %w", i, id, rerr)
			}
			out = append(out, row)
		}
		endTok, eerr := dec.Token()
		if eerr != nil {
			return nil, eerr
		}
		if d, ok := endTok.(json.Delim); !ok || d != ']' {
			return nil, fmt.Errorf("malformed Campaigns array (unterminated)")
		}
		// The enclosing object must close too, and nothing may follow it: a truncated body
		// whose array happened to close is still a truncated body.
		if ferr := finishObject(dec); ferr != nil {
			return nil, ferr
		}
		return out, nil
	}
	return nil, fmt.Errorf("response omitted the Campaigns field; cannot confirm the account's campaign list")
}

// accountCampaignFromElement maps one decoded element to an AccountCampaign, deciding the
// budget fields. It errors only on a BudgetId that is present but is not an id at all; a bad
// DailyBudget is not an error, it is BudgetUnparseable — one campaign's budget being unreadable
// must not hide every other campaign on the account.
func accountCampaignFromElement(id string, e accountCampaignElement) (AccountCampaign, error) {
	row := AccountCampaign{ID: id, Name: e.Name, Status: e.Status, CampaignType: e.CampaignType}

	shared, err := sharedBudgetSet(e.BudgetID)
	if err != nil {
		return AccountCampaign{}, err
	}
	row.SharedBudget = shared

	amount, ok := parseDailyBudget(e.DailyBudget)
	switch {
	case ok:
		row.DailyBudget = amount
	case shared:
		// A shared-budget campaign's own DailyBudget is not what governs its spend, so its
		// absence is not "unknown budget" — SharedBudget already tells the caller not to pace
		// this campaign against a per-campaign number. DailyBudget stays 0 and is NOT flagged.
	default:
		row.BudgetUnparseable = true
	}
	return row, nil
}

// sharedBudgetSet reports whether a BudgetId names a shared budget. null, absent, "" and 0
// all mean "no shared budget" (Microsoft's own empty forms for an unset `long`); a positive
// int64 means one is set. Anything else — a negative, a fraction, a non-numeric string — is
// neither, and is refused rather than guessed: guessing "not shared" would pace the campaign
// against a number that is not its own, and guessing "shared" would hide a real budget.
func sharedBudgetSet(raw json.RawMessage) (bool, error) {
	v, isNull, ok := rawScalar(raw)
	if !ok {
		return false, fmt.Errorf("budget id is not a number")
	}
	if isNull || v == "" || v == "0" {
		return false, nil
	}
	n := json.Number(v)
	if numberID(&n) == "" {
		return false, fmt.Errorf("budget id %q is not a valid id", clipID(v))
	}
	return true, nil
}

// parseDailyBudget reads a DailyBudget as a finite, non-negative amount. ok=false covers null,
// absent, non-numeric, negative and non-finite values alike — every one of them is "this
// campaign's budget is not known", which the caller flags rather than rendering as 0.
func parseDailyBudget(raw json.RawMessage) (float64, bool) {
	v, isNull, ok := rawScalar(raw)
	if !ok || isNull || v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, false
	}
	return f, true
}

// rawScalar renders a raw JSON scalar that should hold a number — a bare number, or a quoted
// string as Microsoft's JSON reference renders a `long` — as its trimmed text. isNull is true
// for an absent field or a JSON null. ok is false for an object, an array, a boolean, or bytes
// that are not JSON at all.
func rawScalar(raw json.RawMessage) (v string, isNull, ok bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return "", true, true
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return "", false, false
		}
		return strings.TrimSpace(s), false, true
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var n json.Number
		if err := json.Unmarshal(t, &n); err != nil {
			return "", false, false
		}
		return n.String(), false, true
	default:
		return "", false, false
	}
}

// SubmitAccountCampaignReport submits an account-wide, per-campaign performance report over the
// last `days` days and returns its ReportRequestId together with the window it covers. It does
// not wait for the report: the caller persists reportID and collects it later with
// CheckAccountCampaignReport.
//
// The window is the house convention linkedin/monitor.go uses — `days` calendar days ENDING
// TODAY, inclusive, in UTC: end = now (UTC), start = end minus (days-1) days. windowStart and
// windowEnd are returned so the caller can record which window the saved report answers
// without re-deriving it from a clock that will have moved by the time it reads the report.
//
// days is checked only for days >= 1 (a zero or negative window has no meaning and would put
// start after end); the upper bound belongs to the service layer, as in linkedin/monitor.go.
func (c *Client) SubmitAccountCampaignReport(ctx context.Context, days int) (reportID string, windowStart, windowEnd time.Time, err error) {
	if days < 1 {
		return "", time.Time{}, time.Time{}, fmt.Errorf("microsoft account report window must be at least 1 day, got %d", days)
	}
	if err := ValidateMonitorAccountID(c.account.AccountID); err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	end := c.now().UTC()
	start := end.AddDate(0, 0, -(days - 1))

	// Scope carries AccountIds and NO Campaigns element — the inverse of submitReport, and for
	// the same documented reason. AccountThroughCampaignReportScope's scope is the UNION of
	// AccountIds and Campaigns; that union is what makes AccountIds WRONG on a campaign-scoped
	// read (it widens the read to the whole account), and it is exactly what an account-wide
	// read wants. With no Campaigns element the union is simply "every campaign in this
	// account", which, with Aggregation Summary and CampaignId as a column, is one row per
	// campaign that served in the window.
	//
	// This also sidesteps the open 2027 question recorded on submitReport — the community
	// reports of InvalidAccountThruCampaignReportScope are about OMITTING AccountIds, which this
	// request does not do — so that diagnostic is deliberately not applied here.
	//
	// The id goes out as a QUOTED string for the reasons given on submitReport (Reporting v13
	// quotes `long`; a bare 64-bit number risks precision loss).
	//
	// Everything else — type, Csv, ReturnOnlyCompleteData=false, Summary aggregation, the
	// columns (ConversionsQualified, not the deprecated Conversions), and the
	// GreenwichMeanTimeDublinEdinburghLisbonLondon ReportTimeZone with its BST caveat — is
	// submitReportDefinition's, shared with the campaign read and reasoned there.
	scope := map[string]any{
		"AccountIds": []string{c.account.AccountID},
	}
	id, err := c.submitReportDefinition(ctx, scope, start, end)
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	return id, start, end, nil
}

// AccountReportStatus is the state of a submitted account report, as of one Poll.
type AccountReportStatus string

const (
	// AccountReportStatusPending means Microsoft is still building the report. Ask again later with
	// the same report id.
	AccountReportStatusPending AccountReportStatus = "Pending"
	// AccountReportStatusSuccess means the report is built; Rows and Partial are populated.
	AccountReportStatusSuccess AccountReportStatus = "Success"
	// AccountReportStatusError means Microsoft gave up building the report. Polling the same id
	// again cannot help; the caller must submit a fresh report.
	AccountReportStatusError AccountReportStatus = "Error"
)

// AccountReportRow is one campaign's totals over the report window.
type AccountReportRow struct {
	CampaignID  string
	Impressions int64
	Clicks      int64
	// Spend is a decimal in the ACCOUNT's currency, unconverted.
	Spend float64
	// Conversions is ConversionsQualified summed over the campaign's rows, or nil when
	// Microsoft did not report it — the column was absent, or ANY of this campaign's cells
	// was blank. nil is "unknown", never zero.
	Conversions *float64
}

// AccountReportResult is the outcome of one CheckAccountCampaignReport.
type AccountReportResult struct {
	Status AccountReportStatus
	// Rows is set only for AccountReportStatusSuccess: one row per campaign that appears in the
	// report, in first-appearance order. Empty (non-nil) when nothing served. A campaign
	// ABSENT from Rows served nothing in the window.
	Rows []AccountReportRow
	// Partial is true when Microsoft flagged the report's data as potentially incomplete. The
	// rows are still returned — see CheckAccountCampaignReport.
	Partial bool
}

// CheckAccountCampaignReport polls a report submitted by SubmitAccountCampaignReport EXACTLY
// ONCE — no loop, no sleep — and, if it is built, downloads and folds it per campaign.
//
// One poll because the caller has a 20s request budget and Microsoft's reports take minutes;
// waiting here would only spend that budget to learn "Pending" more slowly. Pending and Error
// are answers, not errors. An absent or unrecognized status IS an error, for the reason pollOnce
// and pollReport give: reading a value we cannot interpret as "pending" would turn a contract
// change into a report that never arrives.
//
// SUCCESS WITH NO DOWNLOAD URL is a legitimate, empty, Success here — unlike GetCampaignMetrics,
// which refuses it with ErrNoRowsInReport. The campaign-scoped read refuses because it cannot
// tell "this campaign served nothing" from "no such campaign in this account's scope": the
// question named one campaign, and an empty answer does not say which of those two is true.
// The account-scoped read asks no such question. Its scope is the account itself (already
// validated, and the credentials just built a report for it), and the campaign list comes
// from ListAccountCampaigns, not from this report — so an empty report can only mean "no
// campaign on this account served in the window", which is a real and common measurement for
// an idle account. Refusing it would make every idle account permanently unreadable. The same
// reasoning makes a header-only CSV zero rows rather than an error.
//
// PARTIAL DATA IS REPORTED, NOT REFUSED — the opposite of GetCampaignMetrics, which refuses a
// flagged report with ErrReportDataIncomplete. That refusal is sound there because
// model.CampaignMetrics has no field to carry the caveat, so returning the numbers would
// present an under-count as complete, and a caller can retry once aggregation settles. Neither
// holds for the monitor. Its window ALWAYS ends today (SubmitAccountCampaignReport), and today
// is the day Microsoft flags as still aggregating, so a monitor that refused flagged reports
// would essentially never show data. And this result DOES carry the caveat: Partial is set,
// and the monitor response states the time its metrics are as of, so the reader is told the
// last day may still rise rather than being shown a smaller number as final.
func (c *Client) CheckAccountCampaignReport(ctx context.Context, reportID string) (*AccountReportResult, error) {
	if strings.TrimSpace(reportID) == "" {
		return nil, fmt.Errorf("microsoft report id is required")
	}
	if err := ValidateMonitorAccountID(c.account.AccountID); err != nil {
		return nil, err
	}
	status, downloadURL, err := c.pollOnce(ctx, reportID)
	if err != nil {
		return nil, err
	}
	switch AccountReportStatus(status) {
	case AccountReportStatusPending:
		return &AccountReportResult{Status: AccountReportStatusPending}, nil
	case AccountReportStatusError:
		return &AccountReportResult{Status: AccountReportStatusError}, nil
	case AccountReportStatusSuccess:
		// handled below
	default:
		return nil, fmt.Errorf("microsoft report %s returned unrecognized status %q", clipID(reportID), status)
	}
	if downloadURL == "" {
		return &AccountReportResult{Status: AccountReportStatusSuccess, Rows: []AccountReportRow{}}, nil
	}
	records, err := c.downloadReportRecords(ctx, downloadURL)
	if err != nil {
		return nil, err
	}
	return foldAccountReportRows(records)
}

// accountReportAcc accumulates one campaign's totals. Conversions are held apart from the
// published value so a blank cell on a LATER row can still withdraw the campaign's sum.
type accountReportAcc struct {
	row            AccountReportRow
	convTotal      *float64
	convIncomplete bool
}

// foldAccountReportRows folds a downloaded account report into one row per campaign.
//
// It applies foldReportRows' per-value discipline unchanged — columns resolved by header name,
// the three metric columns required, negative/non-finite values and int64 overflow refused as
// errors, ConversionsQualified optional and parsed as a double — and differs only where the
// account scope changes the meaning:
//
//   - Rows are GROUPED by CampaignId and summed. With Aggregation Summary Microsoft should emit
//     one row per campaign, but nothing in the contract forbids more, and summing is what a
//     duplicate would mean; keeping the last would silently drop the others.
//   - A blank conversion cell withdraws Conversions for THAT campaign only. The campaign-scoped
//     read withdraws its single total; here each campaign's total is a separate claim, and one
//     campaign without Universal Event Tracking data says nothing about another's. Withdrawing
//     account-wide would erase real conversion counts to protect against a gap elsewhere.
//   - An empty or non-numeric CampaignId is an ERROR for the whole read. The row cannot be
//     attributed, and dropping it would let its campaign read as "served nothing" — the
//     linkedin/monitor.go pivotValues rule.
//   - No rows, and the incomplete-data flag, are answers rather than errors (see
//     CheckAccountCampaignReport).
func foldAccountReportRows(records [][]string) (*AccountReportResult, error) {
	header, rows, preamble, err := reportHeaderAndRows(records)
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i, name := range header {
		idx[strings.ToLower(strings.TrimSpace(name))] = i
	}
	idCol, idOK := idx["campaignid"]
	impCol, impOK := idx["impressions"]
	clkCol, clkOK := idx["clicks"]
	spendCol, spendOK := idx["spend"]
	convCol, convOK := idx["conversionsqualified"]
	if !idOK || !impOK || !clkOK || !spendOK {
		return nil, fmt.Errorf("microsoft account report csv missing required columns (have %v)", header)
	}

	order := make([]string, 0)
	byID := map[string]*accountReportAcc{}
	for i, row := range rows {
		if idCol >= len(row) {
			return nil, fmt.Errorf("account report row %d: row has %d columns, wanted column %d", i, len(row), idCol)
		}
		raw := strings.TrimSpace(row[idCol])
		n := json.Number(raw)
		id := numberID(&n)
		if id == "" {
			return nil, fmt.Errorf("account report row %d: CampaignId %q is not a campaign id; the row cannot be attributed", i, clipID(raw))
		}
		imp, err := parseReportInt(row, impCol)
		if err != nil {
			return nil, fmt.Errorf("campaign %s impressions: %w", id, err)
		}
		clk, err := parseReportInt(row, clkCol)
		if err != nil {
			return nil, fmt.Errorf("campaign %s clicks: %w", id, err)
		}
		spend, err := parseReportFloat(row, spendCol)
		if err != nil {
			return nil, fmt.Errorf("campaign %s spend: %w", id, err)
		}
		if imp < 0 {
			return nil, fmt.Errorf("campaign %s impressions: negative value %d", id, imp)
		}
		if clk < 0 {
			return nil, fmt.Errorf("campaign %s clicks: negative value %d", id, clk)
		}
		if math.IsNaN(spend) || math.IsInf(spend, 0) || spend < 0 {
			return nil, fmt.Errorf("campaign %s spend: non-finite or negative value %v", id, spend)
		}

		acc, seen := byID[id]
		if !seen {
			acc = &accountReportAcc{row: AccountReportRow{CampaignID: id}}
			byID[id] = acc
			order = append(order, id)
		}
		// Checked accumulation, as in foldReportRows: the per-value guards bound each cell,
		// not the running total, and a wrapped int64 is a number rather than an error.
		if imp > 0 && acc.row.Impressions > math.MaxInt64-imp {
			return nil, fmt.Errorf("campaign %s impressions: total would overflow", id)
		}
		acc.row.Impressions += imp
		if clk > 0 && acc.row.Clicks > math.MaxInt64-clk {
			return nil, fmt.Errorf("campaign %s clicks: total would overflow", id)
		}
		acc.row.Clicks += clk
		spendTotal := acc.row.Spend + spend
		if math.IsInf(spendTotal, 0) {
			return nil, fmt.Errorf("campaign %s spend: total would overflow", id)
		}
		acc.row.Spend = spendTotal

		if !convOK {
			continue
		}
		conv, present, cerr := parseConversionCell(row, convCol)
		if cerr != nil {
			return nil, fmt.Errorf("campaign %s conversionsQualified: %w", id, cerr)
		}
		if !present {
			acc.convIncomplete = true
			continue
		}
		if math.IsNaN(conv) || math.IsInf(conv, 0) || conv < 0 {
			return nil, fmt.Errorf("campaign %s conversionsQualified: non-finite or negative value %v", id, conv)
		}
		var running float64
		if acc.convTotal != nil {
			running = *acc.convTotal
		}
		total := running + conv
		if math.IsInf(total, 0) {
			return nil, fmt.Errorf("campaign %s conversionsQualified: total would overflow", id)
		}
		acc.convTotal = &total
	}

	out := &AccountReportResult{
		Status:  AccountReportStatusSuccess,
		Rows:    make([]AccountReportRow, 0, len(order)),
		Partial: reportDataIsIncomplete(preamble),
	}
	for _, id := range order {
		acc := byID[id]
		row := acc.row
		// Published only when every one of THIS campaign's rows carried a value (and the
		// column existed at all — convTotal stays nil otherwise).
		if !acc.convIncomplete {
			row.Conversions = acc.convTotal
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}
