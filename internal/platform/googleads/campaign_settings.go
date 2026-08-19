// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Campaign settings readback: what the platform CURRENTLY holds for a campaign's
// configuration, as opposed to what a dispatch asked for.
//
// This is the read GetCampaignMetrics cannot be: metrics report impressions, clicks,
// cost and CTR, none of which describe a campaign's CONFIGURATION, so no reading of
// them can tell an operator that the budget upstream is not the budget the campaign
// row records. The two can legitimately disagree — adoption binds a campaign this
// service never created, and nothing here or anywhere else pushes the recorded config
// upstream — so the disagreement needs a way to be SEEN.
//
// Strictly read-only. Every call this file makes is a GAQL search (googleAds:search,
// a POST that reads); nothing in this file mutates, and nothing about a readback may
// ever be written back onto the campaign row. The row means "what this dispatch asked
// for"; overwriting it with an observation would destroy the only record of the
// request and make a transient bad read permanent.
// ---------------------------------------------------------------------------

// CampaignSettings is the campaign configuration Google Ads currently holds.
//
// EVERY field is a pointer, and that is the load-bearing decision in this type. A
// setting Google did not return is ABSENT (nil), never a zero value: a campaign whose
// budget could not be read and a campaign with a genuinely zero budget mean opposite
// things to an operator deciding whether to intervene, and a `0` that stands in for
// "unread" is indistinguishable from a measurement. The same reasoning the brief-wide
// metrics read applies to whole rows — a non-ok row omits `metrics` entirely rather
// than carrying zeroes — applies here per FIELD, because a settings read is partial in
// a way a metrics read is not: campaign_budget is a separate resource joined onto the
// campaign, so its fields can be missing while the campaign's own fields are present.
type CampaignSettings struct {
	// CampaignID is the id Google echoed back, not the one requested. A response that
	// answers about a different campaign is refused before it reaches this struct.
	CampaignID string
	// Name is the campaign name as Google currently holds it.
	Name *string
	// Status is Google's own ENABLED/PAUSED/REMOVED vocabulary, carried verbatim rather
	// than mapped into this service's lifecycle statuses. The two are different axes —
	// see model.PlatformCampaignRef — and a caller that cannot interpret a value must be
	// able to tell "Google said something we do not handle" from "Google said nothing".
	Status *string
	// BudgetAmountMicros is campaign_budget.amount_micros: the DAILY spend amount, in
	// micros of the ad account's own currency. Google Ads REST renders int64 fields as
	// JSON strings (the same encoding gaqlMetricsRow decodes, and the reason every id
	// field in this package is a string); this is the parsed value.
	//
	// It is nil when the budget carries a CUSTOM_PERIOD total instead (the two are
	// mutually exclusive in the API), and nil when the budget could not be read at all.
	// BudgetPeriod is what distinguishes those two cases.
	BudgetAmountMicros *int64
	// BudgetTotalAmountMicros is campaign_budget.total_amount_micros: the whole-flight
	// spend cap used when BudgetPeriod is CUSTOM_PERIOD. Mutually exclusive with
	// BudgetAmountMicros — Google populates one or the other, never both.
	BudgetTotalAmountMicros *int64
	// BudgetPeriod is campaign_budget.period: DAILY or CUSTOM_PERIOD.
	//
	// Google's vocabulary has exactly those two real values (plus UNKNOWN/UNSPECIFIED),
	// verified against the v23 BudgetPeriodEnum proto. There is NO `LIFETIME` value,
	// which matters because this service's own model.BudgetType spells the same idea
	// "lifetime" — the two vocabularies are NOT the same and are deliberately not
	// translated here. The dispatcher maps them, where the mapping can be stated once.
	BudgetPeriod *string
	// BudgetDeliveryMethod is campaign_budget.delivery_method: STANDARD or ACCELERATED.
	BudgetDeliveryMethod *string
	// BudgetExplicitlyShared is campaign_budget.explicitly_shared: whether this budget is
	// shared across campaigns. A shared budget is why an upstream amount can differ from
	// what one campaign asked for without anybody having edited that campaign.
	BudgetExplicitlyShared *bool
	// AdvertisingChannelType is campaign.advertising_channel_type (SEARCH, DEMAND_GEN, ...).
	AdvertisingChannelType *string
	// BiddingStrategyType is campaign.bidding_strategy_type. OUTPUT_ONLY upstream, which
	// is precisely why it belongs in a readback: it is a setting no request can state.
	BiddingStrategyType *string
	// StartDateTime and EndDateTime are campaign.start_date_time / campaign.end_date_time,
	// carried as the raw strings Google returns in the ad ACCOUNT's timezone.
	//
	// The FIELD NAMES matter and are not the request-side ones. In Google Ads API v23 —
	// the version this client pins — `campaign.start_date` and `campaign.end_date` were
	// REPLACED by these; the old names are rejected as unrecognized fields, so a query
	// written from the request-side vocabulary fails outright rather than returning a
	// wrong value. Confirmed against Google's v23 release notes and the v23 campaign.proto.
	//
	// Format is 'yyyy-MM-dd HH:mm:ss', NOT the YYYY-MM-DD this service's own config dates
	// use, so they are not compared as strings anywhere. Carried verbatim rather than
	// parsed: a timezone this client does not know makes any instant it computed a guess.
	StartDateTime *string
	EndDateTime   *string
}

