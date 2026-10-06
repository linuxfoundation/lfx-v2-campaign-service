// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Campaign creation (GA-2): campaignBudget:mutate -> campaigns:mutate
// ---------------------------------------------------------------------------

const (
	// microsPerUnit converts a currency amount to Google Ads "micros"
	// (amount * 1,000,000). Budgets are expressed in micros of the account's
	// currency.
	microsPerUnit = 1_000_000

	// maxBudget caps the budget well below the int64 micros-overflow threshold
	// (math.MaxInt64 / 1e6 ≈ 9.2e12) so the *microsPerUnit conversion can never wrap
	// to a negative value. Mirrors the reddit/twitter clients' budget cap. This is a
	// sanity bound on caller input, NOT the account's minimum — Google enforces a
	// currency-dependent minimum server-side and rejects a too-low budget with a
	// campaignBudgetError, which surfaces as a definite failure.
	maxBudget = 1_000_000_000.0

	// maxBudgetNameBytes / maxCampaignNameRunes bound the composed names. Google Ads
	// v23 applies DIFFERENT limits in DIFFERENT UNITS (verified against the v23 System
	// Limits table + the RPC field references):
	//   - CampaignBudget.name: 1..255 inclusive, in UTF-8 BYTES (trimmed).
	//   - Campaign.name:        up to 256 CHARACTERS (StringLengthError.TOO_LONG).
	// The unit difference matters: a multibyte name hits the budget's byte ceiling
	// sooner than 255 characters, while the campaign limit is counted in characters.
	// Each name is validated against its own limit+unit before any create call, so an
	// over-limit name is rejected up front rather than creating the budget and then
	// failing the campaign mutate (which would orphan the budget). (Names must also be
	// unique per account, and an unbounded caller EventName could produce an oversized
	// payload — both reasons to cap.)
	maxBudgetNameBytes   = 255
	maxCampaignNameRunes = 256

	// advertisingChannelSearch is the only channel type this client creates today.
	advertisingChannelSearch = "SEARCH"

	// euPoliticalAdvertisingNo declares a campaign does NOT contain EU political
	// advertising — required on every v23 campaign create (see campaignCreate).
	euPoliticalAdvertisingNo = "DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING"

	// errCodeDuplicateBudgetName is Google's CampaignBudgetError code when a
	// non-shared campaign BUDGET name already exists.
	errCodeDuplicateBudgetName = "DUPLICATE_NAME"
	// errCodeDuplicateCampaignName is Google's CampaignError code when a CAMPAIGN
	// name already exists — a DIFFERENT code from the budget's DUPLICATE_NAME. Using
	// the wrong one means the campaign-duplicate branch never fires.
	//
	// Because :mutate has no idempotency key, a retried create that reuses a
	// deterministic name fails with the family-appropriate code — callers treat it as
	// "already exists, reconcile by name" rather than a fresh failure. See
	// isDuplicateBudgetNameErr / isDuplicateCampaignNameErr.
	errCodeDuplicateCampaignName = "DUPLICATE_CAMPAIGN_NAME"
)

// CampaignInput is the platform-agnostic request to create a Google Ads campaign:
// a PAUSED search-campaign shell (GA-2), an ad group + responsive search ad (GA-3),
// and keyword/audience targeting on that ad group (GA-4).
type CampaignInput struct {
	// EventName is the human-readable campaign subject, folded into the budget and
	// campaign names. Caller-supplied and otherwise unbounded, so it is trimmed and
	// the composed names are length-capped before any create call.
	EventName string
	// EventSlug is the URL-safe event identifier, used (with EventName/Project as
	// fallbacks) to build the ad's utm_campaign click-through param — see
	// buildAdFinalURL in ad_copy.go.
	EventSlug string
	// Project is folded into the composed name alongside EventName.
	Project string
	// RegistrationURL is the event's destination page. Required for the ad group +
	// ad cascade (GA-3): it becomes the ad's final URL, tagged with UTM params via
	// buildAdFinalURL. Mirrors the reddit/microsoft clients' RegistrationURL field.
	RegistrationURL string
	// Headlines / Descriptions are caller-supplied Responsive Search Ad copy.
	// Optional: composeAdCopy pads missing slots with deterministic placeholders
	// derived from EventName/Project up to the platform minimum (3 headlines, 2
	// descriptions), so a caller may leave these nil.
	Headlines    []string
	Descriptions []string
	// Budget is the campaign daily budget in whole units of the ad ACCOUNT's
	// currency. IMPORTANT: this is NOT a USD amount and the client performs NO
	// foreign-exchange conversion — Google interprets the resulting amountMicros in
	// the account's own currency, so a value of 50 becomes 50 of whatever the account
	// is denominated in (USD, EUR, JPY, …). The caller must supply an amount already
	// denominated in the account currency. Converted to micros (×1,000,000); must be
	// > 0 and <= maxBudget. (Renamed from BudgetUSD, which implied an FX conversion
	// this client does not do — mirrors the meta client's Budget field.)
	Budget float64
	// NameSuffix, when non-empty, is appended to the composed budget/campaign names
	// to make them unique+deterministic per logical campaign. A caller that wants
	// at-most-once retry semantics passes a stable value (e.g. the brief id): a
	// retry then reuses the same name and Google rejects it with DUPLICATE_NAME,
	// which the client reports as UNCONFIRMED-already-exists rather than re-creating.
	NameSuffix string
	// Keywords are positive Search keyword criteria attached to the ad group
	// (GA-4). Optional: an ad group created with none still exists but matches
	// no query, so a caller that wants the campaign to actually serve once
	// enabled must supply at least one. See validateKeywords for the
	// text/match-type/count rules.
	Keywords []Keyword
	// NegativeKeywords are CAMPAIGN-level exclusions: queries this campaign must
	// never pay for, however broadly Keywords match. Optional; see
	// validateNegativeKeywords for the text/match-type/count rules.
	//
	// Campaign level rather than ad group, deliberately — an exclusion expresses
	// intent for the whole campaign and must keep applying to an ad group a human
	// adds later in the UI. SEARCH only, and REFUSED on EVERY non-Search channel
	// rather than ignored — see the guard in preflightCampaignKind.
	//
	// Keywords and AudienceSegments were once accepted-and-ignored off Search and are
	// now refused on the same terms, so the contrast this comment used to draw between
	// them is gone: the rule is simply that a Search-only field refuses off Search.
	// The reasoning that closed it is worth keeping, because it is why the refusal is
	// the right default rather than a strictness. A dropped exclusion leaves the
	// operator believing the campaign is protected while it pays for exactly the
	// queries they named; a dropped positive keyword leaves them believing the
	// campaign can serve a query it will never see. Neither silence is recoverable
	// from the created campaign, so neither is defensible.
	NegativeKeywords []Keyword
	// CPCBid is the ad group's manual CPC bid in whole units of the ad ACCOUNT's
	// currency — the same no-FX-conversion caveat Budget carries applies here.
	//
	// 0 means UNSET and no bid field is sent, which is exactly what every campaign
	// created before this field existed did; no default is invented. SEARCH only, and
	// REFUSED on every other channel — no non-Search channel here has a manual bidding
	// strategy and their ad-group payloads carry no bid field at all. See
	// validateCPCBid (adgroup_ad.go) for the accepted range.
	CPCBid float64
	// BiddingStrategy names the campaign's bidding strategy in the caller's
	// vocabulary — the labels the Google Ads UI uses, lower-cased and hyphenated.
	// See bidding.go for the accepted set, which channel takes which, and why
	// `target-cpa`/`target-roas` are accepted alongside the `maximize-` spellings
	// Google folded them into.
	//
	// Empty means the CHANNEL DEFAULT, which is the strategy this client hard-coded
	// before the field existed — manual CPC on Search, maximize clicks on Demand Gen
	// — so an existing caller's payload is unchanged to the byte.
	BiddingStrategy string
	// TargetCPA is the target cost per acquisition in whole units of the ad ACCOUNT's
	// currency, carried only by the conversion-bidding strategies. 0 means UNSET:
	// optional under `maximize-conversions`, REQUIRED under `target-cpa`, and
	// REFUSED under any strategy that cannot bid to it rather than accepted and
	// discarded.
	TargetCPA float64
	// TargetROAS is the target return on ad spend as a RATIO, not a percentage —
	// 4.0 means four units of conversion value per unit spent. Same unset/required/
	// refused rules as TargetCPA, against `maximize-conversion-value` and
	// `target-roas`.
	TargetROAS float64
	// ConversionActions are the conversion actions this campaign optimizes toward,
	// as bare numeric ids or full `customers/<id>/conversionActions/<id>` resource
	// names. Empty means the campaign inherits the ACCOUNT's conversion goals, which
	// is what every campaign created before this field existed does.
	//
	// SEARCH only, and REFUSED on Demand Gen rather than dropped — that channel does
	// not take campaign.selective_optimization. See validateConversionActions.
	ConversionActions []string
	// StartDate / EndDate are the campaign's flight window as YYYY-MM-DD, matching
	// the vocabulary the meta and reddit dispatch configs already use. Each is
	// INDEPENDENTLY optional: empty means the field is not sent, so an empty
	// StartDate leaves Google's own default (the campaign starts today) and an empty
	// EndDate leaves it running until someone stops it.
	//
	// They are interpreted in the ad ACCOUNT's timezone, not UTC, because that is
	// what Google does with them — see campaignCreate.StartDateTime. That is also
	// why no "start date is in the past" check exists here, unlike the meta client's:
	// this client does not know the account's timezone, so a UTC "today" would refuse
	// a start date Google accepts for an account several hours behind.
	StartDate string
	EndDate   string
	// AudienceSegments are EXISTING Google Ads audience resource names (Customer
	// Match "user list" resources the caller has already built elsewhere — this
	// client does not create audiences) attached to the ad group as observation-only
	// criteria (GA-4): see validateAudienceSegments for the accepted resource-name
	// shapes and createAdGroupTargeting for why they're observation-only rather than
	// restrictive.
	AudienceSegments []string
	// GeoTargets are ISO 3166-1 alpha-2 country codes the campaign should serve
	// in (LFXV2-3283), resolved to Google's numeric geo target constants by
	// validateGeoTargets and attached as location criteria.
	//
	// Optional at this layer, and the default is the pre-LFXV2-3283 behaviour:
	// left empty, NO location criteria are created and the campaign serves
	// wherever the ACCOUNT's defaults allow — which for an event campaign is
	// usually the whole world, and is the defect this field exists to let a
	// caller fix. It is optional rather than required because this client is
	// also used to adopt/manage campaigns whose targeting a human set in the
	// Google Ads UI; the DISPATCHER is where a missing geo is worth warning
	// about, since only it knows the campaign was created from a brief.
	//
	// The ATTACH LEVEL differs per channel and is not a detail the caller
	// controls: Search takes campaign-level criteria, Demand Gen rejects those
	// and takes ad-group-level ones. See geo.go.
	//
	// Entries are ISO alpha-2 country codes OR raw numeric geo target constant ids
	// from Google's published table, which is how a CITY, region, metro or postal
	// code is addressed — see resolveGeoList.
	GeoTargets []string
	// ExcludedGeoTargets are locations this campaign must NOT serve in, in the same
	// vocabulary as GeoTargets. Optional.
	//
	// Exclusions are not merely the inverse of inclusions: an event campaign
	// routinely targets a country and carves out the regions a different campaign
	// already covers, which no inclusion list can express. They are attached in the
	// SAME atomic mutate as the inclusions — see createCampaignGeoTargeting for why
	// that is load-bearing rather than an optimisation.
	//
	// A location present in both lists is REFUSED at preflight: Google lets the
	// exclusion win, so the campaign would silently not serve where the caller
	// plainly asked it to.
	ExcludedGeoTargets []string
	// ProximityTargets are radius targets — "within N miles of this point" —
	// expressed as decimal degrees plus a radius and an explicit unit. Optional.
	//
	// Refused at preflight on DEMAND GEN only, because that channel attaches location
	// criteria at the ad group level and this client has not verified a proximity
	// criterion there. Accepted on Search and on Performance Max, both of which attach
	// location at the campaign level. Refusing locally is free; discovering it after the
	// campaign exists is not. See validateGeoPlan.
	ProximityTargets []ProximityTarget
	// Languages are the languages a Search campaign serves in, as ISO 639-1 codes
	// (EN, DE, JA) or raw numeric language constant ids. Optional; left empty the
	// campaign serves in EVERY language, which is Google's default and rarely what
	// an event campaign wants.
	Languages []string
	// AdSchedules restrict WHEN the campaign serves, one interval per day of week,
	// optionally each with its own bid modifier. Optional; left empty the campaign
	// runs around the clock.
	AdSchedules []AdSchedule
	// DeviceBidModifiers adjust the bid per device, or exclude a device outright
	// with a modifier of 0. Optional.
	DeviceBidModifiers []DeviceBidModifier
	// ExcludedAgeRanges and ExcludedGenders are campaign-level demographic
	// EXCLUSIONS ("18-24", "MALE", …). Optional. Exclusion-only is Google's own
	// shape at campaign level, not a narrowing — see campaign_criteria.go.
	ExcludedAgeRanges []string
	ExcludedGenders   []string
	// Sitelinks, Callouts and StructuredSnippets are the campaign's ad extensions.
	// All optional, all SEARCH ONLY (refused at preflight on Demand Gen), and all
	// created as account-level assets that are then linked to the campaign — see
	// assets.go. Left empty the ad serves as a bare headline+description, which
	// costs more per click than the same bid with extensions attached.
	Sitelinks          []Sitelink
	Callouts           []string
	StructuredSnippets []StructuredSnippet
	// AdGroups splits the campaign into one ad group per theme, each with its own
	// keywords, bid and up to three responsive search ads. Optional and SEARCH
	// ONLY (refused at preflight on Demand Gen, which builds its own ad group and
	// no ad).
	//
	// Left empty, the campaign gets exactly the single ad group with a single ad
	// this client has always built, from Headlines/Descriptions/Keywords/CPCBid
	// above — and those same fields remain the per-group fallback when a spec
	// omits one. See adgroup_plan.go for why more than one group is worth having:
	// Google scores Ad Rank per keyword against the ad that would serve for it.
	AdGroups []AdGroupSpec
	// DemandGenCreative is the image-and-text creative for the Demand Gen ad, and
	// is the mirror image of the three fields above: optional and DEMAND GEN ONLY,
	// refused at preflight on Search, which takes its creative as RSA copy plus
	// campaign-level extension assets instead.
	//
	// Left empty the Demand Gen cascade creates no ad at all — the behaviour this
	// client had before the creative existed, kept so that campaigns already in the
	// database still validate. A Demand Gen campaign with no ad cannot serve; the
	// closing steps say so. See demandgen_creative.go.
	DemandGenCreative DemandGenCreative
	// PerformanceMaxCreative is the asset group for a Performance Max campaign, and
	// is the third member of the same family: optional and PERFORMANCE MAX ONLY,
	// refused at preflight on every other channel.
	//
	// Left empty the Performance Max cascade creates the campaign and no asset group
	// — which is a campaign that cannot serve, said so in the closing steps. That is
	// kept rather than refused because it is also what ADOPTION of a campaign whose
	// asset group was built by hand upstream looks like. See pmax_creative.go.
	PerformanceMaxCreative PerformanceMaxCreative
}

