// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	// The account monitor converts X timestamps into the AD ACCOUNT's timezone (an IANA name
	// such as "America/Los_Angeles"), so it needs the zone database at runtime. The service ships
	// on a static base image; embedding the database keeps time.LoadLocation working whether or
	// not that image carries /usr/share/zoneinfo. Costs ~450 KB of binary.
	_ "time/tzdata"
)

// Account monitor primitives — the X counterpart of microsoft/monitor.go, and REPORT-BACKED for
// the same reason in a different shape.
//
// X does have a synchronous analytics endpoint (GetCampaignMetrics uses it), but two of its
// documented limits rule it out for an account-wide read over 7–90 days:
//
//   - "7-day maximum time range (end_time - start_time)" per synchronous request, against a
//     monitor window of up to 90 days; and
//   - "250 requests per 15-minute window (user-level)" — one budget shared by every foundation
//     on the shared LF OAuth token, which an account-wide fan-out of 20-entity requests would
//     spend quickly.
//
// The asynchronous stats-jobs API covers "up to 90 days" per job, 20 entities per job, and is
// rate limited by concurrent jobs per account instead. Source for all three:
// https://docs.x.com/x-ads-api/analytics (Synchronous Analytics, Asynchronous Analytics).
//
// So, as on Microsoft, the read is split into stateless steps the caller drives across requests
// (service.Orchestrator.ReadReportedAccountCampaigns):
//
//	ListAccountCampaigns        -> the live campaign list, with budgets and line-item flights
//	SubmitAccountCampaignReport -> active_entities, then one stats job per <=20 active campaigns;
//	                               ONE composite report id the caller persists
//	CheckAccountCampaignReport  -> ONE job-status read; on SUCCESS, the downloaded rows
//
// UNVERIFIED CONTRACT: no X Ads credentials were available when this was written. Every
// endpoint, parameter and field below follows the public documentation cited next to it and has
// NOT been exercised against a live account, so every field read is optional-and-checked and
// decoding fails closed. Specific open points are marked UNVERIFIED where they are relied on.

// NoActiveCampaignsReportID is the report id SubmitAccountCampaignReport returns when X reports
// no active campaign in the window: there is nothing to build a stats job for, and the honest
// report is an empty one. CheckAccountCampaignReport answers it as a finished report with zero
// rows WITHOUT calling X. It cannot collide with a real composite id, which is only digits and
// commas.
const NoActiveCampaignsReportID = "none"

// statsJobMaxEntities is X's per-job entity cap: "Up to 20 entity IDs per request"
// (https://docs.x.com/x-ads-api/analytics, Asynchronous Analytics).
const statsJobMaxEntities = 20

// maxStatsJobsPerReport bounds how many stats jobs one submission creates (so
// MaxMonitorActiveCampaigns active campaigns). The binding constraint is time, not X's
// 100-concurrent-jobs limit: every job POST is paced on the client's 1 write/sec pacer (see
// SubmitAccountCampaignReport), and the whole submission shares the monitor's 20s call budget
// with the campaign list. An account with more active campaigns than this is refused with
// ErrTooManyActiveCampaigns rather than given a report that silently covers some of them — the
// caller treats a finished report as authoritative for the whole account, so a partial one would
// read the uncovered campaigns as having served nothing.
const maxStatsJobsPerReport = 10

// MaxMonitorActiveCampaigns is the most campaigns active in one window the account monitor
// builds a report for: maxStatsJobsPerReport jobs of statsJobMaxEntities campaigns each.
const MaxMonitorActiveCampaigns = maxStatsJobsPerReport * statsJobMaxEntities

// ErrTooManyActiveCampaigns is returned by SubmitAccountCampaignReport, before any job is
// created, when X reports more than MaxMonitorActiveCampaigns campaigns active in the window.
// It is PERMANENT for as long as the account stays that busy: retrying cannot help, so the
// caller surfaces it rather than retrying it as an upstream failure.
var ErrTooManyActiveCampaigns = fmt.Errorf("twitter: the account has more than %d campaigns active in the report window", MaxMonitorActiveCampaigns)

// ErrReportWindowNotWholeHours is returned by SubmitAccountCampaignReport when a day start (local
// midnight, or the first instant after a skipped one) bounding the report window is not a whole
// UTC hour — an account whose timezone has a fractional-hour offset (Asia/Kolkata,
// Asia/Kathmandu, Australia/Adelaide, ...). It comes before any stats request or job creation,
// not before every request: the account timezone is read first (an account GET when the cache is
// cold), and the caller has usually listed the account's campaigns and line items already.
// X accepts "whole hours only" for start_time/end_time, so such an account's calendar days cannot
// be queried exactly, and querying a shifted window while reporting the account's own days would
// misattribute up to 45 minutes of delivery to the wrong day. PERMANENT for the account's zone.
var ErrReportWindowNotWholeHours = errors.New("twitter: the account's timezone does not put local midnight on a whole UTC hour, which X's report window requires")

// maxStatsWindow is the longest window a stats job or an active_entities read accepts: "up to
// 90 days" (https://docs.x.com/x-ads-api/analytics). A 90-day window that crosses a DST
// fall-back is 90 days plus an hour of wall time; see accountReportWindow.
const maxStatsWindow = 90 * 24 * time.Hour

// Download caps for a stats-job results file. A TOTAL-granularity file for 20 campaigns is a
// few KB; these exist so a hostile or broken file cannot exhaust memory, compressed or after
// decompression (a gzip bomb). They are the defaults of the client's statsFileCompressedCap /
// statsFileDecompressedCap fields, which only tests lower.
const (
	defaultStatsFileCompressedCap   = 8 << 20
	defaultStatsFileDecompressedCap = 32 << 20
)

// statsFileHost is the ONE host a stats-job results file is downloaded from. X's Asynchronous
// Analytics documentation shows the finished job's url on X's static-content host —
// "https://ton.twimg.com/advertiser-api-async-analytics/....json.gz" — and documents that the
// file "requires no authentication once obtained" (https://docs.x.com/x-ads-api/analytics).
// The url is upstream data, and fetching whatever host it names would let a compromised or
// spoofed job answer point this service at an arbitrary https endpoint (an SSRF surface,
// including internal hosts reachable from the cluster), so exactly this host is admitted — not
// a *.twimg.com suffix, since the docs show no other host and a wider pattern would admit
// hosts nobody has seen serve these files. If X moves the files, downloads fail closed with an
// error and the constant is updated deliberately. Tests substitute a TLS stand-in through
// withStatsFileHosts.
const statsFileHost = "ton.twimg.com"

