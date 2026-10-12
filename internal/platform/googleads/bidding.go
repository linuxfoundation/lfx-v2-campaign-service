// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// A Google Ads campaign carries EXACTLY ONE bidding strategy, as a oneof: the create
// payload names one of `manualCpc`, `targetSpend`, `maximizeConversions`,
// `maximizeConversionValue` (and others this client does not offer), and naming two is
// rejected. Until this file existed the choice was hard-coded per channel — `manualCpc{}`
// on Search, `targetSpend{}` on Demand Gen — which is why no campaign this service created
// could ever bid toward a conversion.
//
// The names below are the CALLER's vocabulary, not Google's. They are the labels the Google
// Ads UI uses, lower-cased and hyphenated, because the operator picking one is reading that
// UI and not the proto. `target-cpa` and `target-roas` are the two places the two
// vocabularies genuinely disagree: Google folded the standalone TargetCpa and TargetRoas
// strategies into MaximizeConversions and MaximizeConversionValue with a target set, and the
// UI still calls them by the old names. So both spellings are accepted and both resolve to
// the surviving strategy — the difference being only that the target is REQUIRED when the
// caller names the target-bearing label, and optional when they name the maximize- one.
const (
	biddingManualCPC               = "manual-cpc"
	biddingMaximizeClicks          = "maximize-clicks"
	biddingMaximizeConversions     = "maximize-conversions"
	biddingTargetCPA               = "target-cpa"
	biddingMaximizeConversionValue = "maximize-conversion-value"
	biddingTargetROAS              = "target-roas"
)

const (
	// minTargetCPA/maxTargetCPA bound a target cost per acquisition in whole units of
	// the ad ACCOUNT's currency. The floor is the smallest amount that still rounds to a
	// non-zero micros value under the same "0 means unset" convention validateCPCBid
	// uses; the ceiling is this client's sanity bound, not a Google limit — a six-figure
	// target CPA is a typo (a budget pasted into the wrong field) far more often than an
	// intent.
	minTargetCPA = 0.01
	maxTargetCPA = 1_000_000.0

	// minTargetROAS/maxTargetROAS are Google's own documented bounds for
	// target_roas, which is a RATIO and not a percentage: 1.0 means "break even", 4.0
	// means "four units of conversion value per unit spent".
	//
	// They are reproduced rather than widened or narrowed, and narrowing them to catch
	// the percentage spelling would be the wrong trade. 400 meaning 400% is a plausible
	// caller mistake, but 400 is also a value Google accepts, so refusing it would refuse
	// a create upstream would have taken — the over-refusal this package's guards must
	// never commit. The spelling is documented on CampaignInput.TargetROAS and named in
	// the ceiling's own error message instead; only a value above 1000 can be refused on
	// evidence rather than on suspicion.
	minTargetROAS = 0.01
	maxTargetROAS = 1000.0

	// maxConversionActions caps the selective-optimization list. This client's bound, not
	// Google's: the list is sent inline on the campaign create and an unbounded one turns
	// a caller typo into a multi-megabyte payload. It is set far above any plausible
	// account's conversion-action count so it never refuses a real intent.
	maxConversionActions = 100
)

// conversionActionIDRE and conversionActionResourceRE pin the two spellings a caller may
// use for a conversion action. Both are checked against the string BEFORE it reaches a
// create payload, because a malformed resource name is rejected by Google AFTER the budget
// mutate has committed — the orphan this package's whole preflight exists to avoid.
var (
	conversionActionIDRE       = regexp.MustCompile(`^\d+$`)
	conversionActionResourceRE = regexp.MustCompile(`^customers/(\d+)/conversionActions/(\d+)$`)
)

// manualCPC is `campaign.manual_cpc`. It has fields in the proto (enhanced_cpc_enabled,
// long deprecated) and this client sends none of them, so the type is deliberately empty
// and marshals to `{}` — which is what names the strategy.
type manualCPC struct{}

// targetSpend is `campaign.target_spend`, the strategy the Google Ads UI calls Maximize
// Clicks. Empty for the same reason manualCPC is: its only remaining field,
// cpc_bid_ceiling_micros, is marked deprecated in the proto, and sending a deprecated field
// risks a rejection that would land after the budget mutate.
type targetSpend struct{}

