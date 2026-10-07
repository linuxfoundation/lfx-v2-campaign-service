// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// Ad-set-level monitor-and-optimize (LFXV2-2665): the ad sets under one campaign with their
// status, budget and delivery counters, and a single-ad-set pause/resume.
//
// Every read here is a GET and every 2xx body is passed through identityjson.Check as RAW BYTES
// before it is decoded: a duplicated id, campaign_id or counter would otherwise be resolved
// silently in favour of the last value, and the whole point of these reads is attributing rows to
// one campaign.

const (
	// adSetListPageSize is the /{campaign}/adsets `limit` per page.
	adSetListPageSize = 100
	// adSetListMaxPages bounds the listing at adSetListPageSize*adSetListMaxPages ad sets. A
	// `paging.next` on the last page is an ERROR, never a silently truncated list: a campaign's
	// ad sets presented without some of them reads as the whole campaign.
	adSetListMaxPages = 10
	// adSetInsightsPageSize / adSetInsightsMaxPages bound the level=adset Insights walk the same
	// way.
	adSetInsightsPageSize = 500
	adSetInsightsMaxPages = 20
)

// adSetListFields is the field set the listing asks for. campaign_id and account_id are not
// displayed: they are the per-row proof that the edge answered about THIS campaign in THIS
// account, which the caller checks.
const adSetListFields = "id,name,status,effective_status,daily_budget,lifetime_budget,bid_strategy,campaign_id,account_id"

// adSetStateFields is the field set the pre-write read asks for.
const adSetStateFields = "id,campaign_id,account_id,status"

// ErrAdSetAccountMismatch marks an ad set Meta reports under a different ad account than the one
// the caller is scoped to. GET /{id} and the /{campaign}/adsets edge are NOT account-scoped, so
// this is the read's own provenance proof.
var ErrAdSetAccountMismatch = errors.New("meta: the ad set is reported under a different ad account")

// ValidateAdSetID reports whether id is a canonical Meta ad set id: the campaign id rule (digits,
// no leading zero, at most 32). Nothing is trimmed. It contacts nothing. Its refusal is
// ErrInvalidAdSetID (bid_update.go), the sentinel the bid path already returns for an id it cannot
// address.
func ValidateAdSetID(id string) error {
	if ValidateCampaignID(id) != nil {
		return ErrInvalidAdSetID
	}
	return nil
}

// statusTokenRE is the charset a Meta status / effective_status / bid_strategy value must match
// to be returned verbatim (ACTIVE, CAMPAIGN_PAUSED, LOWEST_COST_WITHOUT_CAP, ...). A value outside
// it is refused rather than echoed: it is upstream content headed for an API response.
var statusTokenRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// maxAdSetNameBytes bounds an ad set name returned verbatim. Meta's own limit is far below it.
const maxAdSetNameBytes = 1024

// AdSetSummary is one ad set under a campaign with its delivery counters over the window.
type AdSetSummary struct {
	ID string
	// Listed is false for an ad set that reported delivery in the window but that the listing
	// did not return (typically deleted or archived upstream since). Only its counters are known;
	// every descriptive field is nil.
	Listed          bool
	Name            *string
	Status          *string
	EffectiveStatus *string
	BidStrategy     *string
	// DailyMinor / LifetimeMinor are the ad set's own budget in the account currency's MINOR
	// units, nil when not reported. At most one is set (Meta documents them as exclusive); both
	// nil is a Campaign Budget Optimization ad set, whose budget lives on the campaign.
	DailyMinor    *int64
	LifetimeMinor *int64
	Impressions   int64
	Clicks        int64
	// CostMicros is spend in micros of the account currency (CampaignAdSets.Currency).
	CostMicros int64
	Ctr        float64
}

// CampaignAdSets is the ad-set read for one campaign.
type CampaignAdSets struct {
	CampaignID string
	Window     MetricsWindow
	// Currency is the ad account's ISO 4217 currency, and every Insights row agreed with it.
	Currency string
	// CurrencyOffset is the minor-unit multiplier for Currency; CurrencyKnown is false when the
	// currency is outside the supported map, in which case a budget cannot be rendered in whole
	// units and the caller must leave it absent rather than guess a scale.
	CurrencyOffset int64
	CurrencyKnown  bool
	AdSets         []AdSetSummary
}