// AdGroupResult is one ad group the cascade attempted, and everything created
// under it. ID empty means the ad group's own mutate failed or was unconfirmed;
// AdIDs empty means the group exists but its ads did not get created.
type AdGroupResult struct {
	Name  string   `json:"name"`
	ID    string   `json:"id,omitempty"`
	AdIDs []string `json:"adIds,omitempty"`
	// KeywordCriteriaIDs/AudienceCriteriaIDs are this group's own criteria, with
	// the same three-way ambiguity on empty that CampaignResult.GeoCriterionIDs
	// documents: none asked for, the mutate failed, or it was unconfirmed.
	KeywordCriteriaIDs  []string `json:"keywordCriteriaIds,omitempty"`
	AudienceCriteriaIDs []string `json:"audienceCriteriaIds,omitempty"`
}

// CampaignResult reports what CreateCampaign created. The Google Ads hierarchy is
// campaignBudget -> campaign, so both names and both IDs are surfaced for
// reconcile/cleanup. The NAMES matter on an ambiguous/duplicate failure BEFORE an id
// is known: the budget and campaign have DIFFERENT deterministic names (LFX | Budget
// | … vs LFX | Search Campaign | …), so a caller reconciling a possibly-orphaned
// budget must look it up by CampaignBudgetName — CampaignName would not find it.
type CampaignResult struct {
	Platform     string `json:"platform"`
	AccountLabel string `json:"accountLabel,omitempty"`
	// CustomerID is the ad account this campaign was created under. CampaignID is unique
	// only WITHIN a customer, so a later account-scoped request (a metrics read, a status
	// toggle) must confirm the connection it resolves still points here — the project's
	// Google Ads connection can be re-pointed at another account, and the same id under a
	// different customer reads as "no such campaign" or, worse, another account's campaign.
	// Persisted with the rest of the blob; absent on rows created before this field existed,
	// where the caller falls back to the ocid in GoogleAdsURL.
	CustomerID         string `json:"customerId,omitempty"`
	CampaignName       string `json:"campaignName"`
	CampaignBudgetName string `json:"campaignBudgetName"`
	CampaignID         string `json:"campaignId"`
	CampaignBudgetID   string `json:"campaignBudgetId"`
	// AdGroupName/AdGroupID/AdID are set by the GA-3 ad group + ad cascade
	// (createAdGroupAndAd in adgroup_ad.go). AdGroupID/AdID are empty on a
	// pre-ad-group failure; AdID alone is empty if the ad group was created but
	// the ad step failed/is unconfirmed.
	//
	// On a campaign with several ad groups these describe the FIRST one only, and
	// AdID the first ad of it. They are kept populated so every reader that
	// predates multi-group support keeps resolving to a real ad group rather than
	// an empty string; AdGroups below is the complete picture.
	AdGroupName string `json:"adGroupName,omitempty"`
	AdGroupID   string `json:"adGroupId,omitempty"`
	AdID        string `json:"adId,omitempty"`
	// AdGroups is every ad group this cascade attempted, in creation order.
	//
	// It is the only field that says how far a multi-group cascade got: an entry
	// is appended BEFORE the group's mutate is sent, so a group that failed is
	// present with an empty ID, and a group never reached has no entry at all.
	// Always at least one entry once the ad group stage started, including for a
	// single-group campaign.
	AdGroups []AdGroupResult `json:"adGroups,omitempty"`
	// KeywordCriteriaIDs/AudienceCriteriaIDs are set by the GA-4 targeting step
	// (createAdGroupTargeting in targeting.go) when the corresponding input list
	// was non-empty. Both are empty if targeting was never attempted or failed
	// before any criterion resource name could be parsed.
	//
	// Like the scalar ad group fields above, on a multi-group campaign these are
	// the FIRST group's criteria; each group's own are in its AdGroups entry.
	KeywordCriteriaIDs  []string `json:"keywordCriteriaIds,omitempty"`
	AudienceCriteriaIDs []string `json:"audienceCriteriaIds,omitempty"`
	// GeoCriterionIDs are the location criteria created for CampaignInput.GeoTargets
	// (LFXV2-3283).
	//
	// EMPTY IS AMBIGUOUS ON ITS OWN, and a reconciler must not read it as "no targets were
	// asked for". Three states produce an empty slice:
	//
	//   - the caller supplied no geo targets, so the campaign is deliberately UNTARGETED and
	//     serves wherever the account allows;
	//   - the criteria mutate FAILED, so the campaign exists untargeted but that was not the
	//     intent;
	//   - the mutate was UNCONFIRMED, so criteria may exist upstream with ids this run could
	//     not read — the case where a blind retry would double-create them.
	//
	// The returned ERROR is what distinguishes them: a nil error with an empty slice is the
	// first case and only the first. The second and third always arrive alongside a non-nil
	// error, and the third says UNCONFIRMED in its text. `CampaignInput.GeoTargets` tells a
	// caller what was asked for, if it needs to compare.
	//
	// The criteria live at DIFFERENT levels per channel (campaign for Search, ad group for
	// Demand Gen), so these ids are campaignCriterion ids on the Search path and
	// adGroupCriterion ids on the Demand Gen path; reconcile against the level the campaign's
	// channel uses.
	GeoCriterionIDs []string `json:"geoCriterionIds,omitempty"`
	// NegativeKeywordCriteriaIDs are the campaign-level negative keyword criteria
	// created for CampaignInput.NegativeKeywords. Empty is ambiguous in exactly the
	// three ways GeoCriterionIDs documents above — none asked for, the mutate failed,
	// or the mutate was unconfirmed — and the returned ERROR is likewise what tells
	// them apart: a nil error with an empty slice is the first case and only the first.
	//
	// Always campaignCriterion ids: unlike the geo criteria, these exist on the Search
	// path only, since Demand Gen has no keyword criteria.
	NegativeKeywordCriteriaIDs []string `json:"negativeKeywordCriteriaIds,omitempty"`
	// TargetingCriterionIDs are the campaign-level language, ad schedule, device and
	// demographic criteria created for the corresponding CampaignInput fields, in
	// that order. Empty is ambiguous in exactly the three ways GeoCriterionIDs
	// documents — none asked for, the mutate failed, or the mutate was unconfirmed —
	// and the returned ERROR is what tells them apart.
	//
	// Always campaignCriterion ids: these exist on the Search path only, since
	// validateCriteriaPlan refuses them on Demand Gen.
	TargetingCriterionIDs []string `json:"targetingCriterionIds,omitempty"`
	// ExtensionAssetIDs are the sitelink/callout/structured-snippet ASSETS created
	// for this campaign, in that order; ExtensionLinkIDs are the asset ids Google
	// confirmed LINKED to the campaign.
	//
	// They are reported separately because the two can legitimately differ: assets
	// are created account-wide by one mutate and linked by a second, so a failure
	// between them leaves created-but-unlinked assets, and a caller reconciling the
	// campaign needs to know they exist. ExtensionAssetIDs non-empty with
	// ExtensionLinkIDs empty is exactly that case, and the returned error says so.
	ExtensionAssetIDs []string `json:"extensionAssetIds,omitempty"`
	ExtensionLinkIDs  []string `json:"extensionLinkIds,omitempty"`
	// CreativeAssetIDs are the IMAGE assets created for a Demand Gen ad — marketing
	// images, then logos, in the order demandGenImageSlots lists them.
	//
	// Kept separate from ExtensionAssetIDs rather than folded into it, even though
	// both are account-level `assets` ids created by an `assets:mutate`: the two
	// exist on different channels (extensions are Search-only, these Demand Gen-only)
	// and are attached to different things — an extension is linked to the CAMPAIGN
	// by a second mutate, an image is referenced by the AD itself. A reconciler
	// chasing one would look in the wrong place for the other.
	//
	// Non-empty with AdID empty is the Demand Gen counterpart of the
	// created-but-unlinked case ExtensionAssetIDs documents: the images exist
	// account-wide and no ad references them. The returned error says so.
	// On a Performance Max campaign the same field carries the ASSET GROUP's assets —
	// text, then images in slot order, then videos — because they are created by the
	// same assets:mutate and have the same reconcile story. What differs is the
	// unlinked case: here it is AssetGroupID, not AdID, that is empty when the assets
	// exist with nothing referencing them.
	CreativeAssetIDs []string `json:"creativeAssetIds,omitempty"`
	// AssetGroupID is the Performance Max asset group, empty on every other channel
	// and on a Performance Max campaign created without a creative. Non-empty with a
	// returned error means the group exists but some or all of its asset LINKS do not
	// — the state that most needs finding, because an empty asset group looks
	// finished in the Google Ads UI.
	AssetGroupID string   `json:"assetGroupId,omitempty"`
	GoogleAdsURL string   `json:"googleAdsUrl"`
	Steps        []string `json:"steps"`
}

