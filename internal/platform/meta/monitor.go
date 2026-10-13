// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

const (
	// monitorPageSize is the per-page limit for the account-monitor's campaign-list and
	// insights reads. Meta accepts a much larger `limit`, but a smaller page just means
	// more round trips, not a truncated result — see fetchAccountCampaignList and
	// fetchAccountCampaignInsights, both of which now follow `paging.next` to exhaustion.
	monitorPageSize = 500
	// monitorMaxPages bounds the walk at monitorPageSize*monitorMaxPages rows. Exceeding
	// it is an ERROR, never a silently truncated result, matching ListAdAccounts'
	// adAccountMaxPages precedent in accounts.go.
	monitorMaxPages = 50
	// monitorAdSetPageSize / monitorAdSetMaxPages bound the account-level ad-set budget read
	// (fetchAccountAdSetBudgets) at 500*20 = 10,000 ad sets. Unlike the two reads above,
	// exceeding this cap does NOT fail the request: the ad-set read only supplies budgets the
	// campaign list could not, so hitting the cap leaves every campaign that needed it at
	// unknown pacing (fail closed) rather than pacing it against a partial sum.
	monitorAdSetPageSize = 500
	monitorAdSetMaxPages = 20
)

// AccountCampaignRow is one campaign read live from an ad account for the account-monitor
// endpoint, ported from lfx-self-serve's meta-ads.service.ts (getMetaAnalytics +
// buildCampaignMetrics). Numeric fields carry Meta's own raw counters; the
// internal/service/rules layer computes pacing/action items from them.
type AccountCampaignRow struct {
	CampaignID  string
	Name        string
	Status      string
	Impressions int64
	Clicks      int64
	Ctr         float64
	// SpendUSD is Insights' `spend`, which Meta reports in the AD ACCOUNT's currency (the name
	// predates that being stated). DailyBudget / LifetimeBudget are in the same currency, in
	// whole units, so the pacing ratio the rules layer computes compares like with like.
	SpendUSD float64
	// DailyBudget / LifetimeBudget are the campaign's budget in whole account-currency units:
	// the CAMPAIGN's own field when it has one (CBO), otherwise the sum of its ACTIVE ad sets'
	// budgets (ABO — the shape CreateCampaign produces). 0 means unknown or unset; the rules
	// layer reads a row with neither as PacingUnknown. See ListAccountCampaigns.
	DailyBudget    float64
	LifetimeBudget float64
	StartDate      string
	EndDate        string
	// FetchFailed marks a row whose insights lookup could not be matched/parsed — this
	// dispatcher-level method never fabricates a zero-metrics row for a fetch failure.
	FetchFailed bool
}

