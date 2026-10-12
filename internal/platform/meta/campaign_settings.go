// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// ErrAdSetNotInCampaign reports that the ad set the campaign row recorded is, upstream, an ad set
// of a DIFFERENT campaign. Reading its budget and flight as this campaign's would report another
// campaign's configuration as this one's divergence, so the dispatcher refuses it — the same
// refusal the budget write makes for the same fact.
var ErrAdSetNotInCampaign = errors.New("meta: the recorded ad set belongs to a different campaign")

// settingsCampaignFields is the campaign field set the settings readback asks for. As on the
// adoption read, the campaign-only fields (objective, daily_budget, lifetime_budget,
// bid_strategy) are load-bearing: they make Graph refuse a node that is not a campaign, so an ad
// set or ad id stored as a campaign id cannot be read back as one. The campaign-level budget
// itself is NOT interpreted: the create path puts the budget on the ad set, and a campaign that
// holds one (Campaign Budget Optimization) shares it across ad sets, so it is not the recorded
// ad-set budget's counterpart — the readback reports the ad-set budget `unknown` instead.
const settingsCampaignFields = "id,name,status,account_id,objective,daily_budget,lifetime_budget,bid_strategy"

// settingsAdSetFields is the ad set field set the readback asks for: the budget, flight and bid
// strategy the create path sets ON THE AD SET, plus campaign_id to prove which campaign it is in.
const settingsAdSetFields = "id,campaign_id,daily_budget,lifetime_budget,start_time,end_time,bid_strategy"

// CampaignSettings is one campaign's live configuration as the settings readback reads it, from
// the campaign node and — when the row recorded one — the ad set the create path made under it.
//
// Every optional field is a POINTER and nil means "Meta did not report it", never zero. Amounts
// are in the ACCOUNT CURRENCY'S MINOR UNITS, as Meta reports them.
type CampaignSettings struct {
	// CampaignID is the id Meta ECHOED, already checked against the requested one.
	CampaignID string
	Name       *string
	// Status is the campaign's configured status (ACTIVE, PAUSED, DELETED, ARCHIVED, ...).
	Status *string
	// AccountID is the account Meta reports the campaign under, normalised to "act_<digits>".
	// GET /{id} is NOT account-scoped, so this is the only provenance proof the read carries and
	// the dispatcher compares it with the row's recorded account.
	AccountID string
	// AdSet is the recorded ad set's configuration; nil when no ad set id was supplied, or when
	// Meta reports the ad set does not exist (code 100 / subcode 33).
	AdSet *AdSetSettings
}

// AdSetSettings is the configuration the create path writes onto the ad set.
type AdSetSettings struct {
	ID            string
	DailyMinor    *int64
	LifetimeMinor *int64
	// StartTime / EndTime are raw Graph timestamps ("2026-08-01T00:00:00+0000"), parsed by the
	// caller against the documented layout.
	StartTime   *string
	EndTime     *string
	BidStrategy *string
}

// campaignSettingsWire / adSetSettingsWire are string-only decodes, so a decode error can only
// name a JSON kind and this struct's field, never an upstream value.
type campaignSettingsWire struct {
	ID        *string `json:"id"`
	Name      *string `json:"name"`
	Status    *string `json:"status"`
	AccountID *string `json:"account_id"`
}

type adSetSettingsWire struct {
	ID             *string `json:"id"`
	CampaignID     *string `json:"campaign_id"`
	DailyBudget    *string `json:"daily_budget"`
	LifetimeBudget *string `json:"lifetime_budget"`
	StartTime      *string `json:"start_time"`
	EndTime        *string `json:"end_time"`
	BidStrategy    *string `json:"bid_strategy"`
}