// adSetListWire is one listed ad set. Every field is a string pointer so a decode error can name
// only a JSON kind and this struct's field, never an upstream value.
type adSetListWire struct {
	ID              *string `json:"id"`
	Name            *string `json:"name"`
	Status          *string `json:"status"`
	EffectiveStatus *string `json:"effective_status"`
	DailyBudget     *string `json:"daily_budget"`
	LifetimeBudget  *string `json:"lifetime_budget"`
	BidStrategy     *string `json:"bid_strategy"`
	CampaignID      *string `json:"campaign_id"`
	AccountID       *string `json:"account_id"`
}

// adSetInsightsWire is one level=adset Insights row. The counters are RAW so an ABSENT counter
// (Meta omits zero-valued ones — read as 0, exactly as the campaign metrics read does) is
// distinguishable from an explicit JSON null, which is refused: null is not "zero", it is a
// statement that the value is unknown.
type adSetInsightsWire struct {
	AdSetID         *string         `json:"adset_id"`
	CampaignID      *string         `json:"campaign_id"`
	AccountCurrency *string         `json:"account_currency"`
	Impressions     json.RawMessage `json:"impressions"`
	Clicks          json.RawMessage `json:"clicks"`
	Spend           json.RawMessage `json:"spend"`
}

// pagedWire is the envelope of every paged Graph read here. Data is a POINTER so an absent `data`
// (a malformed 2xx proving nothing) is distinct from Meta's authoritative empty `{"data":[]}`.
type pagedWire struct {
	Data   *[]json.RawMessage `json:"data"`
	Paging struct {
		Cursors struct {
			After string `json:"after"`
		} `json:"cursors"`
		Next string `json:"next"`
	} `json:"paging"`
}

// ListCampaignAdSets reads the ad sets under campaignID with their delivery over window, scoped to
// accountID. Three kinds of request, all GETs:
//
//  1. GET /{campaignID}/adsets?fields=…&limit=100[&after=…] — bounded cursor paging. Every row
//     must carry a canonical id (unique across pages), campaign_id == campaignID and an
//     account_id equal to accountID; a row naming another account is ErrAdSetAccountMismatch,
//     any other defect is an unverifiable error.
//  2. GET /{accountID}?fields=currency — the account currency every amount is denominated in.
//  3. GET /{accountID}/insights?level=adset&filtering=[campaign.id EQUAL campaignID]&date_preset=…
//     — bounded cursor paging. Every row must name this campaign, a canonical ad set id at most
//     once, and the account currency read in (2). An absent counter is 0; an explicit null, a
//     non-string or a malformed one fails the read.
//
// Graph code 100 / subcode 33 on the campaign edge is an ordinary error here (unverifiable), never
// an absence: it cannot tell a deleted campaign from one this token cannot load.
//
// All or nothing: any failure fails the whole read, so a partial list is never presented as the
// campaign's ad sets.
func (c *Client) ListCampaignAdSets(ctx context.Context, campaignID, accountID string, window MetricsWindow) (*CampaignAdSets, error) {
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, fmt.Errorf("list meta ad sets: %w", err)
	}
	if err := ValidateAccountID(accountID); err != nil {
		return nil, fmt.Errorf("list meta ad sets: %w", err)
	}
	w := window
	if w == "" {
		w = defaultMetricsWindow
	}
	preset, ok := datePresetFor[w]
	if !ok {
		return nil, fmt.Errorf("list meta ad sets: unsupported window %q", window)
	}

	listed, err := c.listAdSets(ctx, campaignID, accountID)
	if err != nil {
		return nil, fmt.Errorf("list meta ad sets for campaign %s: %w", campaignID, err)
	}
	currency, err := c.accountCurrency(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("list meta ad sets for campaign %s: %w", campaignID, err)
	}
	counters, err := c.adSetInsights(ctx, campaignID, accountID, preset, currency)
	if err != nil {
		return nil, fmt.Errorf("list meta ad sets for campaign %s: %w", campaignID, err)
	}

	out := &CampaignAdSets{CampaignID: campaignID, Window: w, Currency: currency, AdSets: make([]AdSetSummary, 0, len(listed))}
	out.CurrencyOffset, out.CurrencyKnown = currencyOffsetFor(currency)
	seen := make(map[string]struct{}, len(listed))
	for _, as := range listed {
		if m, ok := counters[as.ID]; ok {
			as.Impressions, as.Clicks, as.CostMicros = m.impressions, m.clicks, m.cost
		}
		as.Ctr = ctrOf(as.Clicks, as.Impressions)
		seen[as.ID] = struct{}{}
		out.AdSets = append(out.AdSets, as)
	}
	// Delivery from an ad set the listing did not return (deleted or archived since) is still this
	// campaign's spend; dropping it would under-report the campaign. It is reported with its
	// counters alone, in a stable order after the listed ones.
	var unlisted []string
	for id := range counters {
		if _, ok := seen[id]; !ok {
			unlisted = append(unlisted, id)
		}
	}
	sort.Strings(unlisted)
	for _, id := range unlisted {
		m := counters[id]
		out.AdSets = append(out.AdSets, AdSetSummary{
			ID: id, Impressions: m.impressions, Clicks: m.clicks, CostMicros: m.cost,
			Ctr: ctrOf(m.clicks, m.impressions),
		})
	}
	return out, nil
}

