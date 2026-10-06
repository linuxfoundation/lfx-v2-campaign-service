// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// storedCustomerIDRE is the shape a STORED Google Ads account id must have: digits
// only, no dashes, spaces, or grouping. The client now exports googleads.ValidateCustomerID
// for a caller-supplied id (see this file's account-monitor path), but this copy stays
// separate for the STORED-value check below: a malformed stored value must classify as
// domain.ErrConnectionNotUsable (a broken connection row), never as
// domain.ErrAccountIDMalformed (a caller mistake) — sharing one helper would conflate
// those two error paths. The two regexes must stay in step — widen one and you must
// widen the other.
var storedCustomerIDRE = regexp.MustCompile(`^[0-9]+$`)

// googleAdsCreds is the credential shape stored (encrypted) for a Google Ads
// connection. Google Ads authenticates with an OAuth2 application (client id/secret)
// plus a long-lived refresh token, AND a Google Ads API developer token. Field names
// mirror the generated GoogleAdsCredentials struct EXACTLY (no json tags): the
// connection service persists creds via json.Marshal on the tag-less generated struct,
// so the stored JSON keys are the Go field names (PascalCase) — matching them
// field-for-field avoids relying on encoding/json's case-insensitive fallback (see
// redditCreds).
type googleAdsCreds struct {
	ClientID       string
	ClientSecret   string
	DeveloperToken string
	RefreshToken   string
}

// googleAdsKeywordConfig is one entry in googleAdsConfig.Keywords — the JSON shape a
// caller supplies for a positive Search keyword criterion (GA-4). Maps 1:1 to
// googleads.Keyword; kept as a separate JSON-tagged type here rather than importing
// googleads' struct directly, mirroring how the rest of this file keeps the wire
// shape and the platform client's Go type independently named.
type googleAdsKeywordConfig struct {
	Text      string `json:"text"`
	MatchType string `json:"matchType"`
}

// The wire shapes for the targeting, extension and ad-group inputs the Search path
// gained with LFXV2-2665. Each maps 1:1 to its googleads counterpart and is kept as
// its own JSON-tagged type for the same reason googleAdsKeywordConfig is: the wire
// vocabulary and the platform client's Go type are named independently, so one can
// change without silently changing the other. Every field is validated by the client
// before anything is sent — nothing here re-implements a check.

// googleAdsProximityConfig is one radius target: a point and how far around it to
// serve. RadiusUnit has deliberately no default (see googleads.ProximityTarget).
type googleAdsProximityConfig struct {
	Latitude   float64 `json:"latitude"`
	Longitude  float64 `json:"longitude"`
	Radius     float64 `json:"radius"`
	RadiusUnit string  `json:"radiusUnit"` // "MILES" or "KILOMETERS"
}

// googleAdsAdScheduleConfig is one day-of-week serving interval.
//
// BidModifier is a POINTER on the wire as well as in the client, and for the same
// reason: 0 is the -100% opt-out, so it cannot double as "unset". An absent
// "bidModifier" restricts WHEN the campaign serves without changing what it bids.
type googleAdsAdScheduleConfig struct {
	DayOfWeek   string   `json:"dayOfWeek"`
	StartHour   int      `json:"startHour"`
	StartMinute int      `json:"startMinute"`
	EndHour     int      `json:"endHour"`
	EndMinute   int      `json:"endMinute"`
	BidModifier *float64 `json:"bidModifier"`
}

// googleAdsDeviceBidModifierConfig adjusts the bid for one device, or excludes that
// device outright with a modifier of 0.
//
// BidModifier is a POINTER here while googleads.DeviceBidModifier's is a plain
// float64, and the asymmetry is deliberate. A Go caller constructing the client type
// writes the field it means; a JSON caller can simply OMIT it, and a plain float64
// would decode that absence to 0 — which is not "no adjustment" but the -100%
// opt-out, so `{"device":"TABLET"}` would stop the campaign serving on tablets with
// no error anywhere. The pointer keeps absent distinguishable from zero at the only
// boundary where the two can be confused; googleAdsDeviceBidModifiers refuses nil.
// Same reasoning as googleAdsAdScheduleConfig.BidModifier.
type googleAdsDeviceBidModifierConfig struct {
	Device      string   `json:"device"`
	BidModifier *float64 `json:"bidModifier"`
}

// googleAdsSitelinkConfig is one sitelink extension. The two description lines are
// all-or-nothing: Google rejects an asset carrying only one.
type googleAdsSitelinkConfig struct {
	Text         string `json:"text"`
	Description1 string `json:"description1"`
	Description2 string `json:"description2"`
	FinalURL     string `json:"finalUrl"`
}

// googleAdsStructuredSnippetConfig is one structured-snippet extension. Header must be
// one of Google's predefined headers FOR THE CAMPAIGN'S LANGUAGE, so the client checks
// its shape rather than its membership in an English list.
type googleAdsStructuredSnippetConfig struct {
	Header string   `json:"header"`
	Values []string `json:"values"`
}

// googleAdsCallExtensionConfig is one call extension. countryCode is required
// because Google interprets the number against it — the same number means
// different things in different countries, and there is no default to pick.
type googleAdsCallExtensionConfig struct {
	CountryCode string `json:"countryCode"`
	PhoneNumber string `json:"phoneNumber"`
}

// googleAdsPromotionConfig is one promotion extension.
//
// discountPercent and discountAmount are two arms of a Google oneof: exactly one
// is set, and the client refuses both and neither. promotionCode and
// ordersOverAmount are the other oneof, where BOTH absent is fine. currencyCode
// is required whenever either money field is used. occasion is Google's own
// enum name, checked for shape and not for membership — Google extends that
// enum without this client's involvement, so a hardcoded list would refuse a
// create Google would accept, the same reasoning as the snippet header above.
type googleAdsPromotionConfig struct {
	PromotionTarget     string  `json:"promotionTarget"`
	DiscountPercent     float64 `json:"discountPercent"`
	DiscountAmount      float64 `json:"discountAmount"`
	OrdersOverAmount    float64 `json:"ordersOverAmount"`
	CurrencyCode        string  `json:"currencyCode"`
	PromotionCode       string  `json:"promotionCode"`
	Occasion            string  `json:"occasion"`
	LanguageCode        string  `json:"languageCode"`
	StartDate           string  `json:"startDate"`
	EndDate             string  `json:"endDate"`
	RedemptionStartDate string  `json:"redemptionStartDate"`
	RedemptionEndDate   string  `json:"redemptionEndDate"`
	FinalURL            string  `json:"finalUrl"`
}

// googleAdsPriceOfferingConfig is one row of a price extension. Each row carries
// its OWN destination — Google serves them as separate clickable entries.
type googleAdsPriceOfferingConfig struct {
	Header       string  `json:"header"`
	Description  string  `json:"description"`
	Amount       float64 `json:"amount"`
	CurrencyCode string  `json:"currencyCode"`
	Unit         string  `json:"unit"`
	FinalURL     string  `json:"finalUrl"`
}

// googleAdsPriceConfig is one price extension: a typed table of 3-8 offerings.
// type and priceQualifier are enum names checked for shape, as occasion is.
type googleAdsPriceConfig struct {
	Type           string                         `json:"type"`
	PriceQualifier string                         `json:"priceQualifier"`
	LanguageCode   string                         `json:"languageCode"`
	Offerings      []googleAdsPriceOfferingConfig `json:"offerings"`
}

// googleAdsAdConfig is one responsive search ad inside an ad group. Both lists are
// optional in exactly the way the campaign-level headlines/descriptions are — the
// client pads missing slots to Google's minimums.
type googleAdsAdConfig struct {
	Headlines    []string `json:"headlines"`
	Descriptions []string `json:"descriptions"`
}

// googleAdsAdGroupConfig is one ad group: a theme, the keywords that express it, and
// the ads written for it.
//
// Every field except Name falls back to the campaign-level equivalent when it is
// empty, and that fallback is PER FIELD — a group may override only its bid. cpcBid 0
// means "inherit", not "no bid"; a campaign-level 0 is what means that, and it is
// inherited as such.
type googleAdsAdGroupConfig struct {
	Name             string                   `json:"name"`
	CPCBid           float64                  `json:"cpcBid"`
	Keywords         []googleAdsKeywordConfig `json:"keywords"`
	AudienceSegments []string                 `json:"audienceSegments"`
	Ads              []googleAdsAdConfig      `json:"ads"`
}

const (
	// The accepted `channel` values. Lower-case and hyphenated to match the campaignTypes
	// vocabulary the UI already uses ("search" / "demand-gen"), so callers do not have to
	// translate between two spellings of the same idea.
	googleAdsChannelSearch         = "search"
	googleAdsChannelDemandGen      = "demand-gen"
	googleAdsChannelPerformanceMax = "performance-max"
	googleAdsChannelVideo          = "video"
	googleAdsChannelDisplay        = "display"
	// Google's own advertising_channel_type enum spellings, the vocabulary the settings
	// readback compares in. googleAdsVariantForChannelType maps these back the other way.
	googleAdsChannelTypeSearch         = "SEARCH"
	googleAdsChannelTypeDemandGen      = "DEMAND_GEN"
	googleAdsChannelTypePerformanceMax = "PERFORMANCE_MAX"
	googleAdsChannelTypeVideo          = "VIDEO"
	googleAdsChannelTypeDisplay        = "DISPLAY"
)

// googleAdsConfig is the per-platform campaign config the caller passes for Google Ads
// in CreateCampaigns' Input.Config (delivered here as the Dispatch `config`).
//
// Which campaign the client creates depends on Channel: the default (absent/"search")
// is a PAUSED Search campaign with an ad group + a Responsive Search Ad (GA-3b), which
// can carry keyword/audience targeting (GA-4); "demand-gen" is a PAUSED Demand Gen
// campaign with an ad group and NO ad or keywords (LFXV2-3257); "performance-max" is a
// PAUSED Performance Max campaign with NO ad group and NO ad at all — its creative is an
// ASSET GROUP built from performanceMaxCreative. Several fields below apply to the Search
// path only, and say so.
//
// Budget is in whole units of the ad ACCOUNT's currency (NOT USD — the client does no
// FX), mirroring the meta client.
type googleAdsConfig struct {
	Budget float64 `json:"budget"`
	// Channel selects which Google Ads campaign type to create: "" or "search" (the
	// default) creates the Search campaign with an ad group and a Responsive Search Ad;
	// "demand-gen" creates a Demand Gen campaign, which has no keywords because its
	// creatives are image assets; "performance-max" creates a Performance Max campaign,
	// which has neither ad groups nor ads — its creative is an asset group.
	//
	// ABSENT means SEARCH, deliberately. Every caller that predates this field omits it,
	// and they all mean Search — the value that has been hardcoded since GA-2. Making
	// absence mean anything else would silently repoint existing callers at a different
	// channel. An unrecognised value is REFUSED rather than defaulted, because defaulting
	// a typo'd "demandgen" to Search would spend the Demand Gen budget on Search ads and
	// report success.
	Channel string `json:"channel"`
	// Headlines/Descriptions are optional Responsive Search Ad copy overrides (GA-3b).
	// Left nil/empty, the client composes deterministic placeholder copy from the
	// brief's EventName/Project (see googleads.composeAdCopy).
	Headlines    []string `json:"headlines"`
	Descriptions []string `json:"descriptions"`
	// Keywords are optional positive Search keyword criteria (GA-4). Left empty, the
	// ad group created by GA-3 gets no criteria and can never serve — see
	// googleads.Keyword/validateKeywords.
	Keywords []googleAdsKeywordConfig `json:"keywords"`
	// AudienceSegments are optional EXISTING Google Ads audience resource names (GA-4)
	// — Customer Match user-list resources the caller has already built elsewhere,
	// not created by this dispatcher. See googleads.validateAudienceSegments for the
	// accepted shapes.
	AudienceSegments []string `json:"audienceSegments"`
	// NegativeKeywords are optional CAMPAIGN-level keyword exclusions — queries the
	// campaign must never pay for, however broadly Keywords match. SEARCH ONLY, and
	// REFUSED on every other channel rather than ignored — as Keywords and
	// AudienceSegments now are too: all four of the Search-only fields here refuse
	// off Search rather than being dropped. See
	// googleads.CampaignInput.NegativeKeywords.
	//
	// Left empty, no exclusions are created, which is the behaviour of every caller
	// that predates this field.
	NegativeKeywords []googleAdsKeywordConfig `json:"negativeKeywords"`
	// CPCBid is an optional manual CPC bid for the ad group, in whole units of the ad
	// ACCOUNT's currency (no FX conversion, same as Budget). Search only — no other
	// channel here has a manual bidding strategy, and their ad-group payloads carry no
	// bid field at all, so the value is REFUSED there rather than silently dropped.
	//
	// 0 or absent means no bid is sent and Google's own derivation stands, which is
	// what every campaign created before this field existed did. See
	// googleads.validateCPCBid for the accepted range.
	CPCBid float64 `json:"cpcBid"`
	// BiddingStrategy names how the campaign bids, in the labels the Google Ads UI
	// uses: `manual-cpc`, `maximize-clicks`, `maximize-conversions`, `target-cpa`,
	// `maximize-conversion-value`, `target-roas`.
	//
	// Absent or empty means the CHANNEL DEFAULT — manual CPC on Search, maximize
	// clicks on Demand Gen, maximize conversions on Performance Max — which is what
	// this adapter has always sent for the channels that predate this field, so an
	// older config produces a byte-identical campaign payload. Demand Gen accepts ONLY
	// `maximize-clicks`; see googleads.demandGenBiddingStrategies for the live-API
	// evidence behind that fence. Performance Max accepts only the four
	// conversion-oriented strategies and has no manual form at all; see
	// googleads.performanceMaxBiddingStrategies.
	BiddingStrategy string `json:"biddingStrategy"`
	// TargetCPA is the target cost per acquisition in whole units of the ad ACCOUNT's
	// currency (no FX conversion, same as Budget). Optional under
	// `maximize-conversions`, required under `target-cpa`, refused under any other
	// strategy rather than accepted and discarded.
	TargetCPA float64 `json:"targetCpa"`
	// TargetROAS is the target return on ad spend as a RATIO, not a percentage: 4.0
	// means four units of conversion value per unit spent. Optional under
	// `maximize-conversion-value`, required under `target-roas`, refused elsewhere.
	TargetROAS float64 `json:"targetRoas"`
	// ConversionActions are the conversion actions the campaign should optimize
	// toward, as bare numeric ids or full `customers/<id>/conversionActions/<id>`
	// resource names. Absent means the campaign inherits the ACCOUNT's conversion
	// goals, which is what every Google campaign this service has created does.
	//
	// Search only — campaign.selective_optimization is the create-time conversion
	// selection field and only Search takes it here; the other channels select
	// conversions through a campaign-conversion-goal resource this client does not yet
	// write, so the list is REFUSED there rather than dropped. The ids come from the
	// client's ListConversionActions read.
	ConversionActions []string `json:"conversionActions"`
	// StartDate/EndDate are the campaign's flight window as YYYY-MM-DD, spelled exactly
	// as the meta and reddit configs spell them. Each is independently optional: an
	// absent StartDate leaves Google's default (the campaign starts when enabled) and an
	// absent EndDate leaves it running until someone stops it.
	//
	// Google interprets both in the ad ACCOUNT's timezone, not UTC — which is also why
	// a past start date is not refused here; see googleads.validateFlightWindow. These
	// apply to EVERY channel.
	//
	// They are also what applyCampaignConfig records in the campaigns table's
	// start_date/end_date columns: before this field existed Google passed "" for both,
	// so those columns stayed NULL for every Google campaign and the settings readback
	// had nothing to compare its upstream dates against. Microsoft still passes "" for
	// both (internal/dispatch/microsoft.go), so Google is not the last adapter without a
	// window — only the one whose readback made the absence visible.
	StartDate string `json:"startDate"` // YYYY-MM-DD
	EndDate   string `json:"endDate"`   // YYYY-MM-DD
	// GeoTargets are ISO 3166-1 alpha-2 country codes the campaign should serve in
	// (LFXV2-3283), spelled exactly as the meta and reddit configs spell them so a
	// caller does not have to learn a third vocabulary for the same idea. The client
	// resolves each to a Google numeric geo target constant and attaches location
	// criteria at the level the channel requires — campaign for Search and Performance
	// Max, ad group for Demand Gen.
	//
	// Left empty, NO location criteria are created and the campaign serves wherever the
	// ad ACCOUNT's defaults allow, which for an event campaign is usually worldwide.
	// That is the pre-LFXV2-3283 behaviour and it is preserved deliberately: every
	// caller predating this field omits it, and failing their creates outright would
	// break dispatches that work today. An untargeted create is instead made VISIBLE —
	// see the warning log in Dispatch — rather than silently accepted or refused.
	// GeoTargets also accepts RAW Google geo target constant ids (the numeric strings
	// Google's geo target constant table publishes), which is how a caller targets a
	// city, region or postal code — a metro-level event campaign cannot be expressed in
	// country codes. The two spellings mix freely in one list.
	GeoTargets []string `json:"geoTargets"`
	// ExcludedGeoTargets are locations the campaign must NEVER serve in, in exactly the
	// vocabulary GeoTargets uses (country codes and/or raw constant ids). Applies to
	// EVERY channel.
	//
	// Left empty, nothing is excluded. A location listed in both lists is REFUSED rather
	// than resolved: Google lets the exclusion win, so the campaign would silently not
	// serve where the caller plainly asked it to.
	ExcludedGeoTargets []string `json:"excludedGeoTargets"`
	// ProximityTargets are radius targets — serve within N miles/kilometres of a point.
	// Refused on "demand-gen" alone, which attaches location criteria at the ad group
	// level where this client has not verified proximity; "performance-max" takes
	// campaign-level geo exactly as Search does, so radius targets are accepted there.
	//
	// Left empty, no radius criteria are created and the lists above stand alone.
	ProximityTargets []googleAdsProximityConfig `json:"proximityTargets"`
	// Languages are the languages the campaign serves in, as ISO 639-1 codes (EN, DE,
	// JA) or raw numeric language constant ids. All FIVE slices below — Languages,
	// AdSchedules, DeviceBidModifiers, ExcludedAgeRanges and ExcludedGenders — are
	// campaign-level criteria, and the channels differ: "demand-gen" refuses the whole
	// set, while "performance-max" takes languages and ad schedules (Google documents
	// them as PMax campaign criteria) and refuses only DeviceBidModifiers and the two
	// demographic exclusions. Refused, never silently dropped.
	//
	// Left empty the campaign serves in EVERY language, which is Google's default and
	// rarely what an event campaign wants.
	Languages []string `json:"languages"`
	// AdSchedules restrict WHEN the campaign serves, one interval per day of week, each
	// optionally carrying its own bid modifier. Left empty the campaign runs around the
	// clock.
	AdSchedules []googleAdsAdScheduleConfig `json:"adSchedules"`
	// DeviceBidModifiers adjust the bid per device ("MOBILE", "DESKTOP", "TABLET"), or
	// exclude a device outright with a modifier of 0. Left empty, every device bids the
	// campaign's own bid.
	//
	// There is no TV-screen entry: Google supports that device only on Display and Video
	// campaigns, and these criteria are refused on both Demand Gen and Performance Max,
	// so Search is the only channel that reaches them. See deviceTypes in the googleads
	// package.
	//
	// bidModifier is REQUIRED on every entry — it is a *float64 so that an OMITTED one
	// is refused rather than decoded as 0, which is the -100% opt-out and would switch
	// the device off while reporting a successful create. This is the opposite polarity
	// from adSchedules, where an absent modifier legitimately means "listed but
	// unadjusted"; a device entry with no modifier would say nothing at all.
	DeviceBidModifiers []googleAdsDeviceBidModifierConfig `json:"deviceBidModifiers"`
	// ExcludedAgeRanges and ExcludedGenders are demographic EXCLUSIONS ("18-24",
	// "MALE", …). Exclusion-only is Google's own shape at campaign level, not a
	// narrowing this adapter chose: there is no "target only these" form here. Left
	// empty, no demographic is excluded.
	ExcludedAgeRanges []string `json:"excludedAgeRanges"`
	ExcludedGenders   []string `json:"excludedGenders"`
	// Sitelinks, Callouts and StructuredSnippets are the campaign's ad extensions. All
	// Search only — refused on every other channel, none of which has a text ad to
	// extend — and all created as account-level assets then linked to the campaign.
	//
	// Left empty the ad serves as a bare headline+description, which costs more per
	// click than the same bid with extensions attached — but it is the behaviour of
	// every caller predating these fields, so absence is accepted, not refused.
	Sitelinks          []googleAdsSitelinkConfig          `json:"sitelinks"`
	Callouts           []string                           `json:"callouts"`
	StructuredSnippets []googleAdsStructuredSnippetConfig `json:"structuredSnippets"`
	// CallExtensions, Promotions and Prices are the other three extension types,
	// under exactly the same rules: optional, Search only, account-level assets
	// linked to the campaign. Image and location extensions are NOT here — see
	// internal/platform/googleads/assets_extended.go for why each is a gap rather
	// than an omission.
	CallExtensions []googleAdsCallExtensionConfig `json:"callExtensions"`
	Promotions     []googleAdsPromotionConfig     `json:"promotions"`
	Prices         []googleAdsPriceConfig         `json:"prices"`
	// AdGroups splits the campaign into one ad group per theme, each with its own
	// keywords and up to three responsive search ads. Search only.
	//
	// Left empty the client creates exactly the single ad group with the single ad it
	// created before this field existed, from the campaign-level Headlines/Descriptions/
	// Keywords/AudienceSegments/CPCBid — so this field is purely additive and changes
	// nothing for a caller that omits it.
	AdGroups []googleAdsAdGroupConfig `json:"adGroups"`
	// DemandGenCreative is the image-and-text creative for the Demand Gen ad. The
	// mirror image of Sitelinks/Callouts/StructuredSnippets above: DEMAND GEN ONLY,
	// refused on "search".
	//
	// Left empty the Demand Gen campaign is created without an ad, exactly as it was
	// before this field existed — and that campaign CANNOT SERVE until someone builds
	// an ad in the Google Ads UI. Absence is accepted rather than refused only because
	// every caller predating the field omits it; it is not a sensible configuration.
	DemandGenCreative *googleAdsDemandGenCreativeConfig `json:"demandGenCreative"`
	// PerformanceMaxCreative is the ASSET GROUP for a Performance Max campaign, the
	// third member of the same family and PERFORMANCE MAX ONLY.
	//
	// Left empty the campaign is created with no asset group — which, unlike a missing
	// Demand Gen ad, is also what adopting a campaign whose asset group was built by
	// hand upstream looks like. That campaign cannot serve until a group exists; the
	// result's closing step says so.
	PerformanceMaxCreative *googleAdsPerformanceMaxCreativeConfig `json:"performanceMaxCreative"`
	// VideoCreative is the responsive video ad for a Video (YouTube) campaign, the
	// fourth member of the same family and VIDEO ONLY.
	//
	// Left empty the campaign and its ad group are created with no ad — the same shape
	// a Demand Gen campaign without a creative has, and it cannot serve until someone
	// builds an ad in the Google Ads UI. The result's closing step says so.
	VideoCreative *googleAdsVideoCreativeConfig `json:"videoCreative"`
	// DisplayCreative is the responsive display ad for a Display campaign, the fifth
	// member of the same family and DISPLAY ONLY.
	//
	// Left empty the campaign and its ad group are created with no ad — the same shape a
	// Demand Gen or Video campaign without a creative has, and it cannot serve until
	// someone builds an ad in the Google Ads UI. The result's closing step says so.
	DisplayCreative *googleAdsDisplayCreativeConfig `json:"displayCreative"`
	// AdoptExisting opts THIS dispatch in to adopting a campaign that already carries the
	// composed name instead of creating one. It defaults to FALSE, and the default is the
	// safety property, not a convenience: ComposeName is deterministic in
	// Project/EventName/NameSuffix and does NOT change when the local campaign row is
	// soft-deleted, while getCampaignByPlatformQuery excludes deleted rows — so after a
	// delete the orchestrator reads the pair as "never dispatched" and takes the fresh-claim
	// path. An unconditional lookup would then silently re-attach to the still-live upstream
	// campaign the delete was meant to walk away from, and persist THIS request's
	// budget/config against it while pushing nothing upstream. Requiring the caller to ask
	// makes the one case where adoption is genuinely wanted — binding a campaign that
	// already exists on the account — an explicit act, and leaves the delete/re-dispatch
	// case to the create path, where a duplicate name surfaces as a visible
	// reconciliation outcome rather than a silent rebind.
	AdoptExisting bool `json:"adoptExisting"`
}