// GetCampaignSettings reads the campaign node and, when adSetID is non-empty, that ad set. Both
// are PURE READS (GETs retried on throttle by doRequest), so every failure is definite.
//
//   - Graph code 100 / subcode 33 on the CAMPAIGN → an ERROR (unverifiable, 503), on EVERY HTTP
//     status, exactly as the adoption read (GetCampaign) treats it. Meta documents 100/33 as
//     "does not exist, cannot be loaded due to missing permissions, or does not support this
//     operation", and no further read resolves which: GET /{id} is not account-scoped, and an
//     account that loads says nothing about whether THIS campaign is hidden from the token or
//     refused for the operation. Reporting it absent would be the false 404 the readback must
//     not produce, so this read never returns (nil, nil): Meta's readback never answers 404
//     from the platform. A campaign Meta has deleted or archived still answers 200 with that
//     status (below), which is the only "gone" this read can prove.
//   - code 100 / 33 on the AD SET → the campaign is returned with AdSet nil: the campaign exists,
//     only the recorded child is gone, so the ad-set fields are absent rather than the whole read
//     failing or the campaign being reported absent.
//   - an ad set whose campaign_id is not this campaign → ErrAdSetNotInCampaign.
//   - everything else unusable is an error: a transport failure, a 5xx, an exhausted throttle, a
//     401/403 or any other Graph error, a body identityjson refuses (a duplicated id, name or
//     amount), an id that is not the one asked for, a missing or malformed account_id, a budget
//     amount that is not an integer, and an ad set reporting BOTH a daily and a lifetime budget —
//     mutually exclusive on Meta, so a body carrying both is not one this client may half-read.
//
// Unlike the adoption read, a DELETED or ARCHIVED campaign is NOT an absence here: the readback
// reports what the platform holds, and "this campaign is archived" is exactly the observation an
// operator opening the readback needs. Its status is returned verbatim.
func (c *Client) GetCampaignSettings(ctx context.Context, campaignID, adSetID string) (*CampaignSettings, error) {
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, err
	}
	adSetID = strings.TrimSpace(adSetID)
	if adSetID != "" && !numericIDRE.MatchString(adSetID) {
		return nil, fmt.Errorf("meta campaign settings read for %s: the recorded ad set id is not numeric", campaignID)
	}

	var raw json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, "/"+campaignID+"?fields="+settingsCampaignFields, nil, &raw); err != nil {
		// Every Graph error — 100/33 included, see above — is unverifiable: none proves absence.
		return nil, fmt.Errorf("meta campaign settings read for %s: %w", campaignID, err)
	}
	if err := identityjson.Check(raw); err != nil {
		return nil, fmt.Errorf("meta campaign settings read for %s: %w", campaignID, err)
	}
	var wire campaignSettingsWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("meta campaign settings read for %s: the response is not a campaign object", campaignID)
	}
	if wire.ID == nil || *wire.ID != campaignID {
		return nil, fmt.Errorf("meta campaign settings read for %s: the response does not describe the requested campaign, so nothing in it can be trusted", campaignID)
	}
	account := ""
	if wire.AccountID != nil {
		account = canonicalAccountID(*wire.AccountID)
	}
	if account == "" {
		return nil, fmt.Errorf("meta campaign settings read for %s: the campaign was returned without a readable account_id, so which ad account it belongs to cannot be established", campaignID)
	}
	out := &CampaignSettings{CampaignID: campaignID, Name: wire.Name, Status: wire.Status, AccountID: account}
	if adSetID == "" {
		return out, nil
	}

	var adRaw json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, "/"+adSetID+"?fields="+settingsAdSetFields, nil, &adRaw); err != nil {
		if graphObjectMissing(err) {
			return out, nil
		}
		return nil, fmt.Errorf("meta campaign settings read for %s: ad set %s: %w", campaignID, adSetID, err)
	}
	if err := identityjson.Check(adRaw); err != nil {
		return nil, fmt.Errorf("meta campaign settings read for %s: ad set %s: %w", campaignID, adSetID, err)
	}
	var ad adSetSettingsWire
	if err := json.Unmarshal(adRaw, &ad); err != nil {
		return nil, fmt.Errorf("meta campaign settings read for %s: ad set %s: the response is not an ad set object", campaignID, adSetID)
	}
	if ad.ID == nil || *ad.ID != adSetID {
		return nil, fmt.Errorf("meta campaign settings read for %s: the ad set response does not describe ad set %s, so nothing in it can be trusted", campaignID, adSetID)
	}
	if ad.CampaignID == nil || *ad.CampaignID != campaignID {
		return nil, fmt.Errorf("meta campaign settings read for %s: ad set %s: %w", campaignID, adSetID, ErrAdSetNotInCampaign)
	}
	as := &AdSetSettings{ID: adSetID, StartTime: ad.StartTime, EndTime: ad.EndTime, BidStrategy: ad.BidStrategy}
	var bad bool
	as.DailyMinor = parseOptionalMinorUnits(ad.DailyBudget, &bad)
	as.LifetimeMinor = parseOptionalMinorUnits(ad.LifetimeBudget, &bad)
	if bad {
		return nil, fmt.Errorf("meta campaign settings read for %s: ad set %s reported a budget amount that is not an integer", campaignID, adSetID)
	}
	if as.DailyMinor != nil && as.LifetimeMinor != nil {
		return nil, fmt.Errorf("meta campaign settings read for %s: ad set %s reports both a daily and a lifetime budget, which Meta documents as mutually exclusive", campaignID, adSetID)
	}
	out.AdSet = as
	return out, nil
}

