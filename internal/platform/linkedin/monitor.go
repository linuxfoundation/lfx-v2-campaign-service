// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// AccountCampaignRow is one campaign read live from an ad account for the account-monitor
// endpoint, ported from lfx-self-serve's linkedin-ads.service.ts (getLinkedInAnalytics).
// The internal/service/rules layer computes pacing/action items from these raw fields.
//
// This port deliberately does NOT fetch per-campaign creative counts — see
// internal/service/rules/monitor_linkedin.go's doc comment on the omitted "no creatives"
// rule — so this row carries no creative-count field.
type AccountCampaignRow struct {
	CampaignID  string
	Name        string
	Status      string
	Impressions int64
	Clicks      int64
	Ctr         float64
	// Spend is Ad Analytics' costInLocalCurrency: the account's own currency, the same one its
	// budgets are in, so the two can be paced against each other on any account.
	Spend       float64
	Conversions *float64
	DailyBudget float64
	TotalBudget float64
	StartDate   string
	EndDate     string
	// PacingUnknown is set on a row whose budget names a currency when the campaign list's
	// budgets disagree about the account's currency: which of them is in the spend's currency
	// cannot be told, so none is paced (see budgetCurrencies).
	PacingUnknown bool
	// budgetCurrencyNamed records that this row's dailyBudget or totalBudget carried a
	// currencyCode, so a disagreement elsewhere in the list can withhold its pacing.
	budgetCurrencyNamed bool
	// budgetCurrencyAmbiguous records that this row's budget currency cannot be read: a
	// currencyCode that is null or not a JSON string, or an element identityjson would not trust
	// (a repeated key, exact or case-folded, that encoding/json would silently resolve). Its
	// pacing is withheld whatever the rest of the list says.
	budgetCurrencyAmbiguous bool
	// FetchFailed marks a row this service could not read trustworthily: a malformed
	// dailyBudget/totalBudget amount from fetchAccountCampaignList, or an unparseable
	// costInLocalCurrency from the analytics pivot (monitorMetricsRow.FetchFailed, ORed in below). A
	// campaign the analytics pivot omits from a SUCCESSFUL response is NOT a fetch failure —
	// LinkedIn omits campaigns with no activity in the window, and the zero-value fields left
	// in place are the honest "no delivery" reading (see ListAccountCampaigns).
	FetchFailed bool
	// FailureClass is why FetchFailed is set. Every LinkedIn row failure is a value LinkedIn
	// returned that would not parse (a budget amount or costInLocalCurrency) — the analytics read is one
	// account-level call that fails the whole read rather than one row — so it is always decode.
	FailureClass model.MonitorFailureClass
}

// monitorPageSize is the finder page size for the account-scoped campaign list, the
// documented LinkedIn maximum for the adCampaigns search finder.
const monitorPageSize = 100

// monitorMaxPages bounds the campaign-list walk. Exceeding it is an error, matching this
// package's other paginated walks (listCreativeURNs, ListAdAccounts).
const monitorMaxPages = 50

// ListAccountCampaigns ports getLinkedInAnalytics: every ACTIVE/PAUSED campaign on
// accountID, merged with one account-level Ad Analytics pivot=CAMPAIGN read covering the
// last `days` days (getLinkedInAnalytics's own dateRangeParams(days) window).
//
// days is not re-validated here: this client trusts it is already within the service layer's
// 7..90 bound (internal/service/connection_monitor.go's validateMonitorDays, itself mirroring
// the design layer's Minimum/Maximum) before it reaches fetchAccountCampaignAnalytics' window
// arithmetic. That is a deliberate asymmetry, not an oversight — today's only caller is that
// service layer; a future non-HTTP caller of this package directly would need its own bound.
func (c *Client) ListAccountCampaigns(ctx context.Context, accountID string, days int) ([]AccountCampaignRow, error) {
	rows, _, err := c.ListAccountCampaignsWithCurrency(ctx, accountID, days)
	return rows, err
}