// withStatsFileHosts replaces the hosts (host[:port], matched exactly) a results file may be
// downloaded from over https. Unexported: a TEST seam, so a TLS httptest server can stand in for
// statsFileHost. Never used in production.
func withStatsFileHosts(hosts ...string) Option {
	return func(c *Client) { c.statsFileHosts = append([]string(nil), hosts...) }
}

// withStatsFileCaps lowers the download caps. Unexported test seam, as withStatsFileHosts.
func withStatsFileCaps(compressed, decompressed int64) Option {
	return func(c *Client) { c.statsFileCompressedCap, c.statsFileDecompressedCap = compressed, decompressed }
}

// statsJobSubmitMargin is the time a stats-job submission keeps in hand beyond its pacer
// intervals: the job POSTs' own round trips. See SubmitAccountCampaignReport's budget check.
const statsJobSubmitMargin = 2 * time.Second

// ErrStatsJobBudget is returned by SubmitAccountCampaignReport, before ANY job is created, when
// the context's deadline leaves too little time for the paced job POSTs to complete. Creating
// some jobs and running out mid-loop would abandon them uncollected (each holds one of X's 100
// concurrent-job slots for the account until it expires), so the submission is declined whole
// and the caller retries on a later read with a fresh budget.
var ErrStatsJobBudget = errors.New("twitter: not enough time left to create the account report's stats jobs")

// accountTimezoneCacheFor is how long a successfully read account timezone is reused by this
// client. One monitor read asks for it twice (the campaign list and the report submission, both
// on the shared cached client) inside a 20-second budget; a minute covers that read without
// holding a changed account setting for long. Failures are never cached.
const accountTimezoneCacheFor = time.Minute

// maxStatsJobIDLen bounds one job id. X returns job ids as 64-bit integers with an id_str twin
// (example "1120829647711653888", https://docs.x.com/x-ads-api/analytics); an unsigned 64-bit
// value has at most 20 digits.
const maxStatsJobIDLen = 20

var statsJobIDRe = regexp.MustCompile(`^[0-9]+$`)

// ErrInvalidMonitorAccountID marks an account id the account monitor will not act on.
var ErrInvalidMonitorAccountID = errors.New("twitter: invalid account id for the account monitor")

// ValidateMonitorAccountID reports whether id is an account id the account monitor may use: the
// SAME rule design/connection.go's twitter-ads-connection-config.account_id applies
// (Pattern ^[A-Za-z0-9]+$, MaxLength 64), with nothing trimmed.
//
// The connection's rule, not a tighter one, on purpose. Real X account ids are short lower-case
// base-36 handles ("18ce54d4x5t"), but X does not publish a grammar for them, and the monitor
// only ever serves the account the project's connection is bound to (requireTwitterManagedAccount
// in internal/dispatch). A stricter pattern here would make some ids the connection legitimately
// stores impossible to monitor, while admitting nothing a looser one could reach. Untrimmed
// because the id is persisted as part of the saved-report key: " a1" and "a1" would be two keys
// naming one account.
func ValidateMonitorAccountID(id string) error {
	if !accountIDRe.MatchString(id) || len(id) > maxAccountIDLen {
		return fmt.Errorf("%w: must match ^[A-Za-z0-9]+$ and be at most %d characters", ErrInvalidMonitorAccountID, maxAccountIDLen)
	}
	return nil
}

// AccountCampaign is one live campaign on the client's ad account, without metrics.
type AccountCampaign struct {
	ID     string
	Name   string
	Status string // X's entity_status, verbatim (ACTIVE / PAUSED)
	// DailyBudget and TotalBudget are in the ACCOUNT's currency (X's *_local_micro / 1e6, no FX).
	// 0 means the campaign has no budget of that kind. Both are 0 when BudgetUnparseable is set.
	DailyBudget       float64
	TotalBudget       float64
	BudgetUnparseable bool
	// StartDate / EndDate are the ENVELOPE of the campaign's flight as calendar dates
	// (YYYY-MM-DD) in the ACCOUNT's timezone, derived from its line items: the earliest
	// start_time and the latest end_time. EndDate is the last day the flight runs on (inclusive).
	// StartDate is empty when the campaign has no line items; EndDate is empty then too, and
	// whenever any line item is open-ended (no end_time). FlightUnparseable marks a line item
	// whose times could not be read.
	StartDate         string
	EndDate           string
	FlightUnparseable bool
	// Flights is what the envelope cannot say: the days the campaign is actually scheduled on,
	// as the union of its line items' flights — sorted, disjoint, non-adjacent ranges of local
	// calendar days. Line items Sep 1–5 and Oct 1–5 give two ranges, so a window in the gap is
	// not read as scheduled. Empty whenever StartDate is.
	Flights []FlightRange
}

// FlightRange is one contiguous run of scheduled account-local days, StartDate through EndDate
// inclusive (YYYY-MM-DD); EndDate is empty for an open-ended run, which is always the last.
type FlightRange struct {
	StartDate string
	EndDate   string
}

// monitorCampaignElement is one row of GET accounts/:account_id/campaigns. The budgets are kept
// RAW so a value of the wrong JSON type is caught rather than decoded to 0.
type monitorCampaignElement struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	EntityStatus string          `json:"entity_status"`
	DailyBudget  json.RawMessage `json:"daily_budget_amount_local_micro"`
	TotalBudget  json.RawMessage `json:"total_budget_amount_local_micro"`
	Deleted      bool            `json:"deleted"`
}

// monitorLineItemElement is one row of GET accounts/:account_id/line_items.
type monitorLineItemElement struct {
	ID         string  `json:"id"`
	CampaignID string  `json:"campaign_id"`
	StartTime  *string `json:"start_time"`
	EndTime    *string `json:"end_time"`
	Deleted    bool    `json:"deleted"`
}

// lineItemCampaignBatch is how many campaign ids one line_items read filters on. UNVERIFIED: the
// 200 cap is the one X documents for the id filters on active_entities
// (https://docs.x.com/x-ads-api/analytics, Active Entities); the campaign-management reference
// names campaign_ids as a multi-value filter without a number in the summary available here.
const lineItemCampaignBatch = 200