// gaqlSettingsRow is one row of the settings SELECT.
//
// Every scalar is a *string even where the value is logically a number or a bool,
// because absence must survive decoding. Google Ads REST omits unset fields entirely
// and renders int64 as a JSON string; decoding an absent `amountMicros` into an int64
// would yield 0 — the exact "unread reads as a measurement" confusion CampaignSettings
// exists to prevent. A pointer distinguishes "absent" from "present and zero" before
// any parsing decision is made.
type gaqlSettingsRow struct {
	Campaign struct {
		ResourceName           *string `json:"resourceName"`
		ID                     *string `json:"id"`
		Name                   *string `json:"name"`
		Status                 *string `json:"status"`
		AdvertisingChannelType *string `json:"advertisingChannelType"`
		BiddingStrategyType    *string `json:"biddingStrategyType"`
		StartDateTime          *string `json:"startDateTime"`
		EndDateTime            *string `json:"endDateTime"`
	} `json:"campaign"`
	CampaignBudget struct {
		AmountMicros      *string `json:"amountMicros"`
		TotalAmountMicros *string `json:"totalAmountMicros"`
		Period            *string `json:"period"`
		DeliveryMethod    *string `json:"deliveryMethod"`
		ExplicitlyShared  *bool   `json:"explicitlyShared"`
	} `json:"campaignBudget"`
}

// settingsQueryFields is the SELECT list for the settings readback, kept as one
// constant so the query and the tests that assert on it cannot drift apart.
//
// EVERY field name here is VERSION-SCOPED to v23 (googleAdsAPIVersion, client.go). That
// is not a formality: campaign.start_date_time / end_date_time are v23 spellings of
// fields that were start_date / end_date through v22, and Google has continued moving
// this surface — target_cpa.* left the selectable set in v25. Bumping the client's API
// version requires re-checking this list against that version's field reference rather
// than assuming it carries over.
//
// campaign_budget is an ATTRIBUTED RESOURCE of campaign (verified against Google's v23
// field reference), which is what lets its fields be selected in a `FROM campaign`
// query at all. Attribution is not segmentation: unlike a segments.* field it does not
// multiply rows, so the at-most-one-row expectation below still holds.
//
// Deliberately selects NO metrics.* and NO segments.* field. Adding one would segment
// the result, and this query's single-row guard would then start failing on healthy
// campaigns — the guard is what makes reading rows[0] correct.
const settingsQueryFields = "campaign.id, campaign.name, campaign.status, campaign.resource_name, " +
	"campaign.advertising_channel_type, campaign.bidding_strategy_type, " +
	"campaign.start_date_time, campaign.end_date_time, " +
	"campaign_budget.amount_micros, campaign_budget.total_amount_micros, " +
	"campaign_budget.period, campaign_budget.delivery_method, " +
	"campaign_budget.explicitly_shared"