// maximizeConversions is `campaign.maximize_conversions`. A zero TargetCpaMicros is OMITTED
// rather than sent as 0, and the two meanings must not collide: omitted means "bid for as
// many conversions as the budget allows", while an explicit 0 would be a target of nothing.
// omitempty is what keeps them apart, and minTargetCPA is what guarantees an accepted target
// never rounds down into the unset zero.
type maximizeConversions struct {
	TargetCpaMicros int64 `json:"targetCpaMicros,omitempty"`
}

// maximizeConversionValue is `campaign.maximize_conversion_value`, with the same
// omitted-versus-zero argument as maximizeConversions: no TargetRoas means "maximize value
// within the budget", and minTargetROAS keeps an accepted target clear of the unset zero.
type maximizeConversionValue struct {
	TargetRoas float64 `json:"targetRoas,omitempty"`
}

// selectiveOptimization is `campaign.selective_optimization` — the set of conversion actions
// THIS campaign optimizes toward, overriding the account-level conversion goals.
//
// It is the create-time field for conversion selection, which is why this client uses it
// rather than `campaignConversionGoal`: that resource is update-only and addressed by a
// name containing the campaign id, so attaching goals through it would need a second mutate
// AFTER the campaign exists — a step that can fail and leave a campaign bidding toward the
// account's goals rather than the ones the operator chose, with nothing in the result to say
// so.
type selectiveOptimization struct {
	ConversionActions []string `json:"conversionActions"`
}

// biddingFields is the bidding half of a campaign create payload, embedded ANONYMOUSLY into
// both channel payloads so its JSON keys flatten into the campaign object.
//
// It is one shared type rather than a copy per channel because the oneof invariant — at most
// one of these four is ever non-nil — is a property of the Google Ads campaign resource, not
// of a channel. Two copies would be two places to break it. The channels still disagree
// about WHICH strategies they accept, and that disagreement lives in validateBiddingPlan
// where it can be stated once with its reasons.
type biddingFields struct {
	ManualCPC               *manualCPC               `json:"manualCpc,omitempty"`
	TargetSpend             *targetSpend             `json:"targetSpend,omitempty"`
	MaximizeConversions     *maximizeConversions     `json:"maximizeConversions,omitempty"`
	MaximizeConversionValue *maximizeConversionValue `json:"maximizeConversionValue,omitempty"`
	SelectiveOptimization   *selectiveOptimization   `json:"selectiveOptimization,omitempty"`
}

// biddingPlan is the validated, resolved bidding intent: which strategy, its target if it
// takes one, and the conversion actions the campaign optimizes toward. Resolved in the PURE
// preflight with everything else, so an unknown strategy name, an out-of-range target or a
// malformed conversion-action name fails before the budget mutate rather than after a paid
// campaign exists.
type biddingPlan struct {
	// strategy is the normalized caller vocabulary, never empty after validation — the
	// channel default is substituted when the caller names none. It is kept (rather than
	// only the payload) because the cascade reports it in a step line, and
	// `target-cpa` and `maximize-conversions` produce the same payload while meaning
	// different things to the operator reading that line.
	strategy        string
	targetCPAMicros int64
	targetROAS      float64
	// conversionActions are fully-qualified resource names, deduplicated, in the order
	// the caller gave them.
	conversionActions []string
}

// fields renders the plan as the payload half of a campaign create. Exactly one strategy
// pointer is set, which is the oneof invariant the Google Ads campaign resource requires.
func (p biddingPlan) fields() biddingFields {
	var f biddingFields
	switch p.strategy {
	case biddingManualCPC:
		f.ManualCPC = &manualCPC{}
	case biddingMaximizeClicks:
		f.TargetSpend = &targetSpend{}
	case biddingMaximizeConversions, biddingTargetCPA:
		f.MaximizeConversions = &maximizeConversions{TargetCpaMicros: p.targetCPAMicros}
	case biddingMaximizeConversionValue, biddingTargetROAS:
		f.MaximizeConversionValue = &maximizeConversionValue{TargetRoas: p.targetROAS}
	}
	if len(p.conversionActions) > 0 {
		f.SelectiveOptimization = &selectiveOptimization{ConversionActions: p.conversionActions}
	}
	return f
}