// accountElementTZ is the part of GET accounts/:account_id this file reads.
type accountElementTZ struct {
	ID       string `json:"id"`
	Timezone string `json:"timezone"`
}

// AccountTimezone reads the ad account's configured timezone. X documents it as the zone that
// "determines day boundaries for daily budget application and charge aggregation"
// (https://docs.x.com/x-ads-api/campaign-management, GET accounts/:account_id), which is why the
// monitor's window and flight dates are computed in it rather than in UTC. An absent or unknown
// zone is an error: guessing UTC would shift every day boundary for a non-UTC account.
//
// A successful read is reused for accountTimezoneCacheFor, so one monitor read — list, then
// submit — costs one account GET rather than two.
func (c *Client) AccountTimezone(ctx context.Context) (*time.Location, error) {
	if err := ValidateMonitorAccountID(c.account.AccountID); err != nil {
		return nil, err
	}
	c.tzMu.Lock()
	if c.tzLoc != nil && c.timeFn().Sub(c.tzAt) < accountTimezoneCacheFor {
		loc := c.tzLoc
		c.tzMu.Unlock()
		return loc, nil
	}
	c.tzMu.Unlock()
	loc, err := c.readAccountTimezone(ctx)
	if err != nil {
		return nil, err
	}
	c.tzMu.Lock()
	c.tzLoc, c.tzAt = loc, c.timeFn()
	c.tzMu.Unlock()
	return loc, nil
}

// readAccountTimezone is AccountTimezone's uncached read.
func (c *Client) readAccountTimezone(ctx context.Context) (*time.Location, error) {
	resp, err := c.request(ctx, http.MethodGet, "")
	if err != nil {
		return nil, fmt.Errorf("read x ads account: %w", err)
	}
	var acct accountElementTZ
	if resp == nil || len(resp.Data) == 0 || json.Unmarshal(resp.Data, &acct) != nil {
		return nil, errors.New("read x ads account: response carried no account object")
	}
	tz := strings.TrimSpace(acct.Timezone)
	if tz == "" {
		return nil, errors.New("read x ads account: account has no timezone")
	}
	loc, lerr := time.LoadLocation(tz)
	if lerr != nil {
		// The zone name is upstream text; it is not echoed.
		return nil, errors.New("read x ads account: account timezone is not a recognised IANA zone")
	}
	return loc, nil
}