// ListAccountCampaignsWithCurrency is ListAccountCampaigns plus the currency the monitor's amounts
// are in, at no extra call: the currencyCode LinkedIn puts on every campaign's dailyBudget and
// totalBudget in the campaign list this read already walks. Spend is read as
// costInLocalCurrency — the account's currency — so spend and budgets share it on any account.
//
// The answer is that code when every budget the list carried named the same well-formed one. It
// is "" when the list carried no budget, when any budget omitted its code, when the code is not
// three upper-case letters, or when two budgets disagree. A disagreement also sets PacingUnknown
// on every row whose budget named a currency: one account has one currency, so the list cannot be
// trusted to say which budgets are in the spend's. None of these fails the read.
func (c *Client) ListAccountCampaignsWithCurrency(ctx context.Context, accountID string, days int) ([]AccountCampaignRow, string, error) {
	// Same guard client.go:654 and targeting.go apply before interpolating an account/campaign
	// id into a URN or path: the Goa design layer's Pattern(`^[0-9]+$`) already rejects a
	// malformed id at the HTTP boundary, but this dispatcher method is also reachable directly
	// (e.g. from tests or a future non-HTTP caller), so it re-checks rather than trusting the
	// caller.
	if err := ValidateAccountID(accountID); err != nil {
		return nil, "", err
	}
	campaigns, budgetCurrency, err := c.fetchAccountCampaignList(ctx, accountID)
	mismatch := budgetCurrency.disagree()
	if err != nil {
		return nil, "", err
	}
	end := c.now().UTC()
	start := end.AddDate(0, 0, -(days - 1))
	metricsByID, err := c.fetchAccountCampaignAnalytics(ctx, accountID, start, end)
	if err != nil {
		return nil, "", err
	}

	out := make([]AccountCampaignRow, 0, len(campaigns))
	for _, cm := range campaigns {
		row := cm
		if (mismatch && row.budgetCurrencyNamed) || row.budgetCurrencyAmbiguous {
			row.PacingUnknown = true
		}
		// A campaign absent from a SUCCESSFUL account-wide analytics response is not a fetch
		// failure: fetchAccountCampaignAnalytics is one account-level call that either returns
		// this whole map or a non-nil error above, and LinkedIn omits a campaign with no
		// activity in the window from its pivoted results entirely (round-19 review). Leaving
		// the row at its zero defaults reports the honest "no delivery" reading; only a
		// genuine per-row parse failure (m.FetchFailed, from a malformed costInLocalCurrency) marks the
		// row failed.
		//
		// row.FetchFailed may already be true from fetchAccountCampaignList's own budget parse
		// (a malformed dailyBudget/totalBudget amount) — OR it with m.FetchFailed rather than
		// overwrite, or a bad analytics read here would silently clear that earlier failure.
		if m, ok := metricsByID[cm.CampaignID]; ok {
			row.Impressions = m.Impressions
			row.Clicks = m.Clicks
			row.Spend = m.Spend
			row.Conversions = m.Conversions
			row.FetchFailed = row.FetchFailed || m.FetchFailed
			if row.Impressions > 0 {
				row.Ctr = float64(row.Clicks) / float64(row.Impressions) * 100
			}
		}
		if row.FetchFailed {
			row.FailureClass = model.MonitorFailureDecode
		}
		out = append(out, row)
	}
	return out, budgetCurrency.code(), nil
}