// mutateOperation is one {create: <resource>} entry in a :mutate request.
type mutateOperation struct {
	// omitempty so an UPDATE operation doesn't emit "create":null — a :mutate operation
	// must carry exactly ONE of create/update/remove, and a null create alongside an update
	// is an invalid (and confusing) payload.
	Create any `json:"create,omitempty"`
	// Update + UpdateMask carry an UPDATE operation (e.g. a status flip). Both are omitted
	// on a create so the payload stays exactly as the create path sends it today.
	Update     any    `json:"update,omitempty"`
	UpdateMask string `json:"updateMask,omitempty"`
}

// mutateRequest is the POST body for a *:mutate endpoint. partialFailure is left
// false (default): each call carries a single operation, so the request either
// wholly succeeds or wholly fails — there is no partial state to report.
type mutateRequest struct {
	Operations []mutateOperation `json:"operations"`
}

// mutateResponse is the (subset of the) :mutate response we consume. results is
// index-aligned with the request operations; each carries the created resource's
// resourceName (RESOURCE_NAME_ONLY is the default responseContentType).
type mutateResponse struct {
	Results []struct {
		ResourceName string `json:"resourceName"`
	} `json:"results"`
}

// ValidateBudgetMicros validates a budget amount and converts it to Google's micros.
//
// Named for what it does rather than for the field it feeds, because it feeds TWO that are
// mutually exclusive: a DAILY budget's amount_micros and a CUSTOM_PERIOD one's
// total_amount_micros. (It is deliberately not called BudgetAmountMicros — that is already
// the name of CampaignSettings' daily-only field, and a helper sharing that name would read
// as belonging to it.)
//
// EXTRACTED rather than duplicated: the create path (CreateCampaign) and the update path
// (UpdateCampaignBudget) must agree exactly on what a valid budget is, and the failure mode
// of two copies is the one that matters here — an amount the create path refuses but the
// update path accepts would let an operator set, through an edit, a budget the service
// would never have created. One definition makes that impossible rather than merely
// unlikely.
//
// The four checks are each load-bearing, and the first is the least obvious:
//
//   - NaN and Inf are rejected EXPLICITLY, because NaN fails every ordered comparison. A
//     bare `> 0` / `<= maxBudget` pair passes NaN straight through, and it converts to 0
//     micros — a zero budget nobody asked for.
//   - maxBudget bounds the value before the multiply, so the conversion cannot overflow.
//   - Round, never truncate: float64(2.01)*1e6 is 2009999.99…, which int64() would truncate
//     to 2009999, silently dropping a micro on ordinary budgets.
//   - <= 0 micros is checked AFTER the conversion, not before it: a sub-micro amount like
//     0.0000001 is genuinely > 0 yet rounds to 0 micros, and only the post-conversion check
//     catches it.
func ValidateBudgetMicros(budget float64) (int64, error) {
	if math.IsNaN(budget) || math.IsInf(budget, 0) {
		return 0, fmt.Errorf("google-ads campaign budget must be a finite number, got %v", budget)
	}
	if budget > maxBudget {
		return 0, fmt.Errorf("google-ads campaign budget %.2f exceeds the maximum %.0f", budget, maxBudget)
	}
	micros := int64(math.Round(budget * microsPerUnit))
	if micros <= 0 {
		return 0, fmt.Errorf("google-ads campaign budget must be > 0 (rounds to %d micros), got %.6f", micros, budget)
	}
	return micros, nil
}

// campaignBudgetCreate is the create payload for campaignBudgets:mutate.
type campaignBudgetCreate struct {
	Name           string `json:"name"`
	AmountMicros   int64  `json:"amountMicros"`
	DeliveryMethod string `json:"deliveryMethod"`
	// ExplicitlyShared=false makes this a non-shared budget bound to one campaign
	// (Google defaults budgets to shared). A pointer so the false value is always
	// emitted rather than omitted.
	ExplicitlyShared *bool `json:"explicitlyShared"`
}

// campaignCreate is the create payload for campaigns:mutate. Exactly one bidding
// strategy is required, and which one is now the caller's choice — see biddingFields
// and validateBiddingPlan. manualCpc{} remains the DEFAULT because it is the
// dependency-free choice for a PAUSED shell: every maximize-* strategy needs
// conversion tracking configured on the account, which a generic broker cannot
// assume, and it is also what this payload sent before the strategy was selectable.
//
// containsEuPoliticalAdvertising is REQUIRED on every v23 create: omitting it fails
// with FieldError.REQUIRED, and since 2026-04-01 an account with any undeclared
// campaign has ALL mutate calls rejected with
// MutateError.EU_POLITICAL_ADVERTISING_DECLARATION_REQUIRED. These are non-political
// ad campaigns, so we declare DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING.
//
// networkSettings must be set for a SEARCH create: a Campaign that targets NO network
// (which is what an omitted networkSettings resolves to — proto3 bools default false)
// is rejected with CampaignError.CAMPAIGN_MUST_TARGET_AT_LEAST_ONE_NETWORK, AFTER the
// budget mutate has committed (an avoidable orphan). Google documents no protective
// default, and every official create sample sets it. We target Google Search only
// (the conservative choice for a PAUSED broker shell); targetSearchNetwork stays false
// because true would require targetGoogleSearch AND opt this into Search Partners,
// which a generic broker shouldn't assume.
type campaignCreate struct {
	Name                           string               `json:"name"`
	Status                         string               `json:"status"`
	AdvertisingChannelType         string               `json:"advertisingChannelType"`
	CampaignBudget                 string               `json:"campaignBudget"`
	ContainsEuPoliticalAdvertising string               `json:"containsEuPoliticalAdvertising"`
	NetworkSettings                networkSettings      `json:"networkSettings"`
	GeoTargetTypeSetting           geoTargetTypeSetting `json:"geoTargetTypeSetting"`
	// biddingFields is embedded ANONYMOUSLY so its keys flatten into the campaign
	// object. It replaced a fixed `manualCpc` field: the strategy is a oneof, so the
	// payload must be able to name a different one, and a struct that always sent
	// manualCpc could only ever create a manually-bid campaign. Exactly one of its
	// strategy pointers is non-nil — see biddingPlan.fields.
	biddingFields
	// StartDateTime/EndDateTime are campaign.start_date_time / campaign.end_date_time,
	// formatted 'yyyy-MM-dd HH:mm:ss' and interpreted by Google in the ad ACCOUNT's
	// timezone — NOT UTC and NOT the bare YYYY-MM-DD this service's config uses.
	//
	// These are the v23 field names and the OLD ONES WILL NOT WORK: `campaign.start_date`
	// and `campaign.end_date` were REPLACED in v23, and sending either is rejected as an
	// unrecognized field — after the budget mutate has already committed. The pre-v23
	// 2037-12-30 "no end date" sentinel went with them; no end date is now an ABSENT
	// field, which is what omitempty gives us.
	StartDateTime string `json:"startDateTime,omitempty"`
	EndDateTime   string `json:"endDateTime,omitempty"`
}

// geoTargetTypeSetting decides what a location criterion actually MEANS, and Google's default
// is the permissive reading.
//
// With no setting, positiveGeoTargetType defaults to PRESENCE_OR_INTEREST: a user anywhere in
// the world who merely shows INTEREST in the targeted country stays eligible. A campaign
// "targeted at the US" therefore still serves globally, which is exactly the out-of-region spend
// LFXV2-3283 exists to prevent — the criterion would be attached and the budget would still
// leak.
//
// PRESENCE restricts delivery to people actually in the targeted locations. This is the same
// class of trap as targetingSetting above: Google's default is the looser one, so silence is a
// choice rather than an absence of one.
type geoTargetTypeSetting struct {
	PositiveGeoTargetType string `json:"positiveGeoTargetType"`
}

// geoTargetPresence restricts delivery to users physically present in the targeted locations.
const geoTargetPresence = "PRESENCE"

// targetingSetting / targetRestriction declare, per targeting dimension, whether
// a criterion of that dimension RESTRICTS delivery or only observes it (bids/
// reports without narrowing reach). Google requires targetingSetting be set at
// the SAME level the criterion is attached to — an AdGroup's targetingSetting
// cannot even be set while the parent Campaign has one, and a campaign-level
// setting has no effect on ad-group-level criteria (see Google's
// UpdateAudienceTargetRestriction sample, which reads/writes ad_group, not
// campaign, targeting_setting). GA-4's audience criteria are created as
// AdGroupCriterions (createAdGroupAndAd), so this is set on the AD GROUP
// create, not the campaign create — see adGroupCreate in adgroup_ad.go. With
// no targetingSetting at all, Google's default for a Search ad group's
// AUDIENCE dimension is TARGETING (restrictive) — an audience segment added
// for bid/reporting purposes would otherwise silently narrow delivery to that
// segment alone, a serious, easy-to-miss behavior change. bidOnly=true keeps
// AUDIENCE observation-only so keyword targeting (GA-4's other half) remains
// the thing that actually controls reach.
type targetingSetting struct {
	TargetRestrictions []targetRestriction `json:"targetRestrictions"`
}

type targetRestriction struct {
	TargetingDimension string `json:"targetingDimension"`
	BidOnly            bool   `json:"bidOnly"`
}

// networkSettings selects which networks a campaign's ads serve on. For a SEARCH
// campaign, targetGoogleSearch MUST be true (see campaignCreate). The remaining flags
// are sent explicitly as false rather than omitted so the payload is unambiguous.
type networkSettings struct {
	TargetGoogleSearch   bool `json:"targetGoogleSearch"`
	TargetSearchNetwork  bool `json:"targetSearchNetwork"`
	TargetContentNetwork bool `json:"targetContentNetwork"`
}