// parseSettingsInt parses one of Google's string-encoded int64 fields.
//
// Unlike parseMetricInt in metrics.go, an EMPTY string is an ERROR rather than zero.
// The two differ because their absences mean different things: a zero-valued metric is
// omitted by Google and genuinely means zero activity, whereas a budget amount is not a
// counter — an empty one is a field this client could not read, and answering "0" for it
// would report a campaign with no budget, which is a claim about the campaign rather than
// about the read. Absence is handled by the caller as a nil pointer, before this is called.
func parseSettingsInt(s string) (int64, error) {
	if strings.TrimSpace(s) == "" {
		return 0, fmt.Errorf("empty value")
	}
	return strconv.ParseInt(s, 10, 64)
}

// GetCampaignSettings reads the campaign's CURRENT configuration from Google Ads.
//
// It is a pure read — one GAQL search, no mutate of any kind — and it never persists
// anything. Contract on the outcomes, mirroring GetCampaign's fail-closed shape because
// callers make a comparable decision from the result:
//
//   - the campaign exists      -> (settings, nil)
//   - no such campaign         -> (nil, nil)   — a clean, trustworthy absence
//   - anything unverifiable    -> (nil, error)
//
// A field Google did not return is left nil on the result rather than defaulted. The
// whole point of this read is to let an operator see where the recorded request and the
// live campaign disagree, and a defaulted value manufactures agreement or disagreement
// that was never observed.
//
// REMOVED campaigns are NOT excluded here, unlike GetCampaign. That exclusion exists to
// stop a tombstone being adopted; this call is about a campaign the service already has
// a row for, and "the campaign you are tracking has been removed upstream" is the single
// most actionable divergence there is. Filtering it out would report the campaign as
// unreadable and hide the finding.
func (c *Client) GetCampaignSettings(ctx context.Context, campaignID string) (*CampaignSettings, error) {
	if err := c.validateAccountIDs(); err != nil {
		return nil, err
	}
	// Validated as an IDENTITY before interpolation, exactly as GetCampaign does and for
	// the same reasons: "007" is digits but would match campaign 7 server-side and then
	// fail the echo check as a confusing conflict, and campaign.id is an int64 in GAQL so
	// the value is compared UNQUOTED — no escaping question arises once it is proven to be
	// nothing but digits.
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, err
	}

	query := "SELECT " + settingsQueryFields + " FROM campaign WHERE campaign.id = " + campaignID

	rows, err := c.gaqlSearch(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("google-ads campaign settings: %w", err)
	}
	if len(rows) == 0 {
		// A clean absence. Distinct from every error above: the platform answered and holds
		// no such campaign under this account.
		return nil, nil
	}
	// The query filters on a unique id and selects nothing that segments, so more than one
	// row means the response does not mean what this code reads it to mean. Failing loudly
	// is the only safe outcome: silently taking rows[0] would report ONE campaign's settings
	// while the response described several, and a divergence report built on that is worse
	// than no report.
	if len(rows) > 1 {
		return nil, fmt.Errorf("google-ads campaign settings: expected at most 1 row for campaign %s, got %d; refusing to trust this response", campaignID, len(rows))
	}

	raw := rows[0]
	// The same two decode-integrity guards GetCampaign runs, for the same reasons: a
	// malformed byte or an unpaired surrogate escape is silently substituted with U+FFFD
	// by encoding/json with no error, which would hand back a NAME this campaign does not
	// have; and a duplicate JSON key is resolved silently in favour of the last value, so
	// the row would identify more than one campaign.
	if !utf8.Valid(raw) || hasUnpairedSurrogateEscape(raw) {
		return nil, fmt.Errorf("google-ads campaign settings: campaign id %s was returned in a row that cannot survive JSON decoding intact (malformed UTF-8 bytes, or an unpaired surrogate escape); decoding it would substitute U+FFFD and report settings this campaign does not have", campaignID)
	}
	if hasDuplicateKeys(raw) {
		return nil, fmt.Errorf("google-ads campaign settings: a result row declares the same JSON key twice; the row describes more than one campaign and no reading of it can be trusted")
	}

	var row gaqlSettingsRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, &transportError{
			Method: http.MethodPost,
			Path:   c.customerPath("googleAds:search"),
			Err:    fmt.Errorf("decode campaign settings row: %w", err),
		}
	}

	// Identity is established from the row itself, then checked against the request. A row
	// for a DIFFERENT campaign means the WHERE clause was not honoured, which invalidates
	// the whole response — so this errors rather than reporting an absence, because an
	// absence is something a caller is entitled to trust.
	id := ""
	if row.Campaign.ID != nil {
		id = strings.TrimSpace(*row.Campaign.ID)
	}
	if id == "" && row.Campaign.ResourceName != nil {
		// Fall back to the resource name, which carries the id in a shape this client
		// validates strictly against its OWN customer — so a row from another account
		// cannot supply an identity here.
		id = c.campaignIDFromResourceName(*row.Campaign.ResourceName)
	}
	if id == "" {
		return nil, fmt.Errorf("google-ads campaign settings: campaign id %s was returned in a row with no usable id; refusing to report settings that cannot be attributed to a campaign", campaignID)
	}
	if id != campaignID {
		return nil, fmt.Errorf("google-ads campaign settings: query for campaign id %s returned campaign %s; the id filter was not honoured, refusing to trust this response", campaignID, id)
	}

	settings := &CampaignSettings{
		CampaignID:             id,
		Name:                   trimmedOrNil(row.Campaign.Name),
		Status:                 trimmedOrNil(row.Campaign.Status),
		AdvertisingChannelType: trimmedOrNil(row.Campaign.AdvertisingChannelType),
		BiddingStrategyType:    trimmedOrNil(row.Campaign.BiddingStrategyType),
		StartDateTime:          trimmedOrNil(row.Campaign.StartDateTime),
		EndDateTime:            trimmedOrNil(row.Campaign.EndDateTime),
		BudgetPeriod:           trimmedOrNil(row.CampaignBudget.Period),
		BudgetDeliveryMethod:   trimmedOrNil(row.CampaignBudget.DeliveryMethod),
		BudgetExplicitlyShared: row.CampaignBudget.ExplicitlyShared,
	}

	// A budget amount that is PRESENT but unparseable is an error, not an absence. Absence
	// says "Google did not report a budget"; silently converting a malformed one into that
	// same nil would tell an operator no budget was reported when one was, and hide a
	// decoding defect behind a benign-looking gap in the report.
	if row.CampaignBudget.AmountMicros != nil {
		v, perr := parseSettingsInt(*row.CampaignBudget.AmountMicros)
		if perr != nil {
			// The VALUE is never echoed — it comes straight from the upstream response body
			// and the service renders these errors into logs, so a malformed field must not
			// be able to inject arbitrary text (including newlines) into the log stream.
			// Naming the field is the whole diagnosis a responder can act on.
			return nil, &transportError{
				Method: http.MethodPost,
				Path:   c.customerPath("googleAds:search"),
				Err:    fmt.Errorf("decode campaign settings row: non-numeric campaign_budget.amount_micros"),
			}
		}
		settings.BudgetAmountMicros = &v
	}
	if row.CampaignBudget.TotalAmountMicros != nil {
		v, perr := parseSettingsInt(*row.CampaignBudget.TotalAmountMicros)
		if perr != nil {
			return nil, &transportError{
				Method: http.MethodPost,
				Path:   c.customerPath("googleAds:search"),
				Err:    fmt.Errorf("decode campaign settings row: non-numeric campaign_budget.total_amount_micros"),
			}
		}
		settings.BudgetTotalAmountMicros = &v
	}

	return settings, nil
}

// trimmedOrNil normalises an optional string field: a field Google omitted stays nil,
// and one present but blank BECOMES nil.
//
// Collapsing blank into absent is deliberate. A whitespace-only status or channel type
// is not a value a caller can interpret, and carrying it through would let an empty
// string be compared against a recorded value and reported as a DIVERGENCE — a
// difference invented by the read rather than observed on the platform. Absent is the
// honest reading of a field that arrived empty.
func trimmedOrNil(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}