// ListAccountCampaigns ports getMetaAnalytics: an account-level insights read
// (level=campaign, fully paginated — see fetchAccountCampaignInsights) merged with a
// campaign-list read for status/budget/schedule fields insights does not carry, one account
// currency read (budgets are minor units of THAT currency — see minorToWhole), and, only when an
// admitted campaign reports no campaign-level budget, one account-level ad-set budget read
// (fetchAccountAdSetBudgets) that supplies it from the campaign's ACTIVE ad sets. Unlike the
// BFF, which logs a warning and silently drops rows past its first page, both reads here
// walk every page (LFXV2-2519 Part 3): pagination is a pre-cutover fix, not one of the
// threshold/labeling quirks ported over from the BFF.
//
// days selects an explicit `time_range` for the insights read rather than the BFF's hardcoded
// date_preset=last_30d — getMetaAnalytics ignored its own caller-supplied window entirely,
// which is a data-correctness bug (a days=7 request silently answered with 30 days of spend),
// not one of those ported threshold/labeling quirks. See fetchAccountCampaignInsights.
//
// accountID must already be in Meta's "act_<digits>" form (the same form AccountConfig.
// AccountID and normalizeMetaAccountID produce). On the account-monitor path this is a
// caller-supplied id (MonitorMetaAdsAccountPayload.AccountID), not the resolved connection's
// own account — validated at the design layer for an HTTP caller (Pattern/MaxLength; see
// design/connection.go and docs/knowledge/architecture/account-monitor-endpoints.md for why no
// MinLength is declared alongside them) before it reaches here, and again below as
// defense-in-depth for a non-HTTP caller.
//
// days carries no such defense-in-depth check here: unlike accountID, this client trusts days
// is already within the service layer's 7..90 bound (internal/service/connection_monitor.go's
// validateMonitorDays, itself mirroring the design layer's Minimum/Maximum) before it reaches
// fetchAccountCampaignInsights' window arithmetic. That is a deliberate asymmetry, not an
// oversight — today's only caller is that service layer; a future non-HTTP caller of this
// package directly would need to add its own bound rather than rely on one here.
func (c *Client) ListAccountCampaigns(ctx context.Context, accountID string, days int) ([]AccountCampaignRow, error) {
	if err := ValidateAccountID(accountID); err != nil {
		return nil, fmt.Errorf("list account campaigns: %w", err)
	}

	campaigns, err := c.fetchAccountCampaignList(ctx, accountID)
	if err != nil {
		return nil, err
	}
	// One account-currency read per monitor request: every budget Meta reports is in the
	// account currency's MINOR units, and the scale (100 for USD, 1 for JPY/KRW) is the
	// currency's, not a constant. An unknown currency leaves every budget unconverted (0,
	// read as PacingUnknown) rather than guessing a scale 100x wrong.
	currency, err := c.accountCurrency(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("list account campaigns: %w", err)
	}
	offset, currencyKnown := currencyOffsetFor(currency)
	insightsByID, failedInsightIDs, err := c.fetchAccountCampaignInsights(ctx, accountID, days)
	if err != nil {
		return nil, err
	}

	rows := make([]AccountCampaignRow, 0, len(campaigns))
	// needAdSetBudget indexes (into rows) every admitted campaign with no campaign-level
	// budget, whose budget therefore lives on its ad sets.
	var needAdSetBudget []int
	for _, cm := range campaigns {
		row := AccountCampaignRow{
			CampaignID:  cm.ID,
			Name:        cm.Name,
			Status:      cm.Status,
			StartDate:   cm.StartDate,
			EndDate:     cm.EndDate,
			FetchFailed: cm.FetchFailed,
		}
		if currencyKnown {
			row.DailyBudget = minorToWhole(cm.DailyMinor, offset)
			row.LifetimeBudget = minorToWhole(cm.LifetimeMinor, offset)
		}
		if ins, ok := insightsByID[cm.ID]; ok {
			row.Impressions = ins.Impressions
			row.Clicks = ins.Clicks
			row.SpendUSD = ins.SpendUSD
			if row.Impressions > 0 {
				row.Ctr = float64(row.Clicks) / float64(row.Impressions) * 100
			}
		} else if _, failed := failedInsightIDs[cm.ID]; failed {
			row.FetchFailed = true
		}
		// Campaign-list filter ported from getMetaAnalytics: `c.impressions>0 ||
		// c.status==='ACTIVE'` — everything else (a PAUSED campaign that never delivered)
		// is dropped from the monitor view entirely, exactly as upstream. A row marked
		// FetchFailed is admitted regardless of that filter: round-15 review found the
		// filter silently dropping a PAUSED campaign whose insights row failed to parse,
		// making the fetch failure invisible instead of surfacing as unmeasured — the
		// opposite of what FetchFailed exists to do.
		if row.Impressions > 0 || row.Status == StatusActive || row.FetchFailed {
			if currencyKnown && !row.FetchFailed && !positiveMinor(cm.DailyMinor) && !positiveMinor(cm.LifetimeMinor) {
				needAdSetBudget = append(needAdSetBudget, len(rows))
			}
			rows = append(rows, row)
		}
	}

	// Ad-set budgets (LFXV2-2665): CreateCampaign puts the budget on the AD SET (ABO), so a
	// campaign this service created reports no campaign-level budget at all, and pacing used
	// to be unknown for every one of them. One account-level read covers every such campaign.
	if len(needAdSetBudget) == 0 {
		return rows, nil
	}
	totals, err := c.fetchAccountAdSetBudgets(ctx, accountID)
	if err != nil {
		var unreadable *adSetBudgetsUnreadableError
		if !errors.As(err, &unreadable) {
			return nil, err
		}
		// The read completed but cannot be trusted whole (page cap, a body identityjson
		// refuses, an unattributable row): every campaign that needed it stays at unknown
		// pacing. Pacing against a partial sum would understate the budget and raise a false
		// overspend alert.
		return rows, nil
	}
	for _, i := range needAdSetBudget {
		t, ok := totals[rows[i].CampaignID]
		if !ok || t.unknown {
			continue // no active ad set, or one this read could not trust: pacing unknown
		}
		switch {
		case t.dailyMinor > 0 && t.lifetimeMinor > 0:
			// Mixed pacing models under one campaign: no single expectation is honest.
		case t.dailyMinor > 0:
			rows[i].DailyBudget = float64(t.dailyMinor) / float64(offset)
			// Daily pacing (budget x days) does not read the flight, but the row should still
			// show the ad sets' flight when they agree on one.
			if t.windowSet && !t.windowUnknown {
				rows[i].StartDate, rows[i].EndDate = t.start, t.end
			}
		case t.lifetimeMinor > 0:
			// A lifetime budget is paced against its flight, and CreateCampaign writes the
			// flight (start_time/end_time) on the AD SET, not the campaign — the campaign-list
			// dates are empty for it. The ad sets' shared window is used; if they disagree, or
			// any active one's window is unreadable, no single flight is honest and the
			// budget is left unknown rather than paced against the wrong schedule.
			if t.windowSet && !t.windowUnknown {
				rows[i].LifetimeBudget = float64(t.lifetimeMinor) / float64(offset)
				rows[i].StartDate, rows[i].EndDate = t.start, t.end
			}
		}
	}
	return rows, nil
}

