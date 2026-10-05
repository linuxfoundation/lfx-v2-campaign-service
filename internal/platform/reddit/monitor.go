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
func (c *Client) nextPagePath(raw json.RawMessage) (string, error) {
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
	seen := map[string]struct{}{path: {}}
	for page := 1; ; page++ {
		resp, err := c.request(ctx, method, path, body)
		if err != nil {
			return err
		}
		if err := visit(page, resp); err != nil {
			return err
		}
		next, err := c.nextPagePath(resp.Pagination)
		if err != nil {
			return fmt.Errorf("page %d: %w", page, err)
		}
		if next == "" {
			return nil
		}
		if page >= monitorMaxPages {
			return errPageCapReached
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
			return derr
		}
		rows = append(rows, pageRows...)
		return nil
	})
	if werr != nil {
		return 0, 0, 0, werr
	}
	return sumReportRows(rows, campaignID)
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
	if err := ValidateAccountID(accountID); err != nil {
		return nil, fmt.Errorf("list account campaigns: %w", err)
	}
	if days < 1 {
		// The dispatcher validates days first (validateMonitorDays); this keeps a direct
		// caller from rendering a range whose start is after its end.
		return nil, fmt.Errorf("list account campaigns: days must be at least 1, got %d", days)
	}

	end := c.now().UTC()
	startsAt, endsAt := reportRange(end.AddDate(0, 0, -(days-1)), end)

	elements, err := c.listMonitorCampaigns(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("list account campaigns: %w", redactReportPath(err, accountID))
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
				rows[i] = row
				return nil
			}

			impressions, clicks, spendMicros, ferr := c.fetchMonitorCampaignReport(gctx, accountID, e.ID, startsAt, endsAt)
			if ferr != nil {
				// DIVERGES from the BFF here — see AccountCampaignRow.FetchFailed's doc comment.
				row.FetchFailed = true
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
		return nil, fmt.Errorf("list account campaigns: %w", werr)
	}
	return rows, nil
}
