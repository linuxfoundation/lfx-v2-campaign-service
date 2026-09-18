// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// AccountCampaignRow is one campaign read live from an ad account for the account-monitor
// endpoint, ported from lfx-self-serve's reddit-ads.service.ts (getRedditAnalytics).
//
// Conversions is deliberately absent (unlike the sibling platforms' AccountCampaignRow
// types): the BFF hardcodes campaignMetrics[].conversions to the literal 0 on every row
// (reddit-ads.service.ts:300), not a measurement, so the dispatcher maps every row's
// model.AccountCampaignMetrics.Conversions to a non-nil &0 directly rather than this type
// carrying a field that could only ever hold one value.
type AccountCampaignRow struct {
	CampaignID  string
	Name        string
	Status      string
	Impressions int64
	Clicks      int64
	Ctr         float64
	SpendUSD    float64
	// TotalBudget is goal_value/1_000_000 (reddit-ads.service.ts:266). DailyBudget is not a
	// field here: the BFF hardcodes it to the literal 0 (reddit-ads.service.ts:267, "dead
	// branch" per the pacing rule engine's own comment), so there is nothing to carry.
	TotalBudget float64
	StartDate   string
	EndDate     string
	// FetchFailed is set when this campaign's own /reports call failed. DIVERGES from the
	// BFF's own behavior on purpose: reddit-ads.service.ts substitutes a fabricated
	// {impressions:0,clicks:0,spend:0} on a Promise.allSettled rejection and only logs a
	// warning (reddit-ads.service.ts:243-251) — indistinguishable, downstream, from a
	// campaign that genuinely had zero activity. model.AccountCampaignMetrics.FetchFailed's
	// own doc comment names this exact case as one of the reasons the field exists, so this
	// port sets it instead of fabricating the zero. This is a disclosed, deliberate
	// divergence, not a preserved bug.
	FetchFailed bool
}

// AccountTotals is the account-wide aggregate read via a SEPARATE report call
// (fetchAccountMetrics in the BFF), independent of the per-campaign rows above — see
// model.AccountMonitorTotals' doc comment for why Reddit's totals are not a sum of rows.
// Conversions is always 0 (reddit-ads.service.ts:312 hardcodes it), so, like
// AccountCampaignRow, this type carries no field for it.
type AccountTotals struct {
	Impressions   int64
	Clicks        int64
	SpendUSD      float64
	CampaignCount int
}

// campaignElement is one entry of the campaign-list response, per the fields
// getRedditAnalytics reads off RedditCampaignElement.
type campaignElement struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	ConfiguredStatus string `json:"configured_status"`
	GoalValue        *int64 `json:"goal_value"`
	StartTime        string `json:"start_time"`
	EndTime          string `json:"end_time"`
}