// describe is the operator-facing phrase for the cascade's step line, e.g.
// "target CPA 25.00". It names what was actually sent, so a plan whose optional target was
// omitted does not claim one.
func (p biddingPlan) describe() string {
	switch {
	case p.targetCPAMicros != 0:
		return fmt.Sprintf("%s, target CPA %.2f", p.strategy, float64(p.targetCPAMicros)/microsPerUnit)
	case p.targetROAS != 0:
		return fmt.Sprintf("%s, target ROAS %g", p.strategy, p.targetROAS)
	default:
		return p.strategy
	}
}

// defaultBiddingStrategy is the strategy a channel gets when the caller names none.
//
// These are not house preferences — they are the strategies this client hard-coded before
// the field existed, kept so that a create that omits BiddingStrategy produces a
// BYTE-IDENTICAL payload to the one it produced before. Every campaign created by this
// service to date was made with one of these two, and a new default would silently change
// how an existing caller's campaigns bid.
// Performance Max is the one channel whose default was never a hard-coded literal to
// preserve — nothing has been created on it — so its default is simply the strategy the
// channel is built around and the one Google's own create flow starts from.
func defaultBiddingStrategy(kind string) string {
	switch kind {
	case campaignKindDemandGen:
		return biddingMaximizeClicks
	case campaignKindPerformanceMax:
		return biddingMaximizeConversions
	case campaignKindVideo:
		// Same situation as Performance Max — nothing has been created on this channel, so
		// there is no prior literal to preserve — and the same answer: a VIDEO_ACTION
		// campaign exists to buy a conversion on the landing page, and maximize-conversions
		// is the strategy Google's own create flow starts it from. See
		// videoBiddingStrategies for why the set around it is deliberately narrow.
		return biddingMaximizeConversions
	case campaignKindDisplay:
		// Nothing has been created on this channel either, so again there is no prior
		// literal to preserve. Maximize conversions rather than maximize clicks because
		// every campaign this service creates carries ONE registration URL and exists to
		// buy a completion on it — and because manual CPC, which Google does accept on
		// Display, is not a strategy this client can default to: see
		// displayBiddingStrategies.
		return biddingMaximizeConversions
	default:
		return biddingManualCPC
	}
}

// demandGenBiddingStrategies is the set this client will send on DEMAND_GEN.
//
// Only Maximize Clicks, and that is a fence built on recorded evidence rather than caution.
// A validateOnly campaigns:mutate run against the live API at v23 (2026-08-14) returned HTTP
// 400 BIDDING_STRATEGY_TYPE_INCOMPATIBLE_WITH_SHARED_BUDGET for DEMAND_GEN +
// maximizeConversions, and HTTP 200 for DEMAND_GEN + targetSpend — see the long note in
// demandgen.go. Refusing a payload we have observed Google reject is not over-refusal; it is
// the preflight guarantee doing its job, because that rejection lands AFTER the budget
// mutate and orphans the budget.
//
// Widening this set is a live-API question, not a code-reading one: re-run that validateOnly
// check before adding a strategy here.
var demandGenBiddingStrategies = map[string]bool{
	biddingMaximizeClicks: true,
}

// performanceMaxBiddingStrategies is the set this client will send on PERFORMANCE_MAX.
//
// The four CONVERSION-based strategies and nothing else, which is Google's own rule
// rather than a fence this client chose: Performance Max has no manual bidding and no
// maximize-clicks — the campaign exists to buy conversions, and the API rejects the
// other two outright. Refusing them here moves a rejection that would land after the
// budget mutate to before it.
var performanceMaxBiddingStrategies = map[string]bool{
	biddingMaximizeConversions:     true,
	biddingTargetCPA:               true,
	biddingMaximizeConversionValue: true,
	biddingTargetROAS:              true,
}