// ListAccountCampaigns reads every non-deleted, non-draft campaign on the account with its
// budgets, and derives each one's flight from its line items.
//
//   - GET accounts/:account_id/campaigns?with_deleted=false&with_draft=false&count=1000
//   - GET accounts/:account_id/line_items?campaign_ids=<<=200>&with_deleted=false&with_draft=false&count=1000
//
// (https://docs.x.com/x-ads-api/campaign-management). Both are walked through the package's
// cursor reader with the STRICT rule ListAdAccounts uses: an absent or empty next_cursor, a
// repeated cursor, a body with no data, or the page cap is an error, never a short list — a
// campaign missing from this list would simply vanish from the monitor.
//
// Campaigns carry no flight dates in v12 (the create path notes the campaign endpoint "does NOT
// accept start_time/end_time in v12 — flight dates live on the line item"), so the flight comes
// from the line items.
func (c *Client) ListAccountCampaigns(ctx context.Context) ([]AccountCampaign, error) {
	if err := ValidateMonitorAccountID(c.account.AccountID); err != nil {
		return nil, err
	}
	loc, err := c.AccountTimezone(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]AccountCampaign, 0)
	index := make(map[string]int)
	err = c.walkPages(ctx, "campaigns?with_deleted=false&with_draft=false&count="+strconv.Itoa(listPageSize), func(data json.RawMessage) error {
		var els []monitorCampaignElement
		if err := json.Unmarshal(data, &els); err != nil {
			return errors.New("campaign list is not an array of campaigns")
		}
		for _, el := range els {
			if el.Deleted {
				continue
			}
			if !campaignIDRe.MatchString(el.ID) {
				// An unattributable row cannot be skipped: its campaign would vanish from the
				// monitor. Fail the whole read instead.
				return errors.New("campaign list carried a campaign with an unusable id")
			}
			if _, dup := index[el.ID]; dup {
				continue
			}
			ac := AccountCampaign{ID: el.ID, Name: strings.TrimSpace(el.Name), Status: el.EntityStatus}
			daily, dok := parseLocalMicro(el.DailyBudget)
			total, tok := parseLocalMicro(el.TotalBudget)
			if dok && tok {
				ac.DailyBudget, ac.TotalBudget = daily, total
			} else {
				ac.BudgetUnparseable = true
			}
			index[el.ID] = len(out)
			out = append(out, ac)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list x ads campaigns: %w", err)
	}

	if err := c.applyLineItemFlights(ctx, out, index, loc); err != nil {
		return nil, fmt.Errorf("list x ads line items: %w", err)
	}
	return out, nil
}

// flightAcc accumulates one campaign's flight across its line items: the envelope (earliest
// start, latest end, open-ended if any line item is) and every line item's own interval, which
// flightRanges unions.
type flightAcc struct {
	start, end time.Time
	openEnded  bool
	bad        bool
	spans      []flightSpan
}

// flightSpan is one line item's [start, end) instants; end is zero when open-ended.
type flightSpan struct{ start, end time.Time }

// applyLineItemFlights reads the line items of every campaign in out, in batches, and sets each
// campaign's StartDate/EndDate.
func (c *Client) applyLineItemFlights(ctx context.Context, out []AccountCampaign, index map[string]int, loc *time.Location) error {
	flights := make(map[string]*flightAcc, len(out))
	for start := 0; start < len(out); start += lineItemCampaignBatch {
		end := min(start+lineItemCampaignBatch, len(out))
		ids := make([]string, 0, end-start)
		for _, ac := range out[start:end] {
			ids = append(ids, ac.ID)
		}
		path := "line_items?campaign_ids=" + url.QueryEscape(strings.Join(ids, ",")) +
			"&with_deleted=false&with_draft=false&count=" + strconv.Itoa(listPageSize)
		err := c.walkPages(ctx, path, func(data json.RawMessage) error {
			var els []monitorLineItemElement
			if err := json.Unmarshal(data, &els); err != nil {
				return errors.New("line item list is not an array of line items")
			}
			for _, el := range els {
				if el.Deleted {
					continue
				}
				if _, ok := index[el.CampaignID]; !ok {
					// Filtered on these campaigns, so this should not happen; a line item of a
					// campaign the monitor does not list cannot move any row's flight.
					continue
				}
				f := flights[el.CampaignID]
				if f == nil {
					f = &flightAcc{}
					flights[el.CampaignID] = f
				}
				foldLineItemFlight(f, el)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for id, f := range flights {
		ac := &out[index[id]]
		if f.bad {
			ac.FlightUnparseable = true
			continue
		}
		if f.start.IsZero() {
			continue
		}
		ac.StartDate = f.start.In(loc).Format(time.DateOnly)
		if !f.openEnded && !f.end.IsZero() {
			ac.EndDate = lastFlightDay(f.end, loc)
		}
		ac.Flights = flightRanges(f.spans, loc)
	}
	return nil
}

// flightRanges unions line-item intervals into account-local day ranges: each interval becomes
// [its start day, its last day] (lastFlightDay), and ranges that overlap or touch — the next one
// starts on or before the day after the current one ends — merge. An open-ended interval absorbs
// everything after its start. Every bounded span ends after it starts — foldLineItemFlight marks
// the whole flight unparseable rather than record an inverted or zero-length one — so a span's
// last day is never before its first. The days are compared as DATES, not instants, because
// "scheduled on day D" is what the rule engine needs and two line items one serving the morning,
// one the evening of the same day both schedule that day.
func flightRanges(spans []flightSpan, loc *time.Location) []FlightRange {
	type dayRange struct {
		first, last time.Time // UTC-midnight dates; last zero = open-ended
	}
	day := func(t time.Time) time.Time {
		l := t.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
	}
	rs := make([]dayRange, 0, len(spans))
	for _, sp := range spans {
		r := dayRange{first: day(sp.start)}
		if !sp.end.IsZero() {
			r.last = day(sp.end.Add(-time.Nanosecond))
		}
		rs = append(rs, r)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].first.Before(rs[j].first) })
	merged := make([]dayRange, 0, len(rs))
	for _, r := range rs {
		if n := len(merged); n > 0 {
			cur := &merged[n-1]
			if cur.last.IsZero() {
				continue // open-ended: already covers every later day
			}
			if !r.first.After(cur.last.AddDate(0, 0, 1)) {
				if r.last.IsZero() || r.last.After(cur.last) {
					cur.last = r.last
				}
				continue
			}
		}
		merged = append(merged, r)
	}
	out := make([]FlightRange, 0, len(merged))
	for _, r := range merged {
		fr := FlightRange{StartDate: r.first.Format(time.DateOnly)}
		if !r.last.IsZero() {
			fr.EndDate = r.last.Format(time.DateOnly)
		}
		out = append(out, fr)
	}
	return out
}

// foldLineItemFlight widens f by one line item — earliest start, latest end, open-ended if any
// line item has no end_time — and records the line item's own interval, so the gaps between line
// items survive into AccountCampaign.Flights. A start_time that is absent or unparseable marks the
// flight bad — X requires start_time on a line item (the create path sends it as REQUIRED), so its
// absence is not an "unscheduled" state this code can interpret. So does an end_time that is
// unparseable or not after start_time (inverted or zero-length), checked before the envelope or
// the spans change, so every recorded bounded span runs forward.
func foldLineItemFlight(f *flightAcc, el monitorLineItemElement) {
	if el.StartTime == nil {
		f.bad = true
		return
	}
	st, err := time.Parse(time.RFC3339, strings.TrimSpace(*el.StartTime))
	if err != nil {
		f.bad = true
		return
	}
	if el.EndTime == nil || strings.TrimSpace(*el.EndTime) == "" {
		if f.start.IsZero() || st.Before(f.start) {
			f.start = st
		}
		f.openEnded = true
		f.spans = append(f.spans, flightSpan{start: st})
		return
	}
	et, err := time.Parse(time.RFC3339, strings.TrimSpace(*el.EndTime))
	if err != nil || !et.After(st) {
		// An end at or before the start is as unreadable as a malformed one: the interval
		// serves no instant, so recording it would claim a day nothing could serve.
		f.bad = true
		return
	}
	if f.start.IsZero() || st.Before(f.start) {
		f.start = st
	}
	if et.After(f.end) {
		f.end = et
	}
	f.spans = append(f.spans, flightSpan{start: st, end: et})
}

// lastFlightDay returns the last calendar day (in loc) a flight ending at the INSTANT end still
// runs on. end_time is an instant at which serving stops, so an end exactly at local midnight
// means the flight ran through the previous day — the create path writes end dates exactly that
// way (YYYY-MM-DDT00:00:00Z) — while an end mid-day means it ran on that day too.
func lastFlightDay(end time.Time, loc *time.Location) string {
	return end.Add(-time.Nanosecond).In(loc).Format(time.DateOnly)
}

// parseLocalMicro reads an X *_local_micro amount as whole currency units. Absent or null is a
// legitimate "no budget of this kind" (0, true). A non-negative JSON integer is the amount.
// Anything else — a string, a fraction, a negative, an overflow — is malformed (0, false):
// treating it as 0 would present an unreadable budget as "no budget", which the rules read as a
// real fact about the campaign.
func parseLocalMicro(raw json.RawMessage) (float64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return fromMicroCurrency(n), true
}

// walkPages walks one account-scoped cursor-paginated list, calling page for each page's data.
// Strict, like ListAdAccounts: only X's documented null next_cursor ends the walk.
func (c *Client) walkPages(ctx context.Context, path string, page func(json.RawMessage) error) error {
	cursor := ""
	seen := make(map[string]struct{})
	for n := 0; n < maxListPages; n++ {
		resp, err := c.requestPage(ctx, path, cursor)
		if err != nil {
			return err
		}
		if resp == nil {
			return errors.New("empty response")
		}
		if len(resp.Data) == 0 || string(resp.Data) == "null" {
			return errors.New("2xx response carried no data field; the list cannot be confirmed complete")
		}
		if err := page(resp.Data); err != nil {
			return err
		}
		switch cursorVerdict(resp) {
		case cursorExhausted:
			return nil
		case cursorUnknowable:
			return errors.New("response carried no usable next_cursor (X signals the last page with an explicit null); the list cannot be confirmed complete")
		case cursorMore:
			if _, dup := seen[resp.NextCursor]; dup {
				return errors.New("pagination did not terminate (repeated page cursor)")
			}
			seen[resp.NextCursor] = struct{}{}
			cursor = resp.NextCursor
		}
	}
	return fmt.Errorf("exceeded %d pages", maxListPages)
}

// accountReportWindow returns the stats window for the trailing `days` days, today inclusive,
// in the account's timezone: [start of today-(days-1), start of the day after today), where a
// day's start is its local midnight — or, when a DST spring-forward skips that midnight, the
// first instant that exists on the day (localDayStart).
//
// The window QUERIED and the window REPORTED are always the same days: start/end are exactly the
// starts of firstDay and of the day after lastDay. Nothing is shifted to make a window
// fit, because the caller persists firstDay/lastDay and the rules pace on them — a query that
// silently covered different hours than the persisted days would attribute delivery to days it
// was not measured on. Instead:
//
//   - X requires "whole hours only" for start_time/end_time, and DAY granularity additionally
//     requires midnight in the account timezone (https://docs.x.com/x-ads-api/analytics,
//     Synchronous Analytics; the asynchronous section points to the same parameters). The
//     monitor asks for TOTAL, but aligning to account-midnight is what makes the window the
//     account's own calendar days, the unit its daily budgets reset on. When either day start
//     is not a whole UTC hour — a fractional-offset zone such as Asia/Kolkata — the
//     account's days cannot be queried exactly and the window is REFUSED with
//     ErrReportWindowNotWholeHours (fail closed), rather than floored to the hour.
//   - X caps a window at 90 days (maxStatsWindow). A 90-day window that crosses a DST fall-back
//     is 90 days and an hour of wall time, so its EARLIEST day is dropped: the window becomes
//     the trailing 89 whole local days, and firstDay says so (the account monitor response
//     exposes it as metrics_window_start, beside the requested `days`). Shortening by a whole
//     day keeps queried == reported; trimming one hour off the start (the earlier behaviour)
//     queried a window that began an hour into the first reported day.
//
// The returned dates are the window's first and last local calendar days, as UTC-midnight
// values (the convention model.AccountReportSubmission documents for report windows).
func accountReportWindow(now time.Time, loc *time.Location, days int) (start, end time.Time, firstDay, lastDay time.Time, err error) {
	local := now.In(loc)
	y, m, d := local.Date()
	today := localDayStart(y, m, d, loc)
	startLocal := localDayStart(y, m, d-(days-1), loc)
	endLocal := localDayStart(y, m, d+1, loc)
	if endLocal.Sub(startLocal) > maxStatsWindow {
		startLocal = localDayStart(y, m, d-(days-2), loc)
	}
	start, end = startLocal.UTC(), endLocal.UTC()
	if !start.Equal(start.Truncate(time.Hour)) || !end.Equal(end.Truncate(time.Hour)) {
		return time.Time{}, time.Time{}, time.Time{}, time.Time{}, ErrReportWindowNotWholeHours
	}
	firstDay = time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), 0, 0, 0, 0, time.UTC)
	lastDay = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	return start, end, firstDay, lastDay, nil
}

// localDayStart returns the first instant whose local calendar date is ON OR AFTER y-m-d (d may be
// out of range; it is normalized like time.Date). That is local midnight, except where a
// transition removes it: time.Date normalizes a nonexistent local midnight BACKWARDS, into the
// previous day, which would put part of that day into the window and date the window a day early.
//
//   - A DST spring-forward that skips 00:00 (America/Santiago, America/Asuncion, …): the day
//     begins at the first instant after the gap (01:00 for a one-hour gap).
//   - A day skipped ENTIRELY (Pacific/Apia jumped from 2011-12-29 to 2011-12-31): no instant has
//     that date, so the result is the start of the next real day, and the caller's firstDay says
//     so — the reported days stay the queried days.
//
// It walks forward in 15-minute steps (every offset in tzdata is a multiple of 15 minutes, so the
// first step that crosses a gap lands exactly on its end) for at most 48 hours, which covers any
// real transition including Apia's 24-hour jump.
func localDayStart(y int, m time.Month, d int, loc *time.Location) time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, loc)
	want := time.Date(y, m, d, 0, 0, 0, 0, time.UTC) // the intended calendar date, normalized
	for i := 0; i < 48*4 && localDate(t).Before(want); i++ {
		t = t.Add(15 * time.Minute)
	}
	return t
}