// GoogleAdsDispatcher creates Google Ads campaigns for the orchestrator.
type GoogleAdsDispatcher struct {
	creds *credsSource
	// clients caches the built *googleads.Client per connection. The credential cache alone does
	// not remove the OAuth exchange: the access token is cached ON the client instance, so a
	// rebuilt client re-mints it even when the credential was a cache hit (measured at five token
	// hits across five resolves before this). Entries are validated against the same row id and
	// version as the credential, so a rotated credential cannot be served through a stale client.
	//
	// Sharing one instance across concurrent callers is safe for this client specifically: the
	// only fields written after construction are the token cache and the in-flight refresh
	// handle, both exclusively under c.tokenMu (client.go accessTokenValue), and no method
	// stores per-call state on the receiver — the account config is written once at construction
	// and thereafter read-only. That is a per-client property, not a property of the cache; see
	// clientCache's doc comment.
	clients *clientCache
	opts    []googleads.Option
}

// NewGoogleAdsDispatcher builds the adapter from the connection repo + encryptor.
func NewGoogleAdsDispatcher(repo connReader, enc domain.Encryptor, opts ...googleads.Option) *GoogleAdsDispatcher {
	return &GoogleAdsDispatcher{creds: newCredsSource(repo, enc), clients: newClientCache(), opts: opts}
}

// Dispatch implements service.PlatformDispatcher for Google Ads.
func (d *GoogleAdsDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (camp *model.Campaign, err error) {
	// Resolve creds FIRST (pre-create): a missing/undecryptable connection is a
	// not-created error → the orchestrator releases the claim.
	res, err := d.creds.resolve(ctx, brief.ProjectID, platform)
	if err != nil {
		return nil, err // already a preCreateError
	}
	// Record WHICH ACCOUNT served this campaign on every exit that returns a row —
	// including the UNCONFIRMED/degraded paths that return a campaign alongside an error.
	// See stampProvenance for why this is a defer on the named return, not a per-return call.
	defer func() { res.stampProvenance(camp) }()
	// validateGoogleAdsConnection is shared with ToggleStatus so a create and a toggle accept
	// EXACTLY the same connections and cannot drift. Its failures are wrapped with notCreated
	// HERE — create-only claim semantics the toggle path must not apply.
	creds, accountID, err := validateGoogleAdsConnection(brief.ProjectID, res)
	if err != nil {
		return nil, notCreated(err)
	}
	// The THIRD reader of the same stored login_customer_id, and the one that matters most:
	// it is the path that spends money. Create differs from the toggle and discovery paths
	// in what classification BUYS, and the difference is worth stating so this guard is not
	// mistaken for a status-code fix. Create is queued work, not a request: dispatchOne
	// reports every dispatcher error as the same "platform campaign creation failed"
	// (internal/service/orchestrator.go), so there is no caller-facing 503 here to replace.
	// What the check buys is log hygiene and prevention of unnecessary upstream calls.
	// notCreated marks that nothing upstream was attempted. The old path would reach the
	// client with an unvalidated ID, fail there inside CreateCampaign with result==nil, and
	// be wrapped with notCreated anyway (so claim semantics were already correct). This check
	// prevents the raw ID from reaching logs via the client's %q error formatting, and stops
	// a pre-send failure from being reported as a failed create to Google.
	loginCustomerID, err := validatedLoginCustomerID(res)
	if err != nil {
		return nil, notCreated(err)
	}

	var cfg googleAdsConfig
	if err := unmarshalPlatformConfig(config, "googleAdsConfig", &cfg); err != nil {
		return nil, notCreated(err)
	}
	bf, err := decodeBriefFields(brief)
	if err != nil {
		return nil, notCreated(err)
	}
	// The one config check the client cannot make for itself — see
	// googleAdsDeviceBidModifiers. Deliberately before the channel switch, so a
	// malformed entry is refused identically on both channels and on the adoption path.
	deviceBidModifiers, err := googleAdsDeviceBidModifiers(cfg.DeviceBidModifiers)
	if err != nil {
		return nil, notCreated(err)
	}

	in := googleads.CampaignInput{
		EventName: bf.EventName,
		EventSlug: brief.EventSlug,
		// Project is stamped from the AUTHENTICATED project scope (brief.ProjectID),
		// never from caller JSON — the Project name segment is the data pipeline's
		// attribution join key (docs/api-catalog.md), so it must be the canonical LFX
		// slug (matches reddit/meta/twitter).
		Project:          brief.ProjectID,
		Budget:           cfg.Budget,
		RegistrationURL:  bf.RegistrationURL,
		Headlines:        cfg.Headlines,
		Descriptions:     cfg.Descriptions,
		Keywords:         googleAdsKeywords(cfg.Keywords),
		NegativeKeywords: googleAdsKeywords(cfg.NegativeKeywords),
		AudienceSegments: cfg.AudienceSegments,
		GeoTargets:       cfg.GeoTargets,
		CPCBid:           cfg.CPCBid,
		StartDate:        cfg.StartDate,
		// Passed through verbatim for the same reason the targeting inputs below are:
		// which strategies a channel accepts, which targets a strategy carries, and
		// whether a conversion action belongs to this account are all decided by the
		// client's preflight BEFORE its first mutate, so re-stating any of it here
		// would be a second copy of the rules that could drift from the first.
		BiddingStrategy:   cfg.BiddingStrategy,
		TargetCPA:         cfg.TargetCPA,
		TargetROAS:        cfg.TargetROAS,
		ConversionActions: cfg.ConversionActions,
		EndDate:           cfg.EndDate,
		// The LFXV2-2665 targeting/extension/ad-group inputs. Every one of these is
		// passed through verbatim: the channel rules (proximity, languages, schedules,
		// devices, demographics, extensions and ad groups are Search-only) are enforced
		// by the client's preflight, BEFORE its first mutate, so they are refused in one
		// place for both the create path and the ValidateCampaignInput call below rather
		// than re-stated here where the two could drift.
		ExcludedGeoTargets:     cfg.ExcludedGeoTargets,
		ProximityTargets:       googleAdsProximityTargets(cfg.ProximityTargets),
		Languages:              cfg.Languages,
		AdSchedules:            googleAdsAdSchedules(cfg.AdSchedules),
		DeviceBidModifiers:     deviceBidModifiers,
		ExcludedAgeRanges:      cfg.ExcludedAgeRanges,
		ExcludedGenders:        cfg.ExcludedGenders,
		Sitelinks:              googleAdsSitelinks(cfg.Sitelinks),
		Callouts:               cfg.Callouts,
		StructuredSnippets:     googleAdsStructuredSnippets(cfg.StructuredSnippets),
		CallExtensions:         googleAdsCallExtensions(cfg.CallExtensions),
		Promotions:             googleAdsPromotions(cfg.Promotions),
		Prices:                 googleAdsPrices(cfg.Prices),
		AdGroups:               googleAdsAdGroups(cfg.AdGroups),
		DemandGenCreative:      googleAdsDemandGenCreative(cfg.DemandGenCreative),
		PerformanceMaxCreative: googleAdsPerformanceMaxCreative(cfg.PerformanceMaxCreative),
		VideoCreative:          googleAdsVideoCreative(cfg.VideoCreative),
		DisplayCreative:        googleAdsDisplayCreative(cfg.DisplayCreative),
		// NameSuffix = the brief id gives deterministic, at-most-once-retry names: the
		// GA client composes the budget/campaign/ad-group names from these, and a retry
		// with the same suffix is rejected by whichever family it reaches first —
		// CampaignBudgetError DUPLICATE_NAME for the budget, CampaignError
		// DUPLICATE_CAMPAIGN_NAME for the campaign (two different codes; see
		// errCodeDuplicateBudgetName / errCodeDuplicateCampaignName). Either is reported
		// UNCONFIRMED-already-exists rather than creating a second paid campaign — a
		// poor-man's idempotency key until LFXV2-2665 lands provider idempotency keys.
		NameSuffix: brief.ID,
	}

	// login_customer_id is the OPTIONAL manager (MCC) account the ad account is accessed
	// through; it lives in the connection's ProviderConfig (not the credential blob) and
	// has been shape-checked above.
	client := googleads.NewClient(
		googleads.Credentials{
			ClientID:       creds.ClientID,
			ClientSecret:   creds.ClientSecret,
			DeveloperToken: creds.DeveloperToken,
			RefreshToken:   creds.RefreshToken,
		},
		googleads.AccountConfig{
			CustomerID:      accountID,
			LoginCustomerID: loginCustomerID,
			Label:           res.label,
		},
		d.opts...,
	)

	// Resolve the channel BEFORE validation, before the name is composed, before adoption
	// looks anything up, and before any create. Every step below depends on which campaign
	// type this is, and doing it last meant all of them ran assuming Search:
	//   - the name was hardcoded "Search Campaign", so a Demand Gen dispatch with
	//     adoptExisting:true searched for the SEARCH campaign's name — adopting a Search
	//     campaign into the demand-gen slot, or missing the real "DemandGen Campaign".
	//   - an unsupported channel was refused only at the switch, AFTER adoption could already
	//     have bound a campaign.
	// Refusing here also keeps the no-upstream-call guarantee: an unknown channel returns
	// before the client is contacted at all.
	channel := strings.ToLower(strings.TrimSpace(cfg.Channel))
	var campaignKind string
	switch channel {
	case "", googleAdsChannelSearch:
		channel = googleAdsChannelSearch
		campaignKind = googleads.CampaignKindSearch
	case googleAdsChannelDemandGen:
		campaignKind = googleads.CampaignKindDemandGen
	case googleAdsChannelPerformanceMax:
		campaignKind = googleads.CampaignKindPerformanceMax
	case googleAdsChannelVideo:
		campaignKind = googleads.CampaignKindVideo
	case googleAdsChannelDisplay:
		campaignKind = googleads.CampaignKindDisplay
	default:
		return nil, notCreated(fmt.Errorf("google ads: unsupported channel %q (want %q, %q, %q, %q or %q)", cfg.Channel, googleAdsChannelSearch, googleAdsChannelDemandGen, googleAdsChannelPerformanceMax, googleAdsChannelVideo, googleAdsChannelDisplay))
	}
	// Validate the input BEFORE adoption, and note this is not merely tidy ordering.
	// Adoption returns before CreateCampaign runs its preflight, so without this call the
	// same request would be rejected when no campaign exists and accepted when one does —
	// a NaN budget or a malformed registration URL would fail on the first dispatch and
	// silently succeed on the retry. Whether a request is well-formed cannot depend on
	// what happens to be sitting in the ad account. Validating unconditionally (rather
	// than only on the adoption branch) also keeps the two paths' acceptance identical
	// whichever way cfg.AdoptExisting is set.
	//
	// Validate for the RESOLVED kind, which is why the channel switch above now runs
	// first. The client's preflight gates its Search-only refusals on the kind, so
	// validating everything as Search let a Demand Gen request carrying proximity,
	// criteria, extensions, ad groups or a CPC bid pass here and be adopted — reinstating
	// for those fields the exact accepted-if-it-exists asymmetry this call prevents for
	// every other field. The switch is pure-local and contacts nothing, so moving it
	// above this keeps the no-upstream-call guarantee intact.
	if err := client.ValidateCampaignInputKind(campaignKind, in); err != nil {
		return nil, notCreated(err)
	}
	campaignName := googleads.ComposeName(campaignKind, in)
	// Adoption is OPT-IN (see googleAdsConfig.AdoptExisting for why the default must be
	// off). When asked for: ComposeName is deterministic in the brief, so the caller is
	// pointing at a campaign that already carries this exact name on this account. Adopt
	// it rather than create a second PAID campaign. Only a verified absence licenses the
	// create below: FindCampaignByName errors on anything it cannot verify (transport
	// failure, a name that does not match the WHERE clause, a campaign in another
	// customer), so the lookup never reduces an unverifiable response to a clean
	// "not found".
	if cfg.AdoptExisting {
		adoptID, adoptErr := client.FindCampaignByName(ctx, campaignName)
		if adoptErr != nil {
			// notCreated: nothing was created upstream, so the orchestrator releases the
			// claim. FindCampaignByName already wraps with its own context.
			return nil, notCreated(adoptErr)
		}
		if adoptID != "" {
			return campaignFromGoogleAdsAdoption(ctx, adoptID, campaignName, accountID, res.label, cfg), nil
		}
	}
	// The GA client's contract (mirrors reddit/meta/twitter): (nil, err) ONLY when
	// NOTHING was (or may have been) created — a validation/pre-send/definite failure.
	// Otherwise it returns a NON-NIL partial result alongside the error (an ambiguous
	// create, or a duplicate-name that means "already exists", gives a name-only result
	// whose ids may be empty but which still means "may exist"). So the release decision
	// keys on result==nil ALONE — not on an empty id, which would wrongly release the
	// claim on an ambiguous partial and risk a duplicate on retry. Note GA's two-step
	// hierarchy (budget → campaign): a PRE-attachment (budget-stage) orphan is reconciled by
	// its deterministic CampaignBudgetName; but once the campaign attaches, a non-shared
	// budget's name SYNCHRONIZES to the campaign name, so a campaign-stage partial reconciles
	// the budget by CampaignBudgetID instead (see the client's campaignNamePartial contract).
	// Both keys are preserved in the Result blob.
	//   - (nil, err)      → pre-create; notCreated releases the claim.
	//   - (result, err)   → may exist; return the (possibly id-less) campaign + error so
	//                       the orchestrator retains the claim and records the orphan.
	//   - (result, nil)   → success.
	// Channel selection. Both creates share the (result, err) contract above, so the handling
	// below is identical — only which campaign type gets created differs. `channel` was
	// resolved and validated above (an unsupported value returned before the client was
	// contacted), so this switch needs no default arm beyond the unreachable guard: adding one
	// that creates a Search campaign would reintroduce exactly the silent fallback the early
	// validation exists to prevent.
	var (
		result *googleads.CampaignResult
		cerr   error
	)
	switch channel {
	case googleAdsChannelSearch:
		result, cerr = client.CreateCampaign(ctx, in)
	case googleAdsChannelDemandGen:
		result, cerr = client.CreateDemandGenCampaign(ctx, in)
	case googleAdsChannelPerformanceMax:
		result, cerr = client.CreatePerformanceMaxCampaign(ctx, in)
	case googleAdsChannelVideo:
		result, cerr = client.CreateVideoCampaign(ctx, in)
	case googleAdsChannelDisplay:
		result, cerr = client.CreateDisplayCampaign(ctx, in)
	default:
		// Unreachable: the resolution above admits only these five. Refuse rather than fall
		// through to a create, so a future channel added there but not here cannot spend one
		// channel's budget on another.
		return nil, notCreated(fmt.Errorf("google ads: channel %q resolved but has no create path", channel))
	}
	// An untargeted create is ACCEPTED (see googleAdsConfig.GeoTargets for why refusing would
	// break every caller predating the field) but never SILENT: a campaign with no geo targets
	// spends its whole budget worldwide the moment a human enables it, and that should be
	// findable in the logs rather than inferred from a Google Ads bill.
	//
	// Emitted HERE, after the create returns, rather than before it. Logged earlier it fired for
	// requests that never reached Google at all — a rejected input, or an ADOPTION of an
	// existing campaign that may already carry targeting — so it claimed a worldwide spend for
	// campaigns that were never created. The `result != nil` condition includes the partial
	// post-campaign failures, where a campaign does exist upstream and the warning is warranted.
	//
	// Both POSITIVE shapes count. proximityTargets is the second way to restrict where a
	// campaign serves, and a radius criterion restricts spend exactly as a location
	// criterion does — so a proximity-only campaign that warned about having "no geo
	// targeting" would be reporting a worldwide spend that the criteria it carries
	// prevent. ExcludedGeoTargets is deliberately NOT counted: an exclusion narrows an
	// otherwise-unbounded campaign without bounding it, so a campaign carrying only
	// exclusions still serves wherever the account allows minus a few places, which is
	// what this warning exists to say out loud.
	if len(cfg.GeoTargets) == 0 && len(cfg.ProximityTargets) == 0 && result != nil && result.CampaignID != "" {
		slog.WarnContext(ctx, "google ads campaign created with NO geo targeting (it will serve wherever the ad account allows once enabled)",
			"brief_id", brief.ID,
			"channel", channel,
		)
	}

	if cerr != nil {
		if result == nil {
			return nil, notCreated(fmt.Errorf("google ads campaign creation failed before any upstream create: %w", cerr))
		}
		// A non-nil result means SOMETHING may exist upstream (an ambiguous create, a
		// duplicate-name "already exists", or a definite campaign 4xx that still left a
		// created budget orphan), so the claim is RETAINED and the orphan recorded either
		// way. Do NOT prepend "UNCONFIRMED": the client already classifies the outcome
		// precisely — "UNCONFIRMED (may exist)" for an ambiguous create vs "creation failed
		// (budget created)" for a definite 4xx — so a blanket prefix would overwrite that
		// distinction and route an operator to reconcile an ambiguous outcome for what is
		// actually a definite failure. Wrap with a neutral, provider-tagged prefix instead.
		return campaignFromGoogleAds(ctx, result, cfg), fmt.Errorf("google-ads dispatch: %w", cerr)
	}
	return campaignFromGoogleAds(ctx, result, cfg), nil
}

// googleAdsKeywords maps the wire-shaped keyword config to the platform client's
// Keyword type. Returns nil for an empty input so an omitted "keywords" field stays
// nil end-to-end rather than becoming an empty-but-non-nil slice.
func googleAdsKeywords(in []googleAdsKeywordConfig) []googleads.Keyword {
	if len(in) == 0 {
		return nil
	}
	out := make([]googleads.Keyword, len(in))
	for i, kw := range in {
		out[i] = googleads.Keyword{Text: kw.Text, MatchType: kw.MatchType}
	}
	return out
}

// The LFXV2-2665 wire→client mappers. Each follows googleAdsKeywords exactly: a nil
// return for an empty input, so an omitted field stays nil end-to-end instead of
// becoming an empty-but-non-nil slice, and a straight field copy with no validation —
// every value is checked by the client's preflight before anything is sent.

func googleAdsProximityTargets(in []googleAdsProximityConfig) []googleads.ProximityTarget {
	if len(in) == 0 {
		return nil
	}
	out := make([]googleads.ProximityTarget, len(in))
	for i, p := range in {
		out[i] = googleads.ProximityTarget{
			Latitude:   p.Latitude,
			Longitude:  p.Longitude,
			Radius:     p.Radius,
			RadiusUnit: p.RadiusUnit,
		}
	}
	return out
}

// googleAdsAdSchedules carries BidModifier across as the pointer it is on both sides.
// Copying the value would make an absent modifier indistinguishable from an explicit
// 0, which is the -100% opt-out — the interval would stop serving entirely.
func googleAdsAdSchedules(in []googleAdsAdScheduleConfig) []googleads.AdSchedule {
	if len(in) == 0 {
		return nil
	}
	out := make([]googleads.AdSchedule, len(in))
	for i, s := range in {
		out[i] = googleads.AdSchedule{
			DayOfWeek:   s.DayOfWeek,
			StartHour:   s.StartHour,
			StartMinute: s.StartMinute,
			EndHour:     s.EndHour,
			EndMinute:   s.EndMinute,
			BidModifier: s.BidModifier,
		}
	}
	return out
}

// googleAdsDeviceBidModifiers maps the wire entries onto the client type, REFUSING an
// entry that omitted bidModifier rather than letting it decode to the -100% opt-out.
// See googleAdsDeviceBidModifierConfig for why the wire field is a pointer and the
// client field is not.
//
// This cannot live in the client's preflight: by the time the client sees the entry the
// pointer has already been flattened, so absent and 0 are the same value there. It is
// the one validation in this family the dispatcher has to own, and it runs before the
// channel is resolved so the create and adoption paths refuse it identically.
func googleAdsDeviceBidModifiers(in []googleAdsDeviceBidModifierConfig) ([]googleads.DeviceBidModifier, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]googleads.DeviceBidModifier, len(in))
	for i, d := range in {
		if d.BidModifier == nil {
			return nil, fmt.Errorf("google ads: deviceBidModifiers[%d] (device %q) omits bidModifier; supply it explicitly — 0 is not \"no adjustment\" but a -100%% modifier, which stops the campaign serving on that device", i, d.Device)
		}
		out[i] = googleads.DeviceBidModifier{Device: d.Device, BidModifier: *d.BidModifier}
	}
	return out, nil
}

