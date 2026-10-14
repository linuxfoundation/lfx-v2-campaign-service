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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
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
	// the CAMPAIGN's own field when it has one (CBO), otherwise the sum of its ad sets' budgets
	// (ABO — the shape CreateCampaign produces), used only when every ad set is ACTIVE and they
	// share one flight. 0 means unknown or unset; the rules
	// layer reads a row with neither as PacingUnknown. See ListAccountCampaigns.
	DailyBudget    float64
	LifetimeBudget float64
	StartDate      string
	EndDate        string
	// MetricsSince / MetricsUntil are the Insights `time_range` (YYYY-MM-DD, both inclusive) the
	// row's delivery and spend cover — the SAME bounds on every row of one read, taken from one
	// clock reading (fetchAccountCampaignInsights). The rules layer paces against exactly these
	// days rather than re-deriving a window from its own clock, which could fall on the other
	// side of a UTC midnight from the read (PR #316 review).
	MetricsSince string
	MetricsUntil string
	// FetchFailed marks a row whose insights lookup could not be matched/parsed — this
	// dispatcher-level method never fabricates a zero-metrics row for a fetch failure.
	FetchFailed bool
	// FailureClass is why FetchFailed is set. Every Meta row failure is a value Meta returned
	// that would not parse or is not a measurement (an insights counter, a spend that is not a
	// finite plain decimal, or a budget string), so it is always decode.
	FailureClass model.MonitorFailureClass
}