// localDate is t's local calendar date as a UTC-midnight value.
func localDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// statsTime renders an instant the way X's documented examples do: "2026-03-12T00:00:00Z".
func statsTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func (c *Client) statsJobsURL() string {
	return fmt.Sprintf("%s/%s/stats/jobs/accounts/%s", c.baseURL, c.apiVersion, c.account.AccountID)
}

// activeEntity is one row of GET stats/accounts/:account_id/active_entities.
type activeEntity struct {
	EntityID string `json:"entity_id"`
}

// statsJobElement is the part of a stats job (POST response data, or GET status data[]) read here.
type statsJobElement struct {
	IDStr  string  `json:"id_str"`
	Status string  `json:"status"`
	URL    *string `json:"url"`
}

// SubmitAccountCampaignReport asks X to build the account's per-campaign totals for the trailing
// `days` days (today inclusive, in the account's timezone) and returns as soon as X has accepted
// the jobs. It does not wait: the caller persists reportID and collects it later with
// CheckAccountCampaignReport.
//
//  1. GET stats/accounts/:account_id/active_entities?entity=CAMPAIGN&start_time&end_time — the
//     campaigns with any activity in the window. X: "The Active Entities endpoint should dictate
//     how analytics requests are made" (https://docs.x.com/x-ads-api/analytics, Active Entities).
//     A campaign NOT listed had no activity, so the caller's reading of "absent from the
//     finished report" as a measured zero holds.
//  2. POST stats/jobs/accounts/:account_id per <=20 active campaign ids, with entity=CAMPAIGN,
//     granularity=TOTAL, placement=ALL_ON_TWITTER, metric_groups=ENGAGEMENT,BILLING — the same
//     parameter set GetCampaignMetrics sends synchronously, which the asynchronous endpoint
//     mirrors (https://docs.x.com/x-ads-api/analytics, Asynchronous Analytics, example POST).
//
// Each job POST waits on the client's write pacer first. UNVERIFIED whether X counts a job
// creation against the 1 write/sec account budget (the docs do not say); pacing it costs at most
// a second per job and cannot breach that budget either way. A job POST is not retried on a 429
// (idempotent=false): a throttled create may have committed, and a retry could build a second
// job nobody collects.
//
// The returned reportID is the comma-joined id_str of every job, in creation order — one id the
// caller can persist (account_monitor_reports.pending_report_id is TEXT; ten 20-digit ids plus
// separators is ~210 bytes). With no active campaign it is NoActiveCampaignsReportID.
//
// Before the first job POST the remaining time on ctx is compared with what the paced POSTs
// need — one pacer interval per job plus statsJobSubmitMargin — and the submission is refused
// with ErrStatsJobBudget, creating nothing, when it cannot fit. The caller shares one call
// budget across the campaign list, the pending-report check and this submission, so the time
// left here varies; a deadline that fired mid-loop would strand the jobs already created.
//
// Two refusals are PERMANENT and come before any job is created: ErrReportWindowNotWholeHours
// (the account's timezone cannot be queried on its own days; see accountReportWindow) and
// ErrTooManyActiveCampaigns (more than MaxMonitorActiveCampaigns active in the window).
//
// If a later job POST fails anyway, the jobs already created are abandoned: they finish on X's
// side and expire uncollected. That costs job slots (X allows 100 concurrent per account), never
// data.
func (c *Client) SubmitAccountCampaignReport(ctx context.Context, days int) (reportID string, windowStart, windowEnd time.Time, err error) {
	if days < 1 || time.Duration(days)*24*time.Hour > maxStatsWindow {
		return "", time.Time{}, time.Time{}, fmt.Errorf("x account report window must be 1..90 days, got %d", days)
	}
	if err := ValidateMonitorAccountID(c.account.AccountID); err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	loc, err := c.AccountTimezone(ctx)
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	start, end, firstDay, lastDay, err := accountReportWindow(c.timeFn(), loc, days)
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}

	ids, err := c.activeCampaignIDs(ctx, start, end)
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	if len(ids) == 0 {
		return NoActiveCampaignsReportID, firstDay, lastDay, nil
	}
	jobs := (len(ids) + statsJobMaxEntities - 1) / statsJobMaxEntities
	if jobs > maxStatsJobsPerReport {
		return "", time.Time{}, time.Time{}, fmt.Errorf("%w (%d active)", ErrTooManyActiveCampaigns, len(ids))
	}
	if err := c.statsJobsFitBudget(ctx, jobs); err != nil {
		return "", time.Time{}, time.Time{}, err
	}

	jobIDs := make([]string, 0, jobs)
	for i := 0; i < len(ids); i += statsJobMaxEntities {
		chunk := ids[i:min(i+statsJobMaxEntities, len(ids))]
		id, jerr := c.createStatsJob(ctx, chunk, start, end)
		if jerr != nil {
			return "", time.Time{}, time.Time{}, jerr
		}
		jobIDs = append(jobIDs, id)
	}
	return strings.Join(jobIDs, ","), firstDay, lastDay, nil
}