// videoBiddingStrategies is the set this client will send on VIDEO (VIDEO_ACTION).
//
// NOT LIVE-VERIFIED, and that is why it is the narrowest set of the four rather than the
// widest. Every other set here was settled by running a validateOnly campaigns:mutate
// against the live API — demandGenBiddingStrategies records the exact call, its date and
// its two HTTP codes. No such call has been made for VIDEO_ACTION, because the only
// reachable Google account is a production one and a validateOnly mutate is still a POST
// to it.
//
// So the set is chosen the way an unverified set has to be: the two strategies Google's
// published VIDEO_ACTION documentation names — maximize conversions, and target CPA, the
// same strategy with a target attached — and nothing else. The error for the rest is
// explicit and lands at preflight, before any mutate, which costs a caller one clear
// message. Guessing wider costs them an HTTP 400 AFTER the budget has been created and
// paid for, which is exactly what Demand Gen's recorded 400 was.
//
// Widening this set is a live-API question, not a code-reading one: run a validateOnly
// campaigns:mutate for VIDEO/VIDEO_ACTION with the candidate strategy on a non-production
// account and record the result here, as demandgen.go does for its own.
var videoBiddingStrategies = map[string]bool{
	biddingMaximizeConversions: true,
	biddingTargetCPA:           true,
}

// displayBiddingStrategies is the set this client will send on DISPLAY.
//
// NOT LIVE-VERIFIED, on the same terms as videoBiddingStrategies: no validateOnly
// campaigns:mutate has been run for DISPLAY, because the only reachable Google account is
// a production one and a validateOnly mutate is still a POST to it. Widening it is a
// live-API question, not a code-reading one.
//
// The five AUTOMATED strategies, which is wider than Video's two because Display's
// published support for them is not in doubt — a standard Display campaign takes the
// portfolio strategies the Search channel takes, less the manual one.
//
// MANUAL CPC IS THE DELIBERATE OMISSION, and it is the one refusal in this package that
// is NOT a restatement of an upstream rule. Google accepts manualCpc on a Display
// campaign. This client refuses it because its DISPLAY AD GROUP PAYLOAD CARRIES NO BID:
// displayAdGroupCreate has no cpcBidMicros field, and the preflight refuses CPCBid on
// every non-Search channel, so a manual-CPC Display campaign created from here would go
// live bidding a number nobody supplied. That is the same shape of reasoning the campaign
// negative-keyword fence uses about Performance Max — Google accepts the thing; this
// client does not create it — and it is stated as a client limitation rather than dressed
// up as an upstream rule, because the two are not interchangeable and a reader widening
// this set later needs to know which one they are arguing with.
//
// Closing it is an implementation question, not an API one: give the ad group a bid field
// and admit manualCpc here. Until then it is a named gap.
var displayBiddingStrategies = map[string]bool{
	biddingMaximizeClicks:          true,
	biddingMaximizeConversions:     true,
	biddingTargetCPA:               true,
	biddingMaximizeConversionValue: true,
	biddingTargetROAS:              true,
}

// searchBiddingStrategies is the set this client will send on SEARCH — every strategy it
// offers. Search is the channel the manual and the automated strategies were both designed
// for, and `manualCpc` is what this client has always sent there.
var searchBiddingStrategies = map[string]bool{
	biddingManualCPC:               true,
	biddingMaximizeClicks:          true,
	biddingMaximizeConversions:     true,
	biddingTargetCPA:               true,
	biddingMaximizeConversionValue: true,
	biddingTargetROAS:              true,
}

// biddingSetIsLiveVerified reports whether this channel's strategy set was settled by a
// validateOnly campaigns:mutate against the live API, rather than read off Google's
// published documentation.
//
// It exists so the refusal message can tell a caller WHICH of those two a refusal rests
// on. The distinction is not cosmetic: a documented-only set is the one a reader may
// reasonably widen after running the check, and a verified one is a rejection this client
// has actually observed. The default is the UNVERIFIED side, so a channel added later
// understates its own evidence rather than claiming a check nobody ran.
func biddingSetIsLiveVerified(kind string) bool {
	switch kind {
	case campaignKindVideo, campaignKindDisplay:
		return false
	default:
		return true
	}
}

