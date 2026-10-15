// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// monitorReportConcurrency bounds how many per-campaign report calls ListAccountCampaigns
// runs at once (round-19 review): the whole read is capped at a fixed deadline by its caller,
// and a serial one-request-per-campaign loop can exceed that deadline on an account with
// several campaigns even when each individual request is healthy. Mirrors the ported BFF's
// own batch size (reddit-ads.service.ts).
const monitorReportConcurrency = 5

// monitorMaxPages caps every page walk the monitor read makes — the campaign list, and each
// campaign's report. A walk that still has a next page at the cap FAILS rather than returning
// what it has: a truncated campaign list silently drops campaigns from the monitor view, and a
// truncated report silently under-counts spend, and both look exactly like a complete answer.
// Mirrors the page-capped walks Meta's and LinkedIn's monitor reads make for the same reason.
const monitorMaxPages = 50

// Reddit's campaign goal_type values this read knows how to pace. LIFETIME_SPEND is the
// literal this client's own create path sends (client.go), so it is the one value proven to
// exist on campaigns this service creates; DAILY_SPEND is Reddit's per-day counterpart. Any
// other value (or none) means this read cannot tell what goal_value is a budget FOR, so the
// row carries neither budget and is reported as pacing-unknown rather than paced against a
// guess. Named for the monitor so they cannot collide with any write-path constant.
const (
	monitorGoalTypeLifetimeSpend = "LIFETIME_SPEND"
	monitorGoalTypeDailySpend    = "DAILY_SPEND"
)

// AccountCampaignRow is one campaign read live from an ad account for the account-monitor
// endpoint, ported from lfx-self-serve's reddit-ads.service.ts (getRedditAnalytics).
//
// Conversions is deliberately absent (unlike the sibling platforms' AccountCampaignRow
// types): this read does not request conversions from Reddit, so there is nothing for the
// field to carry. The dispatcher leaves model.AccountCampaignMetrics.Conversions nil to say
// exactly that. The BFF hardcoded campaignMetrics[].conversions to a literal 0 on every row
// (reddit-ads.service.ts:300), which is not a measurement — see
// linuxfoundation/lfx-self-serve#3020.
type AccountCampaignRow struct {
	CampaignID  string
	Name        string
	Status      string
	Impressions int64
	Clicks      int64
	Ctr         float64
	SpendUSD    float64
	// TotalBudget and DailyBudget are goal_value/1_000_000, routed by goal_type:
	// LIFETIME_SPEND sets TotalBudget, DAILY_SPEND sets DailyBudget, and anything else —
	// including an absent goal_type — sets neither, so the rule engine reports pacing as
	// unknown. DIVERGES from the BFF, which read goal_value as a lifetime budget whatever
	// goal_type said and hardcoded dailyBudget to 0 (reddit-ads.service.ts:266-267): a
	// DAILY_SPEND campaign's per-day cap was then prorated across its whole flight as if it
	// were the lifetime total, reporting a campaign spending exactly its daily cap as
	// heavily overspending.
	TotalBudget float64
	DailyBudget float64
	StartDate   string
	EndDate     string
	// FetchFailed is set when this campaign's own report read failed or could not be
	// believed (a row attributed to another campaign, a missing field, a page walk past
	// monitorMaxPages). DIVERGES from the BFF's own behavior on purpose: reddit-ads.service.ts
	// substitutes a fabricated {impressions:0,clicks:0,spend:0} on a Promise.allSettled
	// rejection and only logs a warning (reddit-ads.service.ts:243-251) — indistinguishable,
	// downstream, from a campaign that genuinely had zero activity.
	// model.AccountCampaignMetrics.FetchFailed's own doc comment names this exact case as one
	// of the reasons the field exists, so this port sets it instead of fabricating the zero.
	// This is a disclosed, deliberate divergence, not a preserved bug.
	FetchFailed bool
	// FailureClass is why FetchFailed is set (monitorFailureClass), so the service layer can
	// tell a refused token from a throttle from a malformed report without the dropped error.
	FailureClass model.MonitorFailureClass
}

// campaignElement is one entry of the campaign-list response, per the fields
// getRedditAnalytics reads off RedditCampaignElement.
type campaignElement struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	ConfiguredStatus string  `json:"configured_status"`
	GoalValue        *int64  `json:"goal_value"`
	GoalType         *string `json:"goal_type"`
	StartTime        string  `json:"start_time"`
	EndTime          string  `json:"end_time"`
}