// statsJobsFitBudget refuses (ErrStatsJobBudget) when ctx's deadline is closer than the pacer's
// current BACKLOG (writes other callers on this client have already reserved: nextWrite - now)
// plus `jobs` pacer intervals plus statsJobSubmitMargin. Without the backlog, a caller queued
// behind another reader's job POSTs would pass the check and then run out of time mid-loop,
// abandoning the jobs it had created. The deadline is wall-clock, so it is measured with
// time.Until; the backlog is in the pacer's own (injectable) clock. With no deadline there is
// nothing to fit.
func (c *Client) statsJobsFitBudget(ctx context.Context, jobs int) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	pacing := time.Duration(0)
	if c.writeDelay > 0 {
		pacing = time.Duration(jobs) * c.writeDelay
		if backlog := c.nextWriteAt().Sub(c.timeFn()); backlog > 0 {
			pacing += backlog
		}
	}
	need := pacing + statsJobSubmitMargin
	if left := time.Until(deadline); left < need {
		return fmt.Errorf("%w: %d jobs need about %s, %s left", ErrStatsJobBudget, jobs, need, left.Truncate(time.Millisecond))
	}
	return nil
}

// activeCampaignIDs reads the campaigns active in [start, end), deduplicated, in first-seen order.
func (c *Client) activeCampaignIDs(ctx context.Context, start, end time.Time) ([]string, error) {
	params := map[string]string{
		"entity":     "CAMPAIGN",
		"start_time": statsTime(start),
		"end_time":   statsTime(end),
	}
	resp, err := c.doRequestAbs(ctx, http.MethodGet, c.statsURL()+"/active_entities", "stats/active_entities", params, true /* idempotent: read */)
	if err != nil {
		return nil, fmt.Errorf("read x active campaigns: %w", err)
	}
	var els []activeEntity
	if resp != nil && len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &els); err != nil {
			return nil, errors.New("read x active campaigns: data is not a list of entities")
		}
	}
	if els == nil {
		// Absent or null data is not "no active campaigns": an empty answer here produces a
		// report of zeros for the whole account, so it must rest on an explicit [].
		return nil, errors.New("read x active campaigns: 2xx response carried no data field")
	}
	ids := make([]string, 0, len(els))
	seen := make(map[string]struct{}, len(els))
	for _, el := range els {
		if !campaignIDRe.MatchString(el.EntityID) {
			return nil, errors.New("read x active campaigns: an entity has an unusable id")
		}
		if _, dup := seen[el.EntityID]; dup {
			continue
		}
		seen[el.EntityID] = struct{}{}
		ids = append(ids, el.EntityID)
	}
	return ids, nil
}

// createStatsJob POSTs one stats job and returns its id_str.
func (c *Client) createStatsJob(ctx context.Context, campaignIDs []string, start, end time.Time) (string, error) {
	resp, err := c.postStatsJob(ctx, campaignIDs, start, end, "")
	if err != nil {
		return "", err
	}
	var job statsJobElement
	if resp == nil || len(resp.Data) == 0 || json.Unmarshal(resp.Data, &job) != nil {
		return "", errors.New("create x stats job: response carried no job object")
	}
	if !validStatsJobID(job.IDStr) {
		return "", errors.New("create x stats job: response carried no usable id_str")
	}
	return job.IDStr, nil
}

