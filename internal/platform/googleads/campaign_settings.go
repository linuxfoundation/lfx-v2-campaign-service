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
// row records. The two can legitimately disagree: nothing here or anywhere else pushes
// the recorded config upstream, and more than one path lets the two sides drift apart
// — so the disagreement needs a way to be SEEN.
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

// budgetPeriodDaily and budgetPeriodCustom are the two REAL values of Google's v23
// BudgetPeriodEnum. The enum also has UNKNOWN/UNSPECIFIED, which are deliberately not named
// here: only a period that names one of these two can contradict a budget amount field.
const (
	budgetPeriodDaily  = "DAILY"
	budgetPeriodCustom = "CUSTOM_PERIOD"
)

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
// REMOVED campaigns are INCLUDED here, unlike GetCampaign, and including them takes an
// explicit predicate rather than the absence of one. GetCampaign's exclusion exists to stop
// a tombstone being adopted; this call is about a campaign the service already has a row
// for, and "the campaign you are tracking has been removed upstream" is the single most
// actionable divergence there is. Filtering it out would report the campaign as absent and
// hide the finding behind a 404.
//
// GAQL drops removed resources BY DEFAULT unless the status filter names REMOVED, so
// "no predicate" is not neutral here — it is the exclusion, silently. The query below
// therefore lists all three statuses; see the comment on it.
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

	// The status predicate is EXPLICIT, and naming REMOVED is the whole reason it exists.
	// GAQL excludes removed resources by default: a `FROM campaign` query with no predicate
	// naming REMOVED silently drops removed campaigns, so a bare `campaign.id = N` returns
	// zero rows for one — which this method reports as a clean absence (nil, nil), which the
	// dispatcher turns into ErrPlatformCampaignAbsent and the API turns into 404. That hides
	// the single most actionable divergence this endpoint exists to report: "the campaign you
	// are tracking has been removed upstream". Listing all three members is how a GAQL query
	// opts back IN to removed rows; there is no "no filter" that includes them.
	//
	// Single-quoted enum literals and the shared Status* constants, matching the two status
	// predicates in campaign_lookup.go. campaign.id stays UNQUOTED: it is an int64 in GAQL
	// and has already been proven to be nothing but digits by ValidateCampaignID above.
	query := "SELECT " + settingsQueryFields + " FROM campaign WHERE campaign.id = " + campaignID +
		" AND campaign.status IN ('" + StatusEnabled + "', '" + StatusPaused + "', '" + StatusRemoved + "')"

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
	// BOTH identity fields are evidence, so both are validated WHENEVER PRESENT — not the
	// resource name only as a fallback for a missing id. This mirrors campaignRowIdentity,
	// which documents the reasoning: validating the resource name only in the fallback makes
	// the check reachable exactly when the row is least suspicious, so a row carrying a
	// plausible id "555" beside a malformed or cross-customer resource name would sail
	// through and have another account's settings attributed to campaign 555.
	//
	// NOT trimmed, for the same reason canonicalCampaignID is not: an id is identity
	// evidence, so a padded value is a MALFORMED ROW rather than a value to normalise into a
	// match. Trimming here would also fold a whitespace-only resource name into "field
	// absent" and let the row fall through to the id alone, which is precisely the
	// present-but-garbage case this must reject.
	id := ""
	if row.Campaign.ID != nil {
		id = *row.Campaign.ID
	}
	resourceName := ""
	if row.Campaign.ResourceName != nil {
		resourceName = *row.Campaign.ResourceName
	}
	fromName := c.campaignIDFromResourceName(resourceName)
	if resourceName != "" && fromName == "" {
		return nil, fmt.Errorf("google-ads campaign settings: campaign id %s was returned in a row whose resource name %q is malformed or scoped to another customer; refusing to attribute these settings", campaignID, resourceName)
	}
	switch {
	case id == "":
		// campaign.id is an int64 field and can arrive absent; the resource name was
		// validated in FULL above, so it is safe identity evidence here.
		id = fromName
	case fromName != "" && fromName != id:
		return nil, fmt.Errorf("google-ads campaign settings: a row reports id %q but resource name %q; its two identity fields disagree, refusing to trust this response", id, resourceName)
	}
	if id == "" {
		return nil, fmt.Errorf("google-ads campaign settings: campaign id %s was returned in a row with no usable id; refusing to report settings that cannot be attributed to a campaign", campaignID)
	}
	// A non-canonical id ("007", "0", out of range) is not the answer to "which campaign is
	// this", even though every character is a digit.
	//
	// NOT independently revert-binding, and deliberately kept anyway: the equality check
	// below compares against campaignID, which ValidateCampaignID has already proven
	// canonical, so any id reaching here that is non-canonical also fails that check and no
	// revert of this line alone changes a test. It stays because it states the invariant
	// locally — the id is interpolated into resource paths by later calls — and because
	// deleting it would make a future divergence between the two checks silent.
	if canonicalCampaignID(id) == "" {
		return nil, fmt.Errorf("google-ads campaign settings: a row returned id %q, which is not the canonical spelling of a positive int64 campaign id", id)
	}
	if id != campaignID {
		return nil, fmt.Errorf("google-ads campaign settings: query for campaign id %s returned campaign %s; the id filter was not honoured, refusing to trust this response", campaignID, id)
	}

	settings := &CampaignSettings{
		CampaignID:             id,
		Name:                   blankToNil(row.Campaign.Name),
		Status:                 blankToNil(row.Campaign.Status),
		AdvertisingChannelType: blankToNil(row.Campaign.AdvertisingChannelType),
		BiddingStrategyType:    blankToNil(row.Campaign.BiddingStrategyType),
		StartDateTime:          blankToNil(row.Campaign.StartDateTime),
		EndDateTime:            blankToNil(row.Campaign.EndDateTime),
		BudgetPeriod:           blankToNil(row.CampaignBudget.Period),
		BudgetDeliveryMethod:   blankToNil(row.CampaignBudget.DeliveryMethod),
		BudgetExplicitlyShared: row.CampaignBudget.ExplicitlyShared,
	}

	// The two budget amounts are MUTUALLY EXCLUSIVE upstream — amount_micros for a DAILY
	// budget, total_amount_micros for a CUSTOM_PERIOD one — so a row carrying both has
	// contradicted the API's own invariant and cannot be read as an answer about this
	// campaign's budget. Refused rather than resolved by preference, and independently of how
	// any consumer later chooses between the two fields: this is a RESPONSE-INTEGRITY check
	// about a row contradicting the API's own invariant, so it stays correct even though
	// googleAdsUpstreamBudgetAmount now selects by BudgetPeriod rather than taking
	// amount_micros. Fail closed, exactly as the >1-row and unparseable-budget cases do.
	if row.CampaignBudget.AmountMicros != nil && row.CampaignBudget.TotalAmountMicros != nil {
		return nil, fmt.Errorf("google-ads campaign settings: campaign id %s was returned with both campaign_budget.amount_micros and campaign_budget.total_amount_micros, which are mutually exclusive; the row contradicts itself and no reading of its budget can be trusted", campaignID)
	}

	// The amount field must also AGREE WITH THE PERIOD, not merely be unaccompanied. The two
	// fields are selected by period upstream — amount_micros for DAILY, total_amount_micros for
	// CUSTOM_PERIOD — so a row pairing one with the other period is as self-contradictory as a
	// row carrying both, and is refused for the same reason: the two identity halves of the
	// budget disagree, and no reading of it can be trusted.
	//
	// This is an INDEPENDENT RESPONSE-INTEGRITY check, not a guard standing in for the
	// dispatcher. It is about this ROW contradicting itself, and it stays meaningful however
	// carefully a consumer later selects between the two fields: a DAILY row carrying only a
	// total is malformed at the source, and reporting a budget from it — by either field —
	// would attribute to the campaign a number the platform's own invariant says cannot
	// describe it. Refusing beats reconciling, because there is no way to tell WHICH of the
	// period and the amount is the wrong half.
	//
	// Only a period that NAMES one of the two real values can contradict an amount. An ABSENT
	// period passes: absence already means "Google did not report this field" everywhere on
	// CampaignSettings — a partial read is the ordinary case, pinned by
	// TestGetCampaignSettings_UnreadableFieldIsAbsentNotZero — and it cannot start signalling
	// "inconsistent pair" without breaking that meaning. It is also SAFE downstream, and that
	// half is enforced rather than assumed: googleAdsUpstreamBudgetAmount selects the amount
	// THROUGH googleAdsBudgetTypeFromPeriod, so a period that names neither real value selects
	// no amount at all and the field reads `unknown`. UNKNOWN/UNSPECIFIED pass for the same
	// reason: a value Google explicitly declined to name contradicts nothing, and it selects
	// nothing either.
	//
	// settings.BudgetPeriod is used rather than the raw row field because blankToNil has
	// already applied this file's normalisation: a whitespace-only period collapses to nil and
	// is therefore treated as absent here too, exactly as it is for every other optional string.
	//
	// The period is TrimSpace'd for this COMPARISON ONLY — the value blankToNil carries through
	// stays verbatim for the caller. Without this, a padded " DAILY " matched neither arm of the
	// switch below and slipped past BOTH refusals, leaving a self-contradictory row to be
	// reported as though it were coherent. The downstream consumer treats that padded value as
	// unnameable and reads no amount from it, so the visible result today is a needlessly
	// `unknown` field rather than a false divergence — but the ROW is still malformed, and this
	// check is what names it as such instead of passing it on.
	//
	// Trimming is safe HERE and wrong elsewhere, and the distinction is the value's KIND rather
	// than the operation. This is a closed-set ENUM: recognising " DAILY " as DAILY discovers
	// what the value already unambiguously is, and the trimmed form is used only to CHOOSE A
	// REFUSAL, never to populate a compared field. blankToNil's warning is about the opposite
	// case — trimming an opaque IDENTIFIER or a strictly-parsed date invents a well-formed value
	// the platform never reported, manufacturing an agreement rather than detecting one. That is
	// why the trimmed string is confined to this switch and never written back onto settings.
	if settings.BudgetPeriod != nil {
		switch period := strings.TrimSpace(*settings.BudgetPeriod); {
		case period == budgetPeriodDaily && row.CampaignBudget.TotalAmountMicros != nil:
			return nil, fmt.Errorf("google-ads campaign settings: campaign id %s was returned with campaign_budget.period %s but a campaign_budget.total_amount_micros, which belongs to a %s budget; the row contradicts itself and reading its budget would compare a whole-flight cap against a daily amount", campaignID, budgetPeriodDaily, budgetPeriodCustom)
		case period == budgetPeriodCustom && row.CampaignBudget.AmountMicros != nil:
			return nil, fmt.Errorf("google-ads campaign settings: campaign id %s was returned with campaign_budget.period %s but a campaign_budget.amount_micros, which belongs to a %s budget; the row contradicts itself and reading its budget would compare a daily amount against a whole-flight cap", campaignID, budgetPeriodCustom, budgetPeriodDaily)
		}
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

// blankToNil maps an optional string field to absence when it is blank, and otherwise
// carries the value through VERBATIM: a field Google omitted stays nil, one present but
// whitespace-only BECOMES nil, and one carrying anything else is untouched.
//
// Collapsing blank into absent is deliberate. A whitespace-only status or channel type
// is not a value a caller can interpret, and carrying it through would let an empty
// string be compared against a recorded value and reported as a DIVERGENCE — a
// difference invented by the read rather than observed on the platform. Absent is the
// honest reading of a field that arrived empty.
//
// NOT trimming the surviving value is equally deliberate, and it is the half that is easy
// to get wrong: this function runs BEFORE any of the consumers that validate these strings,
// so trimming here normalises a malformed value into a well-formed one behind their backs.
// googleAdsDateOnly parses with a strict layout and, on failure, withholds the value — so an
// upstream "2026-08-01 " trimmed here to "2026-08-01" would parse-fail either way, but
// trimming still matters: it decides WHICH malformed values reach the consumer, and a
// consumer that judges a value it never received cannot judge it correctly.
// googleAdsBudgetTypeFromPeriod has the same shape with " DAILY ". Both would be agreement manufactured by normalisation
// rather than observed on the platform, which is the exact failure this readback's
// absent-is-not-a-value discipline exists to prevent. Leaving the value verbatim keeps a
// malformed field malformed all the way to the consumer that has to judge it.
//
// Whitespace-only is not an exception to that: there is no value under the whitespace to
// preserve, so mapping it to absence withholds a comparison rather than manufacturing one.
func blankToNil(s *string) *string {
	if s == nil {
		return nil
	}
	if strings.TrimSpace(*s) == "" {
		return nil
	}
	return s
}