func ctrOf(clicks, impressions int64) float64 {
	if impressions <= 0 {
		return 0
	}
	return float64(clicks) / float64(impressions)
}

// getChecked performs one GET and returns its 2xx body as raw bytes that identityjson accepted.
// The body is decoded by the caller, never before the check.
func (c *Client) getChecked(ctx context.Context, path string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	if err := identityjson.Check(raw); err != nil {
		return nil, &transportError{Method: http.MethodGet, Path: path, Err: err}
	}
	return raw, nil
}

// decodePage decodes a checked page envelope and requires a present `data` field.
func decodePage(raw json.RawMessage, path string) (*pagedWire, error) {
	var page pagedWire
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, &transportError{Method: http.MethodGet, Path: path, Err: errors.New("the response is not a paged Graph object")}
	}
	if page.Data == nil {
		return nil, &transportError{Method: http.MethodGet, Path: path, Err: errors.New("2xx response with no data field; cannot confirm every row was read")}
	}
	return &page, nil
}

// nextCursor advances a bounded cursor walk: done when there is no `next`, an error when there is
// one but no usable or a repeated cursor.
func nextCursor(page *pagedWire, seen map[string]struct{}) (after string, done bool, err error) {
	if page.Paging.Next == "" {
		return "", true, nil
	}
	after = strings.TrimSpace(page.Paging.Cursors.After)
	if after == "" {
		return "", false, errors.New("more pages are reported but no cursor; cannot guarantee every row was read")
	}
	if _, dup := seen[after]; dup {
		return "", false, errors.New("paging did not terminate (repeated cursor)")
	}
	seen[after] = struct{}{}
	return after, false, nil
}

// listAdSets walks the campaign's /adsets edge.
func (c *Client) listAdSets(ctx context.Context, campaignID, accountID string) ([]AdSetSummary, error) {
	var out []AdSetSummary
	ids := map[string]struct{}{}
	cursors := map[string]struct{}{}
	after := ""
	for page := 0; page < adSetListMaxPages; page++ {
		// Built from the opaque cursor, never from Meta's absolute paging.next URL, which carries
		// the access token as a query parameter and would then reach error text and logs.
		path := "/" + campaignID + "/adsets?fields=" + adSetListFields + "&limit=" + strconv.Itoa(adSetListPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		raw, err := c.getChecked(ctx, path)
		if err != nil {
			return nil, err
		}
		p, err := decodePage(raw, path)
		if err != nil {
			return nil, err
		}
		for i, rowRaw := range *p.Data {
			as, rerr := decodeListedAdSet(rowRaw, campaignID, accountID)
			if rerr != nil {
				if errors.Is(rerr, ErrAdSetAccountMismatch) {
					return nil, fmt.Errorf("page %d row %d: %w", page, i, rerr)
				}
				return nil, &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf("page %d row %d: %w", page, i, rerr)}
			}
			if _, dup := ids[as.ID]; dup {
				return nil, &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf("page %d row %d: the same ad set is listed twice", page, i)}
			}
			ids[as.ID] = struct{}{}
			out = append(out, as)
		}
		next, done, cerr := nextCursor(p, cursors)
		if cerr != nil {
			return nil, fmt.Errorf("ad set listing: %w", cerr)
		}
		if done {
			return out, nil
		}
		after = next
	}
	return nil, fmt.Errorf("ad set listing exceeded %d pages of %d; refusing a partial list", adSetListMaxPages, adSetListPageSize)
}