func googleAdsSitelinks(in []googleAdsSitelinkConfig) []googleads.Sitelink {
	if len(in) == 0 {
		return nil
	}
	out := make([]googleads.Sitelink, len(in))
	for i, s := range in {
		out[i] = googleads.Sitelink{
			Text:         s.Text,
			Description1: s.Description1,
			Description2: s.Description2,
			FinalURL:     s.FinalURL,
		}
	}
	return out
}

func googleAdsStructuredSnippets(in []googleAdsStructuredSnippetConfig) []googleads.StructuredSnippet {
	if len(in) == 0 {
		return nil
	}
	out := make([]googleads.StructuredSnippet, len(in))
	for i, s := range in {
		out[i] = googleads.StructuredSnippet{Header: s.Header, Values: s.Values}
	}
	return out
}

func googleAdsCallExtensions(in []googleAdsCallExtensionConfig) []googleads.CallExtension {
	if len(in) == 0 {
		return nil
	}
	out := make([]googleads.CallExtension, len(in))
	for i, c := range in {
		out[i] = googleads.CallExtension{CountryCode: c.CountryCode, PhoneNumber: c.PhoneNumber}
	}
	return out
}

func googleAdsPromotions(in []googleAdsPromotionConfig) []googleads.PromotionExtension {
	if len(in) == 0 {
		return nil
	}
	out := make([]googleads.PromotionExtension, len(in))
	for i, p := range in {
		out[i] = googleads.PromotionExtension{
			PromotionTarget:     p.PromotionTarget,
			DiscountPercent:     p.DiscountPercent,
			DiscountAmount:      p.DiscountAmount,
			OrdersOverAmount:    p.OrdersOverAmount,
			CurrencyCode:        p.CurrencyCode,
			PromotionCode:       p.PromotionCode,
			Occasion:            p.Occasion,
			LanguageCode:        p.LanguageCode,
			StartDate:           p.StartDate,
			EndDate:             p.EndDate,
			RedemptionStartDate: p.RedemptionStartDate,
			RedemptionEndDate:   p.RedemptionEndDate,
			FinalURL:            p.FinalURL,
		}
	}
	return out
}

func googleAdsPrices(in []googleAdsPriceConfig) []googleads.PriceExtension {
	if len(in) == 0 {
		return nil
	}
	out := make([]googleads.PriceExtension, len(in))
	for i, p := range in {
		price := googleads.PriceExtension{
			Type:           p.Type,
			PriceQualifier: p.PriceQualifier,
			LanguageCode:   p.LanguageCode,
		}
		// Built even when empty so the client, not this mapper, decides what an
		// offering-less price extension means — it refuses it by count, with a
		// message naming Google's 3-8 range.
		if len(p.Offerings) > 0 {
			price.Offerings = make([]googleads.PriceOffering, len(p.Offerings))
			for j, o := range p.Offerings {
				price.Offerings[j] = googleads.PriceOffering{
					Header:       o.Header,
					Description:  o.Description,
					Amount:       o.Amount,
					CurrencyCode: o.CurrencyCode,
					Unit:         o.Unit,
					FinalURL:     o.FinalURL,
				}
			}
		}
		out[i] = price
	}
	return out
}

// googleAdsDemandGenCreativeConfig is the wire shape of the Demand Gen creative.
//
// Images are URLs, matching reddit's and meta's imageUrl: a caller already hosts the
// creative somewhere. Google is the one platform that will not fetch a URL itself —
// ImageAsset takes raw bytes — so the campaign service downloads each one and uploads
// the bytes. See internal/platform/googleads/demandgen_creative.go, which also carries
// the hardening that server-side fetch requires.
type googleAdsDemandGenCreativeConfig struct {
	// Each list is one image shape with its own required aspect ratio and minimum
	// size, enforced by the client's preflight against Google's documented rules.
	// At least one of marketingImages or squareMarketingImages is required.
	MarketingImages       []string `json:"marketingImages"`
	SquareMarketingImages []string `json:"squareMarketingImages"`
	PortraitImages        []string `json:"portraitImages"`
	TallPortraitImages    []string `json:"tallPortraitImages"`
	// LogoImages is required: 1-5 square images.
	LogoImages []string `json:"logoImages"`
	// Headlines and Descriptions are 1-5 each. NOT the Search ad's 15/4 — Demand Gen
	// has its own counts, though the same display-width limits.
	Headlines    []string `json:"headlines"`
	Descriptions []string `json:"descriptions"`
	// BusinessName is required by Google. CallToActionText is optional.
	BusinessName     string `json:"businessName"`
	CallToActionText string `json:"callToActionText"`
}

// googleAdsDemandGenCreative maps the creative config to the client input. A nil
// pointer maps to the zero value, which the client treats as "no creative asked
// for" — the pre-existing no-ad behaviour.
func googleAdsDemandGenCreative(in *googleAdsDemandGenCreativeConfig) googleads.DemandGenCreative {
	if in == nil {
		return googleads.DemandGenCreative{}
	}
	return googleads.DemandGenCreative{
		MarketingImages:       in.MarketingImages,
		SquareMarketingImages: in.SquareMarketingImages,
		PortraitImages:        in.PortraitImages,
		TallPortraitImages:    in.TallPortraitImages,
		LogoImages:            in.LogoImages,
		Headlines:             in.Headlines,
		Descriptions:          in.Descriptions,
		BusinessName:          in.BusinessName,
		CallToActionText:      in.CallToActionText,
	}
}

// googleAdsAdGroups maps the per-theme ad group list, reusing googleAdsKeywords for
// each group's keywords so a group's keyword vocabulary cannot drift from the
// campaign-level one. An empty per-group list stays nil, which is what the client
// reads as "inherit the campaign-level value" — distinct from an empty-but-present
// list, which would mean the group overrides with nothing.
func googleAdsAdGroups(in []googleAdsAdGroupConfig) []googleads.AdGroupSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]googleads.AdGroupSpec, len(in))
	for i, g := range in {
		out[i] = googleads.AdGroupSpec{
			Name:             g.Name,
			CPCBid:           g.CPCBid,
			Keywords:         googleAdsKeywords(g.Keywords),
			AudienceSegments: g.AudienceSegments,
			Ads:              googleAdsAds(g.Ads),
		}
	}
	return out
}

func googleAdsAds(in []googleAdsAdConfig) []googleads.AdSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]googleads.AdSpec, len(in))
	for i, a := range in {
		out[i] = googleads.AdSpec{Headlines: a.Headlines, Descriptions: a.Descriptions}
	}
	return out
}

// googleAdsPerformanceMaxCreativeConfig is the wire shape of the Performance Max
// asset group. Images are URLs for the same reason the Demand Gen creative's are, and
// are fetched by the same hardened client — see
// internal/platform/googleads/pmax_creative.go.
//
// The counts below are Performance Max's own and are NOT the Demand Gen ones beside
// them: three headlines where Demand Gen needs one, a separate long-headline field
// type, and two description minimums with a short-description rule. Stated here as
// well as in the client because this struct is what a caller reads.
type googleAdsPerformanceMaxCreativeConfig struct {
	// marketingImages (1.91:1) and squareMarketingImages (1:1) are BOTH required —
	// unlike Demand Gen, where either satisfies the other. portraitImages (4:5) is
	// optional, and the three share a combined cap of 20.
	MarketingImages       []string `json:"marketingImages"`
	SquareMarketingImages []string `json:"squareMarketingImages"`
	PortraitImages        []string `json:"portraitImages"`
	// LogoImages (1:1) is required, 1-5. LandscapeLogoImages (4:1) is optional.
	LogoImages          []string `json:"logoImages"`
	LandscapeLogoImages []string `json:"landscapeLogoImages"`
	// Headlines 3-15, LongHeadlines 1-5 and Descriptions 2-5 are three DISTINCT asset
	// field types, not one list bucketed by length. At least one description must fit
	// the short slot (display width 60 or less).
	Headlines     []string `json:"headlines"`
	LongHeadlines []string `json:"longHeadlines"`
	Descriptions  []string `json:"descriptions"`
	// BusinessName is required by Google.
	BusinessName string `json:"businessName"`
	// YouTubeVideoIDs are bare video ids, never URLs — this client will not guess
	// which part of a URL is the id.
	YouTubeVideoIDs []string `json:"youtubeVideoIds"`
	// AssetGroupName defaults to the event name plus " - Asset Group". Path1/Path2 are
	// the optional display-path segments rendered after the domain; path2 renders only
	// after path1.
	AssetGroupName string `json:"assetGroupName"`
	Path1          string `json:"path1"`
	Path2          string `json:"path2"`
}

// googleAdsPerformanceMaxCreative maps the asset-group config to the client input,
// validating nothing — every rule lives in the client's preflight, so the two paths
// cannot drift. A nil pointer maps to the zero value, which the client reads as "no
// asset group asked for".
func googleAdsPerformanceMaxCreative(in *googleAdsPerformanceMaxCreativeConfig) googleads.PerformanceMaxCreative {
	if in == nil {
		return googleads.PerformanceMaxCreative{}
	}
	return googleads.PerformanceMaxCreative{
		MarketingImages:       in.MarketingImages,
		SquareMarketingImages: in.SquareMarketingImages,
		PortraitImages:        in.PortraitImages,
		LogoImages:            in.LogoImages,
		LandscapeLogoImages:   in.LandscapeLogoImages,
		Headlines:             in.Headlines,
		LongHeadlines:         in.LongHeadlines,
		Descriptions:          in.Descriptions,
		BusinessName:          in.BusinessName,
		YouTubeVideoIDs:       in.YouTubeVideoIDs,
		AssetGroupName:        in.AssetGroupName,
		Path1:                 in.Path1,
		Path2:                 in.Path2,
	}
}

// googleAdsVideoCreativeConfig is the responsive video ad for the "video" channel.
//
// Unlike its three siblings it carries NO URLs at all. A YouTube video is referenced by
// its bare id and stays on YouTube, so there is nothing here for googleAdsSnapshotConfig
// to sanitize — which is why that function's early return does not mention this field.
// Should an image field ever be added (companion banners are the obvious candidate, and
// are deliberately out of scope — see video_creative.go), both this comment and that
// early return have to change together.
type googleAdsVideoCreativeConfig struct {
	// YouTubeVideoIDs are BARE video ids, not watch URLs — the client refuses a URL
	// rather than guessing which part of it is the id. At least one is required for an
	// ad to be created at all.
	YouTubeVideoIDs []string `json:"youtubeVideoIds"`
	// The four text field types a videoResponsiveAd carries. Headlines, long headlines
	// and descriptions are each required when a creative is supplied; call-to-action
	// text is optional and Google supplies a default when it is omitted.
	Headlines     []string `json:"headlines"`
	LongHeadlines []string `json:"longHeadlines"`
	Descriptions  []string `json:"descriptions"`
	CallToActions []string `json:"callToActions"`
}

// googleAdsVideoCreative maps the video-ad config to the client input, validating
// nothing — every rule lives in the client's preflight, so the two paths cannot drift.
// A nil pointer maps to the zero value, which the client reads as "no ad asked for".
func googleAdsVideoCreative(in *googleAdsVideoCreativeConfig) googleads.VideoCreative {
	if in == nil {
		return googleads.VideoCreative{}
	}
	return googleads.VideoCreative{
		YouTubeVideoIDs: in.YouTubeVideoIDs,
		Headlines:       in.Headlines,
		LongHeadlines:   in.LongHeadlines,
		Descriptions:    in.Descriptions,
		CallToActions:   in.CallToActions,
	}
}

// googleAdsDisplayCreativeConfig is the responsive display ad for the "display" channel.
//
// Unlike the Video creative beside it, this one DOES carry URLs — four arrays of them —
// so it is named in googleAdsSnapshotConfig's early return and sanitized there with the
// Demand Gen and Performance Max creatives.
type googleAdsDisplayCreativeConfig struct {
	// The four image arrays a responsiveDisplayAd carries. The two MARKETING arrays are
	// reciprocally required — Google requires each when the other is absent — and are
	// capped as a combined total; both LOGO arrays are optional and capped separately.
	// Their ratios and minimums differ from the same-named Demand Gen and Performance
	// Max slots, which is why each channel keeps its own slot table.
	MarketingImages       []string `json:"marketingImages"`
	SquareMarketingImages []string `json:"squareMarketingImages"`
	LogoImages            []string `json:"logoImages"`
	SquareLogoImages      []string `json:"squareLogoImages"`
	// LongHeadline is a SINGLE string, not a list — the one place this channel's shape
	// departs from its siblings, where Performance Max and Video both take a list of
	// long headlines. Spelled as a scalar here so the envelope cannot carry a second one
	// that Google would reject after the ad group exists.
	Headlines    []string `json:"headlines"`
	LongHeadline string   `json:"longHeadline"`
	Descriptions []string `json:"descriptions"`
	// BusinessName is required by Google when a creative is supplied; CallToActionText
	// is optional and Google supplies its own button text when it is omitted.
	BusinessName     string `json:"businessName"`
	CallToActionText string `json:"callToActionText"`
}

// googleAdsDisplayCreative maps the display-ad config to the client input, validating
// nothing — every rule lives in the client's preflight, so the two paths cannot drift.
// A nil pointer maps to the zero value, which the client reads as "no ad asked for".
func googleAdsDisplayCreative(in *googleAdsDisplayCreativeConfig) googleads.DisplayCreative {
	if in == nil {
		return googleads.DisplayCreative{}
	}
	return googleads.DisplayCreative{
		MarketingImages:       in.MarketingImages,
		SquareMarketingImages: in.SquareMarketingImages,
		LogoImages:            in.LogoImages,
		SquareLogoImages:      in.SquareLogoImages,
		Headlines:             in.Headlines,
		LongHeadline:          in.LongHeadline,
		Descriptions:          in.Descriptions,
		BusinessName:          in.BusinessName,
		CallToActionText:      in.CallToActionText,
	}
}

// googleAdsSnapshotConfig returns cfg with every caller-supplied URL reduced to
// scheme+host, for storage in config_snapshot — which is persisted UNENCRYPTED in
// Postgres. Same reason and same helper as campaignFromMeta's ImageURL and
// campaignFromReddit's PostURL: a caller-supplied URL can carry a credential in its
// path, query or fragment, and the snapshot is the copy that persists.
//
// The URLs this config carries are the clickable destinations of three extension
// types — a sitelink's finalUrl, a promotion's finalUrl and the finalUrl of EVERY
// price offering, each row of a price table carrying its own — and the image URLs of
// the Demand Gen creative, the Performance Max asset group and the Display creative.
// The Video creative is the one creative with nothing to sanitize: it references a
// YouTube video by bare id and carries no URL field at all, which is why it is the
// only one absent from the early return below. Call extensions carry a phone number
// and no URL, so they are absent for the same kind of reason.
// The ad copy beside them — headlines, descriptions, callouts, snippet values,
// sitelink text — is deliberately
// NOT run through sanitizeSnapshotText: that helper exists for operator-authored prose
// that routinely carries a pasted link (X's tweetText), whereas Google keeps the
// destination in its own finalUrl field and the copy fields are short ad text. If a
// link-bearing free-text field is ever added here, it needs that helper.
func googleAdsSnapshotConfig(cfg googleAdsConfig) googleAdsConfig {
	if len(cfg.Sitelinks) == 0 && len(cfg.Promotions) == 0 && len(cfg.Prices) == 0 &&
		cfg.DemandGenCreative == nil && cfg.PerformanceMaxCreative == nil && cfg.DisplayCreative == nil {
		return cfg
	}
	snapshot := cfg
	// Copy the slice before mutating: cfg is passed by value but Sitelinks shares its
	// backing array with the caller's config, and the FULL url must still reach Google.
	if len(cfg.Sitelinks) > 0 {
		snapshot.Sitelinks = make([]googleAdsSitelinkConfig, len(cfg.Sitelinks))
		copy(snapshot.Sitelinks, cfg.Sitelinks)
		for i := range snapshot.Sitelinks {
			snapshot.Sitelinks[i].FinalURL = sanitizeSnapshotURL(snapshot.Sitelinks[i].FinalURL)
		}
	}
	// A promotion's destination is a clickable ad destination exactly as a sitelink's
	// is, and is copied and reduced the same way.
	if len(cfg.Promotions) > 0 {
		snapshot.Promotions = make([]googleAdsPromotionConfig, len(cfg.Promotions))
		copy(snapshot.Promotions, cfg.Promotions)
		for i := range snapshot.Promotions {
			snapshot.Promotions[i].FinalURL = sanitizeSnapshotURL(snapshot.Promotions[i].FinalURL)
		}
	}
	// A price extension needs a DEEP copy: the top-level copy shares the Offerings
	// backing array with the caller's config, and it is the offerings that carry the
	// URLs. Reducing them in place would strip the path off the config the client is
	// about to send, which is the bug this whole function is written around.
	if len(cfg.Prices) > 0 {
		snapshot.Prices = make([]googleAdsPriceConfig, len(cfg.Prices))
		copy(snapshot.Prices, cfg.Prices)
		for i := range snapshot.Prices {
			if len(cfg.Prices[i].Offerings) == 0 {
				continue
			}
			offerings := make([]googleAdsPriceOfferingConfig, len(cfg.Prices[i].Offerings))
			copy(offerings, cfg.Prices[i].Offerings)
			for j := range offerings {
				offerings[j].FinalURL = sanitizeSnapshotURL(offerings[j].FinalURL)
			}
			snapshot.Prices[i].Offerings = offerings
		}
	}
	// Every creative image URL is sanitized for the same reason the sitelink URL and
	// reddit's/meta's ImageURL are: config_snapshot is persisted UNENCRYPTED, and a
	// creative URL can carry a signed query string that is itself the credential for
	// fetching the asset. The FULL url must still reach the client, which is why this
	// works on a copy — and the copy has to be deep, because the pointer and both
	// string slices are shared with the caller's config.
	if cfg.DemandGenCreative != nil {
		creative := *cfg.DemandGenCreative
		creative.MarketingImages = sanitizeSnapshotURLs(cfg.DemandGenCreative.MarketingImages)
		creative.SquareMarketingImages = sanitizeSnapshotURLs(cfg.DemandGenCreative.SquareMarketingImages)
		creative.PortraitImages = sanitizeSnapshotURLs(cfg.DemandGenCreative.PortraitImages)
		creative.TallPortraitImages = sanitizeSnapshotURLs(cfg.DemandGenCreative.TallPortraitImages)
		creative.LogoImages = sanitizeSnapshotURLs(cfg.DemandGenCreative.LogoImages)
		snapshot.DemandGenCreative = &creative
	}
	// The Performance Max asset group carries image URLs with exactly the same
	// exposure, so it is sanitized the same way and with the same deep copy. Missing
	// this would have made the one channel whose creative is newest the one whose
	// signed URLs persisted in plaintext.
	if cfg.PerformanceMaxCreative != nil {
		creative := *cfg.PerformanceMaxCreative
		creative.MarketingImages = sanitizeSnapshotURLs(cfg.PerformanceMaxCreative.MarketingImages)
		creative.SquareMarketingImages = sanitizeSnapshotURLs(cfg.PerformanceMaxCreative.SquareMarketingImages)
		creative.PortraitImages = sanitizeSnapshotURLs(cfg.PerformanceMaxCreative.PortraitImages)
		creative.LogoImages = sanitizeSnapshotURLs(cfg.PerformanceMaxCreative.LogoImages)
		creative.LandscapeLogoImages = sanitizeSnapshotURLs(cfg.PerformanceMaxCreative.LandscapeLogoImages)
		snapshot.PerformanceMaxCreative = &creative
	}
	// The Display creative carries image URLs with exactly the same exposure, so it is
	// sanitized the same way and with the same deep copy. Video is the only creative
	// absent from this chain, and only because it holds no URL at all.
	if cfg.DisplayCreative != nil {
		creative := *cfg.DisplayCreative
		creative.MarketingImages = sanitizeSnapshotURLs(cfg.DisplayCreative.MarketingImages)
		creative.SquareMarketingImages = sanitizeSnapshotURLs(cfg.DisplayCreative.SquareMarketingImages)
		creative.LogoImages = sanitizeSnapshotURLs(cfg.DisplayCreative.LogoImages)
		creative.SquareLogoImages = sanitizeSnapshotURLs(cfg.DisplayCreative.SquareLogoImages)
		snapshot.DisplayCreative = &creative
	}
	return snapshot
}

// sanitizeSnapshotURLs is sanitizeSnapshotURL over a list, returning a NEW slice so
// the caller's backing array is never written through.
func sanitizeSnapshotURLs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, u := range in {
		out[i] = sanitizeSnapshotURL(u)
	}
	return out
}

// campaignFromGoogleAds maps the client result to the persistence model. The
// orchestrator fills project/brief/job/platform (and, for a retained ambiguous orphan,
// status); this sets what only the dispatcher knows — upstream id, name, the persisted
// budget/type/config, the provider result blob, and a "created" status on the success path.
func campaignFromGoogleAds(ctx context.Context, r *googleads.CampaignResult, cfg googleAdsConfig) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: r.CampaignID,
		CampaignName:       r.CampaignName,
		Status:             campaignStatusCreated,
	}
	// Persist the caller-supplied budget + validated config, mirroring the sibling adapters
	// (a NULL budget/type/config_snapshot row otherwise loses the campaign's configuration).
	// GA's shell uses a DAILY budget (no lifetime flag) and sets no flight dates here — those
	// land with GA-3+; ConfigSnapshot captures the validated config regardless.
	applyCampaignConfig(ctx, c, cfg.Budget, false, cfg.StartDate, cfg.EndDate, googleAdsSnapshotConfig(cfg))
	if raw, err := json.Marshal(r); err != nil {
		// A marshal failure should be near-impossible for this plain struct, but do NOT
		// swallow it: Result is the sole carrier of the reconcile-by-name payload (the
		// deterministic CampaignBudgetName) on the ambiguous-orphan path, so a silently-empty
		// Result loses reconciliation data precisely when it's most needed. Log it (the row is
		// still persisted with its id/status/config). Mirrors the meta/twitter/linkedin adapters.
		slog.WarnContext(ctx, "failed to marshal google ads campaign result blob (Result left empty)",
			"campaign_id", c.PlatformCampaignID, "error", err)
	} else {
		c.Result = raw
	}
	return c
}

