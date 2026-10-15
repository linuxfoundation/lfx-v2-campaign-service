// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
)

// Ad-level read under ONE ad set (decision D7, first step): the ads of an ad set with their
// status, creative and delivery counters. STRICTLY READ-ONLY — a pause/resume per ad is a later
// decision.
//
// It mirrors the ad-set read (ad_sets.go): every 2xx body passes identityjson.Check as RAW BYTES
// before it is decoded (getChecked), paging follows the opaque `after` cursor and never Meta's
// token-bearing paging.next URL, and every row is proven to be THIS ad set's, in THIS campaign,
// under THIS account before it is returned.
//
// One deliberate difference: the ad LISTING is capped and reports `Truncated` rather than failing
// past the cap (Meta itself caps an ad set's ads far below it, so a truncated answer is not
// expected in practice). The Insights walk stays all or nothing, so spend is never under-reported.

const (
	// adListPageSize is the /{ad_set}/ads `limit` per page.
	adListPageSize = 100
	// adListMaxPages bounds the listing at adListPageSize*adListMaxPages ads. A `paging.next` on
	// the last allowed page ends the walk with AdSetAds.Truncated set.
	adListMaxPages = 5
	// adInsightsPageSize / adInsightsMaxPages bound the level=ad Insights walk; past the bound the
	// read FAILS (a partial delivery read would under-report spend).
	adInsightsPageSize = 500
	adInsightsMaxPages = 20
)

// adListFields is the field set the ad listing asks for. adset_id, campaign_id and account_id are
// not displayed: they are the per-row proof that the edge answered about THIS ad set of THIS
// campaign in THIS account, which ListAdSetAds checks. creative{id,name} is a field expansion on
// the same request — no extra call.
const adListFields = "id,name,status,effective_status,creative{id,name},adset_id,campaign_id,account_id"

// adInsightsFields is the level=ad Insights field set.
const adInsightsFields = "ad_id,adset_id,campaign_id,impressions,clicks,spend,account_currency"

// ErrAdAccountMismatch marks an ad Meta reports under a different ad account than the one the
// caller is scoped to. The /{ad_set}/ads edge is not account-scoped, so this is the read's own
// provenance proof.
var ErrAdAccountMismatch = errors.New("meta: the ad is reported under a different ad account")

// validateGraphObjectID reports whether id is a canonical Meta Graph object id (an ad or creative
// id): the same digit rule as a campaign id, which ValidateCampaignID implements.
func validateGraphObjectID(id string) error { return ValidateCampaignID(id) }

// maxAdNameBytes bounds an ad or creative name returned verbatim. Meta's own limit is far below it.
const maxAdNameBytes = 1024

// AdSummary is one ad under an ad set with its delivery counters over the window.
type AdSummary struct {
	ID string
	// Listed is false for an ad that reported delivery in the window but that the listing did not
	// return (typically deleted or archived since, or beyond the listing cap when the listing was
	// truncated). Only its counters are known; every descriptive field is nil.
	Listed          bool
	Name            *string
	Status          *string
	EffectiveStatus *string
	CreativeID      *string
	CreativeName    *string
	Impressions     int64
	Clicks          int64
	// CostMicros is spend in micros of the account currency (AdSetAds.Currency).
	CostMicros int64
	Ctr        float64
}

// AdSetAds is the ad read for one ad set.
type AdSetAds struct {
	AdSetID    string
	CampaignID string
	Window     MetricsWindow
	// Currency is the ad account's ISO 4217 currency, and every Insights row agreed with it.
	Currency string
	// Truncated is true when the listing reported more pages than adListMaxPages allows: Ads holds
	// the listed ads that were read, plus any unlisted ad that delivered.
	Truncated bool
	Ads       []AdSummary
}