// googleAdsErrorEnvelope is the error body shape: the machine-readable error codes
// live at error.details[<GoogleAdsFailure>].errors[].errorCode, which is a
// single-key object (category -> enum). message/requestId are intentionally NOT
// captured — only the codes are retained, and only for internal classification.
type googleAdsErrorEnvelope struct {
	Error struct {
		Details []struct {
			Type   string `json:"@type"`
			Errors []struct {
				ErrorCode map[string]json.RawMessage `json:"errorCode"`
			} `json:"errors"`
		} `json:"details"`
	} `json:"error"`
}

// parseErrorCodes extracts Google's enum error codes from a non-2xx body, e.g.
// "DUPLICATE_NAME" or "REQUIRED". Each errorCode is a single-key object whose VALUE
// is the enum constant; we retain the values (the enum constants) for matching.
// Over-long values and codes beyond the cap are dropped: they are used only for
// enum classification, never surfaced, so bounding them keeps a hostile body from
// being retained even internally. Returns nil on a malformed/absent body.
func parseErrorCodes(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	var env googleAdsErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	var codes []string
	for _, d := range env.Error.Details {
		if !strings.HasSuffix(d.Type, "GoogleAdsFailure") {
			continue
		}
		for _, e := range d.Errors {
			for _, raw := range e.ErrorCode {
				var v string
				if err := json.Unmarshal(raw, &v); err != nil || v == "" {
					continue
				}
				if len(v) > maxErrorCodeLen {
					continue
				}
				codes = append(codes, v)
				if len(codes) >= maxRetainedErrorCodes {
					return codes
				}
			}
		}
	}
	return codes
}

const (
	maxRetainedErrorCodes = 16
	maxErrorCodeLen       = 128
)

// hasErrorCode reports whether the apiError carried the given Google Ads enum
// error code. It reads the ErrorCodes parsed from the FULL body in doRequest — NOT
// the truncated Body — so classification works for error payloads longer than
// maxErrorBodyChars.
func (e *apiError) hasErrorCode(code string) bool {
	for _, c := range e.ErrorCodes {
		if strings.EqualFold(c, code) {
			return true
		}
	}
	return false
}

// isDefiniteClientError reports whether ae is a definite 4xx client-error rejection
// that is NOT ambiguous — i.e. a 4xx EXCEPT 429. A 429 is excluded because
// createOutcomeAmbiguous classifies a mutating 429 as possibly-committed regardless of
// whether it was retried (the create path never retries one; the toggle path's bounded
// retries can still EXHAUST after a request was received), so a duplicate-name code on a 429
// must NOT be read as a known prior create — the throttled request itself may be
// the one that created it. Keeping this exclusion here (the duplicate predicates run
// BEFORE createOutcomeAmbiguous on the create path) preserves the ambiguity contract.
func isDefiniteClientError(ae *apiError) bool {
	return ae.StatusCode >= 400 && ae.StatusCode < 500 &&
		ae.StatusCode != http.StatusTooManyRequests
}

// isDuplicateBudgetNameErr reports whether err is Google's CampaignBudgetError
// DUPLICATE_NAME rejection on a definite 4xx (excluding 429). A 3xx/5xx/429 carrying
// the code stays ambiguous via createOutcomeAmbiguous, so a create that may have
// committed is not mislabeled a known duplicate.
func isDuplicateBudgetNameErr(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) &&
		isDefiniteClientError(ae) &&
		ae.hasErrorCode(errCodeDuplicateBudgetName)
}

// isDuplicateCampaignNameErr reports whether err is Google's CampaignError
// DUPLICATE_CAMPAIGN_NAME rejection on a definite 4xx (excluding 429) — the
// campaign-name analogue of isDuplicateBudgetNameErr (the two families use different
// codes).
func isDuplicateCampaignNameErr(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) &&
		isDefiniteClientError(ae) &&
		ae.hasErrorCode(errCodeDuplicateCampaignName)
}