// decodeListedAdSet validates one listed ad set. Errors never echo an upstream value.
func decodeListedAdSet(raw json.RawMessage, campaignID, accountID string) (AdSetSummary, error) {
	var w adSetListWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return AdSetSummary{}, errors.New("row is not an ad set object of the expected shape")
	}
	if w.ID == nil || ValidateAdSetID(*w.ID) != nil {
		return AdSetSummary{}, errors.New("row has no canonical ad set id")
	}
	if w.CampaignID == nil || *w.CampaignID != campaignID {
		return AdSetSummary{}, errors.New("row names a different campaign than the one whose ad sets were requested")
	}
	if w.AccountID == nil || canonicalAccountID(*w.AccountID) == "" {
		return AdSetSummary{}, errors.New("row has no readable account_id")
	}
	if canonicalAccountID(*w.AccountID) != accountID {
		return AdSetSummary{}, fmt.Errorf("ad set %s: %w", *w.ID, ErrAdSetAccountMismatch)
	}
	for name, v := range map[string]*string{"status": w.Status, "effective_status": w.EffectiveStatus, "bid_strategy": w.BidStrategy} {
		if v != nil && !statusTokenRE.MatchString(*v) {
			return AdSetSummary{}, fmt.Errorf("%s is outside the accepted charset (%d bytes)", name, len(*v))
		}
	}
	if w.Name != nil && len(*w.Name) > maxAdSetNameBytes {
		return AdSetSummary{}, fmt.Errorf("name exceeds %d bytes", maxAdSetNameBytes)
	}
	as := AdSetSummary{
		ID: *w.ID, Listed: true, Name: w.Name, Status: w.Status,
		EffectiveStatus: w.EffectiveStatus, BidStrategy: w.BidStrategy,
	}
	var bad bool
	as.DailyMinor = parseOptionalMinorUnits(w.DailyBudget, &bad)
	as.LifetimeMinor = parseOptionalMinorUnits(w.LifetimeBudget, &bad)
	if bad {
		return AdSetSummary{}, errors.New("a budget amount is not an integer")
	}
	if as.DailyMinor != nil && as.LifetimeMinor != nil {
		return AdSetSummary{}, errors.New("both a daily and a lifetime budget are reported, which Meta documents as mutually exclusive")
	}
	return as, nil
}

// accountCurrency reads the ad account's ISO 4217 currency code.
func (c *Client) accountCurrency(ctx context.Context, accountID string) (string, error) {
	path := "/" + accountID + "?fields=currency"
	raw, err := c.getChecked(ctx, path)
	if err != nil {
		return "", fmt.Errorf("read the ad account's currency: %w", err)
	}
	var acct struct {
		Currency *string `json:"currency"`
	}
	if err := json.Unmarshal(raw, &acct); err != nil || acct.Currency == nil || !currencyRE.MatchString(*acct.Currency) {
		return "", &transportError{Method: http.MethodGet, Path: path, Err: errors.New("the ad account reported no ISO 4217 currency")}
	}
	return *acct.Currency, nil
}

type adSetCounters struct {
	impressions, clicks, cost int64
}