// ListAccountCampaigns ports getMetaAnalytics: an account-level insights read
// (level=campaign, fully paginated — see fetchAccountCampaignInsights) merged with a
// campaign-list read for status/budget/schedule fields insights does not carry, one account
// currency read (budgets are minor units of THAT currency — see minorToWhole), and, only when an
// admitted campaign reports no campaign-level budget, one account-level ad-set budget read
// (fetchAccountAdSetBudgets) that supplies it from the campaign's ad sets when all are ACTIVE. Unlike the
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
	insightsByID, failedInsightIDs, window, err := c.fetchAccountCampaignInsights(ctx, accountID, days)
	if err != nil {
		return nil, err
	}

	editCutoff, cutoffOK := budgetEditCutoff(window.since)
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
			// Every row, delivered or not: a zero-delivery row's zero spend covers the same days.
			MetricsSince: window.since,
			MetricsUntil: window.until,
		}
		// Meta documents daily_budget and lifetime_budget as mutually exclusive, so a campaign
		// reporting BOTH is not a readable budget: neither is the plan, and pacing on either
		// alone would be a guess (PR #316 review). Both stay 0 — pacing unknown — exactly as the
		// same mix on ad sets is, and the campaign is not sent to the ad-set fallback either,
		// since it does report a campaign-level budget.
		bothBudgets := positiveMinor(cm.DailyMinor) && positiveMinor(cm.LifetimeMinor)
		// Pacing applies TODAY's budget to every day of the window, so a campaign-level budget
		// edited inside the window would raise a false under/overspend (PR #316 review). The
		// read carries no budget history, only updated_time — which moves on ANY edit, so this is
		// conservative — and a campaign updated inside the window (or whose updated_time is
		// unreadable) keeps its budget unknown, never falling back to the ad sets either.
		budgetEdited := !cutoffOK || editedSince(cm.UpdatedAt, editCutoff)
		if currencyKnown && !bothBudgets && !budgetEdited {
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
		if row.FetchFailed {
			row.FailureClass = model.MonitorFailureDecode
		}
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
	if !cutoffOK {
		return rows, nil // no window to test edits against: every ad-set budget stays unknown
	}
	totals, err := c.fetchAccountAdSetBudgets(ctx, accountID, editCutoff)
	if err != nil {
		var unreadable *adSetBudgetsUnreadableError
		if !errors.As(err, &unreadable) {
			return nil, err
		}
		// The read completed but cannot be trusted whole (page cap, a body that is not valid
		// JSON or that identityjson refuses, an unattributable row): every campaign that needed it stays at unknown
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
			// Daily pacing expects the budget only on the days of the window the flight ran on
			// (PR #316 review: a campaign started inside the window used to be expected to spend
			// for every requested day, a false underspend). The flight is the ad sets' — the
			// campaign-list row has no dates for an ABO campaign — so a daily budget is used only
			// with the active ad sets' shared window; without one, the days the summed budget
			// could have been spent on are not known, and the budget is left unknown.
			if t.windowSet && !t.windowUnknown {
				rows[i].DailyBudget = float64(t.dailyMinor) / float64(offset)
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
	// UpdatedAt is the campaign's updated_time; zero when absent, null or malformed.
	UpdatedAt time.Time
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
		path := "/" + accountID + "/campaigns?fields=id,name,status,daily_budget,lifetime_budget,start_time,stop_time,updated_time&limit=" + strconv.Itoa(monitorPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var resp struct {
			Data *[]struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
				// RAW, so an explicit JSON null (unknown — not zero, not absent) is told apart
				// from an absent field (no budget at this level). Decoded into a string, null
				// read as "" and sent the campaign to the ad-set fallback (PR #316 review).
				DailyBudget    json.RawMessage `json:"daily_budget"`
				LifetimeBudget json.RawMessage `json:"lifetime_budget"`
				StartTime      string          `json:"start_time"`
				StopTime       string          `json:"stop_time"`
				UpdatedTime    json.RawMessage `json:"updated_time"`
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
			dailyMinor := parseMinorUnits(campaignBudgetField(d.DailyBudget, &unparseable), &unparseable)
			lifetimeMinor := parseMinorUnits(campaignBudgetField(d.LifetimeBudget, &unparseable), &unparseable)
			if unparseable {
				dailyMinor, lifetimeMinor = nil, nil
			}
			// A NEGATIVE budget parses but is not a budget, and it is not "no budget" either:
			// read as non-positive it would send the campaign to the ad-set fallback, which
			// could then pace it on amounts the campaign's own (unreadable) budget overrides.
			// It is unreadable, like an unparseable one (PR #316 review).
			if (dailyMinor != nil && *dailyMinor < 0) || (lifetimeMinor != nil && *lifetimeMinor < 0) {
				unparseable = true
				dailyMinor, lifetimeMinor = nil, nil
			}
			out = append(out, metaCampaignListEntry{
				ID:            d.ID,
				Name:          d.Name,
				Status:        d.Status,
				DailyMinor:    dailyMinor,
				LifetimeMinor: lifetimeMinor,
				StartDate:     dateOnly(d.StartTime),
				EndDate:       dateOnly(d.StopTime),
				FetchFailed:   unparseable,
				UpdatedAt:     metaTimeField(d.UpdatedTime),
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

// monitorSpendRE is the only shape an Insights `spend` takes: a plain decimal string ("12.34",
// "0"). Meta sends no sign, exponent, hex or digit separators, so none is accepted.
var monitorSpendRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// parseMonitorSpend parses an Insights `spend` string. strconv.ParseFloat alone accepts "NaN",
// "Inf", "-1.5", exponents ("1e2"), hex floats ("0x1p+2") and, with a base prefix, underscores —
// none of which is a spend Meta sends, and a negative, NaN or infinite value reaching the rules
// layer would be paced as a real reading (PR #316 review). So the string must first match
// monitorSpendRE (plain decimal), and the parsed value must be finite (a decimal too long for
// float64 parses to ±Inf with ErrRange). Anything else is an error, which the caller records as a
// per-row parse failure (the row is FetchFailed). The error carries the length, never the
// upstream bytes, like parseMetricInt.
func parseMonitorSpend(s string) (float64, error) {
	if !monitorSpendRE.MatchString(s) {
		return 0, fmt.Errorf("spend is not a plain decimal amount (%d bytes)", len(s))
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("spend is not a number (%d bytes)", len(s))
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, fmt.Errorf("spend is not a finite non-negative amount (%d bytes)", len(s))
	}
	return v, nil
}

// insightsWindow is the Insights time_range one account-monitor read used, YYYY-MM-DD inclusive.
type insightsWindow struct{ since, until string }

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
//
// The since/until it sent are returned as an insightsWindow: they are the one clock reading the
// whole monitor read is paced against (see AccountCampaignRow.MetricsSince).
func (c *Client) fetchAccountCampaignInsights(ctx context.Context, accountID string, days int) (map[string]metaInsightsRow, map[string]struct{}, insightsWindow, error) {
	end := c.timeNow().UTC()
	start := end.AddDate(0, 0, -(days - 1))
	window := insightsWindow{since: start.Format("2006-01-02"), until: end.Format("2006-01-02")}
	timeRange := `{"since":"` + window.since + `","until":"` + window.until + `"}`

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
			return nil, nil, insightsWindow{}, fmt.Errorf("list account campaign insights: %w", err)
		}
		if resp.Data == nil {
			return nil, nil, insightsWindow{}, &transportError{
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
				return nil, nil, insightsWindow{}, &transportError{
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
				spend, errS = parseMonitorSpend(row.Spend)
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
			return out, failed, window, nil // fully enumerated
		}
		after = strings.TrimSpace(resp.Paging.Cursors.After)
		if after == "" {
			return nil, nil, insightsWindow{}, fmt.Errorf("list account campaign insights has more pages but no cursor; cannot guarantee every campaign was enumerated")
		}
		if _, dup := seen[after]; dup {
			return nil, nil, insightsWindow{}, fmt.Errorf("list account campaign insights did not terminate (repeated paging cursor)")
		}
		seen[after] = struct{}{}
	}
	return nil, nil, insightsWindow{}, fmt.Errorf("list account campaign insights exceeded %d pages; too many rows to enumerate", monitorMaxPages)
}

// metaTimestampLayouts are the timestamp forms dateOnly accepts: Graph's documented default,
// "2026-01-15T00:00:00-0800" (the layout CreateCampaign writes, client.go), and RFC 3339
// ("…+00:00" / "…Z") — the same pair the settings readback parses (dispatch/meta_settings.go).
var metaTimestampLayouts = []string{"2006-01-02T15:04:05-0700", time.RFC3339}

// campaignBudgetField reads one campaign-list budget member: ABSENT is "" (no budget at this
// level, like the ad-set read); an explicit null or a non-string value sets *bad (unreadable —
// not zero, not absent, so never a reason to fall back to the ad sets).
func campaignBudgetField(raw json.RawMessage, bad *bool) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	var s string
	if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &s) != nil {
		*bad = true
		return ""
	}
	return s
}

// metaTimeField reads a raw Graph timestamp member (updated_time) as a whole Meta timestamp; the
// zero time for an absent, null, non-string or malformed value.
func metaTimeField(raw json.RawMessage) time.Time {
	var s string
	if json.Unmarshal(bytes.TrimSpace(raw), &s) != nil {
		return time.Time{}
	}
	t, _ := parseMetaTimestamp(s)
	return t
}

// parseMetaTimestamp parses a WHOLE Meta timestamp in one of metaTimestampLayouts.
func parseMetaTimestamp(s string) (time.Time, bool) {
	for _, layout := range metaTimestampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// editedSince reports whether an object's updated_time may fall inside the spend window: true
// when it is unknown (zero) or not before cutoff. See budgetEditCutoff.
func editedSince(updated, cutoff time.Time) bool {
	return updated.IsZero() || !updated.Before(cutoff)
}

// budgetEditCutoff is the earliest instant an edit could still have changed the plan for some
// day of the Insights window: the window's first day at 00:00 UTC, less one day. Meta reads
// since/until in the AD ACCOUNT's timezone (up to ±14h from UTC), so the extra day keeps an edit
// made just before the window in UTC but inside it locally on the unknown side. ok=false when the
// window's start does not parse.
func budgetEditCutoff(since string) (time.Time, bool) {
	d, err := time.Parse("2006-01-02", since)
	if err != nil {
		return time.Time{}, false
	}
	return d.AddDate(0, 0, -1), true
}

// dateOnly parses a WHOLE Meta timestamp in one of metaTimestampLayouts and returns its
// YYYY-MM-DD date as written (in the timestamp's own offset), matching
// model.AccountCampaignMetrics.StartDate/EndDate's date-only contract. Anything else — empty,
// truncated, a non-calendar date ("0000-00-00…"), or a valid date followed by trailing text
// ("2026-10-01garbage") — yields "", the unknown-date representation. It used to validate only
// the first 10 characters, so a malformed timestamp with a plausible prefix was read as a flight
// date (PR #316 review).
func dateOnly(s string) string {
	if t, ok := parseMetaTimestamp(s); ok {
		return t.Format("2006-01-02")
	}
	return ""
}

// adSetBudgetTotal is the sum of one campaign's ad-set budgets, in the account currency's minor
// units. unknown marks a campaign at least one of whose ad sets is not ACTIVE, or could not be
// read as exactly one positive budget — its total is then not a statement about the campaign and
// is never used.
//
// start/end are the flight (YYYY-MM-DD) every budgeted active ad set shares; windowSet says one
// was seen, windowUnknown that one was unreadable (absent/null/malformed start_time, malformed or
// null end_time) or that two active ad sets disagree. An absent end_time is an open-ended flight
// ("" end). The window is tracked apart from `unknown`: without one shared window neither a daily
// nor a lifetime total is used, since both are paced against the days the flight ran.
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
// start is absent, empty, null or malformed, or the end is null or malformed. An ABSENT or EMPTY
// ("") end is an open-ended flight, end "" — the same reading the campaign list gives an empty
// stop_time (PR #316 review: an empty end_time was classified as malformed, which left every
// open-ended DAILY ABO campaign pacing unknown). Daily pacing then runs to the window's edge;
// a lifetime budget with no end stays unknown in the rules layer. Only a non-empty value that is
// not a whole Meta timestamp (dateOnly) is malformed.
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
		if s == "" {
			return "", false, true // reported as empty: no value, like an absent field
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
// absent — it is a statement that the value is unknown. effective_status is RAW too (PR #316
// review): decoded into *string, a non-string value (e.g. 123) failed the WHOLE row's decode and
// with it the whole read, costing every campaign its ad-set budget; read separately, it marks only
// its own campaign unknown.
type adSetBudgetWire struct {
	ID              *string         `json:"id"`
	CampaignID      *string         `json:"campaign_id"`
	EffectiveStatus json.RawMessage `json:"effective_status"`
	DailyBudget     json.RawMessage `json:"daily_budget"`
	LifetimeBudget  json.RawMessage `json:"lifetime_budget"`
	StartTime       json.RawMessage `json:"start_time"`
	EndTime         json.RawMessage `json:"end_time"`
	UpdatedTime     json.RawMessage `json:"updated_time"`
}

// fetchAccountAdSetBudgets reads every ad set on the account with ONE account-level paged call,
//
//	GET /{accountID}/adsets?fields=id,campaign_id,daily_budget,lifetime_budget,effective_status,start_time,end_time,updated_time&limit=500
//
// and sums their budgets per campaign_id. Every ad set must be ACTIVE: a campaign with any ad set
// in another effective_status (paused itself, under a paused campaign, archived, in review …) is
// unknown, because the campaign's Insights spend still carries what that ad set spent in the
// window before it stopped, and when it stopped is not on this read (PR #316 review). Meta's
// default edge read omits DELETED ad sets, so one deleted inside the window is not seen here —
// a residual this read cannot close without ad-set-level Insights. `id` is requested beyond the four fields the
// sum needs because it is the only proof a row is not a repeat — a duplicated row would double
// a budget. start_time/end_time are requested because CreateCampaign writes the FLIGHT on the ad
// set too, and a lifetime budget is paced against it: the active ad sets' shared window becomes
// the row's flight, and a campaign whose active ad sets disagree on it (or report an unreadable
// one) has no honest flight, so its lifetime budget stays unknown (see adSetBudgetTotal).
//
// The walk is bounded by monitorAdSetMaxPages; a `paging.next` on the last allowed page is an
// adSetBudgetsUnreadableError, never a partial sum. Every 2xx body is taken undecoded (rawBody)
// and must be valid JSON and pass identityjson.Check before it is decoded, like every other Meta
// read that attributes rows; a body that fails either is an adSetBudgetsUnreadableError, so only
// the budgets are lost, never the monitor's delivery metrics.
//
// Per-ad-set defects mark only that ad set's CAMPAIGN unknown: a non-ACTIVE ad set, an explicit
// null, a non-string or unparseable or negative budget, both a daily and a lifetime budget (Meta documents them as
// mutually exclusive), neither (an active ad set under a campaign with no budget of its own has
// nothing to pace against), or an unreadable effective_status. A row that cannot be attributed
// at all — no canonical id or campaign_id, or an id seen twice — fails the whole read.
func (c *Client) fetchAccountAdSetBudgets(ctx context.Context, accountID string, editCutoff time.Time) (map[string]*adSetBudgetTotal, error) {
	out := map[string]*adSetBudgetTotal{}
	ids := map[string]struct{}{}
	cursors := map[string]struct{}{}
	after := ""
	for page := 0; page < monitorAdSetMaxPages; page++ {
		// Built from the opaque cursor, never from Meta's absolute paging.next URL, which
		// carries the access token as a query parameter.
		path := "/" + accountID + "/adsets?fields=id,campaign_id,daily_budget,lifetime_budget,effective_status,start_time,end_time,updated_time&limit=" + strconv.Itoa(monitorAdSetPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		// rawBody, not a json.RawMessage: decoding inside doRequest turned a syntax-invalid 2xx
		// into a transportError that failed the WHOLE monitor, metrics included, instead of
		// leaving only these budgets unknown (PR #316 review). The bytes are classified here.
		var body rawBody
		if err := c.doRequest(ctx, http.MethodGet, path, nil, &body); err != nil {
			return nil, fmt.Errorf("list account ad-set budgets: %w", err)
		}
		raw := body.b
		unreadable := func(format string, args ...any) error {
			return &adSetBudgetsUnreadableError{err: fmt.Errorf("page %d: "+format, append([]any{page}, args...)...)}
		}
		if !json.Valid(raw) {
			return nil, unreadable("the response is not valid JSON")
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
			status, statusOK := adSetStatusField(w.EffectiveStatus)
			if !statusOK {
				t.unknown = true // cannot tell whether this ad set is spending
				continue
			}
			if status != StatusActive {
				// A paused (or otherwise non-ACTIVE) ad set's budget is not part of what the
				// campaign is expected to spend NOW — but the campaign-level Insights spend still
				// includes whatever it spent in the window before it stopped (PR #316 review: two
				// ad sets at 10/day for 7 days, one then paused, read 140 spent against 70
				// expected — a false overspend). When it stopped is not on this read, so its
				// share of the window cannot be prorated: the campaign's ad-set budget is
				// unknown rather than compared against spend from a different set of ad sets.
				t.unknown = true
				continue
			}
			t.addWindow(adSetWindow(w.StartTime, w.EndTime))
			// Today's budget is applied to every day of the window, so an ad set edited inside
			// it — or one whose updated_time is unreadable — makes its campaign unknown: the
			// plan for the window's earlier days cannot be reconstructed from this read
			// (PR #316 review). updated_time moves on any edit, so this is conservative.
			if editedSince(metaTimeField(w.UpdatedTime), editCutoff) {
				t.unknown = true
			}
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

// adSetStatusField reads effective_status. ok=false for an absent, null or non-string value, or
// one outside the status-token charset: the ad set's spending state is then unknown.
func adSetStatusField(raw json.RawMessage) (string, bool) {
	var s string
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &s) != nil || !statusTokenRE.MatchString(s) {
		return "", false
	}
	return s, true
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