// fetchAccountCampaignList walks GET /adAccounts/{id}/adCampaigns?q=search over
// ACTIVE/PAUSED campaigns, decoding the budget/schedule fields the shared responseElement
// type does not carry — this uses its own response shape rather than doRequest's
// linkedInResponse for that reason.
//
// It also returns what the budgets said about the account's currency: every dailyBudget and
// totalBudget object carries a currencyCode, which is the account's (see
// ListAccountCampaignsWithCurrency for what the monitor makes of it).
func (c *Client) fetchAccountCampaignList(ctx context.Context, accountID string) ([]AccountCampaignRow, budgetCurrencies, error) {
	nestedPath := "adAccounts/" + accountID + "/adCampaigns"
	var out []AccountCampaignRow
	var currencies budgetCurrencies
	pageToken := ""
	for page := 0; page < monitorMaxPages; page++ {
		// Rest.li 2.0 nested literal, mirroring findByName's
		// "(name:(values:List(...)))" pattern (client.go:1444) — NOT flat
		// bracket-indexed query keys, which LinkedIn's finder does not parse.
		params := map[string]string{
			"q":        "search",
			"search":   "(status:(values:List(ACTIVE,PAUSED)))",
			"pageSize": strconv.Itoa(monitorPageSize),
		}
		if pageToken != "" {
			params["pageToken"] = pageToken
		}
		var resp struct {
			// Elements stay RAW so each can be checked with identityjson and decoded on its own.
			Elements *[]json.RawMessage `json:"elements"`
			Metadata *linkedInMetadata  `json:"metadata"`
		}
		// The page is fetched RAW and checked with identityjson before it is decoded, as the
		// Meta audience read's pages are: encoding/json resolves a repeated key — exact or
		// case-folded — in favour of the last value with no error, so a budget carrying
		// `"currencyCode":"USD","CurrencyCode":"EUR"` would read as EUR. An untrusted page does
		// not fail the monitor (its rows were read this way before the currency existed); it
		// withholds the account currency, and an untrusted ELEMENT also withholds that
		// campaign's pacing, since its budget's currency is ambiguous.
		var page json.RawMessage
		if err := c.doMonitorGET(ctx, nestedPath, params, &page); err != nil {
			return nil, budgetCurrencies{}, fmt.Errorf("list account campaigns: %w", err)
		}
		pageTrusted := identityjson.Check(page) == nil
		if !pageTrusted {
			currencies.unusable = true
		}
		if err := json.Unmarshal(page, &resp); err != nil {
			return nil, budgetCurrencies{}, fmt.Errorf("list account campaigns: response is not a campaign page of the expected shape")
		}
		if resp.Elements == nil {
			return nil, budgetCurrencies{}, fmt.Errorf("list account campaigns: response has no elements field")
		}
		for _, raw := range *resp.Elements {
			var el monitorCampaignElement
			if err := json.Unmarshal(raw, &el); err != nil {
				return nil, budgetCurrencies{}, fmt.Errorf("list account campaigns: an element is not a campaign of the expected shape")
			}
			row := AccountCampaignRow{CampaignID: el.ID.String(), Name: el.Name, Status: el.Status}
			// An element identityjson refuses has no currency this read can believe — the decoded
			// one is merely the last of a repeated key — so it is not folded into the account's
			// set, which is withheld anyway (pageTrusted is false), and its pacing is withheld.
			elTrusted := pageTrusted || identityjson.Check(raw) == nil
			if !elTrusted && (el.DailyBudget != nil || el.TotalBudget != nil) {
				row.budgetCurrencyAmbiguous = true
			}
			if el.DailyBudget != nil {
				if elTrusted {
					named, usable := currencies.observe(el.DailyBudget.CurrencyCode)
					row.budgetCurrencyNamed = row.budgetCurrencyNamed || named
					row.budgetCurrencyAmbiguous = row.budgetCurrencyAmbiguous || !usable
				}
				if v, ok := parseUSDAmount(el.DailyBudget.Amount); ok {
					row.DailyBudget = v
				} else {
					row.FetchFailed = true
				}
			}
			if el.TotalBudget != nil {
				if elTrusted {
					named, usable := currencies.observe(el.TotalBudget.CurrencyCode)
					row.budgetCurrencyNamed = row.budgetCurrencyNamed || named
					row.budgetCurrencyAmbiguous = row.budgetCurrencyAmbiguous || !usable
				}
				if v, ok := parseUSDAmount(el.TotalBudget.Amount); ok {
					row.TotalBudget = v
				} else {
					row.FetchFailed = true
				}
			}
			if el.RunSchedule != nil {
				if el.RunSchedule.Start > 0 {
					row.StartDate = time.UnixMilli(el.RunSchedule.Start).UTC().Format("2006-01-02")
				}
				if el.RunSchedule.End > 0 {
					row.EndDate = time.UnixMilli(el.RunSchedule.End).UTC().Format("2006-01-02")
				}
			}
			out = append(out, row)
		}
		// An ABSENT metadata block is not an exhausted cursor — mirrors the same false-absence
		// guard in accounts.go's adAccount picker (accounts.go:202-211), though only that half of
		// it: unlike accounts.go, this loop has no seen-cursor dedup, so a server that repeats a
		// pageToken is only bounded by monitorMaxPages below, not caught immediately. Without the
		// metadata check, a malformed or truncated intermediate page reads as "no more pages" and
		// silently returns a partial campaign list as a complete one (round-24 review).
		if resp.Metadata == nil {
			return nil, budgetCurrencies{}, fmt.Errorf("list account campaigns: response has no metadata; cannot confirm all campaigns were enumerated")
		}
		if resp.Metadata.NextPageToken == "" {
			return out, currencies, nil
		}
		pageToken = resp.Metadata.NextPageToken
	}
	return nil, budgetCurrencies{}, fmt.Errorf("list account campaigns: exceeded %d pages", monitorMaxPages)
}