// campaignFromGoogleAdsAdoption builds the campaign model for an ADOPTED campaign. The lookup
// answers only "a campaign with this name exists here" — it says nothing about the budget, ad
// group and ad a create would also have made, so an adoption of a campaign whose previous
// attempt died mid-sequence yields a campaign that will not serve. It is still the right
// outcome: the alternative is a duplicate paid campaign, and the shell is now recorded and
// visible for reconciliation rather than orphaned. Completing a partial adoption is
// LFXV2-3042's follow-up, and needs an ad-group lookup this client does not yet have.
func campaignFromGoogleAdsAdoption(ctx context.Context, campaignID, campaignName, accountID, accountLabel string, cfg googleAdsConfig) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: campaignID,
		CampaignName:       campaignName,
		// `created_degraded`, NOT `created` — the same status twitter.go stamps for its
		// Reused case, and for the same reason: the campaign exists but this request's
		// budget/config were never pushed to it, and no budget or ad group was created,
		// so it may be serving under settings nobody here chose (or not serving at all).
		// Both statuses are terminal to isReusableCampaign, so this does not invite a
		// re-dispatch; what it does is make the row say what actually happened, which is
		// what an operator reconciling against the platform has to go on. A clean
		// `created` here would assert a wiring that this path deliberately never does.
		Status: campaignStatusCreatedDegraded,
	}
	// The config is applied for the same reason as on the create path, and it is worth
	// being precise about what these columns mean, because "adopted" invites the reading
	// that they should be left NULL as unknown.
	//
	// budget_amount/budget_type/config_snapshot record the CALLER-SUPPLIED config for this
	// dispatch — what was asked for — not a readback of platform state; the create path
	// stamps them from the same cfg before anything is read back. Leaving them NULL here
	// would not express "unknown", it would lose the request: the row would be the only
	// record that this dispatch happened and would say nothing about what it asked for,
	// and every sibling adapter's rows would disagree with it in shape for no reason a
	// reader could recover.
	//
	// What adoption does NOT do is push this config upstream. The campaign already exists
	// with whatever budget and settings it was created with, and this path deliberately
	// creates no budget and no ad group (see the Steps below). So the row records the
	// request while the platform keeps its own state, and the two can legitimately
	// disagree. ReadMetrics cannot close that gap however it is read — it returns
	// impressions, clicks, cost and CTR, none of which describe the campaign's
	// configuration.
	//
	// ReadSettings (LFXV2-3067) is the capability that can: it reads the live campaign
	// config and reports, per field, where it diverges from what this row recorded. It
	// does NOT reconcile them, and deliberately so — it never writes back onto this row,
	// because these columns mean "what this dispatch asked for" and an observation
	// written over them would destroy the only record of the request. Divergence is
	// surfaced for an operator to act on, on demand; nothing polls, and no status is
	// stored.
	applyCampaignConfig(ctx, c, cfg.Budget, false, cfg.StartDate, cfg.EndDate, googleAdsSnapshotConfig(cfg))
	// The blob must carry CustomerID: googleAdsCreationCustomerID reads it as this row's
	// provenance, and what an ABSENT one costs is per-operation, not one rule (see that
	// helper's comment). Comparison-only callers — the account-mismatch check on read and
	// toggle — treat absence as "unknown, proceed", so omitting it silently disables that
	// check. ReadSettings (LFXV2-3067) instead fails CLOSED on absence, joining
	// domain.ErrCampaignProvenanceUnknown, because a settings readback whose account
	// cannot be verified has no meaning to report. So omitting CustomerID here does not
	// merely weaken mismatch detection, it makes settings readback impossible for this
	// row until it is re-dispatched.
	adoptionResult := &googleads.CampaignResult{
		Platform:     "google-ads",
		AccountLabel: accountLabel,
		CustomerID:   accountID,
		CampaignID:   campaignID,
		CampaignName: campaignName,
		GoogleAdsURL: "https://ads.google.com/aw/campaigns?ocid=" + accountID,
		Steps:        []string{"Campaign adopted: " + campaignID + " (already exists on account, no budget/ad group created)"},
	}
	if raw, err := json.Marshal(adoptionResult); err != nil {
		slog.WarnContext(ctx, "failed to marshal adoption result blob (Result left empty)",
			"campaign_id", campaignID, "error", err)
	} else {
		c.Result = raw
	}
	return c
}

// validateGoogleAdsConnection checks a resolved connection is usable and returns the decoded
// credentials + trimmed customer id. Shared by Dispatch and ToggleStatus so a create and a
// toggle accept EXACTLY the same connections; each caller applies its own error wrapping
// (Dispatch wraps with notCreated for claim semantics, the toggle path does not).
//
// The customer id is trimmed ONCE and the trimmed value returned, so a whitespace-padded id
// can't pass the empty check here and then fail the client's digits-only validator as a
// confusing downstream error.
func validateGoogleAdsConnection(projectID string, res *resolved) (creds googleAdsCreds, accountID string, err error) {
	// Same defer as validateGoogleAdsCredentials, for the same reason: the account-id
	// branch below is a stored-state defect too, and on the LF fallback row it names a
	// connection the project cannot go select an account on.
	defer func() { err = res.systemScoped(err) }()
	creds, err = validateGoogleAdsCredentials(projectID, res)
	if err != nil {
		return creds, "", err
	}
	accountID = strings.TrimSpace(res.accountID)
	if accountID == "" {
		// BOTH sentinels, for the same reason every other branch on this path wraps two:
		// ErrConnectionNotUsable decides the HTTP status, ErrAccountNotSelected names the
		// reason for the log line's fixed vocabulary (unusableConnectionReason).
		//
		// This guard used to return a bare error. That was NOT harmless, and calling it
		// unreachable would be wrong: Goa's Required("account_id") was a presence check on
		// the JSON KEY — the generated validator was `if body.AccountID == nil` — and the
		// Go field is a plain string, so `"account_id": ""` (or whitespace) satisfied it and
		// was stored. An account-less row was reachable all along; it was just an unintended,
		// undocumented state nobody had reasoned about. What changed is that credentials-first
		// bootstrap makes it a SUPPORTED, omission-based lifecycle state and the NORMAL state
		// of a freshly created connection — which is what makes the bare error's cost
		// unavoidable rather than merely latent: it falls to each handler's default arm and
		// answers 503, "the platform did not respond", for a project that simply has not
		// finished setting up, with a remedy (wait) that can never work.
		return creds, "", fmt.Errorf("google ads connection for project %s has no account id (customer id): %w: %w",
			projectID, domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected)
	}
	return creds, accountID, nil
}

// validateGoogleAdsCredentials is validateGoogleAdsConnection WITHOUT the account-id
// requirement, for the one operation that cannot have one yet: account discovery.
//
// It serves BOTH lifecycles now that GoogleAdsConnectionConfig no longer declares
// Required("account_id"): re-pointing an existing connection ("which other customer ids
// does this credential reach?") and first-time bootstrap, where the connection is created
// with credentials only and the account is chosen from this endpoint's answer. The
// relaxation was written for the endpoint's own semantics rather than for whatever the row
// happens to hold, which is why enabling bootstrap needed only the design change.
//
// The ACTIVE-status check below is what makes bootstrap possible at all rather than being
// merely compatible with it: a credentials-only connection is stored as active, so if this
// demanded some other status the caller could never reach the endpoint that tells them
// which account to pick. See domain.ErrAccountNotSelected for how the paths that DO need
// an account id report its absence.
//
// Every other check — active status, decodable blob, all four OAuth fields present — still
// applies, because a discovery call against a stale or half-configured connection should
// fail as a connection problem rather than as an opaque error from Google.
//
// Each of those failures is tagged with domain.ErrConnectionNotUsable HERE, alongside the
// sentinel naming the specific defect, because every caller needs it and none of them can
// add it later: the handlers classify on that sentinel, and an untagged inactive-or-
// incomplete connection falls to their default arm and answers 503 — "the platform did not
// respond" for a connection the platform was never asked about, with a remedy (retry) that
// no amount of waiting can satisfy. Tagging at the point of detection is also what keeps
// the two resolve paths below from having to agree about it.
// Every error below is tagged for its AUDIENCE here, in the validator, rather than by each
// caller. Caller-side tagging is what produced the defect this replaces: discovery applied
// systemScoped and the create, toggle and metrics paths did not, so the same LF-system-row
// defect reached a project as a 400 telling it to edit a connection it does not own. A named
// return plus defer means a return site added later cannot forget; systemScoped is a no-op for
// project-owned rows and idempotent, so a caller that also tags costs nothing.
func validateGoogleAdsCredentials(projectID string, res *resolved) (creds googleAdsCreds, err error) {
	defer func() { err = res.systemScoped(err) }()
	if res.status != model.StatusActive {
		return creds, fmt.Errorf("%w: %w: google ads connection for project %s is %s, not active",
			domain.ErrConnectionNotUsable, domain.ErrConnectionInactive, projectID, res.status)
	}
	if err := json.Unmarshal(res.plaintext, &creds); err != nil {
		// The unmarshal error is DROPPED, not wrapped. It is the one error on this path
		// derived from the DECRYPTED credential blob, and encoding/json quotes its input:
		// a *json.SyntaxError names the offending character and a *json.UnmarshalTypeError
		// names the field it was reading. Wrapping it put credential-derived bytes into
		// every log line and error chain downstream, for exactly the connection whose
		// credentials are malformed. Nothing is lost that a reader could act on — the
		// remedy is "re-save the credential", not "fix byte 41" — and the sentinel keeps
		// the condition distinguishable without carrying a payload.
		return creds, fmt.Errorf("%w: %w: google ads credentials for project %s are not valid JSON",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable, projectID)
	}
	// Trim ONCE, in place, so the emptiness check and the values handed to NewClient are
	// the same strings. Checking the untrimmed value lets a whitespace-only field — the
	// normal result of a copy-paste into a credential form — pass as present and reach
	// Google, where it fails as an opaque upstream error instead of the local
	// incomplete-credentials error that tells the operator what to fix. Trimming only at
	// the check would be worse still: the check would pass and the untrimmed value would
	// then be sent.
	creds.ClientID = strings.TrimSpace(creds.ClientID)
	creds.ClientSecret = strings.TrimSpace(creds.ClientSecret)
	creds.DeveloperToken = strings.TrimSpace(creds.DeveloperToken)
	creds.RefreshToken = strings.TrimSpace(creds.RefreshToken)
	if creds.ClientID == "" || creds.ClientSecret == "" || creds.DeveloperToken == "" || creds.RefreshToken == "" {
		return creds, fmt.Errorf("%w: %w: google ads credentials are incomplete (need clientId, clientSecret, developerToken, refreshToken)",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
	}
	return creds, nil
}

// validatedLoginCustomerID returns the trimmed manager id, having checked it is a shape the
// client will accept.
//
// It is a separate helper, and called by THREE readers — the toggle and discovery
// resolvers, and the create dispatcher — because it used to be inline in the discovery one.
// Originally, the toggle path read the same stored column, passed it to the same client,
// and classified the same defect differently. A malformed manager id reached the client
// uninspected, failed there at validateLoginCustomerID, and arrived at the orchestrator
// indistinguishable from an upstream failure: same call, same error type. The handler's
// default arm answered 503, "the platform did not respond", for a stored value no amount
// of retrying will repair. Checking the value where it is READ, rather than where it is
// used, is what makes it classifiable; doing that in one place keeps all three paths
// from drifting apart.
//
// The client keeps its own validateLoginCustomerID as the backstop for every other caller.
// This is not a duplicate of it: by the time that one fires, the information needed to tell
// a bad stored row from a bad upstream response is gone.
//
// An empty value is legal and means "no manager", so only a NON-empty malformed one fails.
func validatedLoginCustomerID(res *resolved) (string, error) {
	loginCustomerID := strings.TrimSpace(res.providerConfig["login_customer_id"])
	if loginCustomerID != "" && !storedCustomerIDRE.MatchString(loginCustomerID) {
		// The offending VALUE is deliberately not echoed. A manager id is
		// account-identifying configuration, this error reaches the discovery
		// endpoint's log, and the rest of this path keeps error text to a fixed
		// sentinel vocabulary with no payload attached (see the unusable-reason
		// vocabulary log fragment). Naming the field and the rule is everything an
		// operator needs to go fix the row; the value adds nothing they do not
		// already have and puts account data in a log line.
		//
		// systemScoped for the same reason as the credential defect in
		// validateGoogleAdsCredentials: on the LF fallback row there is no project-owned
		// connection to edit, so a bare 400 aims the remedy at someone who cannot apply
		// it. EVERY stored-state defect on this path has to carry it, not just the first
		// one. It lands here rather than at the call sites precisely because there are
		// now three of them — tagging at the caller is what let the toggle path drift
		// away from the discovery path in the first place.
		return "", res.systemScoped(fmt.Errorf("%w: %w: stored login_customer_id is invalid (must be digits only, no dashes or spaces)",
			domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid))
	}
	return loginCustomerID, nil
}

// resolveGoogleAdsClient resolves + validates the project's connection and builds a client
// for the TOGGLE and METRICS paths (see validateGoogleAdsConnection for the shared rules).
//
// Both callers operate on an ALREADY-CREATED campaign, so it resolves via resolveExisting,
// which follows the customer id the campaign was CREATED under (googleAdsCreationCustomerID)
// rather than recomputing one from current config. The campaign's customer id is fixed at
// creation and the provenance guard refuses to address it under any other account — including
// when the forced-system flag made that creating account the LF system one.
func (d *GoogleAdsDispatcher) resolveGoogleAdsClient(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign) (*googleads.Client, error) {
	res, err := d.creds.resolveExisting(ctx, projectID, platform, googleAdsCreationCustomerID(campaign))
	if err != nil {
		return nil, err
	}
	return d.cachedGoogleAdsClient(projectID, platform, res)
}

// cachedGoogleAdsClient returns the client for this connection, building it only when there is no
// live entry for the row identity the credential just resolved from.
//
// Reusing the CLIENT is what removes the OAuth exchange — the token is cached on the instance, so
// a client rebuilt per call re-mints it however cheap the credential lookup became. The validation
// is the credential's own (row id + version), so a rotation invalidates the client in the same
// step that invalidates the credential; a stale client is a stale credential.
//
// Not applied to the DISCOVERY path, which deliberately builds an account-agnostic client with an
// empty CustomerID, nor to adoption's owned-connection path, which is a rare one-shot rather than
// the polling loop this exists for.
func (d *GoogleAdsDispatcher) cachedGoogleAdsClient(projectID string, platform model.Provider, res *resolved) (*googleads.Client, error) {
	key, connID, version := res.cacheIdentity(projectID, platform)
	built, err := d.clients.buildOnce(key, connID, version, func() (any, error) {
		return d.googleAdsClientFor(projectID, res)
	})
	if err != nil {
		return nil, err
	}
	client, isClient := built.(*googleads.Client)
	if !isClient {
		// Unreachable: this cache is written only by the closure above. Rebuild rather than
		// assert, so a future second writer cannot turn a type confusion into a panic.
		return d.googleAdsClientFor(projectID, res)
	}
	return client, nil
}

// resolveOwnedGoogleAdsClient is resolveGoogleAdsClient for the one path that must NOT accept
// the LF system fallback: adoption.
//
// Every other platform call names a campaign this project already has a ROW for, and the row —
// scoped by project_id — is the authorization. Adoption's caller instead names an ARBITRARY
// upstream id, and credsSource.resolve deliberately falls back to the single LF-owned system
// account for any project with no connection of its own. Under that fallback every such project
// shares ONE ad account, so project A could name a campaign project B created there, bind it to
// its own brief, and thereafter read its spend and pause it. The account-mismatch guards do not
// help: both projects resolve to the same customer id, which is the whole problem.
//
// It therefore calls resolveOwned, which does not consult the system scope, rather than calling
// resolve and rejecting a result tagged fromSystem. The difference is not stylistic: resolve
// loads, validates and DECRYPTS the LF row before returning, so an LF credential that is missing
// or no longer decrypts comes back as an error INSTEAD of a resolved value — a 500 about a row
// this path would have refused anyway, for a caller whose remedy is simply to connect their own
// ad account. Not looking is the only version of the refusal that stays correct as the fallback
// grows new failure modes.
//
// There is no upstream metadata that would fix this instead. A campaign's name, labels and
// budget are all set by whoever created it, so none of them is evidence of which project owns
// it. Requiring a project-owned connection is the check that holds here, and it costs nothing
// real: a project with no ad account of its own has no campaign of its own to adopt.
//
// It is worth being exact about what that establishes, because it is less than it looks.
// Google Ads is ONE shared customer across every foundation (docs/architecture.md, "Account
// Tenancy"), so two projects with their own connections still resolve the same account, and
// this check does not stop one from naming the other's campaign. It cannot: that project's
// credential already grants read and pause on every campaign in the customer, straight
// through Google's API, so no rule applied here can be more restrictive than the credential
// the call is made with. Account tenancy is where that boundary lives. What this check
// removes is the case where a project holds NO credential at all and the fallback would have
// lent it one. The service's own invariant — one upstream campaign, one brief — is enforced
// where it can be: migration 000020's index, keyed globally rather than per project for
// exactly this reason.
func (d *GoogleAdsDispatcher) resolveOwnedGoogleAdsClient(ctx context.Context, projectID string, platform model.Provider) (*googleads.Client, error) {
	res, err := d.creds.resolveOwned(ctx, projectID, platform)
	if err != nil {
		// resolveOwned never consults the LF system scope, so an absence here means the
		// PROJECT has no connection — the one thing adoption requires. Returned unchanged it
		// would land in the adopt switch's default arm: a 503 telling the caller the platform
		// could not be reached. It was never contacted, and no amount of retrying will change
		// that; the remedy is permanent and actionable, so it gets the 409 sentinel. Every
		// OTHER failure (a repo error, an unusable project connection) is passed through,
		// because those are genuinely different remedies.
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("%w: project %s has no %s connection, and adoption cannot fall back to the LF system account: %w",
				domain.ErrAdoptionRequiresOwnConnection, projectID, platform, err)
		}
		return nil, err
	}
	return d.googleAdsClientFor(projectID, res)
}

// googleAdsClientFor validates an already-resolved connection and builds the client. Split out
// so the owned-connection check above can run BETWEEN resolution and client construction without
// resolving (and decrypting) twice.
func (d *GoogleAdsDispatcher) googleAdsClientFor(projectID string, res *resolved) (*googleads.Client, error) {
	creds, accountID, err := validateGoogleAdsConnection(projectID, res)
	if err != nil {
		return nil, err
	}
	loginCustomerID, err := validatedLoginCustomerID(res)
	if err != nil {
		return nil, err
	}
	return googleads.NewClient(
		googleads.Credentials{
			ClientID:       creds.ClientID,
			ClientSecret:   creds.ClientSecret,
			DeveloperToken: creds.DeveloperToken,
			RefreshToken:   creds.RefreshToken,
		},
		googleads.AccountConfig{
			CustomerID:      accountID,
			LoginCustomerID: loginCustomerID,
			Label:           res.label,
		},
		d.opts...,
	), nil
}

// resolveGoogleAdsDiscoveryClient builds a client for the ACCOUNT-DISCOVERY path: same
// credential resolution and the same connection checks as resolveGoogleAdsClient, minus the
// account-id requirement (see validateGoogleAdsCredentials for the two lifecycles that
// serves: re-pointing an existing connection, and first-time bootstrap of one created with
// credentials only).
//
// CustomerID is left empty regardless of whether the connection stores one, because the
// upstream operation is account-AGNOSTIC: it asks which customer ids the credential itself
// reaches, so scoping it to one of them would narrow the answer to a subset of the question.
// The manager id, when present, is what lets discovery expand an MCC hierarchy rather than
// returning only the manager itself.
func (d *GoogleAdsDispatcher) resolveGoogleAdsDiscoveryClient(ctx context.Context, projectID string, platform model.Provider) (*googleads.Client, error) {
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	// Everything from here to NewClient inspects STORED state, before any request exists.
	// A failure means the connection needs editing, not retrying, so each one carries
	// domain.ErrConnectionNotUsable — the validator tags its own, and validatedLoginCustomerID
	// tags the login_customer_id check below. Errors from creds.resolve above are deliberately
	// NOT tagged: that layer distinguishes ErrNotFound (no connection — a 404) from a real
	// storage failure (which IS transient and IS a 503), and flattening both into "not
	// usable" would lose it.
	creds, err := validateGoogleAdsCredentials(projectID, res)
	if err != nil {
		// Already tagged for BOTH axes by the validator: domain.ErrConnectionNotUsable for
		// the status, and — when these credentials came from the LF fallback row —
		// domain.ErrSystemConnectionNotUsable for the audience, so the defect pages
		// whoever installed the LF credential instead of returning a 400 to a project that
		// owns no connection to fix. This used to be tagged here, at the caller, which is
		// why only THIS path had it.
		return nil, err
	}
	loginCustomerID, err := validatedLoginCustomerID(res)
	if err != nil {
		return nil, err
	}
	return googleads.NewClient(
		googleads.Credentials{
			ClientID:       creds.ClientID,
			ClientSecret:   creds.ClientSecret,
			DeveloperToken: creds.DeveloperToken,
			RefreshToken:   creds.RefreshToken,
		},
		googleads.AccountConfig{
			LoginCustomerID: loginCustomerID,
			Label:           res.label,
		},
		d.opts...,
	), nil
}