// createOutcomeAmbiguous reports whether a failed MUTATING request MAY have been
// committed upstream (so a caller must reconcile/verify before retrying, to avoid a
// duplicate — :mutate has no idempotency key). A 5xx apiError or any transportError
// is ambiguous regardless of method; a 3xx is ambiguous only on a mutating method
// (a GET redirect is not a create). A 429 on a mutating call is ALSO ambiguous,
// whether or not doRequest retried it: the CREATE path passes idempotent=false and
// never retries (a throttled create may already have committed), while the status
// TOGGLE passes true and its bounded retries can still EXHAUST — in both cases a
// request reached Google, so the caller must reconcile rather than blind-retry. A definite 4xx (Google
// rejected it) and a pre-send error are NOT ambiguous. Mirrors the sibling clients.
func createOutcomeAmbiguous(err error) bool {
	var te *transportError
	if errors.As(err, &te) {
		return true
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.StatusCode >= 500 || ae.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return ae.StatusCode >= 300 && ae.StatusCode < 400 && isMutatingMethod(ae.Method)
}

// isMutatingMethod reports whether an HTTP method can create/modify server state,
// so a 3xx on it may hide a committed mutation. Mirrors the sibling clients.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// resourceID returns the trailing id segment of a Google Ads resourceName, e.g.
// "customers/123/campaigns/456" -> "456". Empty if the name is empty or malformed.
func resourceID(resourceName string) string {
	if resourceName == "" {
		return ""
	}
	i := strings.LastIndex(resourceName, "/")
	if i < 0 || i == len(resourceName)-1 {
		return ""
	}
	return resourceName[i+1:]
}

// campaignPreflight is everything CreateCampaign validates and computes BEFORE its first
// mutate. It exists as a type so the checks can have a second caller without being written
// twice — see ValidateCampaignInput.
type campaignPreflight struct {
	amountMicros     int64
	budgetName       string
	campaignName     string
	finalURL         string
	adGroupName      string
	headlines        []string
	descriptions     []string
	keywords         []Keyword
	audienceSegments []string
	// geo is the whole resolved location intent — inclusions, exclusions and
	// proximity — with every country code already mapped to its Google geo target
	// constant id. Resolved during the preflight so an unmapped country code, a
	// contradictory include/exclude pair or an out-of-range radius fails BEFORE the
	// budget mutate, rather than after a paid campaign exists. See validateGeoPlan.
	geo geoPlan
	// criteria is the resolved non-location targeting intent — languages, ad
	// schedules, device bid modifiers and demographic exclusions — resolved here for
	// the same reason geo is: an unmapped language, an inverted schedule or an
	// out-of-range bid modifier must fail before anything is paid for. See
	// validateCriteriaPlan.
	criteria criteriaPlan
	// assets are the validated ad extensions — sitelinks, callouts and structured
	// snippets — resolved here for the same reason the criteria are: an over-long
	// callout or a sitelink with no destination must fail before anything is paid
	// for. See validateAssetPlan.
	assets assetPlan
	// creative is the validated Demand Gen ad creative — image URLs, headlines,
	// descriptions and business name. Everything LOCALLY decidable is resolved here
	// with the rest; the image bytes themselves are fetched by the Demand Gen
	// cascade in a separate step that still runs before the budget mutate, because
	// whether a URL serves a 600x314 JPEG is not knowable from the string. See
	// validateDemandGenCreative and fetchDemandGenImages.
	creative demandGenCreativePlan
	// pmax is the validated Performance Max asset group — image URLs bucketed by
	// slot, the three text field types, the business name and the group's own name
	// and display paths. Resolved here with everything else for the same reason
	// creative is, and its image bytes are fetched by the Performance Max cascade in
	// the same pre-budget step. See validatePerformanceMaxCreative.
	pmax performanceMaxPlan
	// adGroups are the ad groups the cascade will create, always at least one. When
	// the caller asked for none, it holds exactly the single group the fields above
	// describe, so the cascade has one shape to walk rather than two. A duplicate
	// group name or a fourth ad in a group fails here, before the budget mutate —
	// discovering either mid-cascade strands the groups already created. See
	// validateAdGroupPlans.
	adGroups []adGroupPlan
	// negativeKeywords are the validated campaign-level exclusions, and cpcBidMicros
	// the validated ad-group bid already converted to micros (0 = unset). Both are
	// computed here, with everything else, so a bad exclusion or an out-of-range bid
	// fails before the budget mutate rather than after a paid campaign exists.
	negativeKeywords []Keyword
	cpcBidMicros     int64
	// bidding is the resolved bidding strategy, its target if it takes one, and the
	// conversion actions the campaign optimizes toward. Resolved here for the same
	// reason everything else is: an unknown strategy name or a conversion action
	// naming another account is rejected by Google AFTER the budget mutate, and a
	// target silently dropped by the wrong strategy is never rejected at all. See
	// validateBiddingPlan.
	bidding biddingPlan
	// startDateTime/endDateTime are the caller's YYYY-MM-DD flight window already
	// rendered into the 'yyyy-MM-dd HH:mm:ss' form campaignCreate sends; empty means
	// the corresponding field is omitted.
	startDateTime string
	endDateTime   string
}

// campaignDateRE pins the flight-window format BEFORE time.Parse sees it.
//
// time.Parse("2006-01-02", …) is more permissive than the layout suggests — it
// accepts single-digit months and days, so "2026-1-5" parses cleanly and would then
// be rendered back as a date the caller never wrote. The meta client pairs its own
// parse with exactly this kind of regex for the same reason.
var campaignDateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// campaignDateOnlyLayout is the Go layout for the YYYY-MM-DD dates CampaignInput
// carries — the calendar-date half of what Google is sent, not the wire format.
const campaignDateOnlyLayout = "2006-01-02"

// validateFlightWindow validates the optional start/end dates and renders them into
// the 'yyyy-MM-dd HH:mm:ss' strings campaign.start_date_time/end_date_time take.
//
// Each date is independently optional: an empty one yields an empty string, which
// campaignCreate omits. The day boundaries are explicit — 00:00:00 for a start and
// 23:59:59 for an end — so a one-day window covers that whole day rather than
// collapsing to a zero-length instant at midnight.
//
// Only ONE cross-field rule is enforced, and only when both dates are present: the
// end must not be BEFORE the start. The same day on both sides is accepted on
// purpose — with the boundaries above it is a well-defined 24-hour flight, which is
// what a one-day event promo wants, and rejecting it would contradict the whole
// reason those boundaries are explicit.
//
// There is deliberately no "start date is in the past" check, which the meta client
// does have. Google interprets these in the ad ACCOUNT's timezone; this client does
// not know that timezone, so a UTC "today" would refuse a start date that is still
// today for an account several hours behind — refusing a create Google would have
// accepted.
func validateFlightWindow(startDate, endDate string) (startDateTime, endDateTime string, err error) {
	var start, end time.Time
	if startDate != "" {
		if !campaignDateRE.MatchString(startDate) {
			return "", "", fmt.Errorf("google-ads campaign start date %q is not in YYYY-MM-DD format", startDate)
		}
		start, err = time.Parse(campaignDateOnlyLayout, startDate)
		if err != nil {
			return "", "", fmt.Errorf("google-ads campaign start date %q is not a valid calendar date: %w", startDate, err)
		}
		startDateTime = startDate + " 00:00:00"
	}
	if endDate != "" {
		if !campaignDateRE.MatchString(endDate) {
			return "", "", fmt.Errorf("google-ads campaign end date %q is not in YYYY-MM-DD format", endDate)
		}
		end, err = time.Parse(campaignDateOnlyLayout, endDate)
		if err != nil {
			return "", "", fmt.Errorf("google-ads campaign end date %q is not a valid calendar date: %w", endDate, err)
		}
		endDateTime = endDate + " 23:59:59"
	}
	if startDate != "" && endDate != "" && end.Before(start) {
		return "", "", fmt.Errorf("google-ads campaign end date %s must not be before start date %s", endDate, startDate)
	}
	return startDateTime, endDateTime, nil
}

// flightWindowStep renders the flight window for the campaign-created step log.
//
// The step log is what an operator reconciles a campaign against the account with,
// so an omitted date is reported as the DEFAULT IT LEAVES IN PLACE rather than as a
// blank: "starts today" and "no end date" are Google's behaviour for an absent
// start_date_time/end_date_time, and a reader should not have to know that to
// interpret the line. The times are the account-timezone boundaries actually sent,
// not reformatted, so the line can be compared against the campaign directly.
func flightWindowStep(startDateTime, endDateTime string) string {
	start := "starts immediately (no start date set)"
	if startDateTime != "" {
		start = "starts " + startDateTime
	}
	end := "no end date"
	if endDateTime != "" {
		end = "ends " + endDateTime
	}
	return start + ", " + end + ", account timezone"
}

// ValidateCampaignInput runs exactly the input validation CreateCampaign runs before it
// touches Google, and reports the first failure. It mutates nothing and sends nothing.
//
// It exists for the ADOPTION path. A dispatch that finds an existing campaign by name
// returns before CreateCampaign is ever called, so without this the same input would be
// accepted or rejected depending on whether a same-name campaign happened to exist — a
// bad budget or an invalid registration URL would fail cleanly on a first dispatch and
// silently succeed on a retry. Validity is a property of the request; it cannot depend on
// hidden state at the far end.
//
// It delegates to the SAME helper CreateCampaign uses rather than repeating the checks,
// because a second copy would pass review once and drift on the next change to either.
func (c *Client) ValidateCampaignInput(in CampaignInput) error {
	return c.ValidateCampaignInputKind(CampaignKindSearch, in)
}

// ValidateCampaignInputKind is ValidateCampaignInput for a caller that already knows
// which channel the request is for. Prefer it: ValidateCampaignInput assumes Search.
//
// The kind is load-bearing now in a way it was not when this entry point was written.
// preflightCampaignKind used to take kind only to compose the name, so validating as
// Search was harmless for every channel. It now GATES refusals, and those refusals are
// PER CHANNEL rather than Search-versus-everything. Demand Gen refuses the widest set:
// proximity targeting, all five campaign criteria kinds, the extension assets, the
// ad-group list, the CPC bid, keywords and audience segments. Performance Max refuses
// only the half it genuinely cannot carry — device bid modifiers and campaign-level
// demographic exclusions, plus the Search-only targeting fields — and accepts proximity,
// languages and ad schedules at the campaign level like Search. So a Demand Gen request
// carrying one of those validates clean as Search and is then refused by
// CreateDemandGenCampaign. On the create path
// that is merely a late error; on the ADOPTION path, which returns before any create
// runs, it is the exact defect this function exists to prevent: the same request
// accepted when a campaign happens to exist and refused when it does not.
//
// Unknown kinds are not rejected here. The dispatch layer refuses an unrecognised
// channel before it reaches the client at all, and a kind this package does not know
// gates nothing — it simply gets the un-restricted Search treatment, which is the
// pre-existing behaviour and cannot refuse something upstream would have accepted.
func (c *Client) ValidateCampaignInputKind(kind string, in CampaignInput) error {
	_, err := c.preflightCampaignKind(kind, in)
	return err
}

// campaignKind is the name segment that distinguishes one channel's campaign from
// another's on the SAME brief. Google rejects a duplicate campaign name within an
// account, and (more importantly) two channels sharing a name are indistinguishable
// to anyone reconciling them by name after an ambiguous create.
// Exported because the DISPATCH layer must compose the same name this client will, before
// it calls a create: adoption looks a campaign up BY NAME, so a dispatch that composed the
// name itself from a local literal would look up a name the client never writes the moment
// the two drift. One definition, both callers.
const (
	CampaignKindSearch         = "Search Campaign"
	CampaignKindDemandGen      = "DemandGen Campaign"
	CampaignKindPerformanceMax = "PerformanceMax Campaign"
)

// Unexported aliases retained so this package's own call sites read unchanged.
const (
	campaignKindSearch         = CampaignKindSearch
	campaignKindDemandGen      = CampaignKindDemandGen
	campaignKindPerformanceMax = CampaignKindPerformanceMax
)

// preflightCampaign validates the input and composes the names, for the given campaign
// KIND. The kind is a parameter rather than a constant because Demand Gen and Search are
// separate campaigns under one brief: composing both as "Search Campaign" made them
// collide upstream and become indistinguishable in a reconcile. The legacy Express path
// draws the same distinction (buildCampaignName(body, 'Search' | 'DemandGen')).
func (c *Client) preflightCampaign(in CampaignInput) (*campaignPreflight, error) {
	return c.preflightCampaignKind(campaignKindSearch, in)
}

// budgetKindFor returns the name segment distinguishing one channel's BUDGET from another's
// on the same brief. The mapping is deliberately ASYMMETRIC — Search keeps the bare "Budget"
// it has always used, and only Demand Gen gets a channel-specific one.
//
// The asymmetry is the point, not an oversight. A non-shared budget's name is this client's
// idempotency key: ComposeName is deterministic in the brief, so a retry recomposes the same
// name and Google refuses it with DUPLICATE_NAME, which the caller reports as
// already-exists rather than creating a second budget. Renaming SEARCH's budget would break
// that for every Search campaign already in flight — its budget is named the old way
// upstream, so a retry would compose a name that does NOT collide and would create a second
// budget for the same campaign. Demand Gen has no such history: nothing has been created
// under a Demand Gen budget name yet, so it is free to take a distinct one.
//
// Without this, both channels composed "LFX | Budget | <project> | <event> | <brief id>" —
// identical, because every other segment is the same for one brief. Demand Gen on a brief
// that already had a Search campaign therefore failed at the BUDGET step with DUPLICATE_NAME
// and never reached the campaign create at all.
func budgetKindFor(campaignKind string) string {
	switch campaignKind {
	case campaignKindDemandGen:
		return "DemandGen Budget"
	case campaignKindPerformanceMax:
		// Same reasoning as Demand Gen's, and it applies to every channel added after
		// Search: nothing has been created under this name yet, so it is free to take a
		// distinct one, and sharing Search's would make the two collide on one brief.
		return "PerformanceMax Budget"
	default:
		return "Budget"
	}
}

func (c *Client) preflightCampaignKind(kind string, in CampaignInput) (*campaignPreflight, error) {
	if err := c.validateAccountIDs(); err != nil {
		return nil, err
	}
	// Require BOTH attribution fields (mirrors the meta/twitter/reddit clients, which
	// require Project and EventName independently). Project is the canonical
	// attribution key the data pipeline parses out of the campaign name, so a campaign
	// with no Project segment is mis-attributed even if EventName is present.
	//
	// Validate the SANITIZED values, not the raw input: composeName only includes a
	// segment when its sanitizeNamePart is non-empty, so a delimiter-only value like
	// "|||" passes a raw TrimSpace check yet sanitizes to nothing — which would drop
	// the Project segment while still creating a paid budget/campaign. Checking the
	// sanitized value here keeps validation and composition consistent.
	if sanitizeNamePart(in.Project) == "" {
		return nil, fmt.Errorf("google-ads campaign requires a non-empty Project")
	}
	if sanitizeNamePart(in.EventName) == "" {
		return nil, fmt.Errorf("google-ads campaign requires a non-empty EventName")
	}
	// Validate the budget and compute amountMicros ONCE. Reject NaN/Inf explicitly
	// (NaN passes every ordered comparison, so `> 0`/`<= max` alone would let it
	// through and create a 0 budget), and reject anything that rounds to <= 0 micros
	// (a sub-micro budget like 0.0000001 is > 0 but converts to 0 amountMicros).
	amountMicros, err := ValidateBudgetMicros(in.Budget)
	if err != nil {
		return nil, err
	}

	budgetName := ComposeName(budgetKindFor(kind), in)
	campaignName := ComposeName(kind, in)
	// Budget name is limited in UTF-8 BYTES (len is the byte count); campaign name in
	// CHARACTERS (utf8.RuneCountInString). See maxBudgetNameBytes/maxCampaignNameRunes.
	if err := validateEntityName("budget", budgetName, len(budgetName), maxBudgetNameBytes, "UTF-8 bytes"); err != nil {
		return nil, err
	}
	if err := validateEntityName("campaign", campaignName, utf8.RuneCountInString(campaignName), maxCampaignNameRunes, "characters"); err != nil {
		return nil, err
	}
	// Validate the ad-group/ad inputs (destination URL, ad copy, ad-group name,
	// keywords/audience segments) BEFORE the first (budget) mutate: a failure
	// here is purely local input validation, and surfacing it only after the
	// budget+campaign already committed (inside createAdGroupAndAd, which runs
	// last) would orphan a real paid campaign for what amounts to a bad
	// RegistrationURL, over-length name, or invalid targeting value.
	finalURL, headlines, descriptions, adGroupName, keywords, audienceSegments, err := precomputeAdGroupAdInputs(in)
	if err != nil {
		return nil, err
	}
	// Resolve geo BEFORE the first (budget) mutate, for the same reason as the ad-group
	// inputs above: an unmapped country code is pure local input validation, and refusing
	// it only after the budget and campaign have committed would orphan a real paid
	// campaign over a typo like "USA".
	geo, err := validateGeoPlan(kind, in)
	if err != nil {
		return nil, err
	}
	criteria, err := validateCriteriaPlan(kind, in)
	if err != nil {
		return nil, err
	}
	assets, err := validateAssetPlan(kind, in)
	if err != nil {
		return nil, err
	}
	// The Demand Gen creative is the mirror of the extension assets above: refused
	// on Search for the same capability reason they are refused on Demand Gen. Only
	// its locally decidable half is settled here — counts, display widths, business
	// name, URL shape — so ValidateCampaignInputKind refuses exactly what the create
	// cascade refuses. The bytes are fetched by that cascade, still before the budget
	// mutate. See demandgen_creative.go.
	creative, err := validateDemandGenCreative(kind, in)
	if err != nil {
		return nil, err
	}
	// The Performance Max asset group is the same shape of check on the third
	// channel, and refuses off Performance Max exactly as the two above refuse off
	// theirs. Its image bytes are fetched by that cascade, still before the budget
	// mutate. See pmax_creative.go.
	pmax, err := validatePerformanceMaxCreative(kind, in)
	if err != nil {
		return nil, err
	}
	// The three remaining inputs join the same pre-mutate block for the same
	// orphan-avoidance reason: each is purely local, and each would otherwise be
	// rejected by Google only at a mutate that runs after the budget and campaign have
	// committed — the negatives at a campaignCriteria:mutate two steps later, the bid
	// at the adGroups:mutate after that, and a malformed date at the campaign create
	// itself, which is already one paid resource too late.
	// REFUSED on Demand Gen rather than dropped, for the same reason the CPC bid below
	// is. createCampaignNegativeKeywords is called only from CreateCampaign's cascade;
	// CreateDemandGenCampaign never reads pf.negativeKeywords, so accepting the list
	// there would validate every term and then discard the lot — and the operator would
	// read "campaign created" and believe the exclusions are live while the campaign
	// keeps paying for exactly the queries they named. A silent drop is worse here than
	// anywhere else in this preflight precisely because the field's whole purpose is to
	// STOP spend. Every other Search-only input in this block already refuses
	// (proximity, the five criteria kinds, extension assets, the ad-group list, the
	// bid); this was the one that neither refused nor applied.
	//
	// The fence is `!= campaignKindSearch` rather than `== campaignKindDemandGen`:
	// createCampaignNegativeKeywords is reached only from CreateCampaign's cascade, so
	// the drop this refusal exists to prevent happens on EVERY channel that is not
	// Search, and a fence naming one channel would have silently admitted the next one
	// added. Performance Max is the case that proves it — Google does accept a campaign
	// negative keyword list there, but this client does not create one, and "Google
	// would allow it" is no comfort to an operator whose exclusions were validated and
	// discarded.
	if len(in.NegativeKeywords) > 0 && kind != campaignKindSearch {
		return nil, fmt.Errorf("google-ads: campaign-level negative keywords are not supported on %s (this client attaches them only on the Search cascade); omit NegativeKeywords, or create a Search campaign to exclude queries", kind)
	}
	negativeKeywords, err := validateNegativeKeywords(in.NegativeKeywords)
	if err != nil {
		return nil, err
	}
	// The bid is REFUSED on Demand Gen rather than dropped. demandGenAdGroupCreate has
	// no cpcBidMicros field and demandgen.go never reads this value, so accepting the
	// bid there would validate it, convert it, and then silently discard it — the same
	// defect LFXV2-3283 fixed for geo, and the reason every other Search-only input in
	// this preflight refuses instead of ignoring. The refusal costs nothing upstream:
	// Demand Gen bids via targetSpend and rejects manualCpc, so no bid supplied here
	// could ever have reached a Google call that wanted it.
	// Widened from a Demand Gen test to every non-Search channel for the same reason
	// the negatives above were: no other channel's payload carries an ad-group bid, and
	// none of them has manual bidding at all, so accepting a bid there would validate a
	// number and then discard it.
	if in.CPCBid != 0 && kind != campaignKindSearch {
		return nil, fmt.Errorf("google-ads: a CPC bid is not supported on %s (only Search bids manually here; the other channels have no manual bidding strategy and their ad-group payloads carry no bid); omit CPCBid, or create a Search campaign for manual bidding", kind)
	}
	cpcBidMicros, err := validateCPCBid(in.CPCBid)
	if err != nil {
		return nil, err
	}
	// Ad-group keywords and audience segments are REFUSED on every non-Search channel
	// for the same reason the two fences above are, and they are the last pair in this
	// preflight that still validated and then dropped. createAdGroupTargeting is called
	// only from createAdGroupAndAd, which is on CreateCampaign's Search cascade;
	// CreateDemandGenCampaign and CreatePerformanceMaxCampaign never read
	// pf.keywords/pf.audienceSegments — Demand Gen's ad group takes no criteria at all,
	// and Performance Max has no ad group to hang them on. Accepting them there would
	// check every term and resource name and then discard the lot, leaving an operator
	// who named an audience believing their campaign is targeted when it is not.
	// `!= campaignKindSearch` rather than a per-channel list, for the same reason: the
	// drop is a property of not being on the Search cascade, so a fence naming one
	// channel would silently admit the next one added.
	if len(keywords) > 0 && kind != campaignKindSearch {
		return nil, fmt.Errorf("google-ads: ad-group keywords are not supported on %s (this client attaches keyword criteria only on the Search cascade); omit Keywords, or create a Search campaign to target queries", kind)
	}
	if len(audienceSegments) > 0 && kind != campaignKindSearch {
		return nil, fmt.Errorf("google-ads: audience segments are not supported on %s (this client attaches audience criteria only on the Search cascade); omit AudienceSegments, or create a Search campaign to target audiences", kind)
	}
	// AFTER validateCPCBid, because the bidding plan refuses a CPC bid under an
	// automated strategy and the caller is better served by hearing that their bid is
	// out of range than that it is incompatible with a strategy they would then fix
	// only to meet the range error on the next attempt.
	bidding, err := validateBiddingPlan(kind, c.account.CustomerID, in)
	if err != nil {
		return nil, err
	}
	startDateTime, endDateTime, err := validateFlightWindow(in.StartDate, in.EndDate)
	if err != nil {
		return nil, err
	}
	// The ad group list is resolved LAST of the pre-mutate block because it is the
	// only validator that needs other preflight results: every per-group fallback
	// — the composed name, the bid, the keywords, the audiences, the ad copy —
	// comes from the single group the campaign-level fields already produced, so
	// "inherit" means exactly what the single-group path would have done.
	adGroups, err := validateAdGroupPlans(kind, in, adGroupPlan{
		name:             adGroupName,
		cpcBidMicros:     cpcBidMicros,
		keywords:         keywords,
		audienceSegments: audienceSegments,
		ads:              []adPlan{{headlines: headlines, descriptions: descriptions}},
	})
	if err != nil {
		return nil, err
	}
	return &campaignPreflight{
		amountMicros:     amountMicros,
		budgetName:       budgetName,
		campaignName:     campaignName,
		finalURL:         finalURL,
		adGroupName:      adGroupName,
		headlines:        headlines,
		descriptions:     descriptions,
		keywords:         keywords,
		audienceSegments: audienceSegments,
		geo:              geo,
		criteria:         criteria,
		assets:           assets,
		creative:         creative,
		pmax:             pmax,
		adGroups:         adGroups,
		negativeKeywords: negativeKeywords,
		cpcBidMicros:     cpcBidMicros,
		bidding:          bidding,
		startDateTime:    startDateTime,
		endDateTime:      endDateTime,
	}, nil
}

// CreateCampaign creates a PAUSED Google Ads search campaign as a four-resource
// cascade: a non-shared campaign budget, a campaign referencing that budget, an ad
// group under the campaign, and a responsive search ad in that ad group. Everything
// is created PAUSED so nothing serves until a human enables it.
//
// Each stage may leave a PARTIAL result: the returned *CampaignResult is populated
// as far as the cascade got, and past the campaign stage a failure is returned
// ALONGSIDE a non-nil result rather than as (nil, err), so the caller can record
// what exists upstream. See the ad group/ad stage below for that contract.
//
// Because :mutate has no idempotency key, every failure is classified by whether
// the request may have committed upstream (createOutcomeAmbiguous). An ambiguous
// budget/campaign failure — a mutating 3xx/5xx or a transport error, or a 2xx with
// no resourceName — is reported UNCONFIRMED (verify before retrying) rather than a
// clean failure, and once the budget exists its id is returned in a partial result
// so the orphan is reconcilable. A definite 4xx means only THAT mutate was rejected,
// NOT that nothing was created: a 4xx on the SECOND (campaign) mutate still leaves
// the budget from the FIRST mutate committed, and the returned partial result
// carries that budget id so the caller can reconcile the orphan. (Only a 4xx on the
// first/budget mutate means nothing was created.) A DUPLICATE_NAME 4xx on a retry
// with a stable NameSuffix is surfaced as UNCONFIRMED-already-exists (the resource
// likely exists from a prior attempt; reconcile by name rather than treating it as
// created here).
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	pf, err := c.preflightCampaign(in)
	if err != nil {
		return nil, err
	}
	amountMicros, budgetName, campaignName := pf.amountMicros, pf.budgetName, pf.campaignName
	// The ad group, its bid, its copy and its targeting are no longer read
	// individually here: validateAdGroupPlans has already folded them into
	// pf.adGroups as the first (and, absent CampaignInput.AdGroups, only) group,
	// and the cascade walks that list rather than the scalars.
	finalURL := pf.finalURL
	geo := pf.geo
	negativeKeywords := pf.negativeKeywords
	startDateTime, endDateTime := pf.startDateTime, pf.endDateTime

	var steps []string
	googleAdsURL := "https://ads.google.com/aw/campaigns?ocid=" + c.account.CustomerID

	// campaignNamePartial carries BOTH deterministic names (no ids yet) so an
	// ambiguous/duplicate budget or campaign create is reconcilable by name rather
	// than discarded. CampaignBudgetName is the reliable reconcile key ONLY for a
	// PRE-ATTACHMENT (budget-stage) failure — before the campaign attaches, the budget
	// carries `budgetName`. Once the campaign attaches, a non-shared
	// (explicitlyShared=false) budget's name SYNCHRONIZES to the campaign name, so at a
	// campaign-stage ambiguous failure the budget's current name is unknown (it may be
	// campaignName). That is why the budget-stage partial (`budgetPartial`) also carries
	// CampaignBudgetID: past attachment, reconcile the budget by ID, not name. Mirrors
	// the meta/twitter name-only partials.
	campaignNamePartial := func() *CampaignResult {
		return &CampaignResult{
			Platform:     "google-ads",
			AccountLabel: c.account.Label,
			// Stamped on the PARTIAL, not just the success result: every returned result —
			// including every ambiguous/duplicate partial — descends from this closure, so a
			// caller reconciling a possibly-created campaign knows which account to look in.
			CustomerID:         c.account.CustomerID,
			CampaignName:       campaignName,
			CampaignBudgetName: budgetName,
			GoogleAdsURL:       googleAdsURL,
			Steps:              steps,
		}
	}

	// If the caller's context is ALREADY cancelled/expired before the first mutate,
	// nothing has been sent — return a clean (nil, err) rather than firing the request.
	// doRequest would otherwise fail inside httpClient.Do and classify it as an
	// ambiguous transportError → UNCONFIRMED, wrongly implying the budget MIGHT exist.
	// (This is only observable when the OAuth token is already cached: with no cached
	// token the token fetch surfaces the ctx error pre-send anyway, but the cached-token
	// path reaches httpClient.Do directly, so guard it explicitly here.)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads campaign creation aborted before any request (context already done): %w", ctxErr)
	}

	// Step 1: create the campaign budget.
	shared := false
	budgetReq := mutateRequest{Operations: []mutateOperation{{Create: campaignBudgetCreate{
		Name:             budgetName,
		AmountMicros:     amountMicros,
		DeliveryMethod:   "STANDARD",
		ExplicitlyShared: &shared,
	}}}}
	budgetPath := c.customerPath("campaignBudgets:mutate")
	budgetResp, err := c.doRequest(ctx, http.MethodPost, budgetPath, budgetReq, false)
	if err != nil {
		switch {
		case isDuplicateBudgetNameErr(err):
			// A retry with a stable NameSuffix hit a name that already exists: the
			// budget was (almost certainly) created by a prior attempt. Not created
			// here, but NOT a clean failure either — reconcile by name.
			return campaignNamePartial(), fmt.Errorf("google-ads campaign budget %q already exists (DUPLICATE_NAME) — a prior attempt likely created it; verify in Google Ads before retrying: %w", budgetName, err)
		case createOutcomeAmbiguous(err):
			return campaignNamePartial(), fmt.Errorf("google-ads campaign budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
		default:
			return nil, fmt.Errorf("google-ads campaign budget creation failed: %w", err)
		}
	}
	budgetResource, budgetID, err := firstResourceName(budgetResp)
	if err != nil {
		// A 2xx with no/malformed resourceName is a malformed success: the budget MAY
		// have been created. UNCONFIRMED, not a clean failure.
		return campaignNamePartial(), fmt.Errorf("google-ads campaign budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
	}
	steps = append(steps, fmt.Sprintf("Campaign budget created: %s (%.2f/day in account currency, STANDARD delivery, non-shared)", budgetID, in.Budget))

	// budgetPartial carries the created budget id (plus the campaign name) so an
	// ambiguous/failed CAMPAIGN create leaves the budget reconcilable, not orphaned
	// anonymously.
	budgetPartial := func() *CampaignResult {
		r := campaignNamePartial()
		r.CampaignBudgetID = budgetID
		return r
	}

	// The budget is now committed. If the caller's context has already been
	// cancelled/timed out, do NOT fire the campaign :mutate — surface the created
	// budget as a reconcilable partial + UNCONFIRMED, so a retry reconciles the
	// orphan budget by name rather than blind-proceeding on a dead context.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return budgetPartial(), fmt.Errorf("google-ads campaign creation aborted after budget %s created (context done before campaign create; the budget may need reconciling): %w", budgetID, ctxErr)
	}

	// Step 2: create the campaign referencing the budget.
	campaignCreateVal := campaignCreate{
		Name:                           campaignName,
		Status:                         "PAUSED",
		AdvertisingChannelType:         advertisingChannelSearch,
		CampaignBudget:                 budgetResource,
		ContainsEuPoliticalAdvertising: euPoliticalAdvertisingNo,
		NetworkSettings:                networkSettings{TargetGoogleSearch: true},
		// Always set, not only when geo targets are supplied: an untargeted campaign is
		// unaffected (there are no location criteria for it to qualify), and a campaign that
		// gains criteria later — by adoption, or by a human adding them in the Google Ads UI —
		// then restricts by presence rather than silently reverting to the permissive default.
		GeoTargetTypeSetting: geoTargetTypeSetting{PositiveGeoTargetType: geoTargetPresence},
		// Resolved in the preflight. With no BiddingStrategy supplied this is
		// `manualCpc:{}` and nothing else — the exact payload this create sent before
		// the strategy was selectable.
		biddingFields: pf.bidding.fields(),
		// Both omitempty: an empty string here is "the caller gave no such date", and the
		// field disappears rather than being sent empty — which Google rejects outright.
		StartDateTime: startDateTime,
		EndDateTime:   endDateTime,
	}
	campaignReq := mutateRequest{Operations: []mutateOperation{{Create: campaignCreateVal}}}
	campaignPath := c.customerPath("campaigns:mutate")
	campaignResp, err := c.doRequest(ctx, http.MethodPost, campaignPath, campaignReq, false)
	if err != nil {
		switch {
		case isDuplicateCampaignNameErr(err):
			return budgetPartial(), fmt.Errorf("google-ads campaign %q already exists (DUPLICATE_CAMPAIGN_NAME; budget %s created) — a prior attempt likely created it; verify in Google Ads before retrying: %w", campaignName, budgetID, err)
		case createOutcomeAmbiguous(err):
			return budgetPartial(), fmt.Errorf("google-ads campaign creation UNCONFIRMED (budget %s created; campaign %q may exist — verify in Google Ads before retrying): %w", budgetID, campaignName, err)
		default:
			return budgetPartial(), fmt.Errorf("google-ads campaign creation failed (budget %s created): %w", budgetID, err)
		}
	}
	campaignResource, campaignID, err := firstResourceName(campaignResp)
	if err != nil {
		return budgetPartial(), fmt.Errorf("google-ads campaign creation UNCONFIRMED (budget %s created; 2xx with no/malformed resource name — a campaign may exist; verify in Google Ads before retrying): %w", budgetID, err)
	}
	// Validate that the campaign resource is in the exact current-account campaign shape
	// before forwarding it to createAdGroupAndAd. A malformed 2xx such as
	// customers/1234567890/adGroups/222 would be treated as a confirmed campaign and
	// only fail after the real campaign has been created, persisting an untrustworthy ID.
	if err := c.validateCampaignResource(campaignResource); err != nil {
		return budgetPartial(), fmt.Errorf("google-ads campaign creation UNCONFIRMED (budget %s created; malformed campaign resource name %q — verify in Google Ads before retrying): %w", budgetID, campaignResource, err)
	}
	// The strategy is named from the PLAN, not from a literal: a step line that said
	// "manual CPC" on a campaign created with maximize-conversions would be the only
	// record of the bid an operator ever reads, and it would be wrong.
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, SEARCH, %s, %s)", campaignID, pf.bidding.describe(), flightWindowStep(startDateTime, endDateTime)))
	if n := len(pf.bidding.conversionActions); n > 0 {
		steps = append(steps, fmt.Sprintf("Conversion actions attached: %d (campaign optimizes toward these instead of the account goals)", n))
	}

	res := budgetPartial()
	res.CampaignID = campaignID
	res.Steps = steps

	// Location criteria go on the CAMPAIGN for Search (Demand Gen differs — see
	// geo.go). Attached here, immediately after the campaign create and BEFORE the
	// ad group, so a geo failure surfaces with as little built on top of it as
	// possible. Like every step past the campaign create, a failure is returned
	// ALONGSIDE the non-nil res: the campaign exists and is PAUSED either way.
	if !geo.empty() {
		geoIDs, geoErr := c.createCampaignGeoTargeting(ctx, campaignResource, campaignID, geo)
		if geoErr != nil {
			return res, geoErr
		}
		res.GeoCriterionIDs = geoIDs
		steps = append(steps, fmt.Sprintf("Geo targeting applied: %d location criteria (%s)", len(geoIDs), geoStep(in, geo)))
		res.Steps = steps
	}

	// Negative keywords are campaign-level criteria, so they are attached here —
	// after geo, still before the ad group — for the same reason geo is: the less
	// that has been built on top of a failure, the easier it is to reconcile. Same
	// partial-result contract as every step past the campaign create.
	if len(negativeKeywords) > 0 {
		negIDs, negErr := c.createCampaignNegativeKeywords(ctx, campaignResource, campaignID, negativeKeywords)
		if negErr != nil {
			return res, negErr
		}
		res.NegativeKeywordCriteriaIDs = negIDs
		steps = append(steps, fmt.Sprintf("Negative keywords applied: %d campaign-level exclusions", len(negIDs)))
		res.Steps = steps
	}

	// Language, ad schedule, device and demographic criteria are campaign-level too,
	// and go in last of the three criteria steps — still before the ad group, so a
	// failure has as little built on top of it as possible. Separate mutate from both
	// of the above, for the reason createCampaignTargetingCriteria documents. Same
	// partial-result contract: the campaign exists either way.
	if !pf.criteria.empty() {
		critIDs, critErr := c.createCampaignTargetingCriteria(ctx, campaignResource, campaignID, pf.criteria)
		if critErr != nil {
			return res, critErr
		}
		res.TargetingCriterionIDs = critIDs
		steps = append(steps, fmt.Sprintf("Campaign targeting applied: %d criteria (%s)", len(critIDs), criteriaStep(pf.criteria)))
		res.Steps = steps
	}

	// Ad extensions are the last campaign-level step before the ad group. They go
	// AFTER the criteria rather than before because they are the only step here
	// that takes two mutates and can leave something behind (unlinked assets) —
	// the less that is already built when that happens, the cheaper the
	// reconciliation. Same partial-result contract: the campaign exists either way,
	// and the asset ids are reported even when the linking half fails.
	if !pf.assets.empty() {
		assetIDs, linkIDs, assetErr := c.createCampaignAssets(ctx, campaignResource, campaignID, pf.assets)
		res.ExtensionAssetIDs = assetIDs
		if assetErr != nil {
			return res, assetErr
		}
		res.ExtensionLinkIDs = linkIDs
		steps = append(steps, fmt.Sprintf("Ad extensions applied: %d assets (%s)", len(linkIDs), assetStep(pf.assets)))
		res.Steps = steps
	}

	// The campaign+budget are now committed. GA-3: extend the shell with a PAUSED
	// ad group + responsive search ad per planned group. Any failure here
	// (ambiguous, duplicate, or definite) is returned ALONGSIDE the now-non-nil
	// res — the campaign/budget exist regardless, so this can never become a
	// (nil, err) return past this point. The caller (GoogleAdsDispatcher.Dispatch)
	// treats a non-nil result + error as "retain the claim, record the partial" —
	// the same contract already used for an ambiguous/duplicate budget or campaign.
	//
	// The plan line is recorded BEFORE the cascade runs, not after: a failure on
	// the third of four groups returns a partial result, and the operator reading
	// it needs to know four were intended. res.Steps is appended to directly from
	// here on — createAdGroupAndAd and the targeting step do the same, so the
	// local `steps` slice is no longer the live one past this point.
	if len(pf.adGroups) > 1 {
		res.Steps = append(res.Steps, "Ad group split planned: "+adGroupPlanStep(pf.adGroups))
	}
	if err := c.createAdGroupsAndAds(ctx, campaignResource, campaignID, finalURL, pf.adGroups, res); err != nil {
		return res, err
	}
	return res, nil
}