// adSetInsights walks level=adset Insights for the campaign and returns counters per ad set id.
func (c *Client) adSetInsights(ctx context.Context, campaignID, accountID, preset, currency string) (map[string]adSetCounters, error) {
	filter, err := json.Marshal([]map[string]any{{"field": "campaign.id", "operator": "EQUAL", "value": campaignID}})
	if err != nil {
		return nil, fmt.Errorf("encode filtering: %w", err)
	}
	out := map[string]adSetCounters{}
	cursors := map[string]struct{}{}
	after := ""
	for page := 0; page < adSetInsightsMaxPages; page++ {
		// Every interpolated value is a constant, the allow-listed preset, the validated account
		// id, or query-escaped.
		path := "/" + accountID + "/insights?level=adset" +
			"&fields=adset_id,campaign_id,impressions,clicks,spend,account_currency" +
			"&date_preset=" + preset +
			"&filtering=" + url.QueryEscape(string(filter)) +
			"&limit=" + strconv.Itoa(adSetInsightsPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		raw, rerr := c.getChecked(ctx, path)
		if rerr != nil {
			return nil, fmt.Errorf("ad set insights: %w", rerr)
		}
		p, perr := decodePage(raw, path)
		if perr != nil {
			return nil, fmt.Errorf("ad set insights: %w", perr)
		}
		malformed := func(format string, args ...any) error {
			return &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf(format, args...)}
		}
		for i, rowRaw := range *p.Data {
			var row adSetInsightsWire
			if err := json.Unmarshal(rowRaw, &row); err != nil {
				return nil, malformed("page %d row %d: row is not an insights object of the expected shape", page, i)
			}
			if row.CampaignID == nil || *row.CampaignID != campaignID {
				return nil, malformed("page %d row %d: row names a campaign other than the one requested", page, i)
			}
			if row.AdSetID == nil || ValidateAdSetID(*row.AdSetID) != nil {
				return nil, malformed("page %d row %d: row has no canonical adset_id", page, i)
			}
			if row.AccountCurrency == nil || *row.AccountCurrency != currency {
				return nil, malformed("page %d row %d: account_currency disagrees with the ad account's currency", page, i)
			}
			if _, dup := out[*row.AdSetID]; dup {
				// level=adset yields one row per ad set; a repeat would double-count.
				return nil, malformed("page %d row %d: duplicate row for one ad set", page, i)
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
			out[*row.AdSetID] = adSetCounters{impressions: impressions, clicks: clicks, cost: cost}
		}
		next, done, cerr := nextCursor(p, cursors)
		if cerr != nil {
			return nil, fmt.Errorf("ad set insights: %w", cerr)
		}
		if done {
			return out, nil
		}
		after = next
	}
	return nil, fmt.Errorf("ad set insights exceeded %d pages of %d rows; refusing a partial read", adSetInsightsMaxPages, adSetInsightsPageSize)
}

// rawCounter reads one Insights counter: ABSENT is 0 (Meta omits zero-valued fields, as the
// campaign metrics read treats them); an explicit null or any non-string JSON value is refused.
// The error never echoes the value.
func rawCounter(raw json.RawMessage, parse func(string) (int64, error)) (int64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, errors.New("is an explicit null")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("is not a JSON string (%d bytes)", len(raw))
	}
	return parse(s)
}

// AdSetState is what the pre-write read learns about one ad set.
type AdSetState struct {
	ID         string
	CampaignID string
	// AccountID is normalised to "act_<digits>".
	AccountID string
	// Status is the ad set's configured status (ACTIVE, PAUSED, DELETED, ARCHIVED).
	Status string
}

// GetAdSetState reads one ad set's identity and configured status: GET
// /{adSetID}?fields=id,campaign_id,account_id,status. A pure read, so every failure is definite —
// nothing has been written. The id must be echoed, and campaign_id, account_id and status must all
// be present and well-formed; the caller decides whether they are the ones it expects.
func (c *Client) GetAdSetState(ctx context.Context, adSetID string) (*AdSetState, error) {
	if err := ValidateAdSetID(adSetID); err != nil {
		return nil, err
	}
	path := "/" + adSetID + "?fields=" + adSetStateFields
	raw, err := c.getChecked(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read meta ad set %s: %w", adSetID, err)
	}
	var w adSetListWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("read meta ad set %s: the response is not an ad set object", adSetID)
	}
	if w.ID == nil || *w.ID != adSetID {
		return nil, fmt.Errorf("read meta ad set %s: the response does not describe the requested ad set", adSetID)
	}
	if w.CampaignID == nil || ValidateCampaignID(*w.CampaignID) != nil {
		return nil, fmt.Errorf("read meta ad set %s: the response carries no canonical campaign_id", adSetID)
	}
	account := ""
	if w.AccountID != nil {
		account = canonicalAccountID(*w.AccountID)
	}
	if account == "" {
		return nil, fmt.Errorf("read meta ad set %s: the response carries no readable account_id", adSetID)
	}
	if w.Status == nil || !statusTokenRE.MatchString(*w.Status) {
		return nil, fmt.Errorf("read meta ad set %s: the response carries no readable status", adSetID)
	}
	return &AdSetState{ID: adSetID, CampaignID: *w.CampaignID, AccountID: account, Status: *w.Status}, nil
}