// adListWire is one listed ad. Every field is a pointer so a decode error can name only a JSON
// kind and this struct's field, never an upstream value.
type adListWire struct {
	ID              *string `json:"id"`
	Name            *string `json:"name"`
	Status          *string `json:"status"`
	EffectiveStatus *string `json:"effective_status"`
	Creative        *struct {
		ID   *string `json:"id"`
		Name *string `json:"name"`
	} `json:"creative"`
	AdSetID    *string `json:"adset_id"`
	CampaignID *string `json:"campaign_id"`
	AccountID  *string `json:"account_id"`
}

// adInsightsWire is one level=ad Insights row; counters are RAW for rawCounter's absent-vs-null
// rule.
type adInsightsWire struct {
	AdID            *string         `json:"ad_id"`
	AdSetID         *string         `json:"adset_id"`
	CampaignID      *string         `json:"campaign_id"`
	AccountCurrency *string         `json:"account_currency"`
	Impressions     json.RawMessage `json:"impressions"`
	Clicks          json.RawMessage `json:"clicks"`
	Spend           json.RawMessage `json:"spend"`
}

// ListAdSetAds reads the ads under adSetID — which the caller has already proven belongs to
// campaignID under accountID (GetAdSetState) — with their delivery over window. Three kinds of
// request, all GETs:
//
//  1. GET /{adSetID}/ads?fields=…&limit=100[&after=…] — bounded cursor paging (Truncated past the
//     cap). Every row must carry a canonical id (unique across pages), adset_id == adSetID,
//     campaign_id == campaignID and an account_id equal to accountID; a row naming another
//     account is ErrAdAccountMismatch, any other defect is an unverifiable error.
//  2. GET /{accountID}?fields=currency — the account currency spend is denominated in.
//  3. GET /{accountID}/insights?level=ad&filtering=[adset.id EQUAL adSetID]&date_preset=… —
//     bounded cursor paging, all or nothing. Every row must name this ad set and campaign, a
//     canonical ad id at most once, and the account currency read in (2).
//
// Graph code 100 / subcode 33 is an ordinary error here (unverifiable), never an absence.
func (c *Client) ListAdSetAds(ctx context.Context, adSetID, campaignID, accountID string, window MetricsWindow) (*AdSetAds, error) {
	if err := ValidateAdSetID(adSetID); err != nil {
		return nil, fmt.Errorf("list meta ads: %w", err)
	}
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, fmt.Errorf("list meta ads: %w", err)
	}
	if err := ValidateAccountID(accountID); err != nil {
		return nil, fmt.Errorf("list meta ads: %w", err)
	}
	w := window
	if w == "" {
		w = defaultMetricsWindow
	}
	preset, ok := datePresetFor[w]
	if !ok {
		return nil, fmt.Errorf("list meta ads: unsupported window %q", window)
	}

	listed, truncated, err := c.listAds(ctx, adSetID, campaignID, accountID)
	if err != nil {
		return nil, fmt.Errorf("list meta ads for ad set %s: %w", adSetID, err)
	}
	currency, err := c.accountCurrency(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("list meta ads for ad set %s: %w", adSetID, err)
	}
	counters, err := c.adInsights(ctx, adSetID, campaignID, accountID, preset, currency)
	if err != nil {
		return nil, fmt.Errorf("list meta ads for ad set %s: %w", adSetID, err)
	}

	out := &AdSetAds{AdSetID: adSetID, CampaignID: campaignID, Window: w, Currency: currency, Truncated: truncated,
		Ads: make([]AdSummary, 0, len(listed))}
	seen := make(map[string]struct{}, len(listed))
	for _, ad := range listed {
		if m, ok := counters[ad.ID]; ok {
			ad.Impressions, ad.Clicks, ad.CostMicros = m.impressions, m.clicks, m.cost
		}
		ad.Ctr = ctrOf(ad.Clicks, ad.Impressions)
		seen[ad.ID] = struct{}{}
		out.Ads = append(out.Ads, ad)
	}
	// Delivery from an ad the listing did not return is still this ad set's spend: reported with
	// its counters alone, in a stable order after the listed ones.
	var unlisted []string
	for id := range counters {
		if _, ok := seen[id]; !ok {
			unlisted = append(unlisted, id)
		}
	}
	sort.Strings(unlisted)
	for _, id := range unlisted {
		m := counters[id]
		out.Ads = append(out.Ads, AdSummary{
			ID: id, Impressions: m.impressions, Clicks: m.clicks, CostMicros: m.cost,
			Ctr: ctrOf(m.clicks, m.impressions),
		})
	}
	return out, nil
}