// firstResourceName decodes a :mutate response and returns
// (results[0].resourceName, its trailing id). It errors if the body is malformed,
// carries no result/resourceName, OR the resourceName is present but MALFORMED
// (e.g. "customers/123/campaigns/" or "noslash") such that no id can be extracted —
// accepting that would let creation continue with an empty, unreconcilable id or
// report success with a blank id. The caller treats the error as UNCONFIRMED.
func firstResourceName(body []byte) (resourceName, id string, err error) {
	var mr mutateResponse
	if uErr := json.Unmarshal(body, &mr); uErr != nil {
		return "", "", fmt.Errorf("decode mutate response: %w", uErr)
	}
	if len(mr.Results) == 0 || mr.Results[0].ResourceName == "" {
		return "", "", fmt.Errorf("mutate response carried no resource name")
	}
	rn := mr.Results[0].ResourceName
	rid := resourceID(rn)
	if rid == "" {
		return "", "", fmt.Errorf("mutate response resource name %q is malformed (no id segment)", rn)
	}
	return rn, rid, nil
}

// validateCampaignResource validates that a resource name is exactly the
// current-account campaign resource shape: customers/{currentCustomerID}/campaigns/{numericID}.
// A malformed 2xx such as customers/1234567890/adGroups/222 could otherwise be
// accepted as a confirmed campaign and only fail later when forwarded to adGroups:mutate,
// after the real campaign has been created. This guard ensures that only a trustworthy
// campaign resource is persisted.
func (c *Client) validateCampaignResource(resourceName string) error {
	return c.validateResourceKind("campaigns", resourceName, true)
}