// knownBiddingStrategies is every name this package recognizes, used only to tell an unknown
// name apart from a name that is known but wrong for the channel. The two produce different
// errors because they need different fixes: a typo is corrected in place, while a valid
// strategy on the wrong channel means creating a different campaign.
//
// Built as the UNION of the per-channel sets rather than aliased to the Search one. An alias
// read correctly only while Search happened to be a superset of every other channel: the
// first strategy a channel accepts that Search does not would have been reported as an
// unknown NAME, which is an over-refusal of a create Google would have taken. The union
// cannot under-report, and it cannot drift, because there is no second list to maintain.
var knownBiddingStrategies = unionStrategies(searchBiddingStrategies, demandGenBiddingStrategies, performanceMaxBiddingStrategies, videoBiddingStrategies, displayBiddingStrategies)

// unionStrategies collects every name in the given sets into a new map. A new map, not one of
// the inputs: the result is a distinct concept from any single channel's set, and sharing
// backing storage with one of them would couple the two silently.
func unionStrategies(sets ...map[string]bool) map[string]bool {
	out := make(map[string]bool)
	for _, set := range sets {
		for name := range set {
			out[name] = true
		}
	}
	return out
}

// sortedKeys renders a strategy set for an error message. Sorted, because map iteration
// order is randomized and an error whose supported-values list reshuffles between two
// identical calls reads as two different errors to anyone diffing logs — and to the golden
// tests that pin these messages.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateBiddingPlan resolves the caller's bidding intent for a given campaign KIND.
//
// Pure: no network, no clock. The customer id is passed in rather than read from a client so
// this stays a free function alongside every other preflight validator; it is needed only to
// qualify a bare conversion-action id into a resource name.
//
// Every refusal here is a refusal to SEND, not a refusal to validate: nothing in this
// function talks to Google, so a plan that survives it has already decided the whole bidding
// half of the payload before a single micro of budget is committed.
func validateBiddingPlan(kind, customerID string, in CampaignInput) (biddingPlan, error) {
	strategy := strings.ToLower(strings.TrimSpace(in.BiddingStrategy))
	if strategy == "" {
		strategy = defaultBiddingStrategy(kind)
	}
	// A SWITCH, not a Demand Gen test with a Search fallback. The earlier shape gave
	// any channel added later the un-restricted Search set by default — including
	// manual CPC on a channel that has no manual bidding — and the rejection would land
	// after the budget mutate, which is the one outcome this whole preflight exists to
	// prevent. Every kind now names its own set.
	//
	// Resolved BEFORE the unknown-name check so that check can advertise the set that
	// actually applies here. Listing every name the package recognizes answered a typo on
	// Demand Gen with six names of which the next guard accepts one, so the caller fixed
	// the typo from the list and got a second error for it — one mistake, two round trips.
	var allowed map[string]bool
	switch kind {
	case campaignKindDemandGen:
		allowed = demandGenBiddingStrategies
	case campaignKindPerformanceMax:
		allowed = performanceMaxBiddingStrategies
	case campaignKindVideo:
		allowed = videoBiddingStrategies
	case campaignKindDisplay:
		allowed = displayBiddingStrategies
	default:
		allowed = searchBiddingStrategies
	}

	if !knownBiddingStrategies[strategy] {
		return biddingPlan{}, fmt.Errorf("google-ads: unknown bidding strategy %q; supported on %s: %s", capForError(in.BiddingStrategy), kind, strings.Join(sortedKeys(allowed), ", "))
	}
	if !allowed[strategy] {
		// The parenthetical differs by channel because the CLAIM behind it does. Some
		// channels' sets were settled by a live validateOnly mutate, and saying so is
		// what tells a caller the refusal is evidence rather than caution. Video's and
		// Display's were not, and repeating the verified sentence there would make this
		// client assert a check it never ran.
		//
		// A helper rather than a second `if`: the predicate is "has this channel's set
		// been live-verified", one fact per channel, and a chain of channel tests here
		// would have to be found and extended again by whoever adds the sixth.
		reason := "which is the only combination verified against the live API; the others were rejected AFTER the budget was created"
		if !biddingSetIsLiveVerified(kind) {
			reason = "a set read off Google's published documentation for this channel; this client has NOT yet verified it against the live API, and sending an unverified strategy fails AFTER the budget is created"
		}
		return biddingPlan{}, fmt.Errorf("google-ads: bidding strategy %q is not supported on %s (this client sends only %s there, %s); omit BiddingStrategy for the channel default, or create a Search campaign", strategy, kind, strings.Join(sortedKeys(allowed), ", "), reason)
	}

	// The two targets are validated against the strategy that can carry them, and REFUSED
	// on one that cannot rather than dropped. A target CPA accepted and then discarded is
	// the same defect LFXV2-3283 fixed for geo: the operator reads "campaign created" and
	// believes Google is bidding to their number while it is bidding to nothing of the
	// kind, and the one field whose entire purpose is to CAP what a conversion costs is
	// the worst place in this payload to lose silently.
	takesCPA := strategy == biddingMaximizeConversions || strategy == biddingTargetCPA
	takesROAS := strategy == biddingMaximizeConversionValue || strategy == biddingTargetROAS
	if in.TargetCPA != 0 && !takesCPA {
		return biddingPlan{}, fmt.Errorf("google-ads: a target CPA is not supported by the %q bidding strategy (only %q and %q carry one); omit TargetCPA, or choose one of those strategies", strategy, biddingMaximizeConversions, biddingTargetCPA)
	}
	if in.TargetROAS != 0 && !takesROAS {
		return biddingPlan{}, fmt.Errorf("google-ads: a target ROAS is not supported by the %q bidding strategy (only %q and %q carry one); omit TargetROAS, or choose one of those strategies", strategy, biddingMaximizeConversionValue, biddingTargetROAS)
	}
	// The target-bearing spellings REQUIRE their target. `target-cpa` with no CPA is not
	// the same request as `maximize-conversions` — the caller named the label whose whole
	// content is the number, so an absent number is a mistake, not a default.
	if strategy == biddingTargetCPA && in.TargetCPA == 0 {
		return biddingPlan{}, fmt.Errorf("google-ads: the %q bidding strategy requires a target CPA; supply TargetCPA, or use %q to bid for conversions with no target", biddingTargetCPA, biddingMaximizeConversions)
	}
	if strategy == biddingTargetROAS && in.TargetROAS == 0 {
		return biddingPlan{}, fmt.Errorf("google-ads: the %q bidding strategy requires a target ROAS; supply TargetROAS, or use %q to bid for value with no target", biddingTargetROAS, biddingMaximizeConversionValue)
	}

	targetCPAMicros, err := validateTargetCPA(in.TargetCPA)
	if err != nil {
		return biddingPlan{}, err
	}
	targetROAS, err := validateTargetROAS(in.TargetROAS)
	if err != nil {
		return biddingPlan{}, err
	}

	// An ad-group CPC bid under an automated strategy is REFUSED, not carried.
	//
	// Google accepts it — cpc_bid_micros stays on the ad group and is simply never used
	// while the campaign bids automatically — which is exactly why it must be refused
	// here. A silent accept is worse than a loud one precisely because upstream is
	// forgiving: the operator who set a 2.50 ceiling and then switched the campaign to
	// Maximize Conversions gets no signal at all that their ceiling stopped applying, and
	// the readback reports a bid the campaign does not honour. The refusal costs nothing —
	// a caller who wants their bid honoured names manual-cpc, which is also the default.
	if strategy != biddingManualCPC && (in.CPCBid != 0 || adGroupsCarryCPCBid(in.AdGroups)) {
		return biddingPlan{}, fmt.Errorf("google-ads: a CPC bid is ignored by the %q bidding strategy (Google keeps the ad-group bid but never bids it while the campaign bids automatically); omit the CPC bid, or use %q", strategy, biddingManualCPC)
	}

	// A device BID ADJUSTMENT under an automated strategy is refused for the same
	// reason, and keyed on the STRATEGY rather than the channel because that is where
	// the constraint actually lives.
	//
	// campaign_criteria.go refuses device bid modifiers on Performance Max, and that
	// arm is right for a different reason — Performance Max takes no device criteria at
	// all, not even the exclusion. But reading it as "automated bidding is a Performance
	// Max property" is how this got missed: Video and Display BOTH default to
	// maximizeConversions, and a Search campaign can be switched to one. On every one of
	// those, Google stores the adjustment and never bids it, so the operator who asked
	// for "-30% on tablet" reads "campaign created" and gets no adjustment and no signal.
	// That is the forgiving-upstream trap the CPC arm above names, one field over.
	//
	// A modifier of exactly 0 is NOT refused, and the distinction is the whole reason
	// this cannot be a flat "no device modifiers" check: 0 is the -100% opt-out, and a
	// device EXCLUSION is honoured under automated bidding exactly as it is under manual.
	// Refusing it would stop a create Google would have honoured, which is the one
	// failure these guards are not allowed to have. The escape is in the message: zero
	// the modifier to exclude the device, or name manual-cpc to bid by device.
	if strategy != biddingManualCPC {
		for i, d := range in.DeviceBidModifiers {
			if d.BidModifier != 0 {
				return biddingPlan{}, fmt.Errorf("google-ads: device bid modifier %d adjusts the bid by %v, which the %q bidding strategy ignores (Google stores the adjustment and never bids it while the campaign bids automatically); set it to 0 to exclude the device instead, or use %q to bid by device", i, d.BidModifier, strategy, biddingManualCPC)
			}
		}
	}

	conversionActions, err := validateConversionActions(kind, customerID, in.ConversionActions)
	if err != nil {
		return biddingPlan{}, err
	}

	return biddingPlan{
		strategy:          strategy,
		targetCPAMicros:   targetCPAMicros,
		targetROAS:        targetROAS,
		conversionActions: conversionActions,
	}, nil
}