// listAds walks the ad set's /ads edge. truncated is true when the last allowed page still
// reported more.
func (c *Client) listAds(ctx context.Context, adSetID, campaignID, accountID string) (ads []AdSummary, truncated bool, err error) {
	ids := map[string]struct{}{}
	cursors := map[string]struct{}{}
	after := ""
	for page := 0; page < adListMaxPages; page++ {
		// Built from the opaque cursor, never from Meta's absolute paging.next URL, which carries
		// the access token as a query parameter.
		path := "/" + adSetID + "/ads?fields=" + url.QueryEscape(adListFields) + "&limit=" + strconv.Itoa(adListPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		raw, gerr := c.getChecked(ctx, path)
		if gerr != nil {
			return nil, false, gerr
		}
		p, perr := decodePage(raw, path)
		if perr != nil {
			return nil, false, perr
		}
		for i, rowRaw := range *p.Data {
			ad, rerr := decodeListedAd(rowRaw, adSetID, campaignID, accountID)
			if rerr != nil {
				if errors.Is(rerr, ErrAdAccountMismatch) {
					return nil, false, fmt.Errorf("page %d row %d: %w", page, i, rerr)
				}
				return nil, false, &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf("page %d row %d: %w", page, i, rerr)}
			}
			if _, dup := ids[ad.ID]; dup {
				return nil, false, &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf("page %d row %d: the same ad is listed twice", page, i)}
			}
			ids[ad.ID] = struct{}{}
			ads = append(ads, ad)
		}
		next, done, cerr := nextCursor(p, cursors)
		if cerr != nil {
			return nil, false, fmt.Errorf("ad listing: %w", cerr)
		}
		if done {
			return ads, false, nil
		}
		after = next
	}
	// The cap: the last allowed page still reported more. Report what was read and say so.
	return ads, true, nil
}

// decodeListedAd validates one listed ad. Errors never echo an upstream value.
func decodeListedAd(raw json.RawMessage, adSetID, campaignID, accountID string) (AdSummary, error) {
	var w adListWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return AdSummary{}, errors.New("row is not an ad object of the expected shape")
	}
	if w.ID == nil || validateGraphObjectID(*w.ID) != nil {
		return AdSummary{}, errors.New("row has no canonical ad id")
	}
	if w.AdSetID == nil || *w.AdSetID != adSetID {
		return AdSummary{}, errors.New("row names a different ad set than the one whose ads were requested")
	}
	if w.CampaignID == nil || *w.CampaignID != campaignID {
		return AdSummary{}, errors.New("row names a different campaign than the ad set's")
	}
	if w.AccountID == nil || canonicalAccountID(*w.AccountID) == "" {
		return AdSummary{}, errors.New("row has no readable account_id")
	}
	if canonicalAccountID(*w.AccountID) != accountID {
		return AdSummary{}, fmt.Errorf("ad %s: %w", *w.ID, ErrAdAccountMismatch)
	}
	for name, v := range map[string]*string{"status": w.Status, "effective_status": w.EffectiveStatus} {
		if v != nil && !statusTokenRE.MatchString(*v) {
			return AdSummary{}, fmt.Errorf("%s is outside the accepted charset (%d bytes)", name, len(*v))
		}
	}
	if w.Name != nil && len(*w.Name) > maxAdNameBytes {
		return AdSummary{}, fmt.Errorf("name exceeds %d bytes", maxAdNameBytes)
	}
	ad := AdSummary{ID: *w.ID, Listed: true, Name: w.Name, Status: w.Status, EffectiveStatus: w.EffectiveStatus}
	if w.Creative != nil {
		if w.Creative.ID == nil || validateGraphObjectID(*w.Creative.ID) != nil {
			return AdSummary{}, errors.New("creative has no canonical id")
		}
		if w.Creative.Name != nil && len(*w.Creative.Name) > maxAdNameBytes {
			return AdSummary{}, fmt.Errorf("creative name exceeds %d bytes", maxAdNameBytes)
		}
		ad.CreativeID, ad.CreativeName = w.Creative.ID, w.Creative.Name
	}
	return ad, nil
}