// validateResourceKind validates that resourceName is exactly the current-account
// resource shape customers/{currentCustomerID}/{kind}/{id}, checking segment count,
// resource kind, and that the resource belongs to THIS account. A malformed or
// wrong-account 2xx (e.g. a different customer's adGroups resource, or a campaigns
// resource returned where an adGroups resource was expected) could otherwise be
// accepted as confirmed and persisted, or forwarded into a later mutate/resourceName
// only to fail confusingly downstream.
//
// requireNumericID validates the trailing id segment is a plain numeric id — set
// false for composite trailing segments (e.g. adGroupAds' "{adGroupId}~{adId}"),
// whose shape is validated separately by adGroupAdID, the only composite splitter
// in this package.
func (c *Client) validateResourceKind(kind, resourceName string, requireNumericID bool) error {
	pathParts := strings.Split(resourceName, "/")
	// Require exactly 4 segments: customers, {id}, {kind}, {id}
	if len(pathParts) != 4 {
		return fmt.Errorf("%s resource name %q has %d segments, want exactly 4", kind, resourceName, len(pathParts))
	}
	if pathParts[0] != "customers" || pathParts[2] != kind {
		return fmt.Errorf("%s resource name %q has wrong resource kind (want customers/.../%s/...)", kind, resourceName, kind)
	}
	if pathParts[1] != c.account.CustomerID {
		return fmt.Errorf("%s resource name %q is from a different account (want customers/%s/...)", kind, resourceName, c.account.CustomerID)
	}
	if requireNumericID && !numericID(pathParts[3]) {
		return fmt.Errorf("%s resource name %q has a non-numeric id", kind, resourceName)
	}
	return nil
}