// adGroupsCarryCPCBid reports whether any per-group override names a bid. The campaign-level
// CPCBid is not the only way in: validateAdGroupPlans lets each group set its own, and a
// strategy check that looked only at the campaign field would accept exactly the
// multi-ad-group create whose per-group bids are the ones being silently ignored.
func adGroupsCarryCPCBid(groups []AdGroupSpec) bool {
	for _, g := range groups {
		if g.CPCBid != 0 {
			return true
		}
	}
	return false
}

// validateTargetCPA converts a target cost per acquisition to micros under the same
// conventions validateCPCBid uses: 0 is UNSET and returns (0, nil) with no default invented,
// NaN/Inf are rejected before any ordered comparison (every comparison against NaN is false,
// so a NaN would slip past both bounds and round to garbage), and the conversion ROUNDS
// rather than truncating so 0.07 does not become one micro less than the caller asked for.
//
// Not routed through ValidateBudgetMicros for the reason validateCPCBid is not: that helper
// treats 0 as an error, and 0 is this field's "the caller named no target".
func validateTargetCPA(cpa float64) (int64, error) {
	if cpa == 0 {
		return 0, nil
	}
	if math.IsNaN(cpa) || math.IsInf(cpa, 0) {
		return 0, fmt.Errorf("google-ads: target CPA must be a finite number, got %v", cpa)
	}
	if cpa < minTargetCPA {
		return 0, fmt.Errorf("google-ads: target CPA must be at least %.2f in the account currency, got %.4f", minTargetCPA, cpa)
	}
	if cpa > maxTargetCPA {
		return 0, fmt.Errorf("google-ads: target CPA %.2f exceeds the maximum %.0f in the account currency", cpa, maxTargetCPA)
	}
	return int64(math.Round(cpa * microsPerUnit)), nil
}