// campaignListResponse tolerates the same three response shapes fetchCampaigns does
// (reddit-ads.service.ts:191-203): resp.data as a bare array, resp.data.campaigns as an
// array, or (defensively) resp.data itself treated as the array in the fallback arm. Ported
// faithfully rather than simplified, because a live Reddit account's actual shape has never
// been exercised against this client (see the UNVERIFIED-CONTRACT banner on
// GetCampaignMetrics) — narrowing to one shape here could silently return zero campaigns
// against a real account that uses either of the other two.
func decodeCampaignList(data json.RawMessage) []campaignElement {
	if len(data) == 0 {
		return nil
	}
	// Shape 1: a bare array.
	var bare []campaignElement
	if err := json.Unmarshal(data, &bare); err == nil {
		return bare
	}
	// Shape 2: {"campaigns": [...]}.
	var wrapped struct {
		Campaigns []campaignElement `json:"campaigns"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Campaigns != nil {
		return wrapped.Campaigns
	}
	// Shape 3 / ultimate fallback: fetchCampaigns never throws on a shape mismatch, it
	// returns an empty list. Mirrored here rather than erroring.
	return nil
}

// monitorReportRow is one entry of a monitor report's "metrics" array. Unlike
// metrics.go's reportRow (used by the verified single-campaign GetCampaignMetrics path),
// this does NOT require every field present and does NOT validate a campaign id: the BFF's
// fetchAccountMetrics/fetchCampaignMetrics read each field with `?? 0` and perform no
// validation at all (reddit-ads.service.ts:147-153, 166-172). Faithfully porting that means
// an absent/null field here is read as 0, not refused — a DIFFERENT, looser contract than
// GetCampaignMetrics' deliberately-strict one, and that difference is intentional: this is
// the monitor read's own literal behavior, not a relaxation of the verified path.
type monitorReportRow struct {
	Impressions *int64 `json:"impressions"`
	Clicks      *int64 `json:"clicks"`
	Spend       *int64 `json:"spend"`
}

type monitorReportEnvelope struct {
	Metrics []monitorReportRow `json:"metrics"`
}

// fetchMonitorReport POSTs a report request to path and returns the first metrics row's
// impressions/clicks/spend (spend converted from microcurrency to whole units), all
// defaulting to 0 exactly as fetchAccountMetrics/fetchCampaignMetrics do. path is either the
// account-wide reports endpoint or a single campaign's nested reports endpoint — both use the
// identical request body shape (reddit-ads.service.ts:138-144, 157-163).
func (c *Client) fetchMonitorReport(ctx context.Context, path, startDate, endDate string) (impressions, clicks int64, spendUSD float64, err error) {
	reqBody := map[string]any{
		"data": map[string]any{
			"starts_at": startDate + "T00:00:00Z",
			"ends_at":   endDate + "T00:00:00Z",
			"fields":    []string{"IMPRESSIONS", "CLICKS", "SPEND"},
		},
	}
	resp, rerr := c.request(ctx, http.MethodPost, path, reqBody)
	if rerr != nil {
		return 0, 0, 0, rerr
	}
	if len(resp.Data) == 0 {
		return 0, 0, 0, nil
	}
	var env monitorReportEnvelope
	if derr := json.Unmarshal(resp.Data, &env); derr != nil {
		// DIVERGES from the BFF here (round-21 review): the BFF's optional-chaining
		// `?.metrics ?? []` also reads a malformed body as "no data" (0s), but that silently
		// converts an upstream-data failure into a legitimate-looking zero-delivery
		// measurement, indistinguishable from a campaign that genuinely had no activity. The
		// caller (ListAccountCampaigns) already turns any non-nil error from this function
		// into FetchFailed=true on that row rather than aborting the whole account read, so
		// returning an error here — instead of a fabricated zero — costs nothing and fixes
		// the false zero-delivery alert this could otherwise trigger.
		return 0, 0, 0, fmt.Errorf("decode monitor report: malformed JSON (%d bytes)", len(resp.Data))
	}
	if len(env.Metrics) == 0 {
		return 0, 0, 0, nil
	}
	row := env.Metrics[0]
	if row.Impressions != nil {
		impressions = *row.Impressions
	}
	if row.Clicks != nil {
		clicks = *row.Clicks
	}
	if row.Spend != nil {
		spendUSD = float64(*row.Spend) / 1_000_000
	}
	return impressions, clicks, spendUSD, nil
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
// accountID, each with its own per-campaign report over the trailing `days` days, ported
// verbatim including the pacing inputs (goal_value/1e6 as TotalBudget, start_time/end_time as
// the flight window) the rules.EvaluateRedditMonitor rule engine consumes.
//
// The account-wide totals (fetchAccountMetrics) are NOT returned here — see AccountTotals /
// FetchAccountTotals, called separately, exactly as getRedditAnalytics makes that a SEPARATE
// call from the per-campaign fan-out.
func (c *Client) ListAccountCampaigns(ctx context.Context, accountID string, days int) ([]AccountCampaignRow, error) {
	if err := ValidateAccountID(accountID); err != nil {
		return nil, fmt.Errorf("list account campaigns: %w", err)
	}

	end := c.now().UTC()
	start := end.AddDate(0, 0, -(days - 1))
	startDate := start.Format("2006-01-02")
	endDate := end.Format("2006-01-02")

	resp, err := c.request(ctx, http.MethodGet, "/ad_accounts/"+accountID+"/campaigns", nil)
	if err != nil {
		return nil, fmt.Errorf("list account campaigns: %w", redactReportPath(err, accountID))
	}
	elements := decodeCampaignList(resp.Data)

	// activeCampaigns: filter to configured_status ACTIVE or PAUSED, matching
	// reddit-ads.service.ts:219 exactly (every other configured_status, e.g. ARCHIVED or
	// DELETED, is dropped from the monitor view).
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
			if e.GoalValue != nil {
				row.TotalBudget = float64(*e.GoalValue) / 1_000_000
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
			// Reddit-supplied id into a request path (updateEntityStatus, GetCampaignMetrics)
			// rejects one that fails accountIDRe first; this fan-out was the one path that
			// skipped that guard, letting a malformed upstream id retarget this project's live
			// bearer token at an arbitrary Reddit path.
			if !accountIDRe.MatchString(e.ID) {
				row.FetchFailed = true
				rows[i] = row
				return nil
			}

			impressions, clicks, spendUSD, ferr := c.fetchMonitorReport(gctx,
				"/ad_accounts/"+accountID+"/campaigns/"+e.ID+"/reports", startDate, endDate)
			if ferr != nil {
				// DIVERGES from the BFF here — see AccountCampaignRow.FetchFailed's doc comment.
				row.FetchFailed = true
			} else {
				row.Impressions = impressions
				row.Clicks = clicks
				row.SpendUSD = spendUSD
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

// FetchAccountTotals ports fetchAccountMetrics: a single account-wide report over the
// trailing `days` days, independent of the per-campaign rows ListAccountCampaigns returns.
// campaignCount is supplied by the caller (the count of rows ListAccountCampaigns returned),
// matching getRedditAnalytics' own accountTotals.campaignCount = campaignMetrics.length
// (reddit-ads.service.ts:313), which counts the FILTERED active campaigns, not every
// campaign fetchCampaigns returned.
//
// Unlike ListAccountCampaigns' per-campaign fetch, a failure here is NOT translated into a
// FetchFailed row — there is no row to mark. Mirrors getRedditAnalytics' own handling
// (reddit-ads.service.ts:256-262): the caller logs a warning and treats the totals as zero.
// The error is still returned so the caller (dispatch/reddit.go) decides whether to log
// there, rather than this package silently swallowing it.
func (c *Client) FetchAccountTotals(ctx context.Context, accountID string, days, campaignCount int) (AccountTotals, error) {
	if err := ValidateAccountID(accountID); err != nil {
		return AccountTotals{}, fmt.Errorf("fetch account totals: %w", err)
	}
	end := c.now().UTC()
	start := end.AddDate(0, 0, -(days - 1))
	impressions, clicks, spendUSD, err := c.fetchMonitorReport(ctx,
		"/ad_accounts/"+accountID+"/reports", start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return AccountTotals{CampaignCount: campaignCount}, fmt.Errorf("fetch account totals: %w", redactReportPath(err, accountID))
	}
	return AccountTotals{
		Impressions:   impressions,
		Clicks:        clicks,
		SpendUSD:      spendUSD,
		CampaignCount: campaignCount,
	}, nil
}