// positiveMinor reports whether a reported minor-unit budget is a real budget. Meta's floor is
// one minor unit, so a reported 0 is "no budget at this level", exactly like an absent field.
func positiveMinor(v *int64) bool { return v != nil && *v > 0 }

// minorToWhole converts a reported minor-unit budget to whole account-currency units with the
// account currency's own offset (100 for USD, 1 for JPY/KRW). nil or non-positive is 0.
func minorToWhole(v *int64, offset int64) float64 {
	if !positiveMinor(v) || offset <= 0 {
		return 0
	}
	return float64(*v) / float64(offset)
}

type metaCampaignListEntry struct {
	ID     string
	Name   string
	Status string
	// DailyMinor / LifetimeMinor are the CAMPAIGN's budget fields in the account currency's
	// minor units, nil when not reported. They are converted to whole units only once the
	// account currency is known (ListAccountCampaigns).
	DailyMinor    *int64
	LifetimeMinor *int64
	StartDate     string
	EndDate       string
	// FetchFailed marks a campaign whose daily_budget/lifetime_budget string was present but
	// unparseable — the row's budget fields are left nil and MUST NOT be read as a measured
	// zero (round-31+ review, same convention as AccountCampaignRow.FetchFailed).
	FetchFailed bool
}

// fetchAccountCampaignList reads GET /act_.../campaigns for status/budget/schedule fields
// that Insights does not report. Budgets arrive from Meta as minor-unit strings in the account
// currency and are kept in minor units here; ListAccountCampaigns converts them with the
// account currency's own offset (the BFF divided by 100 unconditionally, which reads a JPY or
// KRW budget 100x too small).
//
// It follows `paging.cursors.after` to exhaustion (LFXV2-2519 Part 3, Misha's pre-cutover
// condition) rather than reading one page as the ported BFF logic does: past 100 campaigns
// that single-page read silently dropped rows, which is a data-completeness defect, not a
// threshold quirk safe to carry over verbatim like the five other known bugs. The cursor
// walk mirrors ListAdAccounts in accounts.go: the path is rebuilt from the opaque cursor
// rather than following Meta's absolute `paging.next` URL, since that URL carries the
// access_token as a query parameter.
func (c *Client) fetchAccountCampaignList(ctx context.Context, accountID string) ([]metaCampaignListEntry, error) {
	out := make([]metaCampaignListEntry, 0, monitorPageSize)
	after := ""
	seen := make(map[string]struct{})
	for page := 0; page < monitorMaxPages; page++ {
		path := "/" + accountID + "/campaigns?fields=id,name,status,daily_budget,lifetime_budget,start_time,stop_time&limit=" + strconv.Itoa(monitorPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var resp struct {
			Data *[]struct {
				ID             string `json:"id"`
				Name           string `json:"name"`
				Status         string `json:"status"`
				DailyBudget    string `json:"daily_budget"`
				LifetimeBudget string `json:"lifetime_budget"`
				StartTime      string `json:"start_time"`
				StopTime       string `json:"stop_time"`
			} `json:"data"`
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := c.doRequest(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, fmt.Errorf("list account campaigns: %w", err)
		}
		if resp.Data == nil {
			return nil, &transportError{
				Method: http.MethodGet,
				Path:   path,
				Err:    fmt.Errorf("campaign list returned a 2xx response with no data field"),
			}
		}
		for _, d := range *resp.Data {
			var unparseable bool
			dailyMinor := parseMinorUnits(d.DailyBudget, &unparseable)
			lifetimeMinor := parseMinorUnits(d.LifetimeBudget, &unparseable)
			out = append(out, metaCampaignListEntry{
				ID:            d.ID,
				Name:          d.Name,
				Status:        d.Status,
				DailyMinor:    dailyMinor,
				LifetimeMinor: lifetimeMinor,
				StartDate:     dateOnly(d.StartTime),
				EndDate:       dateOnly(d.StopTime),
				FetchFailed:   unparseable,
			})
		}
		if resp.Paging.Next == "" {
			return out, nil // fully enumerated
		}
		after = strings.TrimSpace(resp.Paging.Cursors.After)
		if after == "" {
			return nil, fmt.Errorf("list account campaigns has more pages but no cursor; cannot guarantee every campaign was enumerated")
		}
		if _, dup := seen[after]; dup {
			return nil, fmt.Errorf("list account campaigns did not terminate (repeated paging cursor)")
		}
		seen[after] = struct{}{}
	}
	return nil, fmt.Errorf("list account campaigns exceeded %d pages; too many campaigns to enumerate", monitorMaxPages)
}

type metaInsightsRow struct {
	Impressions int64
	Clicks      int64
	SpendUSD    float64
}

// fetchAccountCampaignInsights ports getMetaAnalytics' account-level insights read
// (level=campaign), following `paging.cursors.after` to exhaustion rather than reading one
// page — see fetchAccountCampaignList's doc comment for why this departs from the ported
// BFF behavior.
//
// days is rendered as an explicit `time_range={"since":...,"until":...}` rather than a
// date_preset — Meta's Graph API Insights accepts time_range as a direct alternative,
// letting the caller-supplied window reach the query at all (the BFF's date_preset=last_30d
// ignored it). The dates are rendered from UTC and inclusive of `days` calendar days ending
// today, matching the same days-1 convention Google/Reddit's monitor dispatchers already use;
// Meta itself interprets since/until in the ad account's configured timezone, so a boundary
// day's spend can differ slightly from a strict UTC reading.
//
// This deliberately diverges from Meta's own last_30d preset it replaces (round-16 review):
// that preset EXCLUDES today (it is the trailing 30 days as of Meta's last completed reporting
// day), while `until: today` here INCLUDES it — so a days=30 request now returns 29 full days
// plus one partial trailing day, not 30 full ones, biasing pacing toward "underspending" for
// that partial day. Kept anyway rather than shifted back a day, because the alternative is
// Meta alone excluding today while Google/Reddit's identical days-1 convention includes it;
// cross-platform consistency wins over exact preset-parity with a behavior this port is
// already changing. See docs/knowledge/architecture/account-monitor-endpoints.md for the same
// note next to the ticket history.
func (c *Client) fetchAccountCampaignInsights(ctx context.Context, accountID string, days int) (map[string]metaInsightsRow, map[string]struct{}, error) {
	end := c.timeNow().UTC()
	start := end.AddDate(0, 0, -(days - 1))
	timeRange := `{"since":"` + start.Format("2006-01-02") + `","until":"` + end.Format("2006-01-02") + `"}`

	out := make(map[string]metaInsightsRow, monitorPageSize)
	failed := make(map[string]struct{})
	after := ""
	seen := make(map[string]struct{})
	for page := 0; page < monitorMaxPages; page++ {
		path := "/" + accountID + "/insights?level=campaign&fields=campaign_id,impressions,clicks,spend&time_range=" + url.QueryEscape(timeRange) + "&limit=" + strconv.Itoa(monitorPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var resp struct {
			Data *[]struct {
				CampaignID  string `json:"campaign_id"`
				Impressions string `json:"impressions"`
				Clicks      string `json:"clicks"`
				Spend       string `json:"spend"`
			} `json:"data"`
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := c.doRequest(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, nil, fmt.Errorf("list account campaign insights: %w", err)
		}
		if resp.Data == nil {
			return nil, nil, &transportError{
				Method: http.MethodGet,
				Path:   path,
				Err:    fmt.Errorf("account insights returned a 2xx response with no data field"),
			}
		}
		for _, row := range *resp.Data {
			// A row with no campaign_id cannot be attributed to any specific campaign, so it
			// cannot be recorded in `failed` either (that map is keyed by campaign id) — unlike
			// the parse-failure case below, there is no id to mark FetchFailed on individually.
			// Silently skipping it would let ListAccountCampaigns read the real campaign this
			// row belonged to as a legitimate zero-activity omission, so the whole read is
			// rejected instead (round-31 review, same class as the LinkedIn pivot-value fix).
			if row.CampaignID == "" {
				return nil, nil, &transportError{
					Method: http.MethodGet,
					Path:   path,
					Err:    fmt.Errorf("account insights returned a row with no campaign_id"),
				}
			}
			impressions, errI := parseMetricInt(row.Impressions)
			clicks, errC := parseMetricInt(row.Clicks)
			// Meta omits spend entirely for a zero-delivery campaign rather than sending "0",
			// so an empty string is a legitimate zero here, exactly like parseMetricInt treats
			// empty impressions/clicks above — only a NON-empty unparseable value is a failure.
			var spend float64
			var errS error
			if row.Spend != "" {
				spend, errS = strconv.ParseFloat(row.Spend, 64)
			}
			if errI != nil || errC != nil || errS != nil {
				// Per-row parse failure: skip this row rather than fail the whole read, but
				// never fabricate a zero — the caller sees no insights entry for this
				// campaign id, and its id is recorded in `failed` so ListAccountCampaigns can
				// mark the row FetchFailed instead of silently treating it as zero-delivery.
				// spend is included in this gate (round-19 review): a swallowed spend parse
				// used to produce a confident $0 row, which the pacing/action-item rules then
				// read as a real Underspending signal instead of an unknown. CampaignID is
				// already known non-empty here (guarded above).
				failed[row.CampaignID] = struct{}{}
				continue
			}
			out[row.CampaignID] = metaInsightsRow{Impressions: impressions, Clicks: clicks, SpendUSD: spend}
		}
		if resp.Paging.Next == "" {
			return out, failed, nil // fully enumerated
		}
		after = strings.TrimSpace(resp.Paging.Cursors.After)
		if after == "" {
			return nil, nil, fmt.Errorf("list account campaign insights has more pages but no cursor; cannot guarantee every campaign was enumerated")
		}
		if _, dup := seen[after]; dup {
			return nil, nil, fmt.Errorf("list account campaign insights did not terminate (repeated paging cursor)")
		}
		seen[after] = struct{}{}
	}
	return nil, nil, fmt.Errorf("list account campaign insights exceeded %d pages; too many rows to enumerate", monitorMaxPages)
}

// dateOnly validates and truncates a Meta ISO-8601 timestamp ("2026-01-15T00:00:00-0800") to
// its YYYY-MM-DD date component, matching model.AccountCampaignMetrics.StartDate/EndDate's
// date-only contract. It parses the 10-char prefix as an actual calendar date rather than
// merely slicing it, so a malformed timestamp (e.g. "0000-00-00...") does not silently pass
// through as a fabricated date — empty/malformed input yields "" (round-31+ review).
func dateOnly(s string) string {
	if len(s) < 10 {
		return ""
	}
	prefix := s[:10]
	if _, err := time.Parse("2006-01-02", prefix); err != nil {
		return ""
	}
	return prefix
}

// adSetBudgetTotal is the sum of one campaign's ACTIVE ad-set budgets, in the account
// currency's minor units. unknown marks a campaign at least one of whose active ad sets could
// not be read as exactly one positive budget — its total is then not a statement about the
// campaign and is never used.
//
// start/end are the flight (YYYY-MM-DD) every budgeted active ad set shares; windowSet says one
// was seen, windowUnknown that one was unreadable (absent/null/malformed start_time, malformed or
// null end_time) or that two active ad sets disagree. An absent end_time is an open-ended flight
// ("" end). The window is tracked apart from `unknown`: it decides only lifetime pacing.
type adSetBudgetTotal struct {
	dailyMinor    int64
	lifetimeMinor int64
	unknown       bool

	start, end    string
	windowSet     bool
	windowUnknown bool
}

// addWindow folds one active ad set's flight into the campaign's shared window.
func (t *adSetBudgetTotal) addWindow(start, end string, ok bool) {
	switch {
	case !ok:
		t.windowUnknown = true
	case !t.windowSet:
		t.start, t.end, t.windowSet = start, end, true
	case t.start != start || t.end != end:
		t.windowUnknown = true
	}
}

// adSetWindow reads an ad set's start_time/end_time into date-only form. ok=false when the
// start is absent, null or malformed, or the end is null or malformed; an ABSENT end is "".
func adSetWindow(startRaw, endRaw json.RawMessage) (start, end string, ok bool) {
	timeField := func(raw json.RawMessage) (v string, present, valid bool) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			return "", false, true
		}
		var s string
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &s) != nil {
			return "", true, false
		}
		d := dateOnly(s)
		return d, true, d != ""
	}
	start, startPresent, startValid := timeField(startRaw)
	end, _, endValid := timeField(endRaw)
	if !startPresent || !startValid || !endValid {
		return "", "", false
	}
	return start, end, true
}

// adSetBudgetsUnreadableError marks an ad-set budget read that reached Meta and was answered,
// but whose answer cannot be trusted WHOLE: the page cap was hit, a body identityjson refused or
// that does not decode, a row that cannot be attributed to one ad set and one campaign, or a
// paging cursor that cannot be followed. ListAccountCampaigns degrades it to unknown pacing for
// the campaigns that needed it. A failed REQUEST (transport, Graph error, cancelled context) is
// not this type and fails the monitor read like its two sibling reads do.
type adSetBudgetsUnreadableError struct{ err error }

func (e *adSetBudgetsUnreadableError) Error() string {
	return "meta account ad-set budgets unreadable: " + e.err.Error()
}
func (e *adSetBudgetsUnreadableError) Unwrap() error { return e.err }

// adSetBudgetWire is one row of the account-level /adsets edge. The identifiers are pointers so
// an absent one is distinguishable from an empty one; the budgets are RAW so an ABSENT field (no
// budget at this level) is distinguishable from an explicit JSON null, which is not zero and not
// absent — it is a statement that the value is unknown.
type adSetBudgetWire struct {
	ID              *string         `json:"id"`
	CampaignID      *string         `json:"campaign_id"`
	EffectiveStatus *string         `json:"effective_status"`
	DailyBudget     json.RawMessage `json:"daily_budget"`
	LifetimeBudget  json.RawMessage `json:"lifetime_budget"`
	StartTime       json.RawMessage `json:"start_time"`
	EndTime         json.RawMessage `json:"end_time"`
}

// fetchAccountAdSetBudgets reads every ad set on the account with ONE account-level paged call,
//
//	GET /{accountID}/adsets?fields=id,campaign_id,daily_budget,lifetime_budget,effective_status,start_time,end_time&limit=500
//
// and sums the budgets of the ACTIVE ones (effective_status == ACTIVE: an ad set that is paused
// itself, or under a paused campaign, is not spending and its budget is not part of what the
// campaign is expected to spend) per campaign_id. `id` is requested beyond the four fields the
// sum needs because it is the only proof a row is not a repeat — a duplicated row would double
// a budget. start_time/end_time are requested because CreateCampaign writes the FLIGHT on the ad
// set too, and a lifetime budget is paced against it: the active ad sets' shared window becomes
// the row's flight, and a campaign whose active ad sets disagree on it (or report an unreadable
// one) has no honest flight, so its lifetime budget stays unknown (see adSetBudgetTotal).
//
// The walk is bounded by monitorAdSetMaxPages; a `paging.next` on the last allowed page is an
// adSetBudgetsUnreadableError, never a partial sum. Every 2xx body passes identityjson.Check as
// raw bytes before it is decoded, like every other Meta read that attributes rows.
//
// Per-ad-set defects mark only that ad set's CAMPAIGN unknown: an explicit null, a non-string or
// unparseable or negative budget, both a daily and a lifetime budget (Meta documents them as
// mutually exclusive), neither (an active ad set under a campaign with no budget of its own has
// nothing to pace against), or an unreadable effective_status. A row that cannot be attributed
// at all — no canonical id or campaign_id, or an id seen twice — fails the whole read.
func (c *Client) fetchAccountAdSetBudgets(ctx context.Context, accountID string) (map[string]*adSetBudgetTotal, error) {
	out := map[string]*adSetBudgetTotal{}
	ids := map[string]struct{}{}
	cursors := map[string]struct{}{}
	after := ""
	for page := 0; page < monitorAdSetMaxPages; page++ {
		// Built from the opaque cursor, never from Meta's absolute paging.next URL, which
		// carries the access token as a query parameter.
		path := "/" + accountID + "/adsets?fields=id,campaign_id,daily_budget,lifetime_budget,effective_status,start_time,end_time&limit=" + strconv.Itoa(monitorAdSetPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var raw json.RawMessage
		if err := c.doRequest(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return nil, fmt.Errorf("list account ad-set budgets: %w", err)
		}
		unreadable := func(format string, args ...any) error {
			return &adSetBudgetsUnreadableError{err: fmt.Errorf("page %d: "+format, append([]any{page}, args...)...)}
		}
		if err := identityjson.Check(raw); err != nil {
			return nil, unreadable("%v", err)
		}
		var p pagedWire
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, unreadable("the response is not a paged Graph object")
		}
		if p.Data == nil {
			return nil, unreadable("2xx response with no data field")
		}
		for i, rowRaw := range *p.Data {
			var w adSetBudgetWire
			if err := json.Unmarshal(rowRaw, &w); err != nil {
				return nil, unreadable("row %d is not an ad set object of the expected shape", i)
			}
			if w.ID == nil || ValidateAdSetID(*w.ID) != nil {
				return nil, unreadable("row %d has no canonical ad set id", i)
			}
			if _, dup := ids[*w.ID]; dup {
				return nil, unreadable("row %d repeats an ad set already read", i)
			}
			ids[*w.ID] = struct{}{}
			if w.CampaignID == nil || ValidateCampaignID(*w.CampaignID) != nil {
				return nil, unreadable("row %d has no canonical campaign_id", i)
			}
			t := out[*w.CampaignID]
			if t == nil {
				t = &adSetBudgetTotal{}
				out[*w.CampaignID] = t
			}
			if w.EffectiveStatus == nil || !statusTokenRE.MatchString(*w.EffectiveStatus) {
				t.unknown = true // cannot tell whether this ad set is spending
				continue
			}
			if *w.EffectiveStatus != StatusActive {
				continue
			}
			t.addWindow(adSetWindow(w.StartTime, w.EndTime))
			daily, dailyOK := adSetBudgetField(w.DailyBudget)
			lifetime, lifetimeOK := adSetBudgetField(w.LifetimeBudget)
			switch {
			case !dailyOK || !lifetimeOK:
				t.unknown = true
			case daily > 0 && lifetime > 0, daily == 0 && lifetime == 0:
				t.unknown = true
			case daily > 0:
				if t.dailyMinor > math.MaxInt64-daily {
					t.unknown = true
				} else {
					t.dailyMinor += daily
				}
			default:
				if t.lifetimeMinor > math.MaxInt64-lifetime {
					t.unknown = true
				} else {
					t.lifetimeMinor += lifetime
				}
			}
		}
		next, done, cerr := nextCursor(&p, cursors)
		if cerr != nil {
			return nil, unreadable("%v", cerr)
		}
		if done {
			return out, nil
		}
		after = next
	}
	return nil, &adSetBudgetsUnreadableError{err: fmt.Errorf("exceeded %d pages of %d ad sets; refusing a partial sum", monitorAdSetMaxPages, monitorAdSetPageSize)}
}

// adSetBudgetField reads one ad-set budget field in minor units. ABSENT, "" and "0" are 0 — no
// budget at this level (Meta's floor is one minor unit). ok=false for an explicit null (unknown,
// not zero), a non-string, a negative value, or a string that is not an integer: a fraction is
// refused rather than truncated, because truncation is a silent change to money.
func adSetBudgetField(raw json.RawMessage) (minor int64, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0, true
	}
	if bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, false
	}
	var bad bool
	v := parseMinorUnits(s, &bad)
	if bad || (v != nil && *v < 0) {
		return 0, false
	}
	if v == nil {
		return 0, true
	}
	return *v, true
}