// monitorCampaignElement is one element of the campaign list, decoded only after the element
// has been checked. Each budget's currencyCode is RAW and decoded on its own
// (budgetCurrencies.observe), so a value of the wrong JSON type costs only the currency and that
// campaign's pacing — never the element, the page or the read.
type monitorCampaignElement struct {
	ID          flexibleID      `json:"id"`
	Name        string          `json:"name"`
	Status      string          `json:"status"`
	DailyBudget *monitorBudget  `json:"dailyBudget"`
	TotalBudget *monitorBudget  `json:"totalBudget"`
	RunSchedule *monitorRunTime `json:"runSchedule"`
}

type monitorBudget struct {
	Amount       string          `json:"amount"`
	CurrencyCode json.RawMessage `json:"currencyCode"`
}

type monitorRunTime struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// budgetCurrencies folds the currencyCode of every budget object the campaign list carried. One
// account has one currency, so the set yields a code only when every budget named the same
// well-formed one; a budget with no code leaves it unconfirmed, two different codes (a malformed
// one included) are a disagreement, and a code that is null or not a JSON string — or a page
// identityjson would not trust — makes the set unusable.
type budgetCurrencies struct {
	codes    map[string]struct{}
	missing  bool
	unusable bool
}

// observe folds one budget's raw currencyCode. named reports that the budget carried the field at
// all; usable is false when it was null or not a JSON string, which leaves that budget's currency
// unknown.
func (b *budgetCurrencies) observe(raw json.RawMessage) (named, usable bool) {
	if len(raw) == 0 {
		b.missing = true
		return false, true
	}
	var code string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &code) != nil {
		b.unusable = true
		return true, false
	}
	if b.codes == nil {
		b.codes = map[string]struct{}{}
	}
	b.codes[code] = struct{}{}
	return true, true
}

// disagree reports whether two budgets named different currencies.
func (b budgetCurrencies) disagree() bool { return len(b.codes) > 1 }

// code is the one well-formed currency every budget named, or "".
func (b budgetCurrencies) code() string {
	if b.missing || b.unusable || len(b.codes) != 1 {
		return ""
	}
	for c := range b.codes {
		return model.NormalizeCurrencyCode(c)
	}
	return ""
}

type monitorMetricsRow struct {
	Impressions int64
	Clicks      int64
	Spend       float64
	Conversions *float64
	// FetchFailed marks a row whose costInLocalCurrency could not be converted — see the round-19-review
	// comment at the parse site in fetchAccountCampaignAnalyticsRaw below.
	FetchFailed bool
}

// fetchAccountCampaignAnalytics ports getLinkedInAnalytics' account-level adAnalyticsV2
// pivot=CAMPAIGN read: one Ad Analytics finder call scoped to the whole account (no
// `campaigns` filter, unlike GetCampaignMetrics), returning one element per campaign.
//
// Unlike makeAdAnalyticsRequest (GetCampaignMetrics' single-campaign read), this is a
// single, non-retried attempt: a 429 here surfaces directly as an error rather than
// being retried with backoff. This monitor view is a best-effort account-wide read, not
// a request in a multi-step write flow, so the added complexity of the retry loop is
// deliberately not carried over — same scope decision as doMonitorGET.
func (c *Client) fetchAccountCampaignAnalytics(ctx context.Context, accountID string, start, end time.Time) (map[string]monitorMetricsRow, error) {
	accountURN := "urn:li:sponsoredAccount:" + accountID
	u, err := url.Parse(c.baseURL + "/adAnalytics")
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	// The legacy SINGULAR `pivotValue` is NOT a projectable field on this schema (LinkedIn
	// rejects it with a 400 "Projected field \"pivotValue\" not present in schema
	// ...AdAnalyticsV8"), so it is not listed here; this read relies on pivotValues arriving with
	// `pivot=CAMPAIGN`, the same way the BFF's fields list (linkedin-ads.service.ts:894) never
	// requests it. The PLURAL `pivotValues` is a documented projectable field (ads-reporting,
	// "Requesting specific metrics in the Analytics Finder"), which the audience read projects.
	u.RawQuery = "q=analytics" +
		"&pivot=CAMPAIGN" +
		"&timeGranularity=ALL" +
		"&dateRange=(start:" + restLiDate(start) + ",end:" + restLiDate(end) + ")" +
		"&accounts=List(" + url.QueryEscape(accountURN) + ")" +
		"&fields=impressions,clicks,costInLocalCurrency,externalWebsiteConversions"

	// AdAnalyticsElement/AdAnalyticsResponse (decoded by doAdAnalyticsAttempt) carry no
	// pivotValue field — they were built for the single-campaign path, which filters by a
	// single campaign URN and never needs to know which campaign a row belongs to. The
	// account-wide pivot response here DOES need pivotValue (it's the only way to attribute
	// each row to a campaign), so this reads and decodes the response itself rather than
	// calling doAdAnalyticsAttempt.
	return c.fetchAccountCampaignAnalyticsRaw(ctx, u.String())
}