// postStatsJob paces, then POSTs one CAMPAIGN stats job over [start, end) with the parameter set
// both job creators share: granularity=TOTAL, placement=ALL_ON_TWITTER,
// metric_groups=ENGAGEMENT,BILLING. segmentation, when non-empty, is sent as segmentation_type
// (the audience read; https://docs.x.com/x-ads-api/analytics, Segmentation). The caller decodes
// the response.
func (c *Client) postStatsJob(ctx context.Context, campaignIDs []string, start, end time.Time, segmentation string) (*apiResponse, error) {
	if err := c.pace(ctx); err != nil {
		return nil, fmt.Errorf("create x stats job: %w", err)
	}
	params := map[string]string{
		"entity":        "CAMPAIGN",
		"entity_ids":    strings.Join(campaignIDs, ","),
		"start_time":    statsTime(start),
		"end_time":      statsTime(end),
		"granularity":   "TOTAL",
		"placement":     "ALL_ON_TWITTER",
		"metric_groups": "ENGAGEMENT,BILLING",
	}
	if segmentation != "" {
		params["segmentation_type"] = segmentation
	}
	resp, err := c.doRequestAbs(ctx, http.MethodPost, c.statsJobsURL(), "stats/jobs", params, false /* a repeated create builds a second job */)
	if err != nil {
		return nil, fmt.Errorf("create x stats job: %w", err)
	}
	return resp, nil
}

func validStatsJobID(id string) bool {
	return len(id) <= maxStatsJobIDLen && statsJobIDRe.MatchString(id)
}

// parseStatsReportID splits a composite report id back into job ids, refusing anything
// SubmitAccountCampaignReport could not have produced.
func parseStatsReportID(reportID string) ([]string, error) {
	parts := strings.Split(reportID, ",")
	if len(parts) == 0 || len(parts) > maxStatsJobsPerReport {
		return nil, errors.New("x account report id names an unexpected number of stats jobs")
	}
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		if !validStatsJobID(p) {
			return nil, errors.New("x account report id is not a list of stats job ids")
		}
		if _, dup := seen[p]; dup {
			return nil, errors.New("x account report id repeats a stats job id")
		}
		seen[p] = struct{}{}
	}
	return parts, nil
}

// AccountReportStatus is the state of a submitted account report, as of one status read.
type AccountReportStatus string

// Account report statuses.
const (
	// AccountReportStatusPending: at least one job is still building (or X's status read did not
	// list it yet). Ask again later with the same report id.
	AccountReportStatusPending AccountReportStatus = "pending"
	// AccountReportStatusSuccess: every job finished and Rows is populated.
	AccountReportStatusSuccess AccountReportStatus = "success"
	// AccountReportStatusFailed: X failed or cancelled a job, or a finished job's file is no
	// longer offered. Asking again cannot help; the caller must submit a fresh report.
	AccountReportStatusFailed AccountReportStatus = "failed"
)

// AccountReportRow is one campaign's totals over the report window.
type AccountReportRow struct {
	CampaignID  string
	Impressions int64
	Clicks      int64
	// SpendMicro is billed_charge_local_micro, in the ACCOUNT's currency micros.
	SpendMicro int64
}

// AccountReportResult is the outcome of one CheckAccountCampaignReport.
type AccountReportResult struct {
	Status AccountReportStatus
	// Rows is set only for AccountReportStatusSuccess: one row per campaign that appears in any
	// job's file, in first-appearance order. Non-nil when empty.
	Rows []AccountReportRow
	// Partial is true on every success: X's billed_charge_local_micro is "an estimate for up to
	// 3 days" after the event and is adjusted "up to 14 days out"
	// (https://docs.x.com/x-ads-api/analytics, Metrics), and the window always includes today.
	Partial bool
}

// Job status literals. "PROCESSING" and "SUCCESS" are the two X documents in its examples
// (https://docs.x.com/x-ads-api/analytics, Asynchronous Analytics). UNVERIFIED: the queued and
// terminal-failure spellings are not in that page; they are the values X's tooling and developer
// forum use. An unrecognised status is an ERROR, not "pending", so a contract change surfaces
// instead of leaving a report that never arrives (the orchestrator abandons an uncheckable
// report after an hour either way).
const (
	jobStatusQueued     = "QUEUED"
	jobStatusProcessing = "PROCESSING"
	jobStatusSuccess    = "SUCCESS"
	jobStatusFailed     = "FAILED"
	jobStatusFailure    = "FAILURE"
	jobStatusCancelled  = "CANCELLED"
)

// CheckAccountCampaignReport reads the status of every job in reportID with ONE request —
// GET stats/jobs/accounts/:account_id?job_ids=<all> ("up to 200 job IDs",
// https://docs.x.com/x-ads-api/analytics) — and, if all have finished, downloads and folds them.
//
// Precedence: any failed or cancelled job fails the whole report (it can never be completed, and
// a report missing a job's campaigns would read them as having served nothing); otherwise any
// job still building — or ABSENT from X's answer, which X's developer forum reports happening
// for a job_ids list — leaves the report pending; otherwise it is downloaded. A SUCCESS job with
// no url is treated as failed: the file is gone (X sets an expires_at on it), so collecting this
// report is impossible and only a fresh one can help.
//
// NoActiveCampaignsReportID is answered without any request: a finished, empty report.
func (c *Client) CheckAccountCampaignReport(ctx context.Context, reportID string) (*AccountReportResult, error) {
	if reportID == NoActiveCampaignsReportID {
		return &AccountReportResult{Status: AccountReportStatusSuccess, Rows: []AccountReportRow{}, Partial: true}, nil
	}
	jobIDs, err := parseStatsReportID(reportID)
	if err != nil {
		return nil, err
	}
	if err := ValidateMonitorAccountID(c.account.AccountID); err != nil {
		return nil, err
	}
	resp, err := c.doRequestAbs(ctx, http.MethodGet, c.statsJobsURL(), "stats/jobs",
		map[string]string{"job_ids": strings.Join(jobIDs, ",")}, true /* idempotent: read */)
	if err != nil {
		return nil, fmt.Errorf("read x stats jobs: %w", err)
	}
	var els []statsJobElement
	if resp != nil && len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &els); err != nil {
			return nil, errors.New("read x stats jobs: data is not a list of jobs")
		}
	}
	if els == nil {
		return nil, errors.New("read x stats jobs: 2xx response carried no data field")
	}
	byID := make(map[string]statsJobElement, len(els))
	for _, el := range els {
		byID[el.IDStr] = el
	}

	pending := false
	urls := make([]string, 0, len(jobIDs))
	for _, id := range jobIDs {
		el, ok := byID[id]
		if !ok {
			pending = true
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(el.Status)) {
		case jobStatusQueued, jobStatusProcessing:
			pending = true
		case jobStatusFailed, jobStatusFailure, jobStatusCancelled:
			return &AccountReportResult{Status: AccountReportStatusFailed}, nil
		case jobStatusSuccess:
			if el.URL == nil || strings.TrimSpace(*el.URL) == "" {
				return &AccountReportResult{Status: AccountReportStatusFailed}, nil
			}
			urls = append(urls, strings.TrimSpace(*el.URL))
		default:
			return nil, errors.New("read x stats jobs: a job has an unrecognised status")
		}
	}
	if pending {
		return &AccountReportResult{Status: AccountReportStatusPending}, nil
	}

	acc := newStatsFold()
	for _, u := range urls {
		data, derr := c.downloadStatsFile(ctx, u)
		if derr != nil {
			return nil, derr
		}
		if ferr := acc.add(data); ferr != nil {
			return nil, ferr
		}
	}
	return &AccountReportResult{Status: AccountReportStatusSuccess, Rows: acc.rows, Partial: true}, nil
}