// campaignListResponse tolerates the same three response shapes fetchCampaigns does
// (reddit-ads.service.ts:191-203): resp.data as a bare array, resp.data.campaigns as an
// array, or (defensively) resp.data itself treated as the array in the fallback arm. Ported
// faithfully rather than simplified: the campaign-list operation (GET
// /ad_accounts/{id}/campaigns) is NOT among the operations LFXV2-3282 checked against Reddit's
// published OpenAPI spec — that check covered only POST /ad_accounts/{id}/reports — and no
// live Reddit account has exercised this client, so narrowing to one shape here could silently
// return zero campaigns against a real account that uses either of the other two.
//
// decodeCampaignList reports ok=false when data matches NEITHER known shape — that is a
// malformed/unrecognized response, not a legitimately empty account, and its caller must not
// treat the two the same way. DIVERGES from the BFF here (round-30+ review): fetchCampaigns
// swallows a shape mismatch into an empty list, indistinguishable downstream from "this
// account really has zero campaigns" — the same false-empty-result failure mode the report
// read's own strict decode (decodeReportRows) refuses to reproduce. An absent/empty body
// (len(data)==0) is still a legitimate empty account: Reddit's own API returns that for "no
// campaigns," not for a decode failure.
func decodeCampaignList(data json.RawMessage) (elements []campaignElement, ok bool) {
	if len(data) == 0 {
		return nil, true
	}
	// Shape 1: a bare array.
	var bare []campaignElement
	if err := json.Unmarshal(data, &bare); err == nil {
		return bare, true
	}
	// Shape 2: {"campaigns": [...]}.
	var wrapped struct {
		Campaigns []campaignElement `json:"campaigns"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Campaigns != nil {
		return wrapped.Campaigns, true
	}
	// Neither shape matched: malformed/unrecognized response.
	return nil, false
}

// paginationEnvelope is the top-level "pagination" object of a Reddit Ads v3 response.
//
// VERIFICATION LEVEL: LFXV2-3282 verified against the published spec that the report
// response carries a "pagination" object beside "data", but this repository does not record
// that object's fields. next_url — an absolute URL for the following page, absent or null on
// the last one — is Reddit's documented v3 list-pagination convention, NOT checked against
// the spec by this repository and never exercised against a live account. A response without
// the key is read as a single page, which is exactly the pre-pagination behaviour, so a wrong
// field name here regresses to the old truncation rather than to a new wrong answer.
type paginationEnvelope struct {
	NextURL *string `json:"next_url"`
}

// nextPagePath turns a response's raw "pagination" object into the request path (relative
// to c.baseURL, query included) of the next page, or "" when there is none.
//
// The next URL is upstream content, and following it sends this project's bearer token.
// So it must resolve to the SAME scheme and host as the configured API base and sit under
// its path; anything else is refused rather than followed. No part of the URL is echoed in
// an error: it can carry the ad account id, and these errors reach the service log.
//
// It must also name the SAME RESOURCE as the request being paged (reqPath, relative to the
// base, query ignored): only the query may change between pages. Same origin alone is not
// enough — a next_url naming another ad account on the same host would be fetched with this
// project's token and its campaigns listed under the requested account, bypassing the
// dispatcher's account binding; a report walk could likewise re-POST its body to another
// resource.
func (c *Client) nextPagePath(raw json.RawMessage, reqPath string) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var p paginationEnvelope
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", fmt.Errorf("pagination is not an object (%d bytes)", len(raw))
	}
	if p.NextURL == nil || strings.TrimSpace(*p.NextURL) == "" {
		return "", nil
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return "", errors.New("api base URL does not parse")
	}
	ref, err := url.Parse(strings.TrimSpace(*p.NextURL))
	if err != nil {
		return "", errors.New("pagination next_url is not a URL")
	}
	next := base.ResolveReference(ref)
	if next.Scheme != base.Scheme || next.Host != base.Host {
		return "", errors.New("pagination next_url points off the API origin; refusing to send credentials there")
	}
	// The containment check runs on the DECODED path, and refuses any dot segment. Checking the
	// escaped form alone let `%2e%2e` through: ResolveReference only removes LITERAL dot
	// segments, and request()'s per-segment decode-and-re-escape turns `%2e%2e` back into a
	// literal `..` that the server resolves outside the API base. Same origin either way — the
	// token cannot leave the host — but "under the API path" is the promise this makes.
	for _, seg := range strings.Split(next.Path, "/") {
		if seg == "." || seg == ".." {
			return "", errors.New("pagination next_url contains a dot segment")
		}
	}
	if !strings.HasPrefix(next.Path, strings.TrimRight(base.Path, "/")+"/") {
		return "", errors.New("pagination next_url is outside the API base path")
	}
	basePath := strings.TrimRight(base.EscapedPath(), "/")
	nextPath := next.EscapedPath()
	if !strings.HasPrefix(nextPath, basePath+"/") {
		return "", errors.New("pagination next_url is outside the API base path")
	}
	rel := strings.TrimPrefix(nextPath, basePath)
	wantPath, _, _ := strings.Cut(reqPath, "?")
	if rel != wantPath {
		return "", errors.New("pagination next_url names a different resource than the request being paged")
	}
	if next.RawQuery != "" {
		rel += "?" + next.RawQuery
	}
	return rel, nil
}

// errPageCapReached is returned by walkPages when a walk still has a next page at
// monitorMaxPages.
var errPageCapReached = fmt.Errorf("response still had more pages after %d; refusing to return a truncated result", monitorMaxPages)

// walkPages issues method/path (with body re-sent on every page), hands each page's response
// to visit, and follows nextPagePath until there is no next page. It fails — never truncates
// — on a cap overrun, a repeated page, or an unfollowable next link.
//
// For a POST report the same body is re-sent to the next page's URL. That is the natural
// reading of a next_url that carries its own page token, but like next_url itself it is not
// verified (see paginationEnvelope).
func (c *Client) walkPages(ctx context.Context, method, path string, body any, visit func(page int, resp *apiResponse) error) error {
	return c.walkPagesCapped(ctx, method, path, body, monitorMaxPages, errPageCapReached, visit)
}

// walkPagesCapped is walkPages with the page cap, and the error returned on reaching it, made
// the caller's. Account discovery (accounts.go) walks with its own, smaller caps; the monitor
// reads keep monitorMaxPages through walkPages above, unchanged.
func (c *Client) walkPagesCapped(ctx context.Context, method, path string, body any, maxPages int, capErr error, visit func(page int, resp *apiResponse) error) error {
	seen := map[string]struct{}{path: {}}
	for page := 1; ; page++ {
		resp, err := c.request(ctx, method, path, body)
		if err != nil {
			return err
		}
		if err := visit(page, resp); err != nil {
			return err
		}
		next, err := c.nextPagePath(resp.Pagination, path)
		if err != nil {
			return fmt.Errorf("page %d: %w", page, err)
		}
		if next == "" {
			return nil
		}
		if page >= maxPages {
			return capErr
		}
		if _, dup := seen[next]; dup {
			return fmt.Errorf("page %d: pagination repeated an earlier page", page)
		}
		seen[next] = struct{}{}
		path = next
	}
}

// listMonitorCampaigns reads every page of the account's campaign list.
//
// It used to read one page: apiResponse dropped the pagination envelope, so an account with
// more campaigns than Reddit's default page size silently lost the rest from the monitor view
// — the same data-completeness defect the account-monitor doc records Meta's BFF read having,
// fixed here the same way (follow every page; fail loudly at a cap).
//
// A campaign id seen twice across pages fails the read: one campaign would otherwise appear
// as two rows, and the account totals (a row sum) would count its spend twice.
func (c *Client) listMonitorCampaigns(ctx context.Context, accountID string) ([]campaignElement, error) {
	var all []campaignElement
	ids := map[string]struct{}{}
	err := c.walkPages(ctx, http.MethodGet, "/ad_accounts/"+accountID+"/campaigns", nil, func(page int, resp *apiResponse) error {
		elements, ok := decodeCampaignList(resp.Data)
		if !ok {
			return fmt.Errorf("page %d: malformed campaign-list response (%d bytes)", page, len(resp.Data))
		}
		for _, e := range elements {
			if e.ID != "" {
				if _, dup := ids[e.ID]; dup {
					return fmt.Errorf("page %d: campaign list returned the same campaign twice", page)
				}
				ids[e.ID] = struct{}{}
			}
			all = append(all, e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// fetchMonitorCampaignReport reads one campaign's impressions/clicks/spend over
// [startsAt, endsAt] from the ONE reporting operation this repository has verified against
// Reddit's published OpenAPI spec: POST /ad_accounts/{id}/reports (LFXV2-3282, see
// GetCampaignMetrics). The request body is GetCampaignMetrics' own — UPPERCASE field enums,
// CAMPAIGN_ID requested as a FIELD so every row carries the id it is attributed to, the
// campaign scoped by the `filter` DSL, no `breakdowns` — and the rows go through the same
// sumReportRows, so the monitor and the single-campaign read cannot disagree about which rows
// they believe.
//
// It used to POST to the per-campaign nested path /ad_accounts/{id}/campaigns/{cid}/reports,
// copied from the BFF; nothing in this repository verifies that operation exists. It also
// read only metrics[0] — any further row was dropped, under-counting a report Reddit split —
// and checked no campaign id, so a row for another campaign would have been reported as this
// one's.
//
// Why per-campaign calls rather than ONE account report broken down by CAMPAIGN_ID: the
// spec's `breakdowns` enum does include CAMPAIGN_ID, but a broken-down account report returns
// one row per campaign and so depends on report pagination for completeness, and this
// repository has not verified that envelope's fields (see paginationEnvelope). A campaign
// missing from an unfollowed next page would read as a legitimate zero. A per-campaign
// aggregate is one row in the expected case, so an unverified pagination field cannot
// silently drop a campaign's numbers; and a per-campaign failure keeps its own FetchFailed
// rather than failing every row. Pagination is still followed, page-capped, in case Reddit
// does split the result.
//
// An empty metrics array is real zero activity, exactly as GetCampaignMetrics treats it; a
// missing or null metrics array is an error (decodeReportRows).
func (c *Client) fetchMonitorCampaignReport(ctx context.Context, accountID, campaignID, startsAt, endsAt string) (impressions, clicks, spendMicros int64, err error) {
	reqBody := map[string]any{
		"data": map[string]any{
			"starts_at": startsAt,
			"ends_at":   endsAt,
			"fields":    []string{"CAMPAIGN_ID", "IMPRESSIONS", "CLICKS", "SPEND"},
			"filter":    "campaign:id==" + campaignID,
		},
	}
	var rows []reportRow
	werr := c.walkPages(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/reports", reqBody, func(_ int, resp *apiResponse) error {
		pageRows, derr := decodeReportRows(resp.Data)
		if derr != nil {
			return fmt.Errorf("%w: %w", errMonitorReportUndecodable, derr)
		}
		rows = append(rows, pageRows...)
		return nil
	})
	if werr != nil {
		return 0, 0, 0, werr
	}
	impressions, clicks, spendMicros, err = sumReportRows(rows, campaignID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%w: %w", errMonitorReportUndecodable, err)
	}
	return impressions, clicks, spendMicros, nil
}

// errMonitorReportUndecodable marks a per-campaign report Reddit answered but this client could
// not believe — undecodable, attributed to another campaign, missing a field, or overflowing —
// so monitorFailureClass can call it decode rather than other.
var errMonitorReportUndecodable = errors.New("reddit: campaign report could not be decoded")

// monitorFailureClass maps the error a per-campaign monitor report read failed with onto the
// closed model.MonitorFailureClass set. Only the error's TYPE and STATUS are read, never its
// text, which is the point: the class reaches a log line and a metric label.
//
// A 2xx whose envelope is not valid JSON is decode even though request() wraps it in a
// transportError (errUndecodable2xxResponse is checked before anything transport-shaped); a 2xx
// body that could not be READ is a failed round trip and stays other.
//
// A 429 that aborted because Reddit's Retry-After exceeded the wait cap surfaces as a
// transportError and so classes as other; a 429 after the bounded retry is an apiError and
// classes as throttle.
func monitorFailureClass(err error) model.MonitorFailureClass {
	var ae *apiError
	switch {
	case errors.Is(err, errMonitorReportUndecodable), errors.Is(err, errPageCapReached),
		errors.Is(err, errUndecodable2xxResponse):
		return model.MonitorFailureDecode
	case errors.Is(err, ErrCredentialRejected):
		return model.MonitorFailureAuth
	case errors.Is(err, errTokenEndpointUnavailable):
		return model.MonitorFailureUpstream5xx
	case errors.As(err, &ae):
		switch {
		case ae.StatusCode == http.StatusUnauthorized, ae.StatusCode == http.StatusForbidden:
			return model.MonitorFailureAuth
		case ae.StatusCode == http.StatusTooManyRequests:
			return model.MonitorFailureThrottle
		case ae.StatusCode >= 500:
			return model.MonitorFailureUpstream5xx
		}
	}
	return model.MonitorFailureOther
}

// ValidateAccountID checks accountID against the same charset restriction
// ListAccountCampaigns enforces, so a dispatcher can reject a malformed id
// before resolving (and decrypting) any stored credential — mirrors
// googleads.ValidateCustomerID's ordering.
func ValidateAccountID(accountID string) error {
	if !accountIDRe.MatchString(accountID) {
		return fmt.Errorf("validate account id: %w", ErrInvalidAccountID)
	}
	return nil
}

// ListAccountCampaigns ports getRedditAnalytics: every ACTIVE/PAUSED campaign visible on
// accountID (every page of the campaign list), each with its own report over the trailing
// `days` days, plus the pacing inputs (goal_value/1e6 routed by goal_type, start_time/end_time
// as the flight window) the rules.EvaluateRedditMonitor rule engine consumes.
//
// The window is days-1 days before today through today, INCLUSIVE of today — the house
// convention every account-monitor read follows (see the account-monitor architecture doc).
// It is rendered through reportRange, the same renderer GetCampaignMetrics uses, so the final
// day runs to its 23:00 hour. The BFF rendered ends_at as today's T00:00:00Z, which stops the
// range as today BEGINS: a days=N read covered N-1 days while claiming N.
//
// The BFF's separate account-wide totals call (fetchAccountMetrics) has no counterpart here:
// this service sums the rows it returns, on every platform — see
// service.monitorTotals and model.AccountMonitorTotals' doc comment.
func (c *Client) ListAccountCampaigns(ctx context.Context, accountID string, days int) ([]AccountCampaignRow, error) {
	rows, _, err := c.listAccountCampaigns(ctx, accountID, days, false)
	return rows, err
}

// ListAccountCampaignsWithCurrency is ListAccountCampaigns plus the ad account's ISO 4217
// currency, for the account monitor's response. Neither the campaign list nor the reports carry a
// currency, so it costs ONE more call, GET /ad_accounts/{id} (readMonitorAccountCurrency), made
// once the campaign list has been read and run CONCURRENTLY with the report fan-out on its own
// budget (monitorCurrencyTimeout), never retried on a 429. So it is off the critical path: a
// slow, throttled or failing account read cannot delay or fail the rows, which are returned as
// ListAccountCampaigns would return them; the currency is then "".
func (c *Client) ListAccountCampaignsWithCurrency(ctx context.Context, accountID string, days int) ([]AccountCampaignRow, string, error) {
	return c.listAccountCampaigns(ctx, accountID, days, true)
}

// listAccountCampaigns is the body of both public reads; withCurrency adds the account read.
func (c *Client) listAccountCampaigns(ctx context.Context, accountID string, days int, withCurrency bool) ([]AccountCampaignRow, string, error) {
	if err := ValidateAccountID(accountID); err != nil {
		return nil, "", fmt.Errorf("list account campaigns: %w", err)
	}
	if days < 1 {
		// The dispatcher validates days first (validateMonitorDays); this keeps a direct
		// caller from rendering a range whose start is after its end.
		return nil, "", fmt.Errorf("list account campaigns: days must be at least 1, got %d", days)
	}

	end := c.now().UTC()
	startsAt, endsAt := reportRange(end.AddDate(0, 0, -(days-1)), end)

	elements, err := c.listMonitorCampaigns(ctx, accountID)
	if err != nil {
		return nil, "", fmt.Errorf("list account campaigns: %w", redactReportPath(err, accountID))
	}
	// The currency read starts after the list (the read's required half) and runs beside the
	// report fan-out, on a context of its own bounded by monitorCurrencyTimeout. Its error is
	// dropped: the currency is only a label.
	currencyDone := make(chan string, 1)
	if withCurrency {
		go func() {
			cctx, cancel := context.WithTimeout(ctx, monitorCurrencyTimeout)
			defer cancel()
			code, _ := c.readMonitorAccountCurrency(cctx, accountID)
			currencyDone <- code
		}()
	} else {
		currencyDone <- ""
	}

	// activeCampaigns: filter to configured_status ACTIVE or PAUSED, matching
	// reddit-ads.service.ts:219 exactly (every other configured_status, e.g. ARCHIVED or
	// DELETED, is dropped from the monitor view). This is the read's only scope filter, and
	// the scope audit (LFXV2-2665, Track M3) found it matches the BFF.
	active := make([]campaignElement, 0, len(elements))
	for _, e := range elements {
		if e.ConfiguredStatus == StatusActive || e.ConfiguredStatus == StatusPaused {
			active = append(active, e)
		}
	}

	rows := make([]AccountCampaignRow, len(active))
	// ctxFailed[i] is true when row i's report failed with a context error, so the check after
	// Wait can tell a row the request's own deadline killed from one that failed on its own.
	ctxFailed := make([]bool, len(active))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(monitorReportConcurrency)
	for i, e := range active {
		g.Go(func() error {
			row := AccountCampaignRow{
				CampaignID: e.ID,
				Name:       e.Name,
				Status:     e.ConfiguredStatus,
				// StartDate/EndDate are left empty unless Reddit's own start_time/end_time
				// parses below — NOT seeded with the report window. round-23 review: an
				// unreported flight start previously defaulted to the report window itself,
				// which redditPacingPct then read as a real flight (start == rangeStart, end ==
				// now), fabricating a plausible-looking pacing percentage — e.g. "3% of budget
				// spent" — against a schedule the campaign never had. An empty StartDate now
				// signals "no known flight" to EvaluateRedditMonitor, which sets PacingUnknown
				// instead of computing a pacing verdict against it.
			}
			if e.GoalValue != nil && e.GoalType != nil {
				switch *e.GoalType {
				case monitorGoalTypeLifetimeSpend:
					row.TotalBudget = float64(*e.GoalValue) / 1_000_000
				case monitorGoalTypeDailySpend:
					row.DailyBudget = float64(*e.GoalValue) / 1_000_000
				}
				// Any other goal_type: neither budget — see AccountCampaignRow.TotalBudget.
			}
			if strings.TrimSpace(e.StartTime) != "" {
				if t, perr := time.Parse(time.RFC3339, e.StartTime); perr == nil {
					row.StartDate = t.UTC().Format("2006-01-02")
				}
			}
			if strings.TrimSpace(e.EndTime) != "" {
				if t, perr := time.Parse(time.RFC3339, e.EndTime); perr == nil {
					row.EndDate = t.UTC().Format("2006-01-02")
				}
			}

			// e.ID is Reddit's own campaign id, returned by the account-level campaign-list
			// call above — not the caller-supplied account_id, which is already validated by
			// this method's own caller. round-23 review: every sibling path that interpolates a
			// Reddit-supplied id into a request rejects one that fails accountIDRe first. Here
			// the id is interpolated into the report's `filter` DSL, where a comma would split
			// one filter term into two and silently widen the report to another campaign.
			if !accountIDRe.MatchString(e.ID) {
				row.FetchFailed = true
				row.FailureClass = model.MonitorFailureDecode
				rows[i] = row
				return nil
			}

			impressions, clicks, spendMicros, ferr := c.fetchMonitorCampaignReport(gctx, accountID, e.ID, startsAt, endsAt)
			if ferr != nil {
				// DIVERGES from the BFF here — see AccountCampaignRow.FetchFailed's doc comment.
				// The error itself is dropped (see below); its class is kept for the service
				// layer's per-request summary log and metric (#220).
				row.FetchFailed = true
				row.FailureClass = monitorFailureClass(ferr)
				ctxFailed[i] = errors.Is(ferr, context.DeadlineExceeded) || errors.Is(ferr, context.Canceled)
			} else {
				row.Impressions = impressions
				row.Clicks = clicks
				row.SpendUSD = float64(spendMicros) / 1_000_000
				if impressions > 0 {
					row.Ctr = float64(clicks) / float64(impressions) * 100
				}
			}
			rows[i] = row
			// Never propagated: a per-campaign report failure already has a home
			// (FetchFailed on that row) and returning it here would cancel gctx, turning one
			// campaign's failure into a partial read of every OTHER campaign in flight.
			return nil
		})
	}
	// Cannot return non-nil: every g.Go returns nil above. Checked anyway so a later edit
	// that starts propagating an error cannot silently drop it.
	if werr := g.Wait(); werr != nil {
		return nil, "", fmt.Errorf("list account campaigns: %w", werr)
	}
	// The caller's deadline or cancellation fails every report still in flight, and each of
	// those rows would read as one campaign's failure. When the caller's context is done and a
	// row failed on a context error, the account was not read — say so, rather than answer with
	// unmeasured rows that are an artefact of the request running out of time (#220). A row
	// that failed on its own (a 5xx, a per-attempt timeout while the caller's context is still
	// live) keeps its FetchFailed answer.
	if cerr := ctx.Err(); cerr != nil {
		for _, failed := range ctxFailed {
			if failed {
				return nil, "", fmt.Errorf("list account campaigns: %w", cerr)
			}
		}
	}
	// Bounded: the read above is held to monitorCurrencyTimeout, and the channel is buffered so an
	// early error return above never strands it.
	return rows, <-currencyDone, nil
}

// monitorCurrencyTimeout bounds the monitor's best-effort account-currency read, which runs beside
// the report fan-out: it must never be what holds the monitor up.
const monitorCurrencyTimeout = 3 * time.Second

// monitorAccountWire is the part of GET /ad_accounts/{id} the monitor reads. ID is a pointer, so
// an absent id is told from an empty one. Currency is kept raw and decoded on its own
// (monitorAccountCurrencyField): it is optional, and a value of the wrong JSON type there must
// cost only the currency — decoded as a typed field it would fail this whole decode, and with it
// the id check.
type monitorAccountWire struct {
	ID       *string         `json:"id"`
	Currency json.RawMessage `json:"currency"`
}

// monitorAccountCurrencyField is the account's currency when the raw field is a JSON string
// holding an ISO 4217-shaped code, and "" for anything else: absent, null, a number, an object,
// an array, a boolean, or a malformed code.
func monitorAccountCurrencyField(raw json.RawMessage) string {
	var code string
	if len(raw) == 0 || json.Unmarshal(raw, &code) != nil {
		return "" // json.Unmarshal of null into a string leaves it "", which is dropped too
	}
	return model.NormalizeCurrencyCode(code)
}

// errMonitorAccountUntrusted marks an ad-account answer the monitor will not take a currency
// from. It carries no upstream text.
var errMonitorAccountUntrusted = errors.New("reddit: ad account response could not be trusted")

// readMonitorAccountCurrency reads the ad account's currency: GET /ad_accounts/{id}, Reddit Ads
// API v3's ad-account read, whose element carries `currency` — the same field name the discovery
// read (accounts.go) takes from the business's ad-account list. UNVERIFIED LIVE, like discovery:
// this repository holds no Reddit credentials.
//
// The answer is held to the identity rules the adoption read uses: identityjson over the raw
// body (no duplicate keys, no invalid UTF-8 or lone surrogates that would decode silently), and
// the body must describe accountID itself. A currency that is absent, null, of the wrong JSON
// type or not three upper-case letters is "" with no error — the platform did not report one
// this service can vouch for. Errors never carry upstream text, and the account id is redacted
// from them.
func (c *Client) readMonitorAccountCurrency(ctx context.Context, accountID string) (string, error) {
	if err := ValidateAccountID(accountID); err != nil {
		return "", err
	}
	// Not retried on a 429: a throttled account read is answered as "currency unknown" rather
	// than waited out, since the currency is only a label (requestNoThrottleRetry's own reason —
	// a duplicated create — does not apply to a GET; this is about time).
	resp, err := c.requestNoThrottleRetry(ctx, http.MethodGet, "/ad_accounts/"+accountID, nil)
	if err != nil {
		return "", fmt.Errorf("read reddit ad account: %w", redactReportPath(err, accountID))
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return "", fmt.Errorf("%w: no account object", errMonitorAccountUntrusted)
	}
	if err := identityjson.Check(resp.raw); err != nil {
		return "", fmt.Errorf("%w: %w", errMonitorAccountUntrusted, err)
	}
	var acct monitorAccountWire
	if err := json.Unmarshal(resp.Data, &acct); err != nil {
		return "", fmt.Errorf("%w: not an account object", errMonitorAccountUntrusted)
	}
	if acct.ID == nil || *acct.ID != accountID {
		return "", fmt.Errorf("%w: it does not describe the requested account", errMonitorAccountUntrusted)
	}
	return monitorAccountCurrencyField(acct.Currency), nil
}