// AccountCurrencyOffset reads the ad account's currency and returns its minor-unit offset (100
// for most currencies, 1 for zero-decimal ones such as JPY). It is the read half of
// ResolveBudgetMinorUnits, for a caller that must turn a REPORTED minor-unit amount back into
// whole units rather than encode a requested one.
//
// known is false when the account reports a currency this service's supported-currency map does
// not carry: the scale is then genuinely unknown, and the caller must treat the amount as
// unreadable rather than guess 100. A failed preflight is an ERROR — it may be transient, and it
// says nothing about the currency.
func (c *Client) AccountCurrencyOffset(ctx context.Context) (offset int64, known bool, err error) {
	accountID := strings.TrimSpace(c.account.AccountID)
	if accountID == "" {
		return 0, false, fmt.Errorf("meta: an ad account must be selected to read its currency")
	}
	var acct accountPreflight
	if err := c.doRequest(ctx, http.MethodGet, "/"+accountID+"?fields=currency", nil, &acct); err != nil {
		return 0, false, fmt.Errorf("meta: read the ad account's currency: %w", err)
	}
	off, ok := currencyOffsetFor(acct.Currency)
	return off, ok, nil
}

// graphCodeInvalidParameter + graphSubcodeObjectMissing is Graph's structured "this node does not
// exist, cannot be loaded with this token, or does not support this operation" answer (code 100,
// error_subcode 33). It does NOT prove absence — see GetCampaignSettings — so it is read only on
// the recorded AD SET, where it leaves the ad-set fields absent (`unknown`) rather than claiming
// anything about the campaign.
const (
	graphCodeInvalidParameter = 100
	graphSubcodeObjectMissing = 33
)

// graphObjectMissing reports Graph's structured "this node does not exist or cannot be loaded"
// answer (code 100 / error_subcode 33) on a 4xx. Only the recorded ad set's read consults it.
func graphObjectMissing(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode >= 400 && ae.StatusCode < 500 &&
		ae.Code == graphCodeInvalidParameter && ae.ErrorSubcode == graphSubcodeObjectMissing
}

// parseOptionalMinorUnits is parseMinorUnits for a field decoded as *string, so an absent field
// (nil) and an empty one both read as "not reported".
func parseOptionalMinorUnits(raw *string, unparseable *bool) *int64 {
	if raw == nil {
		return nil
	}
	return parseMinorUnits(*raw, unparseable)
}