// downloadStatsFile fetches one job's results file and returns its decompressed JSON.
//
// The file URL is a public storage URL (X's example is https://ton.twimg.com/...json.gz), and X
// documents that it "requires no authentication once obtained"
// (https://docs.x.com/x-ads-api/analytics, Asynchronous Analytics). So the request is NOT
// OAuth-signed and carries no Authorization header: signing it would hand our credentials to a
// host that neither needs nor checks them. The URL is upstream data, so it is held to https on
// statsFileHost exactly (or to this client's own API origin, which is how most tests serve it),
// and it never appears in an error — the same discipline microsoft's downloadReportRecords
// applies to its pre-signed URL.
func (c *Client) downloadStatsFile(ctx context.Context, rawURL string) ([]byte, error) {
	if !c.statsFileURLAllowed(rawURL) {
		return nil, errors.New("download x stats file: the job's file url is not on an allowed host")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, errors.New("download x stats file: build request")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("download x stats file: %w", context.Canceled)
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("download x stats file: %w", context.DeadlineExceeded)
		}
		return nil, errors.New("download x stats file: transport error")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, c.statsFileCompressedCap))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download x stats file: unexpected status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.statsFileCompressedCap+1))
	if err != nil {
		return nil, errors.New("download x stats file: read failed")
	}
	if int64(len(raw)) > c.statsFileCompressedCap {
		return nil, fmt.Errorf("download x stats file: file exceeds %d bytes", c.statsFileCompressedCap)
	}
	// The file is gzip ("*.json.gz"). If a transport decompressed it on the way in (a server
	// sending Content-Encoding: gzip makes net/http do so transparently), the bytes are already
	// JSON; the gzip magic number tells the two apart.
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		return raw, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("download x stats file: not a valid gzip file")
	}
	defer func() { _ = zr.Close() }()
	data, err := io.ReadAll(io.LimitReader(zr, c.statsFileDecompressedCap+1))
	if err != nil {
		return nil, errors.New("download x stats file: gzip stream is corrupt")
	}
	if int64(len(data)) > c.statsFileDecompressedCap {
		return nil, fmt.Errorf("download x stats file: decompressed file exceeds %d bytes", c.statsFileDecompressedCap)
	}
	return data, nil
}

// statsFileURLAllowed admits, with no userinfo, an https URL whose host is exactly one of the
// client's statsFileHosts (statsFileHost in production), or a URL on this client's own API
// origin. Everything else — any other https host included — is refused.
func (c *Client) statsFileURLAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Scheme, "https") {
		for _, h := range c.statsFileHosts {
			if strings.EqualFold(u.Host, h) {
				return true
			}
		}
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, base.Scheme) && strings.EqualFold(u.Host, base.Host)
}

// statsFile is a job's results file: "Response JSON mirrors synchronous endpoint structure"
// (https://docs.x.com/x-ads-api/analytics), so it reuses GetCampaignMetrics' statsEntity.
type statsFile struct {
	Data []statsEntity `json:"data"`
}

// statsFold accumulates per-campaign totals across jobs' files.
type statsFold struct {
	rows  []AccountReportRow
	index map[string]int
}

func newStatsFold() *statsFold {
	return &statsFold{rows: make([]AccountReportRow, 0), index: make(map[string]int)}
}

// add folds one decompressed file. Every id_data entry of a campaign is summed (with no
// segmentation requested there is one, but summing is what several would mean), as is a campaign
// appearing in more than one file. Each metric array is summed across its buckets (TOTAL
// granularity has one) — X sends null for a metric with no activity, which is a real zero.
// An entity with an unusable id, or a negative or overflowing value, fails the whole report:
// an unattributable row cannot be dropped without its campaign reading as "served nothing".
func (f *statsFold) add(data []byte) error {
	var file statsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return errors.New("decode x stats file: not a stats document")
	}
	if file.Data == nil {
		return errors.New("decode x stats file: no data field")
	}
	for _, ent := range file.Data {
		if !campaignIDRe.MatchString(ent.ID) {
			return errors.New("decode x stats file: an entity has an unusable id")
		}
		i, ok := f.index[ent.ID]
		if !ok {
			i = len(f.rows)
			f.index[ent.ID] = i
			f.rows = append(f.rows, AccountReportRow{CampaignID: ent.ID})
		}
		r := &f.rows[i]
		for _, seg := range ent.IDData {
			var err error
			if r.Impressions, err = addMetric(r.Impressions, seg.Metrics.Impressions); err != nil {
				return err
			}
			if r.Clicks, err = addMetric(r.Clicks, seg.Metrics.Clicks); err != nil {
				return err
			}
			if r.SpendMicro, err = addMetric(r.SpendMicro, seg.Metrics.BilledChargeLocalMicro); err != nil {
				return err
			}
		}
	}
	return nil
}

func addMetric(total int64, buckets []int64) (int64, error) {
	for _, v := range buckets {
		if v < 0 {
			return 0, errors.New("decode x stats file: a metric is negative")
		}
		if total > math.MaxInt64-v {
			return 0, errors.New("decode x stats file: a metric overflows")
		}
		total += v
	}
	return total, nil
}