// ComposeName builds a deterministic budget/campaign name from the input. The
// NameSuffix (when supplied) makes it unique+stable per logical campaign so a retry
// collides on DUPLICATE_NAME rather than silently double-creating. It is exported so
// the dispatcher can compute the campaign name before calling FindCampaignByName, for
// adopt-on-create idempotency (LFXV2-3042).
func ComposeName(kind string, in CampaignInput) string {
	parts := []string{"LFX", kind}
	if p := sanitizeNamePart(in.Project); p != "" {
		parts = append(parts, p)
	}
	if e := sanitizeNamePart(in.EventName); e != "" {
		parts = append(parts, e)
	}
	if s := sanitizeNamePart(in.NameSuffix); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, " | ")
}

// sanitizeNamePart trims a caller-supplied name segment and strips the "|"
// delimiter: a raw "|" in Project/EventName/NameSuffix would inject extra fields
// into the pipe-delimited composed name and break project attribution / name-based
// reconciliation (which split on "|"). Mirrors the meta/twitter/reddit builders'
// delimiter sanitization. It also replaces ANY control character (incl. NUL, which
// Google Ads v23 explicitly forbids in a name; strings.Fields only folds the
// whitespace control chars like CR/LF, not NUL) with a space, so an embedded control
// char can't reach a paid :mutate as a guaranteed-invalid name. All runs of the
// resulting whitespace are collapsed to a single space.
func sanitizeNamePart(s string) string {
	s = strings.ReplaceAll(s, "|", " ")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

// validateEntityName rejects an empty or over-length composed name before any
// create call. Google enforces DIFFERENT length units per resource (Campaign.name in
// characters, CampaignBudget.name in UTF-8 bytes), so the caller passes the measured
// length and the unit label; measuring in the wrong unit would let a multibyte name
// slip past the budget's byte ceiling (or reject a valid campaign name early).
func validateEntityName(kind, name string, measuredLen, maxLen int, unit string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("google-ads %s name is empty", kind)
	}
	if measuredLen > maxLen {
		return fmt.Errorf("google-ads %s name exceeds %d %s (%d): shorten EventName/Project/NameSuffix", kind, maxLen, unit, measuredLen)
	}
	return nil
}

// Campaign run states in the Google Ads vocabulary. Note ENABLED (not "ACTIVE") — the
// create path uses PAUSED from this same enum.
const (
	// StatusEnabled lets a campaign serve.
	StatusEnabled = "ENABLED"
	// StatusPaused stops a campaign serving.
	StatusPaused = "PAUSED"
	// StatusRemoved is Google's tombstone state. A removed campaign still matches a
	// name query and can never serve or be re-enabled, so FindCampaignByName must not
	// adopt one or read it as an idempotent create hit.
	StatusRemoved = "REMOVED"
)

// campaignStatusUpdate is the update payload for campaigns:mutate. resourceName identifies
// the campaign; only the fields named in the operation's updateMask are applied.
type campaignStatusUpdate struct {
	ResourceName string `json:"resourceName"`
	Status       string `json:"status"`
}

// IsOutcomeUnconfirmed reports whether err leaves the mutation's outcome AMBIGUOUS — Google
// may have applied it despite the error, so the caller must VERIFY before retrying rather
// than assume "not applied". Exported so the dispatcher can classify across the package
// boundary (mirrors the reddit/twitter clients' helper of the same name).
func IsOutcomeUnconfirmed(err error) bool {
	// An explicit NOT-ATTEMPTED marker wins over everything below, including the
	// Unconfirmed() check: it is set only by a call site that knows no request was sent, and
	// it is the one fact the shape-based inference cannot recover. Without this arm a
	// pre-mutation read failure (resolveKeywordCriteria's GAQL query, which fails as a
	// transportError or 5xx just like a mutate does) would be reported as "the changes may
	// have been applied" even though adGroupCriteria:mutate was never built.
	var na interface{ NotAttempted() bool }
	if errors.As(err, &na) && na.NotAttempted() {
		return false
	}
	var u interface{ Unconfirmed() bool }
	if errors.As(err, &u) && u.Unconfirmed() {
		return true
	}
	return createOutcomeAmbiguous(err)
}

// UpdateCampaignStatus toggles a campaign between ENABLED and PAUSED via campaigns:mutate
// with an updateMask of "status".
//
// This method flips ONLY the campaign — it does not cascade to the ad group/ad. The cascade
// is implemented in GoogleAdsDispatcher.ToggleStatus (dispatch/googleads.go), and the two
// directions cascade in OPPOSITE order so neither leaves a parent enabled ahead of its
// children: PAUSE goes campaign-first (stopping delivery immediately) then the ad group/ad;
// ACTIVATE goes children-first, campaign last, so the campaign only reports ENABLED once its
// ad group and ad already do.
//
// Kept as two client methods (rather than one combined call) because the ad group/ad may
// legitimately not exist (a duplicate-name orphan from GA-3b's create path — see
// createAdGroupAndAd) — the dispatcher's activate guard (ErrCampaignNotProvisioned) checks
// for that BEFORE calling either method. Additionally, keeping them separate lets a caller
// pause a campaign whose children failed to create, without that call depending on child ids
// it may not have. ACTIVATE is no longer rejected unconditionally: with GA-4's targeting
// provisioning in place, the dispatcher's guard now rejects only a campaign that is actually
// missing its ad group/ad ids or its keyword criteria (ErrCampaignNotProvisioned), and
// activates every campaign that has them.
//
// The mutate IS sent as idempotent (doRequest's last arg), unlike the create path. That flag
// gates only bounded 429 retries, and the create path's reason for declining them (no
// idempotency key, so a throttled retry could DOUBLE-CREATE) does not apply to a status flip:
// re-applying the same ENABLED/PAUSED converges on identical state. See the call site below.
func (c *Client) UpdateCampaignStatus(ctx context.Context, campaignID, status string) error {
	if err := c.validateAccountIDs(); err != nil {
		return err
	}
	if status != StatusEnabled && status != StatusPaused {
		return fmt.Errorf("google-ads: unsupported campaign status %q (want %s or %s)", status, StatusEnabled, StatusPaused)
	}
	id := strings.TrimSpace(campaignID)
	if id == "" {
		return fmt.Errorf("google-ads: cannot update status: campaign id is empty")
	}
	// The id is interpolated into a resourceName, so keep it strictly numeric — Google
	// campaign ids are digits, and anything else could alter the resource path.
	if !customerIDRE.MatchString(id) {
		return fmt.Errorf("google-ads: campaign id %q is not numeric", campaignID)
	}

	// NOTE on an already-done context: no explicit guard is needed here. doRequest ->
	// accessTokenValue checks ctx.Err() FIRST, before the cached-token fast path, so a
	// cancelled caller never reaches httpClient.Do and the error surfaces as a clean
	// context error rather than an ambiguous transportError. (CreateCampaign's own guard
	// predates that check and is now belt-and-braces.) Pinned by
	// TestGoogleAds_ToggleStatus_AlreadyCanceledContextSendsNothing, which primes the token
	// cache so it exercises exactly the cached path this note describes.

	campaignResource := "customers/" + c.account.CustomerID + "/campaigns/" + id
	req := mutateRequest{Operations: []mutateOperation{{
		Update: campaignStatusUpdate{
			ResourceName: campaignResource,
			Status:       status,
		},
		UpdateMask: "status",
	}}}
	// idempotent=TRUE, unlike the create path. That flag gates ONLY bounded 429 retries, and
	// the create path's reason for declining them (no idempotency key, so a 429 whose first
	// attempt may have committed would DOUBLE-CREATE) does not apply here: re-applying the
	// same ENABLED/PAUSED value converges on the identical state. Passing false would turn
	// ordinary Google throttling into an avoidable UNCONFIRMED failure. A cancellation while
	// waiting to retry is WRAPPED by doRequest as a transportError and stays UNCONFIRMED,
	// which is correct: by then a mutation HAS been sent. That path is reachable without any
	// user cancellation, since the orchestrator wraps every toggle in a toggleCallTimeout —
	// pinned by TestUpdateCampaignStatus_CancelDuringBackoffIsUnconfirmed.
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaigns:mutate"), req, true)
	if err != nil {
		return fmt.Errorf("google-ads campaign %s status update to %s failed: %w", id, status, err)
	}
	// A 2xx that does not acknowledge the one operation is UNCONFIRMED, the same standard
	// the ad group and ad stages hold their own mutates to (checkStatusMutateResults in
	// adgroup_ad.go). Holding the CAMPAIGN stage to it matters most of the three: on PAUSE
	// it runs FIRST and its success is what gates the child cascade, so an unacknowledged
	// campaign flip taken as confirmed would send the children on from a state nobody
	// verified. Wrapped in a dedicated type rather than partialCascadeError, which asserts
	// the preceding stages succeeded — on PAUSE there are none, and the claim would be false.
	if cErr := c.checkStatusMutateResults(resp, "campaigns", "campaign", []string{campaignResource}, true); cErr != nil {
		return &unconfirmedCampaignStatusError{err: fmt.Errorf("google-ads campaign %s status update to %s: %w", id, status, cErr)}
	}
	return nil
}

// unconfirmedCampaignStatusError marks a campaign status mutate Google ACCEPTED but did not
// acknowledge usably. The flip may well have been applied, so it satisfies the same
// Unconfirmed() behavioral interface IsOutcomeUnconfirmed honors — exactly as
// unconfirmedBudgetMutateError and partialCascadeError do — and the dispatcher carries it
// through as "verify upstream before retrying" rather than "nothing was modified", which
// keeps the claim RETAINED. A retry re-applies the same idempotent status.
type unconfirmedCampaignStatusError struct{ err error }

func (e *unconfirmedCampaignStatusError) Error() string     { return e.err.Error() }
func (e *unconfirmedCampaignStatusError) Unwrap() error     { return e.err }
func (e *unconfirmedCampaignStatusError) Unconfirmed() bool { return true }