// resolveOwnedGoogleAdsDiscoveryClient is resolveGoogleAdsDiscoveryClient for the monitor read,
// which — like adoption's resolveOwnedGoogleAdsClient above — must NOT accept the LF system
// fallback.
//
// round-16 review escalated the gap resolveGoogleAdsDiscoveryClient's caller used to accept
// (documented, not fixed, in round-15) to Critical: the four monitor endpoints are externally
// routed (see this branch's httproute.yaml/ruleset.yaml chart changes), so a project with no
// Google Ads connection of its own could read another project's spend/budget/campaign data for
// any customer id the shared LF system credential reaches.
//
// The fix mirrors adoption's precedent exactly, and for the same reason resolveOwned's own doc
// comment gives: Google Ads is ONE shared customer across every foundation (docs/architecture.md,
// "Account Tenancy"), so checking accountID against what ListAccounts would return for THIS
// project is a no-op — two different projects both on the system fallback see the identical
// shared account list from the identical credential, so membership in that list proves nothing
// about ownership. Refusing the system fallback outright, rather than checking membership
// against it, is the only version of this check that actually distinguishes the two projects.
func (d *GoogleAdsDispatcher) resolveOwnedGoogleAdsDiscoveryClient(ctx context.Context, projectID string, platform model.Provider) (*googleads.Client, error) {
	client, _, err := d.resolveOwnedGoogleAdsDiscovery(ctx, projectID, platform)
	return client, err
}

// resolveOwnedGoogleAdsDiscovery is the body of the above, returning the resolved row as well
// as the client. ProbeConnection needs both — the client to make the call, and the row to know
// WHICH customer id the connection is configured for, which is the half of a connection test
// that "does the credential authenticate" does not answer.
//
// extra appends per-caller client options. It exists because the two callers want DIFFERENT
// token-refresh lifetimes off one construction path: the probe's client is used once and
// dropped, so its refresh must stay bounded by the probe's context, while the monitor read
// keeps the default detached refresh.
func (d *GoogleAdsDispatcher) resolveOwnedGoogleAdsDiscovery(ctx context.Context, projectID string, platform model.Provider, extra ...googleads.Option) (*googleads.Client, *resolved, error) {
	res, err := d.creds.resolveOwned(ctx, projectID, platform)
	if err != nil {
		return nil, nil, err
	}
	creds, err := validateGoogleAdsCredentials(projectID, res)
	if err != nil {
		return nil, res, err
	}
	loginCustomerID, err := validatedLoginCustomerID(res)
	if err != nil {
		return nil, res, err
	}
	return googleads.NewClient(
		googleads.Credentials{
			ClientID:       creds.ClientID,
			ClientSecret:   creds.ClientSecret,
			DeveloperToken: creds.DeveloperToken,
			RefreshToken:   creds.RefreshToken,
		},
		googleads.AccountConfig{
			LoginCustomerID: loginCustomerID,
			Label:           res.label,
		},
		// Copied rather than appended in place: d.opts is shared by every call on this
		// dispatcher, and appending to it could publish one caller's extra options to
		// the next through a reused backing array.
		append(append([]googleads.Option(nil), d.opts...), extra...)...,
	), res, nil
}

// ProbeConnection verifies the project's stored Google Ads connection against Google itself:
// it refreshes the stored credential and enumerates the customers that credential reaches,
// then checks the configured customer id is among them.
//
// It satisfies the service-side ConnectionProber interface, type-asserted by
// Orchestrator.ProbeConnection for the connection-test endpoint. Read-only: it creates,
// changes and deletes nothing.
//
// It resolves via resolveOwnedGoogleAdsDiscovery (d.creds.resolveOwned), NOT d.creds.resolve —
// the same rule LinkedIn's VerifyAccountOrg follows and for the same reason: the forced-system
// fallback would let a connection test on a project with no Google Ads connection of its own
// silently verify the shared LF SYSTEM row instead of reporting that this project has nothing
// to test. That matters more here than anywhere, because Google Ads is ONE shared customer
// across every foundation (docs/architecture.md, "Account Tenancy").
//
// It asks via googleads.ProbeAccountReach rather than by checking membership of
// ListAccessibleCustomers, and the difference is a verdict rather than a refactor. That
// enumeration is the account PICKER's: in manager mode it is filtered to ENABLED, non-manager
// clients, because those are the accounts a campaign may be created in. Presence in it does
// prove the connection can dispatch. Absence proves nothing — a suspended, cancelled or closed
// account, and a sub-manager account, are all reached perfectly well by the credential and all
// missing from that list — so reading absence as unreachability reported "the google ads
// credential authenticates but does not reach account X" about a credential that reaches X,
// and sent the operator to repoint an account id that was correct. ProbeAccountReach runs the
// same walk unfiltered and distinguishes the three.
//
// It is still the right probe for the half it always answered: it runs the token refresh — the
// exact call that fails for a revoked refresh token, the production failure this endpoint
// existed and did not catch — and in manager mode it answers about the customers actually
// addressable THROUGH this client, rather than about some list that merely mentions the id.
func (d *GoogleAdsDispatcher) ProbeConnection(ctx context.Context, projectID string, platform model.Provider) error {
	// The client is built here, used for one enumeration and dropped, so its token refresh
	// has no other waiter to protect and no later caller to serve. Left detached it would run
	// on the platform's own request timeout — longer than the bound ProbeConnection puts on
	// the probe — and keep a goroutine and its socket alive past a probe already given up on.
	client, res, err := d.resolveOwnedGoogleAdsDiscovery(ctx, projectID, platform, googleads.WithCallerScopedTokenRefresh())
	if err != nil {
		return err
	}
	subject := probeSubject{platform: platform, accountID: res.accountID}
	// Decided BEFORE the call, not after. probeMembership would reach the same verdict, but
	// only on the path where the enumeration succeeds: an inconclusive 5xx on the way there
	// would answer "google ads could not be reached, try again" about a connection that names
	// no account to reach — sending the operator to wait out an unrelated platform outage
	// instead of to the empty field on their own row, which is the thing they can fix.
	if strings.TrimSpace(res.accountID) == "" {
		return subject.noAccountConfigured()
	}
	// Decided before the call for the same reason. The dashed form the Google Ads UI displays —
	// 866-674-6580 — is the shape this guards: as of LFXV2-2665 account_id carries a Pattern at
	// the design layer (design/connection.go), so an HTTP caller can no longer store it, but Goa
	// validates only the HTTP transport and bootstrap, migrations and rows written before that
	// pattern existed never passed through it. ListAccessibleCustomers answers in the undashed
	// form and can never contain the dashed one, so without this the membership check would miss
	// and report "the
	// credential authenticates but does not reach account 866-674-6580" about a credential that
	// reaches that account perfectly well under the id Google actually uses.
	if err := googleads.ValidateCustomerID(strings.TrimSpace(res.accountID)); err != nil {
		return subject.accountIDNotUsable()
	}
	reach, lerr := client.ProbeAccountReach(ctx, strings.TrimSpace(res.accountID))
	if lerr != nil {
		return subject.probeClass(lerr, googleads.ProbeCredentialRejected, googleads.ProbeInconclusive, googleads.ProbeNotSent)
	}
	switch reach {
	case googleads.AccountReachable:
		return nil
	case googleads.AccountIsManager:
		return subject.accountIsManagerAccount()
	case googleads.AccountNotEnabled:
		return subject.accountNotEnabled()
	default:
		// googleads.AccountUnreachable, and the zero value with it. Now a true statement: the
		// walk behind it is unfiltered, so absence is absence from the hierarchy rather than
		// absence from a list that had already dropped the account for being disabled.
		return subject.accountNotReachable()
	}
}

// ListAccountCampaignMetrics implements service.AccountMetricsReader for Google Ads,
// backing the account-monitor endpoint. It resolves an account-agnostic client the same way
// ListAccounts does (a monitor read names its own target accountID, distinct from whatever
// customer id the project's connection currently points at) — except, per round-16 review, it
// refuses the LF system fallback entirely (resolveOwnedGoogleAdsDiscoveryClient, not
// resolveGoogleAdsDiscoveryClient): see that resolver's doc comment for why membership-checking
// against ListAccounts would not have closed this gap. It then reads every campaign visible on
// that customer id via googleads.Client.ListAccountCampaigns, which itself derives the
// campaign-metrics.service.ts resolveDateRange window (end = today, start = today -
// (days-1)) from its own injected clock rather than the wall clock.
//
// A project with no Google Ads connection of its own now gets domain.ErrNotFound (404 "no
// connection configured for this project", via classifyDiscoveryError) instead of a read served
// from the shared system credential. Same fix applies to the LinkedIn/Meta siblings of this
// method; Reddit needs none, because its dispatcher already scopes accountID to the resolved
// connection's own single account rather than building an account-agnostic client.
func (d *GoogleAdsDispatcher) ListAccountCampaignMetrics(ctx context.Context, projectID string, platform model.Provider, accountID string, days int) ([]model.AccountCampaignMetrics, error) {
	// Validated up front, before any credential is resolved: this is defense-in-depth for a
	// non-HTTP caller that bypasses Goa — an ordinary HTTP request already gets refused by the
	// design attribute's own Pattern (see design/connection.go) before this method ever runs.
	// Without this check, a malformed value reaching here would hit gaqlSearchForCustomer's
	// own unsentineled error, which classifyDiscoveryError's default arm maps to an opaque
	// 503 — this wraps the same shape check in domain.ErrAccountIDMalformed for the clean 400.
	if err := googleads.ValidateCustomerID(accountID); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrAccountIDMalformed, err)
	}
	if err := validateMonitorDays(days); err != nil {
		return nil, err
	}
	client, err := d.resolveOwnedGoogleAdsDiscoveryClient(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	rows, lerr := client.ListAccountCampaigns(ctx, accountID, days)
	if lerr != nil {
		return nil, lerr
	}
	out := make([]model.AccountCampaignMetrics, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.AccountCampaignMetrics{
			PlatformCampaignID: r.CampaignID,
			Name:               r.Name,
			Status:             r.Status,
			IsSearchChannel:    r.IsSearchChannel,
			Spend:              r.SpendUSD,
			Impressions:        r.Impressions,
			Clicks:             r.Clicks,
			Ctr:                r.Ctr,
			Conversions:        r.Conversions,
			BudgetDay:          r.BudgetDailyUSD,
			// Google Ads campaigns read by this port are not schedule-bound the way
			// LinkedIn/Meta/Reddit's are (see model.AccountCampaignMetrics.PacingUnknown's
			// doc comment); StartDate/EndDate are left empty and PacingUnknown false —
			// monitor_google.go's pacing formula does not consult flight dates at all.
			FetchFailed: r.FetchFailed,
			CampaignURL: buildGoogleAdsCampaignURL(r.CampaignID),
		})
	}
	return out, nil
}

// buildGoogleAdsCampaignURL ports the BFF's buildGoogleAdsUrl(campaignId): a direct link to
// the campaign in the Google Ads UI. Empty campaignID (should not happen for a real row) yields
// no URL rather than a malformed one.
func buildGoogleAdsCampaignURL(campaignID string) string {
	if campaignID == "" {
		return ""
	}
	return "https://ads.google.com/aw/campaigns?campaignId=" + campaignID
}

// googleAdsRunStatus maps the service's run-state vocabulary to Google's campaign status.
// Note Google spells the serving state ENABLED, not ACTIVE.
func googleAdsRunStatus(status string) (string, error) {
	switch status {
	case model.CampaignRunActive:
		return googleads.StatusEnabled, nil
	case model.CampaignRunPaused:
		return googleads.StatusPaused, nil
	default:
		return "", fmt.Errorf("unsupported campaign run status %q (want %q or %q)", status, model.CampaignRunActive, model.CampaignRunPaused)
	}
}

// ListAccounts discovers the ad accounts reachable via the project's stored,
// encrypted Google Ads connection credential, returning minimal identifying
// information (customer ID and optionally a display label).
//
// It satisfies the service-side AccountLister interface, which Orchestrator.ReadAccounts
// type-asserts on the dispatcher for the requested platform; a platform whose dispatcher
// does not implement it gets domain.ErrAccountsUnsupported and the ad platform is never
// contacted. The error contract below is what ReadAccounts and the endpoint's status
// mapping rely on.
func (d *GoogleAdsDispatcher) ListAccounts(ctx context.Context, projectID string, platform model.Provider) ([]model.AccessibleAccount, error) {
	client, err := d.resolveGoogleAdsDiscoveryClient(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	customers, lerr := client.ListAccessibleCustomers(ctx)
	if lerr != nil {
		return nil, lerr
	}
	// Convert Google Ads customers to the common AccessibleAccount shape.
	// Each customer's descriptive_name is used as the label; the resource_name
	// (customers/DIGITS) is parsed to extract the numeric customer_id.
	//
	// make(..., 0, n) rather than a nil var: a credential that legitimately reaches
	// zero accounts is an empty list, not an error, and the two must stay
	// distinguishable at the service boundary — a nil slice invites the caller that
	// lands next to read it as "no answer" and report the platform as down for a
	// perfectly valid empty one.
	accounts := make([]model.AccessibleAccount, 0, len(customers))
	for _, cust := range customers {
		// Parse "customers/1234567890" → "1234567890"
		id := strings.TrimPrefix(cust.ResourceName, "customers/")
		accounts = append(accounts, model.AccessibleAccount{
			ID:    id,
			Label: cust.DescriptiveName,
		})
	}
	return accounts, nil
}

// ToggleStatus implements service.StatusToggler for Google Ads.
//
// GA-3b created ad group + ad under the campaign; GA-3c wired the dispatcher-level cascade;
// GA-4 enables ACTIVATE once targeting exists. Both directions cascade, in OPPOSITE orders.
//
// PAUSE cascades from the campaign FIRST (stops delivery immediately, regardless of whether the
// children can be reached) down to the ad group/ad via the persisted ids stored in the campaign's
// Result blob.
//
// ACTIVATE is refused with ErrCampaignNotProvisioned (→409, raised locally without calling
// Google) unless the Result blob shows this CHANNEL's own serving resources were fully
// provisioned. What those are differs per channel and lives in googleAdsActivationGate — the
// ad group/ad plus a keyword criterion on Search, the ad group/ad alone on Demand Gen, the
// asset group on Performance Max, which has neither. A campaign that cannot deliver would
// report false success, the exact lie ErrCampaignNotProvisioned exists to prevent. When the
// gate passes, ACTIVATE cascades children-first (children activated before campaign) so a
// campaign never reports ENABLED before its children do.
func (d *GoogleAdsDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error {
	gaStatus, err := googleAdsRunStatus(status)
	if err != nil {
		return err
	}
	// Refuse ACTIVATE if this campaign's own serving resources were not successfully
	// provisioned. What those ARE is channel-specific and lives in googleAdsActivationGate:
	// the keyword requirement named here is SEARCH-ONLY, because Demand Gen refuses keywords
	// at create and Performance Max has neither ad groups nor keywords. Both inputs come from
	// the persisted Result blob — keywordsProvisioned from KeywordCriteriaIDs (empty means
	// keyword targeting was never attempted, or failed before any criterion resource name
	// could be parsed), assetGroupID from the Performance Max creative.
	targets, keywordsProvisioned, incompleteGroups := googleAdsToggleTargets(campaign)
	assetGroupID := googleAdsToggleAssetGroup(campaign)
	if gaStatus == googleads.StatusEnabled {
		if gErr := googleAdsActivationGate(campaign, targets, keywordsProvisioned, assetGroupID); gErr != nil {
			return gErr
		}
	}
	// A group recorded but not toggleable is reported, never silently skipped. The toggle
	// below still applies to every group that CAN take it — refusing the whole campaign
	// because one group's create failed would strand a campaign that is otherwise ready —
	// but the operator needs to know the campaign's state is not uniform.
	if len(incompleteGroups) > 0 {
		slog.WarnContext(ctx, "google ads campaign has ad groups that were never fully created; the status toggle cannot reach them",
			"campaign_id", campaign.ID,
			"platform_campaign_id", campaign.PlatformCampaignID,
			"status", status,
			"incomplete_ad_groups", strings.Join(incompleteGroups, ", "),
			"toggled_ad_groups", len(targets))
	}
	client, err := d.resolveGoogleAdsClient(ctx, projectID, platform, campaign)
	if err != nil {
		return err
	}
	// Same identity invariant ReadMetrics enforces, and it matters MORE here because this
	// path MUTATES. The stored campaign/ad-group/ad ids are bare numerics, unique only
	// within the customer they were created under, while the connection just resolved is the
	// project's CURRENT one — UpdateGoogleAds can re-point it between create and toggle. On
	// an id collision the mutate succeeds against ANOTHER account's resources, pausing or
	// enabling something this project does not own. Fail before contacting Google, for both
	// PAUSE and ACTIVATE (the check sits above both branches deliberately).
	if created := googleAdsCreationCustomerID(campaign); created != "" && created != client.CustomerID() {
		return fmt.Errorf("toggle google ads campaign status: campaign %s was created under customer %s but the project's current connection resolves to customer %s: %w",
			campaign.PlatformCampaignID, created, client.CustomerID(), domain.ErrCampaignAccountMismatch)
	}

	wrapUnconfirmed := func(uerr error) error {
		if googleads.IsOutcomeUnconfirmed(uerr) {
			return &unconfirmedToggleError{err: uerr}
		}
		return uerr
	}

	if gaStatus == googleads.StatusPaused {
		// Campaign first: pausing the parent stops delivery immediately even if the
		// child update below fails/is unconfirmed.
		if uerr := client.UpdateCampaignStatus(ctx, campaign.PlatformCampaignID, gaStatus); uerr != nil {
			return wrapUnconfirmed(uerr)
		}
		// If no child ids were recorded (e.g. a campaign shell with no fully-created
		// children), there is nothing to pause downstream — only the campaign is toggled.
		if !googleAdsHasToggleChildren(targets, assetGroupID) {
			return nil
		}
		if uerr := googleAdsToggleChildren(ctx, client, targets, assetGroupID, gaStatus); uerr != nil {
			// After the campaign status succeeds, a child failure (even a definite 4xx) is a
			// partial cascade: the parent changed but the child's outcome is unknown. Wrap it
			// as Unconfirmed so the caller knows to verify the state before retry.
			return &unconfirmedToggleError{err: uerr}
		}
		return nil
	}

	// ACTIVATE: children first (googleAdsActivationGate above has confirmed this channel's
	// creative is fully provisioned), campaign last — so the campaign only reports ENABLED
	// once the resources that actually serve already do.
	if uerr := googleAdsToggleChildren(ctx, client, targets, assetGroupID, gaStatus); uerr != nil {
		// UpdateAdGroupsAndAdsStatus flips every ad group first, then every ad
		// (children-first ordering). A definite first-stage failure (4xx from
		// adGroups:mutate) is NOT a partial cascade (nothing changed). A definite
		// second-stage failure (4xx from adGroupAds:mutate after adGroups succeeded) IS a
		// partial cascade and returns partialCascadeError, which
		// wrapUnconfirmed correctly classifies as unconfirmed. Ambiguous outcomes (5xx/timeout)
		// are also wrapped as unconfirmed.
		return wrapUnconfirmed(uerr)
	}
	if uerr := client.UpdateCampaignStatus(ctx, campaign.PlatformCampaignID, gaStatus); uerr != nil {
		// After the children succeed, a campaign failure (even a definite 4xx) is a partial
		// cascade: the children already changed but the campaign's outcome is unknown. Wrap it
		// as Unconfirmed unconditionally so the caller knows to verify state before retry —
		// mirrors the PAUSE path's child-after-campaign wrap above.
		return &unconfirmedToggleError{err: uerr}
	}
	return nil
}

// ReadMetrics implements service.MetricsReader for Google Ads. It resolves the same
// connection ToggleStatus does and reads the campaign's live GAQL metrics.
//
// The platform-agnostic window is translated to Google's GAQL literal by
// googleads.WindowFor — in the platform package, not here, so the GAQL dialect stays behind
// that boundary. A window Google cannot express is reported as
// domain.ErrMetricsWindowUnsupported (400, caller input) rather than a 503, and the platform
// is never contacted for it.
func (d *GoogleAdsDispatcher) ReadMetrics(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	gaWindow, err := googleads.WindowFor(window)
	if err != nil {
		return nil, fmt.Errorf("read google ads metrics: %w", errors.Join(domain.ErrMetricsWindowUnsupported, err))
	}
	client, err := d.resolveGoogleAdsClient(ctx, projectID, platform, campaign)
	if err != nil {
		return nil, err
	}
	// The stored PlatformCampaignID is a bare numeric id, unique only within the customer it
	// was created under, and the connection just resolved is the project's CURRENT one —
	// UpdateGoogleAds can re-point it at a different account between create and read. Querying
	// the id under the wrong customer does not error: it returns no rows, which is
	// indistinguishable from a campaign with genuinely zero activity, and on an id collision
	// it returns ANOTHER account's numbers. Fail loudly instead, before contacting Google.
	if created := googleAdsCreationCustomerID(campaign); created != "" && created != client.CustomerID() {
		return nil, fmt.Errorf("read google ads metrics: campaign %s was created under customer %s but the project's current connection resolves to customer %s: %w",
			campaign.PlatformCampaignID, created, client.CustomerID(), domain.ErrCampaignAccountMismatch)
	}
	m, err := client.GetCampaignMetrics(ctx, campaign.PlatformCampaignID, gaWindow)
	if err != nil {
		return nil, err
	}
	return &model.CampaignMetrics{
		CampaignID: m.CampaignID,
		// The request window, not the client's echoed GAQL literal: the API contract is the
		// platform-agnostic vocabulary, and translating back would reintroduce the dialect.
		Window:      window,
		Impressions: m.Impressions,
		Clicks:      m.Clicks,
		CostMicros:  m.CostMicros,
		Ctr:         m.Ctr,
		// Carried through as a POINTER rather than being flattened, because the DOMAIN field
		// is a pointer and its nil carries meaning across platforms: it marks the adapters
		// whose platform reports no campaign-level conversion count at all (Meta, X, Reddit,
		// email), and the no_conversions rule refuses to fire on it.
		//
		// This adapter never actually produces that nil. googleads.GetCampaignMetrics
		// materialises a non-nil count on BOTH of its paths — an explicit zero for a no-row
		// window, and a zero default when the row omits the conversions member — because for
		// Google a no-activity window is a MEASUREMENT of zero, not an absence of one.
		// Dereferencing here would still be wrong: it would bake that invariant into a second
		// file, so a future path that legitimately returns nil would panic here instead of
		// propagating the absence the type already knows how to express.
		Conversions: m.Conversions,
	}, nil
}

// LookupCampaign implements service.CampaignAdopter for Google Ads: it confirms that
// platformCampaignID names a real, live campaign under the PROJECT'S OWN connection, so an
// existing campaign can be bound to a brief without creating anything.
//
// It resolves the ordinary (non-discovery) client deliberately. Discovery credentials can
// reach every account the login customer administers; adoption must be scoped to the one
// account this project is connected to, or a caller could bind a campaign belonging to a
// different project — or a different foundation — and then read its spend through this
// service. The account check that ReadMetrics performs after the fact is unnecessary here
// for the same reason it is unavailable: there is no stored row yet to compare against, so
// the connection IS the scope, and googleads.GetCampaign issues its query against exactly
// that customer.
//
// (nil, nil) means the campaign is genuinely absent under this connection — that is
// googleads.GetCampaign's contract, and it distinguishes absence from every unverifiable
// answer, each of which it returns as an error rather than an empty result.
func (d *GoogleAdsDispatcher) LookupCampaign(ctx context.Context, projectID string, platform model.Provider, platformCampaignID string) (*model.PlatformCampaignRef, error) {
	// Validate the platform campaign ID BEFORE resolving the connection. A malformed id is
	// a permanent input fault regardless of connection state, so it should produce the same
	// 400 error either way. Validating first avoids decrypting credentials for a request
	// that can never succeed and guarantees the permanent fault masks any contingent one
	// (like a missing or unusable connection).
	if err := googleads.ValidateCampaignID(platformCampaignID); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, err)
	}

	client, err := d.resolveOwnedGoogleAdsClient(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	ref, err := client.GetCampaign(ctx, platformCampaignID)
	if err != nil {
		// A malformed id never reached the network, so it is a 400 rather than the
		// default "the platform could not be reached" 503 the caller would retry.
		//
		// This branch is UNREACHABLE as written: ValidateCampaignID above is the only
		// producer of ErrNotACampaignID, GetCampaign reaches it by calling that same
		// function on the same string, so anything the pre-check admits GetCampaign
		// admits too. It stays because it is the mapping that keeps the two honest —
		// if GetCampaign ever grows a validation the pre-check does not mirror, the
		// result is still a 400 here rather than a retryable 503 for input that can
		// never succeed. Deleting it would make that divergence silent.
		if errors.Is(err, googleads.ErrNotACampaignID) {
			return nil, fmt.Errorf("%w: %w", domain.ErrInvalidPlatformCampaignID, err)
		}
		return nil, fmt.Errorf("look up google ads campaign: %w", err)
	}
	if ref == nil {
		return nil, nil
	}
	// ref.Status (ENABLED/PAUSED) is deliberately not carried across: reaching this line
	// at all already means the campaign is live, because GetCampaign filters REMOVED
	// server-side and errors on any status outside its known set rather than passing it
	// on. Adoptability is decided here, in Google's vocabulary; the service layer never
	// sees it. See model.PlatformCampaignRef.
	// The resolved customer id travels with the ref so the adopted row records the account
	// it was verified under, exactly as a created row does. googleAdsCreationCustomerID
	// reads this back; without it every adopted row answers "unknown" and the
	// account-mismatch guards in ReadMetrics and ToggleStatus wave it through.
	scope, merr := json.Marshal(struct {
		CustomerID string `json:"customerId"`
	}{CustomerID: client.CustomerID()})
	if merr != nil {
		return nil, fmt.Errorf("look up google ads campaign: record account scope: %w", merr)
	}
	// Establish the SLOT from what Google says the campaign is. Adoption's input names no
	// campaign type, so this is the only evidence available — and filing every adopted
	// campaign under 'default' meant adopting a Demand Gen campaign left the 'demand-gen'
	// slot free for a later dispatch to fill with a second paid campaign.
	//
	// Fails closed: an unrecognised or absent channel type is refused rather than defaulted.
	// A campaign type this service has no slot for cannot be bound safely — 'default' would
	// be a guess, and the guess is what creates the duplicate.
	variant, verr := googleAdsVariantForChannelType(ref.AdvertisingChannelType)
	if verr != nil {
		return nil, verr
	}
	return &model.PlatformCampaignRef{ID: ref.ID, Name: ref.Name, Result: scope, Variant: variant}, nil
}

// googleAdsVariantForChannelType maps Google's advertising_channel_type onto this service's
// variant slot. The two vocabularies are deliberately separate: Google's is an upstream enum
// that grows without our involvement, and the mapping is the point at which this service
// decides whether it can represent a campaign at all.
//
// Only the types this service can CREATE are mappable. Anything else — SHOPPING, HOTEL,
// a value Google adds next quarter, or an empty string from a response that omitted the
// field — is refused. Adopting one would file it under some existing slot and leave that
// campaign type's real slot open for a duplicate.
//
// This list grows with the create paths, and must: PERFORMANCE_MAX and then VIDEO were each
// unmappable until this service learned to create them, and a channel left out here is a campaign the
// adoption path refuses even though create would have produced exactly that type.
//
// The slot is keyed on the channel TYPE alone, deliberately. The adopt lookup selects
// campaign.advertising_channel_type and not advertising_channel_sub_type, so every VIDEO
// campaign occupies the single `video` slot whether or not it is the VIDEO_ACTION sub-type
// this service creates — a YouTube reach, bumper or sequence campaign adopted onto a brief
// takes the slot a later Video-action dispatch would have wanted. That is the model, not an
// oversight: SEARCH has sub-types too and has always been treated this way, and one slot per
// advertising_channel_type is what AdoptableVariants enumerates. Narrowing it would mean
// selecting the sub-type in the lookup and refusing a non-VIDEO_ACTION adoption, which is
// safe to do (refusing an ADOPT is not the over-refusal of a CREATE) but is a change to the
// slot model rather than to this function.
func googleAdsVariantForChannelType(channelType string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(channelType)) {
	case googleAdsChannelTypeSearch:
		// Search is this platform's default slot: an absent channel and an explicit
		// "search" both dispatch the same campaign, and both normalise to VariantDefault.
		return model.VariantDefault, nil
	case googleAdsChannelTypeDemandGen:
		return model.NormalizeVariant(googleAdsChannelDemandGen), nil
	case googleAdsChannelTypePerformanceMax:
		return model.NormalizeVariant(googleAdsChannelPerformanceMax), nil
	case googleAdsChannelTypeVideo:
		return model.NormalizeVariant(googleAdsChannelVideo), nil
	case googleAdsChannelTypeDisplay:
		return model.NormalizeVariant(googleAdsChannelDisplay), nil
	case "":
		return "", fmt.Errorf("google ads: the campaign lookup returned no advertising channel type, so which campaign type this is cannot be established; refusing to adopt rather than assume")
	default:
		return "", fmt.Errorf("google ads: campaign type %q is not one this service creates, so it has no slot to adopt into", channelType)
	}
}