// validateTargetROAS bounds a target return on ad spend against Google's own documented
// range. Unlike every other numeric field here it is NOT converted to micros — target_roas is
// a proto double and Google takes the ratio as written — so the only work is rejecting the
// values Google itself would reject, plus the non-finite ones that would slip past both
// bounds.
func validateTargetROAS(roas float64) (float64, error) {
	if roas == 0 {
		return 0, nil
	}
	if math.IsNaN(roas) || math.IsInf(roas, 0) {
		return 0, fmt.Errorf("google-ads: target ROAS must be a finite number, got %v", roas)
	}
	if roas < minTargetROAS {
		return 0, fmt.Errorf("google-ads: target ROAS must be at least %g, got %g", minTargetROAS, roas)
	}
	if roas > maxTargetROAS {
		return 0, fmt.Errorf("google-ads: target ROAS %g exceeds the maximum %g — note this is a RATIO and not a percentage (4.0 means 400%%)", roas, maxTargetROAS)
	}
	return roas, nil
}

// validateConversionActions normalizes and checks the conversion actions a campaign should
// optimize toward.
//
// Two spellings are accepted — a bare numeric id and a full
// `customers/<cid>/conversionActions/<id>` resource name — because the id is what a human
// reads off the Google Ads UI while the resource name is what a GAQL read returns, and
// refusing either would push string assembly onto every caller. A resource name naming a
// DIFFERENT customer is refused rather than rewritten: a conversion action belongs to the
// account that owns it, and silently re-pointing one at this account would attach a
// conversion that does not exist and fail after the budget mutate.
//
// Admitted on Search, Video and Display; refused on Demand Gen and Performance Max. Not
// because conversions are meaningless on those two, but because selective_optimization is
// the only create-time mechanism this client implements and neither takes it — Demand Gen
// carries its goals through conversion_goal_campaign_config, and Performance Max through
// campaign conversion goals, a resource that cannot be addressed until the campaign exists.
// Accepting the list there would validate every name and then drop the lot, leaving the
// operator believing the campaign bids toward the conversions they chose. This is the same
// refuse-don't-drop rule every other channel-incapable input in this preflight follows.
func validateConversionActions(kind, customerID string, actions []string) ([]string, error) {
	if len(actions) == 0 {
		return nil, nil
	}
	// ADMITTED on Video and Display, refused on Demand Gen and Performance Max. The split is not
	// Search-versus-the-rest, however much the earlier `!= campaignKindSearch` looked
	// like it: campaign.selective_optimization is defined for SEARCH, DISPLAY, VIDEO and
	// APP campaigns, so a VIDEO or DISPLAY campaign carrying it is a payload Google
	// accepts, and refusing either would have been over-refusal — the one failure mode
	// this preflight is not allowed to have. DISPLAY is named in that list literally,
	// which makes admitting it the least arguable of the two. Demand Gen has no selective_optimization at all, and
	// Performance Max selects its conversions through CAMPAIGN CONVERSION GOALS instead
	// — a separate resource addressed by a name containing the campaign id, so it needs
	// a mutate after the campaign exists. Those two are the same known gap, refused
	// rather than dropped for the same reason: a campaign that silently bid toward the
	// account's default goals while the operator believed it was bidding toward the ones
	// they named is worse than one that would not create.
	if kind != campaignKindSearch && kind != campaignKindVideo && kind != campaignKindDisplay {
		return nil, fmt.Errorf("google-ads: conversion actions are not supported on %s (this client attaches them through campaign.selective_optimization, which %s does not accept); omit ConversionActions, or create a Search, Video or Display campaign to optimize toward specific conversions", kind, kind)
	}
	if len(actions) > maxConversionActions {
		return nil, fmt.Errorf("google-ads: %d conversion actions exceeds the maximum %d", len(actions), maxConversionActions)
	}
	out := make([]string, 0, len(actions))
	seen := make(map[string]struct{}, len(actions))
	for i, raw := range actions {
		action := strings.TrimSpace(raw)
		if action == "" {
			return nil, fmt.Errorf("google-ads: conversion action %d is empty", i+1)
		}
		var resource string
		switch {
		case conversionActionIDRE.MatchString(action):
			resource = "customers/" + customerID + "/conversionActions/" + action
		case conversionActionResourceRE.MatchString(action):
			owner := conversionActionResourceRE.FindStringSubmatch(action)[1]
			if owner != customerID {
				return nil, fmt.Errorf("google-ads: conversion action %q belongs to customer %s, not the campaign's account %s; a conversion action cannot be shared across accounts", capForError(action), owner, customerID)
			}
			resource = action
		default:
			return nil, fmt.Errorf("google-ads: conversion action %q is neither a numeric id nor a customers/<id>/conversionActions/<id> resource name", capForError(action))
		}
		// Deduplicated rather than refused: naming the same conversion twice is a
		// harmless paste, and Google rejects a selective_optimization list containing a
		// duplicate — after the budget mutate.
		if _, dup := seen[resource]; dup {
			continue
		}
		seen[resource] = struct{}{}
		out = append(out, resource)
	}
	return out, nil
}