// AdSetWriteOutcome classifies a single ad-set status write.
type AdSetWriteOutcome int

// Ad-set write outcomes.
const (
	// AdSetWriteApplied — Meta answered 2xx with success:true.
	AdSetWriteApplied AdSetWriteOutcome = iota
	// AdSetWriteNotSent — the request never left this process (the context was already done, a
	// pre-connect dial failure, or the request could not be built). Definitely not applied.
	AdSetWriteNotSent
	// AdSetWriteRejected — Meta answered a definite refusal (a 4xx that is not a throttle).
	// Definitely not applied.
	AdSetWriteRejected
	// AdSetWriteUnconfirmed — the request may have been applied: a transport failure after the
	// connection was made, a 5xx, a 3xx, a throttle (429 or a rate-limit code), an unreadable
	// error envelope, or a 2xx whose body does not confirm success.
	AdSetWriteUnconfirmed
)

// ClassifyAdSetWrite maps UpdateAdSetStatusOnce's error onto its outcome. nil is APPLIED.
//
// REJECTED ("nothing was changed") is OPT-IN, because telling an operator a write did not land
// when it may have is the dangerous direction. It is answered only when Meta's own Graph error
// envelope was read and says the refusal is definite: not a throttle (429 or a rate-limit code —
// IsOutcomeUnconfirmed), not `is_transient`, not Graph's generic code 1 ("unknown error") or 2
// ("service temporarily unavailable"), and not an HTTP 408. Every other *APIError — an HTML body,
// an unread envelope, a 5xx/3xx — is UNCONFIRMED.
func ClassifyAdSetWrite(err error) AdSetWriteOutcome {
	if err == nil {
		return AdSetWriteApplied
	}
	if IsOutcomeUnconfirmed(err) {
		return AdSetWriteUnconfirmed
	}
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.EnvelopeParsed && !ae.IsTransient && ae.Code != 1 && ae.Code != 2 &&
			ae.StatusCode != http.StatusRequestTimeout && ae.StatusCode >= 400 && ae.StatusCode < 500 {
			return AdSetWriteRejected
		}
		return AdSetWriteUnconfirmed
	}
	// Every other error do() returns is produced before a request is sent: the entry-time
	// context check, a missing token, a body encode or request build failure, or a pre-connect
	// dial error. (A failure after send is a transportError — unconfirmed above.)
	return AdSetWriteNotSent
}

// UpdateAdSetStatusOnce sets an ad set's status to ACTIVE or PAUSED with ONE POST /{adSetID}
// {"status": …}. It is sent EXACTLY ONCE: the throttle retry doRequest applies to declarative
// updates is suppressed (do with retryThrottle=false), so a 429 — which may follow a committed
// write — is reported as unconfirmed instead of being repeated. Classify the error with
// ClassifyAdSetWrite; this function never decides that a failure was definite.
func (c *Client) UpdateAdSetStatusOnce(ctx context.Context, adSetID, status string) error {
	if err := ValidateAdSetID(adSetID); err != nil {
		return err
	}
	if status != StatusActive && status != StatusPaused {
		return fmt.Errorf("meta: status must be %q or %q", StatusActive, StatusPaused)
	}
	path := "/" + adSetID
	var resp struct {
		Success *bool `json:"success"`
	}
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"status": status}, &resp, false); err != nil {
		return fmt.Errorf("meta: update ad set %s status to %s: %w", adSetID, status, err)
	}
	if resp.Success == nil || !*resp.Success {
		// Meta answered 2xx without confirming. It received the request, so the outcome is
		// unknown — never reported as success, never as "nothing changed".
		return &transportError{Method: http.MethodPost, Path: path, Err: errors.New("2xx response did not confirm success")}
	}
	return nil
}