// googleAdsRecordedChannelType recovers the advertising channel type this service ASKED for
// from the row's ConfigSnapshot, expressed in Google's own enum vocabulary so it can be
// compared against campaign.advertising_channel_type as read from the platform.
//
// The snapshot is the googleAdsConfig marshalled whole by applyCampaignConfig, on both the
// create and the adoption path, so Channel is recorded for every row either path wrote.
//
// An ABSENT Channel maps to SEARCH, not to nil — but ONLY once the snapshot has been
// confirmed to be one this adapter wrote. That qualification is the whole of the fix here,
// and without it the default is a fabrication.
//
// `json.Unmarshal` into a `Channel string` cannot tell three different things apart. An
// omitted `"channel"` key, an explicit `"channel": null`, and a config object carrying no
// Google Ads fields whatsoever all decode to `""`, and all three were then reported as a
// recorded SEARCH. Only the FIRST of those means "a legacy caller who meant Search". The
// other two are snapshots that say nothing about the channel at all, and `UpdateCampaign`
// persists arbitrary caller-supplied `config` JSON into this column, so both are reachable
// from an untrusted request rather than only from this service's own writes. Defaulting them
// manufactured a match or a divergence against a value nobody recorded, breaking the rule
// this readback rests on: report agreement only when BOTH sides were actually read.
//
// So "recognisable" is decided BEFORE the default is applied, and it is decided on a property
// this adapter guarantees and a caller is unlikely to reproduce by accident:
// `applyCampaignConfig` marshals the `googleAdsConfig` STRUCT VALUE whole, on both the create
// (googleads.go:391) and the adoption (:454) path, and no field on that struct carries
// `omitempty` — so a snapshot this adapter wrote ALWAYS contains the `"channel"` key, even
// when the value is `""`. Presence of the key is therefore exactly equivalent to "written by
// this adapter", which is the population the legacy default was reasoned about.
//
// Absence still means what it always meant. The default was never "an empty string means
// Search"; it was "a caller who predates the field means Search", and such a caller's row was
// written by applyCampaignConfig and so HAS the key with an empty value. That row still reads
// SEARCH. What changes is that a snapshot which never carried the key — something this
// adapter did not write — no longer borrows a default that was justified for a different
// population. It reports `unknown`, which is the accurate statement: nothing was recorded.
//
// nil is returned wherever nothing was genuinely recorded or the record cannot be trusted:
// a row with no snapshot at all (a legacy row, or one whose marshal failed and was logged),
// a snapshot that is not a JSON object, an object with no `"channel"` key, a `"channel"` that
// is not a JSON string (an explicit null, a number, an object), or a Channel value outside the
// closed set this service creates. That last case is not a divergence to report — it is a row
// this code cannot interpret, and claiming `diverged` from an uninterpretable recorded value
// would be a fabricated finding rather than an observed one.
func googleAdsRecordedChannelType(ctx context.Context, campaign *model.Campaign) *string {
	if len(campaign.ConfigSnapshot) == 0 {
		return nil
	}
	// Decoded into RawMessage first, so presence and JSON type survive the decode. Decoding
	// straight into googleAdsConfig collapses "absent", "null" and "wrong type" onto the same
	// zero value, which is precisely the distinction this function needs to keep.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(campaign.ConfigSnapshot, &raw); err != nil {
		// Not an error for the caller: the readback still reports every other field. The row
		// simply cannot say what was asked for, which is exactly what `unknown` means.
		slog.WarnContext(ctx, "google ads config snapshot could not be decoded; advertising_channel_type has no recorded side",
			"campaign_id", campaign.ID, "error", err)
		return nil
	}
	rawChannel, ok := raw["channel"]
	if !ok {
		// Not a snapshot this adapter wrote — applyCampaignConfig always emits the key. The
		// legacy SEARCH default was justified for rows this service wrote and must not be
		// extended to a config blob some caller supplied through UpdateCampaign.
		slog.WarnContext(ctx, "google ads config snapshot carries no channel key; advertising_channel_type has no recorded side",
			"campaign_id", campaign.ID)
		return nil
	}
	// An explicit JSON null is checked SEPARATELY, before the decode. json.Unmarshal of
	// `null` into a string SUCCEEDS in Go, leaving the zero value untouched — so a decode
	// error alone does not catch it, and `{"channel": null}` would take the SEARCH default
	// by the very route this function exists to close.
	var channel string
	if strings.TrimSpace(string(rawChannel)) == "null" {
		slog.WarnContext(ctx, "google ads config snapshot channel is explicitly null; advertising_channel_type has no recorded side",
			"campaign_id", campaign.ID)
		return nil
	}
	if err := json.Unmarshal(rawChannel, &channel); err != nil {
		// Present but not a JSON string: an explicit null, a number, an object. The key being
		// there does not make its value a recorded channel, and coercing it to "" would
		// resurrect the same fabricated SEARCH by a different route.
		slog.WarnContext(ctx, "google ads config snapshot channel is not a string; advertising_channel_type has no recorded side",
			"campaign_id", campaign.ID)
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "", googleAdsChannelSearch:
		return strPtr(googleAdsChannelTypeSearch)
	case googleAdsChannelDemandGen:
		return strPtr(googleAdsChannelTypeDemandGen)
	case googleAdsChannelPerformanceMax:
		return strPtr(googleAdsChannelTypePerformanceMax)
	case googleAdsChannelVideo:
		return strPtr(googleAdsChannelTypeVideo)
	case googleAdsChannelDisplay:
		return strPtr(googleAdsChannelTypeDisplay)
	default:
		return nil
	}
}

// googleAdsCreationCustomerID recovers the ad account the campaign was CREATED under from
// the persisted googleads.CampaignResult blob, mirroring googleAdsToggleTargets.
//
// Rows written before CampaignResult carried customerId have no such field, so it falls back
// to the ocid query parameter of the stored googleAdsUrl — the create path builds that URL as
// ".../aw/campaigns?ocid=" + customerID, making it a faithful record of the same value.
//
// An empty return means UNKNOWN — provenance was not recorded — and NOT "no mismatch". It is
// deliberately NOT this helper's job to decide what unknown provenance permits: that is a
// per-operation judgement about what an unverifiable account would cost, so each caller states
// its own rule and the two answers legitimately differ.
//
// The dividing line is whether the operation can survive being pointed at the wrong account.
// Callers that only ever COMPARE a recorded id treat absence as permission to proceed, because
// a legacy row cannot prove a mismatch and refusing every one of them would break operations
// that work today. Callers whose entire answer is meaningless without provenance instead fail
// closed, joining domain.ErrCampaignProvenanceUnknown so the remedy — re-dispatch the row to
// write its provenance — is distinguishable from a reconnect.
//
// Deliberately described as a shape rather than a caller list: this helper has several callers
// and gains more, so an enumeration here would be falsified by the next one added. Follow the
// call sites for the current split.
func googleAdsCreationCustomerID(campaign *model.Campaign) string {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var blob struct {
		CustomerID   string `json:"customerId"`
		GoogleAdsURL string `json:"googleAdsUrl"`
	}
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return ""
	}
	if blob.CustomerID != "" {
		return blob.CustomerID
	}
	u, err := url.Parse(blob.GoogleAdsURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("ocid")
}

// googleAdsCampaignAdGroupIDs is the set of ad group ids this campaign records creating —
// what bounds a keyword action to the campaign the caller addressed.
//
// Deliberately NOT googleAdsToggleTargets' list. That one answers "which groups can take a
// status toggle", so it drops a group whose ads never got created; this one answers "which
// groups are this campaign's", and a group that exists with keyword criteria but no ad is
// still this campaign's — its keywords are real, serving nothing, and pausing or removing
// them is a legitimate thing to ask for. Narrowing the membership test to the toggleable
// groups would refuse that, which is over-refusal, not safety.
//
// Falls back to the scalar AdGroupID for rows written before AdGroups existed.
func googleAdsCampaignAdGroupIDs(campaign *model.Campaign) map[string]bool {
	if campaign == nil || len(campaign.Result) == 0 {
		return nil
	}
	var result googleads.CampaignResult
	if err := json.Unmarshal(campaign.Result, &result); err != nil {
		return nil
	}
	ids := make(map[string]bool, len(result.AdGroups)+1)
	// The scalar is a copy of the first entry, so it is added alongside rather than instead:
	// a map makes the overlap free, and including it keeps a legacy row — which has the
	// scalar and no AdGroups — working through the same code path.
	if id := strings.TrimSpace(result.AdGroupID); id != "" {
		ids[id] = true
	}
	for _, g := range result.AdGroups {
		if id := strings.TrimSpace(g.ID); id != "" {
			ids[id] = true
		}
	}
	return ids
}