// adInsights walks level=ad Insights for the ad set and returns counters per ad id.
func (c *Client) adInsights(ctx context.Context, adSetID, campaignID, accountID, preset, currency string) (map[string]adSetCounters, error) {
	filter, err := json.Marshal([]map[string]any{{"field": "adset.id", "operator": "EQUAL", "value": adSetID}})
	if err != nil {
		return nil, fmt.Errorf("encode filtering: %w", err)
	}
	out := map[string]adSetCounters{}
	cursors := map[string]struct{}{}
	after := ""
	for page := 0; page < adInsightsMaxPages; page++ {
		path := "/" + accountID + "/insights?level=ad" +
			"&fields=" + adInsightsFields +
			"&date_preset=" + preset +
			"&filtering=" + url.QueryEscape(string(filter)) +
			"&limit=" + strconv.Itoa(adInsightsPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		raw, rerr := c.getChecked(ctx, path)
		if rerr != nil {
			return nil, fmt.Errorf("ad insights: %w", rerr)
		}
		p, perr := decodePage(raw, path)
		if perr != nil {
			return nil, fmt.Errorf("ad insights: %w", perr)
		}
		malformed := func(format string, args ...any) error {
			return &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf(format, args...)}
		}
		for i, rowRaw := range *p.Data {
			var row adInsightsWire
			if err := json.Unmarshal(rowRaw, &row); err != nil {
				return nil, malformed("page %d row %d: row is not an insights object of the expected shape", page, i)
			}
			if row.AdSetID == nil || *row.AdSetID != adSetID {
				return nil, malformed("page %d row %d: row names an ad set other than the one requested", page, i)
			}
			if row.CampaignID == nil || *row.CampaignID != campaignID {
				return nil, malformed("page %d row %d: row names a campaign other than the ad set's", page, i)
			}
			if row.AdID == nil || validateGraphObjectID(*row.AdID) != nil {
				return nil, malformed("page %d row %d: row has no canonical ad_id", page, i)
			}
			if row.AccountCurrency == nil || *row.AccountCurrency != currency {
				return nil, malformed("page %d row %d: account_currency disagrees with the ad account's currency", page, i)
			}
			if _, dup := out[*row.AdID]; dup {
				// level=ad yields one row per ad; a repeat would double-count.
				return nil, malformed("page %d row %d: duplicate row for one ad", page, i)
			}
			impressions, errI := rawCounter(row.Impressions, parseMetricInt)
			clicks, errC := rawCounter(row.Clicks, parseMetricInt)
			if errI != nil || errC != nil {
				return nil, malformed("page %d row %d: %s", page, i, malformedCounterSummary(errI, errC))
			}
			cost, errS := rawCounter(row.Spend, parseSpendMicros)
			if errS != nil {
				return nil, malformed("page %d row %d: spend %v", page, i, errS)
			}
			out[*row.AdID] = adSetCounters{impressions: impressions, clicks: clicks, cost: cost}
		}
		next, done, cerr := nextCursor(p, cursors)
		if cerr != nil {
			return nil, fmt.Errorf("ad insights: %w", cerr)
		}
		if done {
			return out, nil
		}
		after = next
	}
	return nil, fmt.Errorf("ad insights exceeded %d pages of %d rows; refusing a partial read", adInsightsMaxPages, adInsightsPageSize)
}