// fetchAccountCampaignAnalyticsRaw performs the actual GET + decode, keeping
// fetchAccountCampaignAnalytics's doc comment describing the query in one place while this
// function owns the pivotValue-aware response shape doAdAnalyticsAttempt cannot decode.
func (c *Client) fetchAccountCampaignAnalyticsRaw(ctx context.Context, rawURL string) (map[string]monitorMetricsRow, error) {
	attemptCtx, cancel, token, err := c.authorizedAttempt(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("LinkedIn-Version", c.apiVersion)
	req.Header.Set("X-RestLi-Protocol-Version", "2.0.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &transportError{Method: "GET", Path: "adAnalytics", Err: redactHTTPDoError(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &transportError{Method: "GET", Path: "adAnalytics", Err: redactBodyReadError(err)}
	}
	// A body at exactly maxResponseBytes+1 must be REJECTED, not decoded from its first
	// maxResponseBytes bytes: io.LimitReader returns EOF (not an error) at the cap, so an
	// oversized response whose truncated prefix happens to be valid JSON would otherwise be
	// silently accepted as a complete read (client.go:1128, metrics.go:626, token.go:412 all
	// apply this same check).
	if int64(len(body)) > maxResponseBytes {
		return nil, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("response exceeds %d bytes", maxResponseBytes)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if ce := c.expiredCredentialsError(resp.StatusCode, string(body), http.MethodGet); ce != nil {
			return nil, ce
		}
		return nil, &apiError{StatusCode: resp.StatusCode, Method: "GET", Path: "adAnalytics"}
	}

	var parsed struct {
		// Elements is a pointer so `{}`, `"elements":null`, and a missing field are all
		// distinguishable from a genuine empty array — a value-typed slice decodes all three
		// as the same nil/zero-length slice with no error, which this endpoint's caller
		// (ListAccountCampaigns) would then read as "every listed campaign had zero activity"
		// instead of "the analytics read failed" (round-24 review).
		Elements *[]struct {
			// PivotValues is not named in this read's `fields`; it relies on the field arriving
			// with `pivot=CAMPAIGN`, as the BFF's does. Only the legacy SINGULAR `pivotValue` is
			// unprojectable — the plural is a documented `fields` value (see the RawQuery comment
			// above). Mirrors the BFF's `pivotValues?: string[]` (linkedin-ads.service.ts:907).
			PivotValues []string `json:"pivotValues"`
			Impressions int64    `json:"impressions"`
			Clicks      int64    `json:"clicks"`
			// CostInLocalCurrency is spend in the ad account's own currency — the currency its
			// budgets are in — not costInUsd, which would compare USD spend with a non-USD
			// account's budgets.
			CostInLocalCurrency *string `json:"costInLocalCurrency"`
			Conversions         *int64  `json:"externalWebsiteConversions"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("decode account monitor analytics: malformed JSON (%d bytes)", len(body))}
	}
	if parsed.Elements == nil {
		return nil, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("decode account monitor analytics: response has no elements field")}
	}
	out := make(map[string]monitorMetricsRow, len(*parsed.Elements))
	for _, el := range *parsed.Elements {
		// A row with no pivot value, or one that doesn't resolve to a campaign id, cannot be
		// attributed to any campaign — there is no id to key `FetchFailed` on, unlike the
		// costInLocalCurrency case below. Silently dropping it would let ListAccountCampaigns read the
		// row's real campaign as a legitimate zero-activity omission (round-31 review), so the
		// whole read is rejected instead.
		if len(el.PivotValues) == 0 {
			return nil, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("decode account monitor analytics: element has no pivotValues")}
		}
		id := trailingID(el.PivotValues[0])
		if id == "" {
			return nil, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("decode account monitor analytics: pivotValues did not resolve to a campaign id")}
		}
		row := monitorMetricsRow{Impressions: el.Impressions, Clicks: el.Clicks}
		if el.CostInLocalCurrency != nil {
			// costInUsdToMicros is a plain decimal parser despite its name (the same decimal
			// form LinkedIn uses for both cost fields).
			if micros, err := costInUsdToMicros(*el.CostInLocalCurrency); err == nil {
				row.Spend = float64(micros) / 1_000_000
			} else {
				// A non-empty, unparseable costInLocalCurrency is an upstream-data failure, not a
				// legitimate zero: reporting Spend=0 here would read to the rule engine
				// as a real "no delivery" measurement instead of unknown (round-19 review,
				// same class as the sibling Google Ads/Meta fixes in this range).
				row.FetchFailed = true
			}
		}
		if el.Conversions != nil {
			conv := float64(*el.Conversions)
			row.Conversions = &conv
		}
		out[id] = row
	}
	return out, nil
}

// parseUSDAmount parses a LinkedIn budget "amount" decimal string (e.g. "150.00") to a
// float64. ok=false means s was non-empty but failed to parse — an upstream-data failure,
// not a legitimate zero, mirroring the costInLocalCurrency fix above (round-19 review) and the
// Google Ads/Meta budget-parsing siblings. An empty string is a legitimate zero (LinkedIn
// omits the field rather than sending "0.00" for an unbudgeted campaign) and returns
// (0, true).
func parseUSDAmount(s string) (amount float64, ok bool) {
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// doMonitorGET performs a single, non-retried finder GET and decodes the response body into
// out. It exists because the campaign-list finder's response shape (budget/schedule fields)
// is not the shared linkedInResponse doRequest decodes into; unlike doRequest it does not
// retry on 429, matching this port's general "single read, no special-cased backoff for a
// monitor view" scope (the write paths keep doRequest's full retry discipline).
func (c *Client) doMonitorGET(ctx context.Context, path string, query map[string]string, out any) error {
	u, err := url.Parse(c.baseURL + "/" + path)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	// buildRawQuery (client.go) leaves preEncodedParams (e.g. "search") unescaped so a
	// Rest.li nested literal's structural characters reach LinkedIn verbatim — see its
	// doc comment for why url.Values.Encode() would corrupt that literal.
	u.RawQuery = buildRawQuery(query)

	attemptCtx, cancel, token, err := c.authorizedAttempt(ctx)
	if err != nil {
		cancel()
		return err
	}
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("LinkedIn-Version", c.apiVersion)
	req.Header.Set("X-RestLi-Protocol-Version", "2.0.0")
	req.Header.Set("X-Restli-Method", "FINDER")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &transportError{Method: "GET", Path: path, Err: redactHTTPDoError(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return &transportError{Method: "GET", Path: path, Err: redactBodyReadError(err)}
	}
	// See fetchAccountCampaignAnalyticsRaw's identical check above for why the +1 sentinel
	// must be rejected here rather than silently decoded from its truncated prefix.
	if int64(len(body)) > maxResponseBytes {
		return &transportError{Method: "GET", Path: path, Err: fmt.Errorf("response exceeds %d bytes", maxResponseBytes)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if ce := c.expiredCredentialsError(resp.StatusCode, string(body), http.MethodGet); ce != nil {
			return ce
		}
		return &apiError{StatusCode: resp.StatusCode, Method: "GET", Path: path}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &transportError{Method: "GET", Path: path, Err: fmt.Errorf("decode response: malformed JSON (%d bytes)", len(body))}
	}
	return nil
}