// googleAdsActivationGate refuses an ACTIVATE that would report a launch the campaign
// cannot deliver. What counts as "fully provisioned" is CHANNEL-SPECIFIC, because the four
// channels this dispatcher creates do not have the same serving resources:
//
//   - Search serves through an ad group + ad, and cannot deliver without at least one
//     keyword criterion. Both are required, as they always have been.
//   - Demand Gen serves through an ad group + ad, and its targeting is audience- and
//     creative-driven. Keywords are REFUSED on this channel (campaign.go's Search-only
//     fence), so demanding a keyword criterion here would be unsatisfiable by construction —
//     the gate would instruct the operator to supply the very field the create path rejects.
//   - Video and Display each serve through an ad group + ad like Demand Gen, and keywords
//     are refused on both by the same Search-only fence, so they take exactly Demand Gen's
//     gate — which is why all three share an arm rather than each getting an identical one.
//   - Performance Max has no ad groups and no ads at all. Its creative is an ASSET GROUP,
//     and that is the resource whose absence means nothing can serve.
//
// Keyed on the campaign's Variant, which is part of its identity rather than its config, so
// a row cannot drift into the wrong arm through a config edit. A row written before variants
// existed normalises to VariantDefault and keeps the Search rules it was created under.
//
// Every arm names ADOPTION in its refusal, on every channel. campaignFromGoogleAdsAdoption
// writes provenance and nothing else — no ad group id, no ad id, no asset group id — so an
// adopted row fails whichever gate its variant selects, permanently, no matter how completely
// the campaign is provisioned upstream. Without that clause the message sends an operator
// hunting a provisioning failure in this service's records for a campaign this service never
// created. The refusal itself is right: a cascade can only act on resources the blob recorded,
// and for an adopted row that set is empty, so un-pausing happens in Google Ads.
func googleAdsActivationGate(campaign *model.Campaign, targets []googleads.AdGroupStatusTarget, keywordsProvisioned bool, assetGroupID string) error {
	switch model.NormalizeVariant(campaign.Variant) {
	case googleAdsChannelPerformanceMax:
		// Named for what Performance Max actually has. The ad-group wording below would send
		// an operator looking for a provisioning failure in a resource this channel never
		// creates.
		if assetGroupID == "" {
			return fmt.Errorf("%w: google ads campaign %s cannot be activated because this service has no record of its Performance Max asset group (a Performance Max campaign has no ad groups; the asset group is what serves) — either the creative never completed, or the campaign was ADOPTED, which records no serving resources at all and leaves un-pausing to the Google Ads UI", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
		}
		return nil
	case googleAdsChannelDemandGen, googleAdsChannelVideo, googleAdsChannelDisplay:
		if len(targets) == 0 {
			return fmt.Errorf("%w: google ads campaign %s cannot be activated because this service has no record of its ad group/ad — either they were never fully provisioned, or the campaign was ADOPTED, which records no serving resources at all and leaves un-pausing to the Google Ads UI", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
		}
		// Deliberately no keyword gate on either channel — see the doc comment above.
		return nil
	default:
		// Refuse ACTIVATE if no ad group was fully provisioned: a duplicate-name orphan or
		// unconfirmed create (see createAdGroupAndAd) leaves no id to cascade to, so
		// enabling just the campaign would report success while nothing can serve.
		if len(targets) == 0 {
			return fmt.Errorf("%w: google ads campaign %s cannot be activated because this service has no record of its ad group/ad — either they were never fully provisioned, or the campaign was ADOPTED, which records no serving resources at all and leaves un-pausing to the Google Ads UI", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
		}
		if !keywordsProvisioned {
			return fmt.Errorf("%w: google ads campaign %s cannot be activated because keyword targeting is not yet provisioned (at least one keyword criterion is required)", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
		}
		return nil
	}
}

// googleAdsToggleAssetGroup is the persisted Performance Max asset group id, or "" on every
// other channel and on a Performance Max campaign whose creative never completed. Read from
// the same Result blob googleAdsToggleTargets reads, and for the same reason: the blob
// records what was actually CREATED, which is what a cascade can act on.
func googleAdsToggleAssetGroup(campaign *model.Campaign) string {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var result googleads.CampaignResult
	if err := json.Unmarshal(campaign.Result, &result); err != nil {
		return ""
	}
	return strings.TrimSpace(result.AssetGroupID)
}

// googleAdsHasToggleChildren reports whether this campaign has any serving resource beneath
// the campaign for a status flip to cascade to.
func googleAdsHasToggleChildren(targets []googleads.AdGroupStatusTarget, assetGroupID string) bool {
	return assetGroupID != "" || len(targets) > 0
}

// googleAdsToggleChildren flips whichever serving resources this campaign actually has —
// ALL of them, not the first kind it finds.
//
// The asset group goes first: on ACTIVATE every child must be enabled before the campaign
// is, and the Performance Max asset group is the one that serves. Today a campaign carries
// an asset group or ad groups and never both, because a Performance Max campaign has no ad
// groups and every other channel has no asset group. That is an invariant of what the create
// paths WRITE, though, and this function reads two values parsed independently out of an
// operator-visible Result blob — a hand-repaired row, a future channel with both, or a change
// to what adoption records could present both at once. Returning on the asset group alone
// would then enable it, silently skip every ad group and ad, and still flip the campaign to
// ENABLED: the "reports running and mostly is not" failure googleAdsToggleTargets' own cascade
// exists to prevent. So both are acted on, and the contradiction is logged rather than
// absorbed. Keying on what the blob RECORDED rather than on the variant keeps this consistent
// with googleAdsToggleTargets, which has always derived the cascade from what was created
// rather than from what was asked for.
func googleAdsToggleChildren(ctx context.Context, client *googleads.Client, targets []googleads.AdGroupStatusTarget, assetGroupID, gaStatus string) error {
	if assetGroupID != "" {
		if err := client.UpdateAssetGroupStatus(ctx, assetGroupID, gaStatus); err != nil {
			return err
		}
		if len(targets) > 0 {
			slog.WarnContext(ctx, "google ads campaign records both an asset group and ad groups; cascading to both",
				"asset_group_id", assetGroupID, "ad_group_count", len(targets))
		}
	}
	if len(targets) == 0 {
		return nil
	}
	return client.UpdateAdGroupsAndAdsStatus(ctx, targets, gaStatus)
}

// googleAdsToggleTargets recovers EVERY ad group of a campaign, with its own ads, from
// the persisted CampaignResult blob — the set a status toggle has to cascade over.
//
// The scalar AdGroupID/AdID pair the blob also carries is a copy of the FIRST group's, and
// was the whole story while a campaign could only have one. Since
// multiple ad groups and multiple responsive search ads landed, cascading over that pair
// alone enables group 1's first ad and leaves every other group and ad PAUSED while the
// campaign reports ENABLED — a campaign that says it is running and mostly is not.
//
// The three returns:
//
//   - targets: the groups that can actually be toggled, in the order they were created.
//   - keywordsProvisioned: whether ANY group has a keyword criterion. Campaign-wide on
//     purpose. The activation gate asks whether the campaign can deliver, and a campaign
//     delivers if one of its groups has keywords; asking only about the first group
//     refuses a campaign that would have served, which is the over-refusal this gate
//     must not commit.
//   - incomplete: groups recorded in the blob that cannot be toggled because their create
//     did not finish — present with an empty id, or with no ad. The create path appends an
//     entry BEFORE the group's mutate precisely so a failed group leaves a trace, and the
//     caller reports these rather than silently acting on a subset.
//
// Falls back to the scalar pair when AdGroups is absent: rows written before that field
// existed are single-group by construction, so the pair IS the whole campaign there.
func googleAdsToggleTargets(campaign *model.Campaign) (targets []googleads.AdGroupStatusTarget, keywordsProvisioned bool, incomplete []string) {
	if campaign == nil || len(campaign.Result) == 0 {
		return nil, false, nil
	}
	var result googleads.CampaignResult
	if err := json.Unmarshal(campaign.Result, &result); err != nil {
		return nil, false, nil
	}
	if len(result.AdGroups) == 0 {
		adGroupID, adID := strings.TrimSpace(result.AdGroupID), strings.TrimSpace(result.AdID)
		if adGroupID == "" || adID == "" {
			return nil, len(result.KeywordCriteriaIDs) > 0, nil
		}
		return []googleads.AdGroupStatusTarget{{AdGroupID: adGroupID, AdIDs: []string{adID}}},
			len(result.KeywordCriteriaIDs) > 0, nil
	}
	for i, g := range result.AdGroups {
		if len(g.KeywordCriteriaIDs) > 0 {
			keywordsProvisioned = true
		}
		adGroupID := strings.TrimSpace(g.ID)
		adIDs := make([]string, 0, len(g.AdIDs))
		for _, adID := range g.AdIDs {
			if adID = strings.TrimSpace(adID); adID != "" {
				adIDs = append(adIDs, adID)
			}
		}
		if adGroupID == "" || len(adIDs) == 0 {
			// Named by what the operator will recognise in the Google Ads UI, falling back
			// to the position when the name is empty too.
			name := strings.TrimSpace(g.Name)
			if name == "" {
				name = fmt.Sprintf("#%d", i+1)
			}
			incomplete = append(incomplete, name)
			continue
		}
		targets = append(targets, googleads.AdGroupStatusTarget{AdGroupID: adGroupID, AdIDs: adIDs})
	}
	// The scalar pair is a copy of the first entry, so it adds nothing when AdGroups is
	// populated — EXCEPT when that first entry is one of the incomplete ones, which would
	// mean the two disagree. They cannot: the scalars are written from the same values in
	// the same step. Nothing to merge, and merging would only risk reintroducing the
	// first group twice.
	return targets, keywordsProvisioned, incomplete
}

// ---------------------------------------------------------------------------
// Settings readback (LFXV2-3067)
// ---------------------------------------------------------------------------

// Field names used in the settings readback, in THIS SERVICE's own stable vocabulary
// rather than Google's. Naming the row's side in Google's spelling would make the report
// harder to read, not easier, since the whole report is "what this row records" against
// "what the platform holds".
//
// This vocabulary is NOT a column list, and must not be read as one — the Goa contract
// (design/brief.go, campaign-settings-field) warns consumers of the same thing. Six of the
// ten names do happen to coincide with a campaigns column (budget_amount, budget_type,
// campaign_name, status, start_date, end_date), but advertising_channel_type has no column
// and is recovered from ConfigSnapshot, and budget_delivery_method,
// budget_explicitly_shared and bidding_strategy_type have no column (bidding_strategy_type
// is now expressed in ConfigSnapshot but is still reported with an absent recorded side —
// see settingsFieldBiddingStrategy below for why). Treating the set as column names would
// send a reader looking for persistence fields that do not exist.
// googleAdsDateTimeLayout is the shape Google returns campaign.start_date_time /
// end_date_time in: 'yyyy-MM-dd HH:mm:ss', in the ad account's timezone. Parsing against it
// in full is what stops a malformed value being silently truncated into a plausible date.
const googleAdsDateTimeLayout = "2006-01-02 15:04:05"

const (
	settingsFieldBudgetAmount = "budget_amount"
	settingsFieldBudgetType   = "budget_type"
	settingsFieldName         = "campaign_name"
	settingsFieldStatus       = "status"
	settingsFieldStartDate    = "start_date"
	settingsFieldEndDate      = "end_date"
	// Budget delivery method, explicit budget sharing and bidding strategy are OBSERVED but
	// never compared, so each is reported with an absent recorded side and therefore an
	// `unknown` verdict.
	//
	// For the two budget fields the reason is simply that nothing in a Google Ads dispatch
	// config expresses them. Bidding strategy is the one that now could be compared and
	// deliberately is not: googleAdsConfig.BiddingStrategy records the caller's choice, but
	// the comparison would have to map this client's UI vocabulary onto Google's OUTPUT_ONLY
	// BiddingStrategyTypeEnum, and that mapping is not live-verified at exactly the point it
	// is ambiguous — `target-cpa` and `maximize-conversions` are both sent as
	// maximize_conversions (with and without a target), and which enum value Google reports
	// back for the targeted one has not been observed. A mapping guessed wrong reports a
	// false divergence, which sends an operator to investigate a campaign that is set
	// exactly as asked. Reporting `unknown` under-informs; a false divergence misinforms,
	// and only the second is a cost the operator pays. Comparing this field is a follow-up
	// gated on a live readback of a targeted campaign, not on more reading of the proto.
	//
	// Adoption is a second reason to be careful here: on the adopt path the row records a
	// BiddingStrategy that was never pushed upstream, so the two sides are EXPECTED to
	// disagree and a comparison would flag every adopted campaign.
	//
	// Status also
	// has an absent recorded side, but for an unrelated reason — the row records one, it is
	// just a different axis from Google's delivery status — so do not fold it into this
	// group's rationale. They are
	// reported anyway because the operator's question is "what is this campaign actually set
	// to upstream", and a budget that reads as expected while being ACCELERATED or shared
	// across campaigns is exactly the state that explains a spend anomaly the compared fields
	// cannot.
	//
	// Channel type is NOT one of them and is listed here only because it is grouped with them
	// in the readback: it IS recorded, recovered from ConfigSnapshot and compared, so it can
	// return a real match or divergence. Which of these fields has a recorded counterpart is
	// decided at the call site that builds rb.Fields, where each pair is written out — read
	// the nil recorded sides there rather than trusting any list kept here, since a config
	// that later learns to express one of these would falsify a list but not the call site.
	settingsFieldBudgetDelivery  = "budget_delivery_method"
	settingsFieldBudgetShared    = "budget_explicitly_shared"
	settingsFieldChannelType     = "advertising_channel_type"
	settingsFieldBiddingStrategy = "bidding_strategy_type"
)

// googleAdsBudgetTypeFromPeriod maps Google's campaign_budget.period into this service's
// model.BudgetType vocabulary, which is the only way the two sides of the budget-type
// comparison can be compared at all.
//
// The mapping is stated once, here, because the vocabularies are NOT the same and the
// difference is easy to get wrong: Google's v23 BudgetPeriodEnum has DAILY and
// CUSTOM_PERIOD (plus UNKNOWN/UNSPECIFIED). It has NO `LIFETIME` value, even though
// model.BudgetLifetime spells that idea "lifetime" — CUSTOM_PERIOD is the thing that
// corresponds to it.
//
// Anything outside the two real values returns "", which the caller turns into an ABSENT
// upstream side and therefore an `unknown` verdict. That is deliberate and is the
// fail-closed choice: UNKNOWN literally means "a value this API version cannot name", and
// mapping it to either budget type would manufacture a match or a divergence out of a
// value Google explicitly declined to state.
//
// The match is on the EXACT enum spelling, with no trimming, and that is the half that is
// easy to get wrong. blankToNil in the client deliberately stops normalising these strings
// so a malformed field reaches its consumer malformed; trimming here would undo precisely
// that, turning a " DAILY " Google never sent into a well-formed DAILY and reporting
// `match` against a recorded daily budget. A padded value is not a value this function can
// name, so it takes the same fail-closed path as UNKNOWN: an absent upstream side and an
// `unknown` verdict. googleAdsDateOnly does exactly the same for dates: a value that does
// not parse against the documented layout yields an absent upstream side rather than a raw
// string that could compare equal to the recorded date.
func googleAdsBudgetTypeFromPeriod(period string) model.BudgetType {
	switch period {
	case "DAILY":
		return model.BudgetDaily
	case "CUSTOM_PERIOD":
		return model.BudgetLifetime
	default:
		return ""
	}
}

// googleAdsUpstreamBudgetAmount renders the upstream budget as a whole-currency-unit
// string comparable with the row's budget_amount, and reports whether it could be read.
//
// Google reports the budget in MICROS, in one of two mutually exclusive fields:
// amount_micros for a DAILY budget, total_amount_micros for a CUSTOM_PERIOD one. Reading
// only the first would report a lifetime-budget campaign as having no budget at all —
// an absence that is not true, and one that would quietly suppress a real divergence.
//
// The two sides are compared as whole units rather than micros because that is what the
// row stores; the conversion happens here, once, rather than in the comparison.
//
// The PERIOD selects the field, and an amount whose period does not name one of the two real
// values is not read at all. The two upstream fields are different quantities — a daily RATE
// and a whole-flight CAP — so "whichever one is present" is not a reading of the budget, it is
// a guess at which question the number answers. A campaign recorded as a daily 500 against an
// upstream row carrying only total_amount_micros=500000000 and no period reported `match`,
// though a 500/day rate and a 500 lifetime cap are different budgets and the equal digits were
// a coincidence of the units.
//
// This is where that pair must be caught, and the client says so. Its contradiction check in
// campaign_settings.go refuses an amount that disagrees with a NAMED period and deliberately
// passes an absent or UNKNOWN one — absence means "Google did not report this field" across
// all of CampaignSettings, and cannot start signalling "inconsistent pair" without breaking
// that meaning everywhere else. It permits the partial pair precisely because an unread period
// is supposed to yield `unknown` here rather than a fabricated verdict.
//
// The gate is the SAME mapping the budget-type field uses, googleAdsBudgetTypeFromPeriod, so
// one period cannot name a type for one field and fail to select an amount for the other. It
// already fails closed on UNKNOWN/UNSPECIFIED and on a padded " DAILY " that blankToNil left
// malformed on purpose.
//
// A budget carrying SUB-CENT micros is rendered at full precision instead of at the row's two
// decimal places, and that exception is the whole point of this function's care. The row's
// column is NUMERIC(14,2), so rounding an upstream 10.004 to "10.00" would make it compare
// EQUAL to a recorded 10.00 and report `match` for two budgets that genuinely differ — a
// fabricated agreement manufactured by the formatter rather than by a nil, which is precisely
// the defect this readback exists to make impossible. Sub-cent values cannot come from this
// service's own create path (it rounds to micros from a 2dp amount), but they can and do come
// from campaigns ADOPTED from outside it — exactly the population a readback is for.
func googleAdsUpstreamBudgetAmount(s *googleads.CampaignSettings) *string {
	if s == nil {
		return nil
	}
	if s.BudgetPeriod == nil {
		return nil
	}
	var micros *int64
	switch googleAdsBudgetTypeFromPeriod(*s.BudgetPeriod) {
	case model.BudgetDaily:
		micros = s.BudgetAmountMicros
	case model.BudgetLifetime:
		micros = s.BudgetTotalAmountMicros
	default:
		// A period this API version cannot name establishes nothing about which quantity an
		// amount would be, so no amount is read. Same fail-closed choice, and for the same
		// reason, as googleAdsBudgetTypeFromPeriod's own default arm.
		return nil
	}
	if micros == nil {
		return nil
	}
	// Exact integer arithmetic, never float division: at 2^53 micros float64 stops being able
	// to represent every value, and a budget comparison must not depend on that boundary.
	if *micros%microsPerCent != 0 {
		return strPtr(formatSubCentBudgetUnits(*micros))
	}
	return strPtr(formatBudgetUnits(float64(*micros) / microsPerBudgetUnit))
}

// formatSubCentBudgetUnits renders micros that do not divide evenly into cents, keeping every
// digit the platform reported. The trailing zeroes are trimmed so the value reads as a number
// rather than as padding, but nothing significant is dropped: this string exists to be shown
// beside the recorded amount and to compare UNEQUAL to it.
func formatSubCentBudgetUnits(micros int64) string {
	neg := micros < 0
	if neg {
		micros = -micros
	}
	whole := micros / int64(microsPerBudgetUnit)
	frac := micros % int64(microsPerBudgetUnit)
	out := strconv.FormatInt(whole, 10) + "." + fmt.Sprintf("%06d", frac)
	out = strings.TrimRight(out, "0")
	if neg {
		out = "-" + out
	}
	return out
}

// Budget rendering constants for the settings readback.
const (
	// microsPerBudgetUnit converts Google's micros to whole currency units. Deliberately a
	// third spelling of the same idea: googleads.microsPerUnit and service.microsToUnits are
	// both package-private to packages this one cannot import, so a bare literal was the
	// only thing that compiled. Naming it here at least makes the three greppable together.
	microsPerBudgetUnit = 1_000_000.0
	// budgetDecimalPlaces matches the campaigns.budget_amount column, NUMERIC(14,2).
	budgetDecimalPlaces = 2
	// microsPerCent is the divisor that decides whether an upstream budget is expressible at
	// the row's two decimal places. A remainder means rendering it at 2dp would round it into
	// a value the platform never reported — and, worse, possibly into the recorded one.
	microsPerCent = 10_000
)

// formatBudgetUnits renders a budget amount the same way on both sides of the comparison.
//
// Both sides go through this function, which is what makes the comparison meaningful: a row
// holding 500 and a platform holding 500.00 are the same budget, and formatting them
// differently would report a divergence that does not exist.
func formatBudgetUnits(v float64) string {
	return strconv.FormatFloat(v, 'f', budgetDecimalPlaces, 64)
}

// googleAdsDateOnly reduces Google's 'yyyy-MM-dd HH:mm:ss' campaign date-time to the
// YYYY-MM-DD the campaign row stores, so the two sides are comparable at all.
//
// Only the date part is kept, deliberately: the row's start_date/end_date are DATEs with no
// time component, so comparing a full timestamp against them would report a divergence for
// every campaign that actually agrees. The time is dropped rather than reconciled because
// Google returns it in the ad ACCOUNT's timezone, which this client does not know — any
// instant it computed would be a guess.
//
// The time component is stripped ONLY when the whole value parses as Google's documented
// 'yyyy-MM-dd HH:mm:ss'. Splitting on the first space unconditionally would turn
// "2026-08-01 garbage" into "2026-08-01" and let it compare EQUAL to a recorded 2026-08-01 —
// a fabricated agreement manufactured out of a malformed response, which is the one outcome
// this readback exists to make impossible.
//
// A value that does NOT parse yields an ABSENT upstream side, exactly as
// googleAdsBudgetTypeFromPeriod does for a period it cannot name. Returning the raw string
// instead — which this helper used to do — was the same defect wearing the opposite
// disguise. The reasoning behind that passthrough was that an unparseable value "can only
// read as a divergence, never as a match", and for "2026-08-01 garbage" that is true. But
// it is FALSE for the one malformed shape Google is most likely to send: a date-only
// "2026-08-01" with the required time component missing fails this strict parse and was
// then returned VERBATIM — byte-equal to the recorded side, which this readback formats to
// exactly that YYYY-MM-DD layout. The comparison then reported `match` for a value this
// code could not validate at all. A passthrough is only safe where the two sides cannot
// share a spelling, and here they share it precisely.
//
// So an unparseable value is withheld rather than shown: `unknown` says "this was not
// comparable", which is the honest reading, whereas `match` would be a claim of agreement
// derived from a value that never parsed. The strict layout is what makes the distinction
// meaningful, and dropping the raw string is the cost of not being able to fabricate one.
func googleAdsDateOnly(s *string) *string {
	if s == nil {
		return nil
	}
	t, err := time.Parse(googleAdsDateTimeLayout, *s)
	if err != nil {
		return nil
	}
	return strPtr(t.Format(campaignDateLayout))
}

// boolToStrPtr renders an optional bool for the readback, preserving absence: a nil stays
// nil rather than becoming "false", which would be a claim the platform never made.
func boolToStrPtr(b *bool) *string {
	if b == nil {
		return nil
	}
	return strPtr(strconv.FormatBool(*b))
}

// strPtr returns a pointer to s. Used to build the readback's optional sides, where the
// difference between a nil and a pointer-to-empty is the difference between "not read"
// and "read as empty".
func strPtr(s string) *string { return &s }

// ReadSettings implements service.SettingsReader for Google Ads: it reads the campaign's
// live configuration and compares it against what the campaign row recorded.
//
// STRICTLY READ-ONLY. The only upstream call is googleads.GetCampaignSettings, a GAQL
// search. Nothing here mutates the platform, and nothing here writes to the campaign row:
// the readback is returned to the caller and discarded. The row keeps meaning "what this
// dispatch asked for", which is what makes the divergence legible at all — an
// observation written back would erase the very thing being compared against.
//
// It resolves the same connection ReadMetrics does and enforces the account-identity
// invariant, for the same reason: the stored PlatformCampaignID is unique only within the
// customer it was created under, so querying it under a re-pointed connection can return
// ANOTHER account's campaign. Here that would mean reporting a divergence between this
// campaign's recorded budget and a different campaign's actual one.
//
// It enforces that invariant more strictly than ReadMetrics and ToggleStatus do: absent
// provenance FAILS CLOSED here rather than being waved through, and is refused BEFORE the
// connection is resolved so a broken connection cannot mask it. See the guards below.
func (d *GoogleAdsDispatcher) ReadSettings(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign) (*model.CampaignSettingsReadback, error) {
	// Provenance is checked in TWO arms, and both are load-bearing. They are SPLIT around the
	// client resolution because they rest on different kinds of fact, and only one of them
	// needs a client at all.
	//
	// ABSENCE first, and BEFORE the resolve. A row that records no creating customer is not a
	// row that MATCHES the current connection — it is a row nothing has established anything
	// about, and treating the empty string as "matches" is the same absent-value-read-as-
	// agreement defect this readback exists to prevent everywhere else in its comparisons. The
	// stored PlatformCampaignID is unique only WITHIN a customer, so querying it under an
	// unverified account can, on an id collision, return ANOTHER account's campaign — and this
	// endpoint would then report a divergence between this campaign's recorded budget and a
	// different campaign's actual one. That the endpoint never writes back bounds the blast
	// radius to a misleading report, but a confidently wrong report about somebody else's
	// account is the precise outcome a readback must not produce.
	//
	// Asked before the client is consulted because absent provenance is a purely LOCAL fact
	// about the row, and no answer Google could give would change it. Ordering is the whole
	// point: resolveGoogleAdsClient immediately calls resolveExisting, which resolves and
	// DECRYPTS the project (or system) connection, so an unstamped row read while that
	// connection is disconnected, undecodable or simply down surfaced the transient upstream
	// failure — a 404/500/different 409 — instead of this deterministic one. That inversion
	// hides the only remedy that works: there is nothing to retry, the row must be
	// re-dispatched to write its provenance. This is the identical correction the HubSpot
	// email metrics guard already carries, where the same guard was moved above the portal
	// lookup for the same reason; see the comment on that guard.
	//
	// ErrCampaignProvenanceUnknown is JOINED with ErrCampaignAccountMismatch, never returned
	// alone, so existing errors.Is(err, ErrCampaignAccountMismatch) callers keep matching
	// while the handler's dedicated 409 arm — which until now no supported provider could
	// reach on this endpoint — can tell the two apart. The remedy differs: there is no
	// original account to reconnect to, so the row must be re-dispatched.
	created := googleAdsCreationCustomerID(campaign)
	if created == "" {
		return nil, fmt.Errorf("read google ads campaign settings: campaign %s does not record which customer it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}

	client, err := d.resolveGoogleAdsClient(ctx, projectID, platform, campaign)
	if err != nil {
		return nil, err
	}
	// MISMATCH second, and necessarily AFTER the resolve: unlike absence, this arm is a
	// comparison against the account the connection currently resolves to, so it cannot be
	// answered without a client. A RECORDED customer that disagrees with the project's current
	// connection is a different failure with a different remedy.
	if created != client.CustomerID() {
		return nil, fmt.Errorf("read google ads campaign settings: campaign %s was created under customer %s but the project's current connection resolves to customer %s: %w",
			campaign.PlatformCampaignID, created, client.CustomerID(), domain.ErrCampaignAccountMismatch)
	}

	settings, err := client.GetCampaignSettings(ctx, campaign.PlatformCampaignID)
	if err != nil {
		return nil, fmt.Errorf("read google ads campaign settings: %w", err)
	}
	if settings == nil {
		// The platform answered and holds no such campaign. Reported as an absent PLATFORM
		// CAMPAIGN rather than as an empty readback: every field would otherwise come back
		// `unknown`, which says "we could not read these" when the truth is far more
		// specific and far more urgent — the campaign this row tracks is not there.
		return nil, fmt.Errorf("%w: google-ads campaign %s", domain.ErrPlatformCampaignAbsent, campaign.PlatformCampaignID)
	}

	rb := &model.CampaignSettingsReadback{
		CampaignID: campaign.ID,
		// The id the PLATFORM echoed, not the one requested. GetCampaignSettings refuses a
		// response whose id filter was not honoured, so these agree on every response that
		// gets this far; taking it from the response is what keeps that true by construction.
		PlatformCampaignID: settings.CampaignID,
		Platform:           platform,
		ReadAt:             time.Now().UTC(),
	}

	// Budget amount. The recorded side is nil when the row holds no budget — which is an
	// ordinary state, not a defect: applyCampaignConfig deliberately leaves budget_amount
	// NULL for a zero budget and for one too large for the column.
	var recordedBudget *string
	if campaign.BudgetAmount != nil {
		recordedBudget = strPtr(formatBudgetUnits(*campaign.BudgetAmount))
	}
	// Budget type. Google's period is translated into this service's vocabulary rather than
	// the row's being translated into Google's, so the report speaks one language throughout.
	var recordedBudgetType *string
	if campaign.BudgetType != nil {
		recordedBudgetType = strPtr(string(*campaign.BudgetType))
	}
	var upstreamBudgetType *string
	if settings.BudgetPeriod != nil {
		if bt := googleAdsBudgetTypeFromPeriod(*settings.BudgetPeriod); bt != "" {
			upstreamBudgetType = strPtr(string(bt))
		}
	}
	// Campaign name. Recorded as a plain column, so an empty one is treated as unrecorded
	// rather than compared as "" — comparing an empty string against a real name would
	// report a divergence for a row that simply never captured the name.
	//
	// TrimSpace DETECTS the all-blank legacy value; it does not produce the compared one.
	// Trimming into `recordedName` fabricated agreement: a row holding `" name "` compared
	// equal to an upstream `"name"` and was reported as a MATCH, when the two sides plainly
	// differ and Google is showing the operator a name this service did not record. That
	// padded row is reachable — UpdateCampaign assigns `p.Campaign.CampaignName` verbatim
	// (internal/service/brief.go) with no whitespace validation in the service layer, the
	// design, or the column.
	//
	// This is the same defect fixed on the budget period a moment ago, in a sibling field:
	// a normalisation applied to only ONE side of a comparison reports agreement the data
	// does not support. The recorded value is therefore carried VERBATIM, and any
	// difference — including one that is only whitespace — is a divergence, which is the
	// honest answer for a readback whose whole job is to say whether the two sides agree.
	var recordedName *string
	if strings.TrimSpace(campaign.CampaignName) != "" {
		recordedName = strPtr(campaign.CampaignName)
	}

	// Flight dates. The RECORDED side is now populated whenever the caller supplied one:
	// googleAdsConfig carries startDate/endDate and applyCampaignConfig writes them to the
	// columns on both the create and the adoption path. It is still nil for the two cases
	// that leave the columns NULL — a campaign created before those config fields existed,
	// and a caller that omits them — and those report `unknown` rather than a divergence,
	// which is correct: no date was ever requested, so there is nothing for upstream to
	// diverge FROM.
	//
	// Adoption is NOT automatically in that set — it persists whatever window the adopting
	// request supplied, and the campaign it binds may well be one a prior partial attempt of
	// ours created. What is true of adoption is narrower: the recorded side reflects the
	// ADOPTING request, not whatever created the campaign upstream, so the two can disagree
	// for a reason this comparison is right to surface. Both sides are formatted to the row's
	// YYYY-MM-DD, never compared as raw strings: Google returns 'yyyy-MM-dd HH:mm:ss' in
	// the ad account's timezone, so a raw comparison would report a divergence for every
	// campaign that agrees.
	var recordedStart, recordedEnd *string
	if campaign.StartDate != nil {
		recordedStart = strPtr(campaign.StartDate.UTC().Format(campaignDateLayout))
	}
	if campaign.EndDate != nil {
		recordedEnd = strPtr(campaign.EndDate.UTC().Format(campaignDateLayout))
	}

	// Advertising channel type. This one IS recorded, unlike the three upstream-only
	// observations below that have no recorded side at all (status is also reported with a
	// nil recorded side, but for the different-axis reason given at its entry, not for want
	// of anything to record): googleAdsConfig.Channel is marshalled whole into ConfigSnapshot by
	// applyCampaignConfig on BOTH the create and the adoption path. Passing nil here would
	// discard a request this service holds and report `unknown` forever, so a campaign
	// recorded as demand-gen could never be shown as diverging from an upstream SEARCH —
	// which is a real misconfiguration, and one of the few this endpoint exists to surface.
	//
	// The recorded side is translated INTO Google's vocabulary rather than the upstream side
	// into ours, because the upstream value is the one an operator will recognise from the
	// Google Ads UI, and because googleAdsVariantForChannelType already refuses any channel
	// this service cannot create — mapping the other way would have to invent a slot for
	// PERFORMANCE_MAX to compare against.
	recordedChannelType := googleAdsRecordedChannelType(ctx, campaign)

	rb.Fields = []model.CampaignSettingsField{
		model.CompareSettingsField(settingsFieldBudgetAmount, recordedBudget, googleAdsUpstreamBudgetAmount(settings)),
		model.CompareSettingsField(settingsFieldBudgetType, recordedBudgetType, upstreamBudgetType),
		model.CompareSettingsField(settingsFieldName, recordedName, settings.Name),
		// Status is deliberately NOT compared, and NOT because the row lacks a column for
		// it — campaigns.status exists. It carries this service's own lifecycle vocabulary,
		// mostly provisioning state (pending / created / created_degraded / deleted) and
		// only sometimes a run state, while Google's is the purely delivery-side
		// ENABLED/PAUSED/REMOVED — a different axis, exactly as model.PlatformCampaignRef
		// documents. Comparing them would report a permanent, meaningless divergence on
		// nearly every campaign ever created.
		// The upstream value is still reported, with no recorded counterpart, so an operator
		// can SEE that the campaign is paused upstream without this service pretending the
		// two are the same field.
		model.CompareSettingsField(settingsFieldStatus, nil, settings.Status),
		model.CompareSettingsField(settingsFieldStartDate, recordedStart, googleAdsDateOnly(settings.StartDateTime)),
		model.CompareSettingsField(settingsFieldEndDate, recordedEnd, googleAdsDateOnly(settings.EndDateTime)),
		// Upstream-only observations: nothing this service records expresses these three.
		// Note this is NOT the reason status has no recorded side — the row has a status
		// column; see the status entry above, where the reason is a different axis.
		// advertising_channel_type is deliberately NOT among them — its recorded side comes
		// from ConfigSnapshot rather than a column; see recordedChannelType above.
		model.CompareSettingsField(settingsFieldBudgetDelivery, nil, settings.BudgetDeliveryMethod),
		model.CompareSettingsField(settingsFieldBudgetShared, nil, boolToStrPtr(settings.BudgetExplicitlyShared)),
		model.CompareSettingsField(settingsFieldChannelType, recordedChannelType, settings.AdvertisingChannelType),
		model.CompareSettingsField(settingsFieldBiddingStrategy, nil, settings.BiddingStrategyType),
	}
	rb.SummariseSettings()
	return rb, nil
}

// googleAdsScopeForCustomer reduces the project's campaign scope to the ids that are
// meaningful under customerID, and is the insight reads' half of the account-identity
// invariant ReadMetrics and ApplyKeywordActions already enforce.
//
// The need is the same one ReadMetrics documents: a platform_campaign_id is a bare numeric
// unique only WITHIN its customer, and UpdateGoogleAds can re-point a project's connection
// between create and read. An id carried over from the old customer either matches nothing
// (an empty read that looks like a campaign with no activity) or, on a numeric collision,
// selects ANOTHER account's campaign — which on a customer shared across every foundation
// means reporting a different project's keyword text and spend as this project's own.
//
// Entries with NO recorded provenance are KEPT, deliberately. That matches
// googleAdsCreationCustomerID's documented contract ("an empty return means unknown, and the
// caller must treat that as permission to proceed") and the two sibling read paths: a row
// written before provenance tracking existed cannot prove a mismatch, and dropping every such
// row would silently empty the results of projects whose campaigns all predate it. The
// asymmetry against ApplyKeywordActions — which fails CLOSED on unknown provenance — is the
// one this branch already reasons about: a misleading read is recoverable, an irreversible
// REMOVE is not.
//
// A scope that is non-empty on the way in but empty after filtering is an ERROR, never an
// empty id list. Passing an empty list on would make campaignScopePredicate refuse anyway,
// but relying on that would put the account-wide read one dropped guard away; refusing here
// states the intent where the filtering happens.
//
// A scope that mismatches only PARTLY is likewise an ERROR rather than a reduced id list —
// see the reasoning at the check itself. The two cases answer the same way for the same
// reason: this function returns the ids for a read that promises to cover the project's
// campaigns, and it has no way to say "these, but not those".
func googleAdsScopeForCustomer(scope []model.ProjectCampaignScope, customerID, op string) ([]string, error) {
	ids := make([]string, 0, len(scope))
	skipped := 0
	for _, s := range scope {
		created := googleAdsCreationCustomerID(&model.Campaign{Result: s.Result})
		if created != "" && created != customerID {
			skipped++
			continue
		}
		ids = append(ids, s.PlatformCampaignID)
	}
	// ANY mismatch fails the whole read, not just a read left with nothing.
	//
	// Dropping only the mismatched entries looks like the graceful option and is the worse
	// one. A project with campaigns under both its old and its current customer would get the
	// current-account subset returned as though it were the project's whole picture: both
	// endpoints report success, and neither response has any field that could disclose an
	// omission — no omitted count, no partial-coverage flag, no per-campaign breakdown. The
	// caller cannot tell a project whose other campaigns were silently dropped from one that
	// genuinely has no other campaigns. For the audience read that is the more damaging of the
	// two, because a demographic distribution computed over half a project's campaigns looks
	// exactly like a complete one and is what a re-targeting decision is then made on.
	//
	// Between the two remedies the finding offers — fail closed, or extend the contract with a
	// partial-coverage signal — this takes fail-closed DELIBERATELY, and not merely because it
	// is the smaller change:
	//
	//   - It needs no new field, so design/, gen/ and the published OpenAPI are untouched and
	//     no consumer has to learn a new flag to keep reading these endpoints safely.
	//   - The state is already representable and already handled: ErrCampaignAccountMismatch
	//     maps to a 409 with an actionable remedy (reconnect the original account), which is
	//     the SAME remedy the all-mismatched case has always returned. Widening the existing
	//     arm keeps one answer for one cause rather than splitting it in two.
	//   - It is the conservative direction. A partial-coverage flag is only as good as the
	//     consumers that read it; every one that ignores it silently regains today's defect,
	//     whereas a refusal cannot be misread as complete data.
	//   - It matches how this file already resolves the same tension on the mutate side, where
	//     an unprovable tenant refuses rather than proceeds.
	//
	// A partial-coverage field remains the richer long-term answer and is a contract change
	// worth making deliberately; it is not landed here, where the choice is between silently
	// wrong data and an honest refusal.
	if skipped > 0 {
		return nil, fmt.Errorf("%s: %d of this project's %d campaigns were created under a different customer than the one its connection now resolves to (%s); returning only the rest would report a partial result as complete: %w",
			op, skipped, len(scope), customerID, domain.ErrCampaignAccountMismatch)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s: this project has no campaigns that can be read under the customer its connection resolves to (%s): %w",
			op, customerID, domain.ErrCampaignAccountMismatch)
	}
	return ids, nil
}

// ReadKeywordPerformance implements service.KeywordInsightsReader for Google Ads.
//
// Confined to campaignIDs, the upstream campaigns the calling project owns. An earlier version
// of this read was scoped only by the connection, on the reasoning that "the connection IS the
// scope, exactly as it is for ListAccounts". That reasoning does not hold on this platform:
// Google Ads is ONE customer shared across every foundation (docs/architecture.md, "Account
// Tenancy"), so a connection-scoped GAQL query returns every project's keyword text, campaign
// ids and spend. The campaign ids are read from this service's own rows, scoped by project_id
// in SQL, and are what make the read answer only for the caller.
//
// It still resolves the ordinary client, which accepts the LF system fallback, and that remains
// correct: the call names no upstream id the caller chose, and once the query is campaign-scoped
// a fallback project reads only its own campaigns rather than the whole LF account.
func (d *GoogleAdsDispatcher) ReadKeywordPerformance(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.KeywordPerformance, error) {
	// Translate and validate the window BEFORE resolving credentials: an unsupported window
	// is a permanent input fault regardless of connection state, so it must produce the same
	// 400 either way rather than being masked by a contingent connection failure.
	gaWindow, err := googleads.WindowFor(window)
	if err != nil {
		return nil, fmt.Errorf("read google ads keyword performance: %w", errors.Join(domain.ErrMetricsWindowUnsupported, err))
	}
	// nil campaign: this read is scoped by a SET of campaigns, not one, so there is no single
	// recorded creation account to resolve against. An empty recorded id is the documented
	// "unknown, proceed" case (see credsSource.resolveExisting), which resolves the ordinary
	// project-then-system account — the behaviour the comment above already describes. The
	// per-campaign account identity is then enforced downstream by googleAdsScopeForCustomer,
	// which REFUSES THE WHOLE READ when any entry's recorded customer is not the resolved one.
	client, err := d.resolveGoogleAdsClient(ctx, projectID, platform, nil)
	if err != nil {
		return nil, err
	}
	campaignIDs, err := googleAdsScopeForCustomer(scope, client.CustomerID(), "read google ads keyword performance")
	if err != nil {
		return nil, err
	}
	kp, err := client.GetKeywordPerformance(ctx, gaWindow, campaignIDs)
	if err != nil {
		return nil, err
	}
	rows := make([]model.KeywordRow, 0, len(kp.Rows))
	for _, r := range kp.Rows {
		rows = append(rows, model.KeywordRow{
			CriterionID:  r.CriterionID,
			AdGroupID:    r.AdGroupID,
			CampaignID:   r.CampaignID,
			AdGroupName:  r.AdGroupName,
			CampaignName: r.CampaignName,
			Text:         r.Text,
			MatchType:    r.MatchType,
			Status:       r.Status,
			// Carried as a pointer the whole way rather than dereferenced with a default:
			// nil means Google has not rated the keyword, and every layer that flattens it
			// to 0 turns "unrated" into "rated worst".
			QualityScore: r.QualityScore,
			Impressions:  r.Impressions,
			Clicks:       r.Clicks,
			CostMicros:   r.CostMicros,
			Ctr:          r.Ctr,
			Conversions:  r.Conversions,
		})
	}
	return &model.KeywordPerformance{
		// The REQUEST window, not the client's echoed GAQL literal: the API contract is the
		// platform-agnostic vocabulary, and translating back would reintroduce the dialect.
		Window:    window,
		Rows:      rows,
		Truncated: kp.Truncated,
	}, nil
}

// ReadAudienceInsights implements service.KeywordInsightsReader for Google Ads. Same
// campaign-scoping and same window-first ordering as ReadKeywordPerformance — the demographic
// queries aggregate the whole customer without it, which on a shared account means every other
// project's targeting distribution.
func (d *GoogleAdsDispatcher) ReadAudienceInsights(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.AudienceInsights, error) {
	gaWindow, err := googleads.WindowFor(window)
	if err != nil {
		return nil, fmt.Errorf("read google ads audience insights: %w", errors.Join(domain.ErrMetricsWindowUnsupported, err))
	}
	// nil campaign: this read is scoped by a SET of campaigns, not one, so there is no single
	// recorded creation account to resolve against. An empty recorded id is the documented
	// "unknown, proceed" case (see credsSource.resolveExisting), which resolves the ordinary
	// project-then-system account — the behaviour the comment above already describes. The
	// per-campaign account identity is then enforced downstream by googleAdsScopeForCustomer,
	// which REFUSES THE WHOLE READ when any entry's recorded customer is not the resolved one.
	client, err := d.resolveGoogleAdsClient(ctx, projectID, platform, nil)
	if err != nil {
		return nil, err
	}
	campaignIDs, err := googleAdsScopeForCustomer(scope, client.CustomerID(), "read google ads audience insights")
	if err != nil {
		return nil, err
	}
	ai, err := client.GetAudienceInsights(ctx, gaWindow, campaignIDs)
	if err != nil {
		return nil, err
	}
	buckets := make([]model.AudienceBucket, 0, len(ai.Buckets))
	for _, b := range ai.Buckets {
		buckets = append(buckets, model.AudienceBucket{
			Dimension:   b.Dimension,
			Value:       b.Value,
			Impressions: b.Impressions,
			Clicks:      b.Clicks,
			CostMicros:  b.CostMicros,
			Ctr:         b.Ctr,
			Conversions: b.Conversions,
		})
	}
	return &model.AudienceInsights{Window: window, Buckets: buckets}, nil
}

// ApplyKeywordActions implements service.KeywordActioner for Google Ads.
//
// This MUTATES a live paid campaign, so it carries the full guard set the status toggle
// uses, in the same order and for the same reasons:
//
//  1. The batch is validated locally first. A malformed batch is a permanent fault and must
//     be refused before credentials are decrypted or Google is contacted.
//  2. The campaign must be provisioned — an empty PlatformCampaignID means there is nothing
//     upstream to act on, and an empty ad group id means GA-4's targeting step never ran, so
//     the criteria this batch names cannot belong to this campaign.
//  3. The campaign's creation account must match the account the project's connection NOW
//     resolves to. Criterion ids are bare numerics unique only within their customer, and
//     UpdateGoogleAds can re-point a connection between create and action — so on an id
//     collision this mutate would pause or REMOVE another account's keywords. This is the
//     same invariant ToggleStatus enforces, and it matters at least as much here because
//     REMOVE is irreversible.
//
// Every one of those refusals happens before the platform is contacted.
func (d *GoogleAdsDispatcher) ApplyKeywordActions(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, actions []model.KeywordAction) ([]model.KeywordActionOutcome, error) {
	in := make([]googleads.KeywordAction, 0, len(actions))
	for _, a := range actions {
		in = append(in, googleads.KeywordAction{AdGroupID: a.AdGroupID, CriterionID: a.CriterionID, Action: a.Action})
	}
	// Validate BEFORE resolving the connection, so a batch that can never succeed is refused
	// without decrypting credentials — and so a permanent input fault masks any contingent
	// connection fault rather than the other way round.
	validated, err := googleads.ValidateKeywordActions(in)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrKeywordActionInvalid, err)
	}

	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return nil, fmt.Errorf("%w: google ads campaign has no platform campaign id, so it has no keywords to act on", domain.ErrCampaignNotProvisioned)
	}
	adGroupIDs := googleAdsCampaignAdGroupIDs(campaign)
	if len(adGroupIDs) == 0 {
		return nil, fmt.Errorf("%w: google ads campaign %s has no provisioned ad group, so it has no keyword criteria to act on", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
	}
	// Every criterion must belong to one of THIS campaign's ad groups. Without this a caller
	// holding a criterion id from any campaign in the shared account could pause or remove it
	// through a campaign they do own — the path is permission-evaluated on the campaign, so
	// the campaign is what bounds it. The keywords read returns ad_group_id precisely so a
	// caller can satisfy this.
	//
	// The bound is the campaign's FULL set of ad groups, not just the first. Checking against
	// the scalar AdGroupID refused every action naming group 2 or later — fail-closed, so
	// never unsafe, but it made keyword actions unusable on exactly the multi-group campaigns
	// the feature was added for. The guard itself is unchanged in strength: an ad group this
	// campaign does not record is still refused, locally, before Google is contacted.
	for i, a := range validated {
		if !adGroupIDs[a.AdGroupID] {
			return nil, fmt.Errorf("%w: keyword action %d names ad group %s, which does not belong to campaign %s",
				domain.ErrKeywordActionInvalid, i, a.AdGroupID, campaign.PlatformCampaignID)
		}
	}

	// `campaign`, not nil: this operates on an ALREADY-CREATED campaign, so the account to
	// authenticate as is the one the campaign RECORDS being created under, exactly as
	// ToggleStatus and ReadMetrics resolve it. Passing nil would resolve the project's own
	// connection and the identity guard immediately below would then refuse every campaign
	// created while LFX_FORCE_SYSTEM_ADS_ACCOUNT was on for a project that has a connection
	// of its own — stranding those campaigns on the one path that can stop their keywords
	// from serving. See credsSource.resolveExisting for both directions of that failure.
	client, err := d.resolveGoogleAdsClient(ctx, projectID, platform, campaign)
	if err != nil {
		return nil, err
	}
	// Same identity invariant ToggleStatus and ReadMetrics enforce — but this path FAILS
	// CLOSED on an unrecorded tenant, where those two proceed.
	//
	// The asymmetry is deliberate and rests on what the operation costs when it is wrong.
	// Google Ads is ONE customer shared across every foundation (docs/architecture.md,
	// "Account Tenancy"), and ad-group/criterion ids are account-scoped bare numerics. A
	// legacy row that records no creating customer therefore cannot prove the criteria it
	// names belong to the campaign the caller addressed; if the connection was re-pointed, a
	// numeric collision aims this mutate at ANOTHER project's keyword. Reading the wrong
	// numbers is recoverable and REMOVE is not — Google cannot re-enable a removed criterion,
	// only create a new one with a new id — so a read may proceed on an unknown tenant and a
	// destructive mutation may not.
	//
	// ErrCampaignProvenanceUnknown is the sentinel for exactly this state, and the remedy it
	// carries is the honest one: there is no tenant to reconnect to, so the row must be
	// re-dispatched. Checked BEFORE the mismatch arm below, which would otherwise report
	// "reconnect the original account" about an account that was never recorded.
	created := googleAdsCreationCustomerID(campaign)
	if created == "" {
		return nil, fmt.Errorf("apply google ads keyword actions: campaign %s does not record which ad account it was created under, so its keyword criteria cannot be resolved safely: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	if created != client.CustomerID() {
		return nil, fmt.Errorf("apply google ads keyword actions: campaign %s was created under customer %s but the project's current connection resolves to customer %s: %w",
			campaign.PlatformCampaignID, created, client.CustomerID(), domain.ErrCampaignAccountMismatch)
	}

	// `validated`, not `in`: the ownership loop above checked the NORMALISED batch, so sending
	// the raw one would mean the guard and the request operate on different values. The client
	// re-validates internally and validation is idempotent, so this is not a live defect — but
	// the guard's correctness should not depend on a detail of the callee it does not state.
	outcomes, err := client.ApplyKeywordActions(ctx, validated)
	if err != nil {
		// A criterion that is not a POSITIVE KEYWORD is a permanent input fault, not an
		// upstream failure: the client resolved its type and refused before mutating, and no
		// retry turns a negative keyword or a userList criterion into a keyword. Folded onto
		// ErrKeywordActionInvalid so the service answers 400 with the same "these actions are
		// not valid" remedy the other permanent batch faults get, rather than a 503 inviting a
		// retry that can never succeed.
		if errors.Is(err, googleads.ErrKeywordCriterionNotPositiveKeyword) {
			return nil, fmt.Errorf("%w: %w", domain.ErrKeywordActionInvalid, err)
		}
		// Classify the ambiguous outcomes the way the toggle path at ToggleStatus does. Without
		// this the client's structural marker is the only thing carrying the ambiguity, and any
		// client arm that reports it through createOutcomeAmbiguous alone (a 5xx, a timeout, a
		// mutating 429) would reach the service as an unclassified error and be answered as a
		// DEFINITE failure — telling the caller to retry a batch Google may already have run,
		// which for an irreversible REMOVE is the one wrong answer.
		if googleads.IsOutcomeUnconfirmed(err) {
			return nil, &unconfirmedToggleError{err: err}
		}
		return nil, err
	}
	out := make([]model.KeywordActionOutcome, 0, len(outcomes))
	for _, o := range outcomes {
		out = append(out, model.KeywordActionOutcome{
			AdGroupID:    o.AdGroupID,
			CriterionID:  o.CriterionID,
			Action:       o.Action,
			ResourceName: o.ResourceName,
		})
	}
	return out, nil
}
