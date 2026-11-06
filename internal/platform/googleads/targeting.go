// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/textfold"
)

// ---------------------------------------------------------------------------
// Keyword + audience targeting (GA-4): adGroupCriteria:mutate. GA-3 created an
// ad group with zero criteria, which matches no query — this closes that gap
// by attaching positive Search keywords and/or existing audience segments to
// the ad group createAdGroupAndAd just built. This client does not create
// audiences: AudienceSegments are resource names for existing Customer Match
// user lists that the caller has already built elsewhere, supplied the same
// way reddit's dispatcher passes through cfg.Keywords/cfg.Interests.
// ---------------------------------------------------------------------------

const (
	// MaxKeywordTextRunes is Google Ads v23's KeywordInfo.text limit (System
	// Limits table: 80 characters).
	//
	// EXPORTED because the dispatcher must apply it BEFORE calling this package. The
	// shared negative-keyword batch validator admits 100 runes — Microsoft's
	// NegativeKeyword.Text limit, which is the endpoint's published one — so an 81..100
	// rune text clears every dispatcher guard and is refused here instead, with a plain
	// error that carries neither Unconfirmed() nor NotAttempted(). The dispatcher would
	// then classify it as a platform REJECTION of every keyword in the batch: false twice
	// over, since Google was never contacted and the fault is the caller's to fix. The
	// endpoint's text limit is therefore per-platform, and this is the Google half of it.
	MaxKeywordTextRunes = 80
	// maxKeywords/maxAudienceSegments bound caller input to keep one
	// adGroupCriteria:mutate call (and its log/error output) a sane size. Not a
	// Google Ads platform limit — a sanity cap on this broker's input; Google's own
	// per-ad-group ceiling is orders of magnitude higher.
	//
	// 20 was the original value and it was NOT generous enough: the product's own AI
	// brief generator routinely emits ~38 keywords, so every default paid create was
	// refused here (observed end-to-end 2026-08-13 — "at most 20 keywords are
	// supported, got 38", zero campaigns created). A cap that the system's own
	// upstream stage exceeds by default is not protecting a caller from a mistake, it
	// is blocking a create the ad platform would have accepted. Raised to 60 to clear
	// the generator's real output with headroom while keeping one mutate call bounded.
	maxKeywords         = 60
	maxAudienceSegments = 20

	// maxNegativeKeywords bounds campaign-level NEGATIVE keyword input for the same
	// reason maxKeywords bounds positive input — one bounded campaignCriteria:mutate
	// call — and is deliberately the SAME number rather than a tighter one. A negative
	// list is routinely longer than its positive counterpart (brand terms, competitor
	// names, "free"/"jobs"/"salary" style exclusions), so a smaller cap here would be
	// the sibling of the 20-keyword cap that blocked every default create on
	// 2026-08-13: a limit the system's own upstream stage exceeds is not protecting a
	// caller from a mistake, it is refusing a create Google would have accepted.
	maxNegativeKeywords = 60

	// MatchTypeExact/Phrase/Broad are the only Search keyword match types.
	MatchTypeExact  = "EXACT"
	MatchTypePhrase = "PHRASE"
	MatchTypeBroad  = "BROAD"
)

// Keyword is a single positive Search keyword criterion. Text and MatchType
// are both required; see validateKeywords for the exact rules.
type Keyword struct {
	Text      string
	MatchType string
}

// keywordInfo is the "keyword" criterion payload in an adGroupCriteria:mutate
// create.
type keywordInfo struct {
	Text      string `json:"text"`
	MatchType string `json:"matchType"`
}

// userListInfo is the "userList" criterion payload — a Customer Match /
// remarketing list the caller already built, referenced by resource name.
type userListInfo struct {
	UserList string `json:"userList"`
}

// adGroupCriterionCreate is the create payload for adGroupCriteria:mutate.
// Exactly one of Keyword/UserList/AgeRange/Gender/IncomeRange/ParentalStatus is set per
// operation. AgeRange and Gender are sent only POSITIVE, for an observed Search dimension
// (#497) or Display demographic targeting (#499); their exclusions are campaign criteria
// (campaignCriterionCreate).
//
// Status and Negative are both omitempty, so a positive keyword or audience criterion is
// the same bytes it was before #473. A NEGATIVE criterion — an ad-group negative keyword,
// an income or a parental-status exclusion — is sent as {adGroup, negative: true, <arm>}
// with no status. validateOnly probes on 2026-11-03 accepted that shape for a negative
// keyword and an income range on a Search ad group, and for an income range (including
// INCOME_RANGE_UNDETERMINED) and a parental status on a Display ad group.
//
// There is deliberately no customAudience field. Google Ads has a
// customAudience criterion, but validateAudienceSegments rejects every
// customAudiences resource name before any mutate is built — Custom Audiences
// are not supported on SEARCH campaigns. The Display cascade also sends this
// payload, but only for its income and parental-status exclusions, never an
// audience: Display's audience criteria, custom audiences included, use
// displayAudienceCriterionCreate (display_targeting.go). Carrying the field here
// would make the payload advertise a targeting shape it can never populate.
type adGroupCriterionCreate struct {
	AdGroup        string              `json:"adGroup"`
	Status         string              `json:"status,omitempty"`
	Negative       bool                `json:"negative,omitempty"`
	Keyword        *keywordInfo        `json:"keyword,omitempty"`
	UserList       *userListInfo       `json:"userList,omitempty"`
	AgeRange       *ageRangeInfo       `json:"ageRange,omitempty"`
	Gender         *genderInfo         `json:"gender,omitempty"`
	IncomeRange    *incomeRangeInfo    `json:"incomeRange,omitempty"`
	ParentalStatus *parentalStatusInfo `json:"parentalStatus,omitempty"`
}

// adGroupCriteriaSet is everything one ad group's adGroupCriteria:mutate carries, in the
// order the operations are sent: keywords, audience segments, negative keywords, the
// income and parental-status exclusions, the observed demographics (#497, Search), then the
// Display demographic targeting (#499).
type adGroupCriteriaSet struct {
	keywords         []Keyword
	audienceSegments []string
	negativeKeywords []Keyword
	demographics     adGroupDemographicPlan
	observed         adGroupObservationPlan
	targeted         adGroupDemographicTargetingPlan
}

func (s adGroupCriteriaSet) count() int {
	return len(s.keywords) + len(s.audienceSegments) + len(s.negativeKeywords) + s.demographics.count() + s.observed.count() + s.targeted.count()
}

// adGroupCriteriaIDs are the criterion ids createAdGroupTargeting confirmed, split by kind.
type adGroupCriteriaIDs struct {
	keywords         []string
	audiences        []string
	negativeKeywords []string
	demographics     []string
	observed         []string
	targeted         []string
}

// adGroupCriteriaStep summarises one group's confirmed criteria for the step list. The
// keyword and audience counts are always named, as they were before #473; the newer kinds
// only when present.
func adGroupCriteriaStep(ids adGroupCriteriaIDs) string {
	line := fmt.Sprintf("Keyword/audience targeting attached: %d keyword(s), %d audience segment(s)", len(ids.keywords), len(ids.audiences))
	if n := len(ids.negativeKeywords); n > 0 {
		line += fmt.Sprintf(", %d negative keyword(s)", n)
	}
	if n := len(ids.demographics); n > 0 {
		line += fmt.Sprintf(", %d income/parental exclusion(s)", n)
	}
	if n := len(ids.observed); n > 0 {
		line += fmt.Sprintf(", %d observed demographic(s)", n)
	}
	return line
}

// validateKeywords trims/validates each caller-supplied keyword and
// de-duplicates by (matchType, text) — Google rejects an exact duplicate
// criterion within the same ad group. Returns (nil, nil) for an empty input
// (targeting is optional). An over-limit text or unrecognized match type is a
// hard error: unlike composeAdCopy, there is no deterministic placeholder to
// fall back to for a keyword, so a bad entry must fail loudly rather than be
// silently dropped.
func validateKeywords(keywords []Keyword) ([]Keyword, error) {
	return validateKeywordList("keyword", keywords, maxKeywords)
}

// validateNegativeKeywords is the NEGATIVE-keyword analogue of validateKeywords:
// the same text/match-type/count rules, applied to the campaign-level exclusion
// list, with "negative keyword" in every message so an operator reading a
// rejection knows which of the two lists was refused.
//
// It is a separate entry point rather than a direct call to validateKeywords
// because that function's error strings all say "keyword", and reusing it would
// report a bad NEGATIVE as a bad positive — sending whoever has to fix it looking
// at the wrong half of the config. The rules themselves are genuinely identical,
// so they live once in validateKeywordList and the two callers differ only in the
// noun and the cap.
//
// Dedupe is PER LIST, deliberately: a term appearing as both a positive and a
// negative is legal upstream (that is how a broad positive is narrowed), the two
// criteria live at different levels — ad group vs campaign — and refusing the
// combination here would reject a create Google accepts.
func validateNegativeKeywords(keywords []Keyword) ([]Keyword, error) {
	return validateKeywordList("negative keyword", keywords, maxNegativeKeywords)
}

// validateKeywordList is the shared implementation behind validateKeywords and
// validateNegativeKeywords. `noun` names the list in every error message; `max`
// is that list's own sanity cap.
func validateKeywordList(noun string, keywords []Keyword, max int) ([]Keyword, error) {
	if len(keywords) == 0 {
		return nil, nil
	}
	if len(keywords) > max {
		return nil, fmt.Errorf("google-ads: at most %d %ss are supported, got %d", max, noun, len(keywords))
	}
	seen := map[string]struct{}{}
	out := make([]Keyword, 0, len(keywords))
	for _, kw := range keywords {
		text := strings.TrimSpace(kw.Text)
		if text == "" {
			return nil, fmt.Errorf("google-ads: %s text must not be empty", noun)
		}
		if utf8.RuneCountInString(text) > MaxKeywordTextRunes {
			return nil, fmt.Errorf("google-ads: %s %q exceeds the %d-character limit", noun, text, MaxKeywordTextRunes)
		}
		matchType := strings.ToUpper(strings.TrimSpace(kw.MatchType))
		switch matchType {
		case MatchTypeExact, MatchTypePhrase, MatchTypeBroad:
		default:
			return nil, fmt.Errorf("google-ads: %s %q has unsupported match type %q (want %s, %s, or %s)",
				noun, text, kw.MatchType, MatchTypeExact, MatchTypePhrase, MatchTypeBroad)
		}
		// Google Ads treats keyword text as case-insensitive for uniqueness within an
		// ad group, so the dedupe key case-folds text (the stored/sent Text keeps its
		// original casing) — otherwise "Kubernetes" and "kubernetes" both pass this
		// preflight check as distinct, then the whole adGroupCriteria:mutate fails as a
		// definite 4xx after the budget/campaign/ad group/ad already exist.
		//
		// The fold is textfold.Key, NOT strings.ToLower, because this key has to agree with
		// the dispatcher's: internal/dispatch keys every negative-keyword comparison on
		// textfold.Key, and the two folds disagree in BOTH directions — ToLower collapses
		// "İ" onto "i" where simple folding keeps them distinct, and keeps "ΟΣ" apart from
		// "ος" where simple folding joins them. Under the old key a dispatcher-approved
		// batch of two could shrink to one operation here, Google would acknowledge one id,
		// and the dispatcher — still expecting two — would answer UNCONFIRMED while one
		// exclusion was silently never applied, the same way on every retry.
		//
		// validateKeywords (the POSITIVE create path) inherits this key, where that reason
		// does not reach — no dispatcher compares positive keywords. It inherits it anyway so
		// both entry points fold identically: one helper with two folds would be a worse
		// defect than either fold, and the behaviour is deliberately not forked here. The
		// trade on that path is not uniformly an improvement — simple folding joins "ΟΣ"/"ος"
		// that ToLower kept apart, which strictly reduces duplicate operations, but keeps
		// "İ"/"i" apart where ToLower collapsed them, so a pair Google considers one keyword
		// could now be sent twice. That direction is bounded by this preflight being a
		// best-effort guard, not the authority: Google refusing a duplicate is the atomic
		// 4xx this check exists to make RARE, never the thing it promises cannot happen.
		//
		// Whitespace is the one input this key does not normalise itself: it trims but does
		// not collapse internal runs, while googleAdsNegativeKey collapses both. The two
		// still agree on the negative path, because every text reaching it has already been
		// through NormalizeNegativeKeywordText (strings.Fields joined by one space) in
		// validateNegativeKeywordBatch, so there is no internal run left to disagree about.
		// Collapsing here as well would be inert on that path and would change the POSITIVE
		// one, which no dispatcher key constrains — so the agreement is kept where it is
		// load-bearing rather than widened where it is not.
		key := matchType + "\x00" + textfold.Key(text)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Keyword{Text: text, MatchType: matchType})
	}
	return out, nil
}

// audienceCriterionField reports which oneof field a caller-supplied audience
// resource name maps to, inferred from its resource-collection segment.
// Google Ads has several audience-criterion shapes (userInterest,
// combinedAudience, detailedDemographic, …); this client recognizes only the
// two that name a list the caller already built rather than a Google-defined
// category — a Customer Match user list or a custom audience.
//
// Only "userList" is ever actually mutated. "customAudience" is recognized
// solely so validateAudienceSegments can reject it with the reason ("not
// supported for SEARCH campaigns") instead of the generic
// unrecognized-resource-name error, which would send a caller looking for a
// typo in a resource name that is perfectly well formed.
//
// These resource names are Google Ads' own, supplied through this dispatcher's
// configuration. They are unrelated to the campaign_audiences resource in
// docs/api-catalog.md: that resource's platform enum is "hubspot" only
// (design/audience.go), so it holds HubSpot master-list pointers, which can
// never appear here.
//
// Validates that the resource name has the exact shape .../userLists/{id} or
// .../customAudiences/{id}, where {id} is numeric.
func audienceCriterionField(resourceName string) (field string, ok bool) {
	const userListPattern = "/userLists/"
	const customAudiencePattern = "/customAudiences/"

	var field_name string
	var pattern string

	switch {
	case strings.Contains(resourceName, userListPattern):
		field_name = "userList"
		pattern = userListPattern
	case strings.Contains(resourceName, customAudiencePattern):
		field_name = "customAudience"
		pattern = customAudiencePattern
	default:
		return "", false
	}

	// Parse the complete resource name: customers/{numericCustomerId}/{userLists|customAudiences}/{numericId}
	// Extract the prefix before the pattern (should be customers/{id}/) and validate it.
	patternIdx := strings.Index(resourceName, pattern)
	if patternIdx < 0 {
		return "", false // Should not happen given the switch above, but be safe.
	}

	// Verify the prefix is "customers/{numericId}" (the / before pattern is part of pattern)
	prefix := resourceName[:patternIdx]
	if !strings.HasPrefix(prefix, "customers/") {
		return "", false
	}
	custID := prefix[len("customers/"):]
	if custID == "" || !numericID(custID) {
		return "", false
	}

	// Extract and validate the ID portion after the pattern to ensure it's numeric
	// and there's nothing after it.
	idPart := resourceName[patternIdx+len(pattern):]
	if idPart == "" || !numericID(idPart) {
		return "", false // Empty or non-numeric ID
	}

	return field_name, true
}

// campaignNegativeKeywordCreate is the create payload for a NEGATIVE keyword
// criterion on campaignCriteria:mutate.
//
// It is a separate type from geo.go's campaignCriterionCreate — which carries the
// `location` arm of the same oneof — for the reason that file already states for
// its own two payloads: one type per criterion SHAPE, named for what it carries,
// so a payload can never emit two arms of the oneof or silently omit the field
// that gives it meaning. Here that field is `negative`, which is emitted
// explicitly (no omitempty) rather than relying on a default: a campaign keyword
// criterion with negative=false is a POSITIVE campaign-level keyword, which is
// not a thing this client ever means to create, so the false case must be
// impossible to reach by omission.
type campaignNegativeKeywordCreate struct {
	Campaign string       `json:"campaign"`
	Negative bool         `json:"negative"`
	Keyword  *keywordInfo `json:"keyword,omitempty"`
}

// validateAudienceSegments trims/validates each caller-supplied audience
// resource name and de-duplicates. Returns (nil, nil) for an empty input.
// An unrecognized resource-name shape is a hard error — see
// audienceCriterionField. It judges the SEARCH ad group's segments, and those
// sent to any other non-Display channel, where the preflight's fence then
// refuses them. Search does not support Custom Audiences (Google limits them to
// Display, Demand Gen, Gmail, Video, and Performance Max), so only userLists are
// accepted. Display's lists are judged by validateDisplayAudienceList instead,
// which also checks the account.
func validateAudienceSegments(segments []string) ([]string, error) {
	if len(segments) == 0 {
		return nil, nil
	}
	if len(segments) > maxAudienceSegments {
		return nil, fmt.Errorf("google-ads: at most %d audience segments are supported, got %d", maxAudienceSegments, len(segments))
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(segments))
	for _, s := range segments {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("google-ads: audience segment resource name must not be empty")
		}
		field, ok := audienceCriterionField(s)
		if !ok {
			return nil, fmt.Errorf("google-ads: audience segment %q is not a recognized resource name (want a .../userLists/{id} Customer Match user-list resource name)", s)
		}
		// Reject customAudiences: SEARCH campaigns do not support Custom Audiences
		// per Google's documentation. A Display campaign takes them, in its own
		// CustomAudiences list.
		if field == "customAudience" {
			return nil, fmt.Errorf("google-ads: custom audiences are not supported for SEARCH campaigns; only userLists are accepted (a Display campaign takes custom audiences in CustomAudiences)")
		}
		// audienceCriterionField accepts any digit run, so "customers/1/userLists/011" and
		// ".../userLists/11" would pass the de-dupe below as two names for one list. Both
		// ids must be canonical; the shape check above guarantees exactly four parts.
		if parts := strings.Split(s, "/"); canonicalCampaignID(parts[1]) == "" ||
			canonicalCampaignID(parts[3]) == "" {
			return nil, fmt.Errorf("google-ads: audience segment %q does not spell its ids "+
				"canonically (base-10, positive, no leading zeros); write it as Google "+
				"returns it", s)
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

// createAdGroupTargeting attaches one ad group's criteria — keywords, audience
// segments, negative keywords, income/parental exclusions, the observed demographics
// (#497) and the Display demographic targeting (#499), whichever are set —
// to the just-created ad group as a SINGLE adGroupCriteria:mutate call carrying one
// operation per criterion. Batched into one call (not one per criterion) so
// the whole set shares one atomic outcome: partialFailure is left false (the
// package default — see mutateRequest), so this either wholly succeeds or
// wholly fails, same "no partial state" reasoning as every other mutate here,
// just extended to N operations instead of 1. The exclusions ride in the same
// batch for the reason createCampaignTargetingCriteria gives for its own: a group
// whose keywords committed and whose exclusions did not is serving the queries and
// the audience it was told to avoid. On Display the same call carries the group's
// exclusions and its demographic targeting (#499), which share one outcome the same way;
// Display's audience criteria go through createDisplayAudienceCriteria instead.
//
// Every POSITIVE criterion is created ENABLED (not PAUSED, unlike the ad group/ad
// shell): a criterion's own status is one more gate on top of its ancestors
// (ad group, ad, campaign) already being enabled/eligible — Google will not
// serve it while any ancestor is PAUSED, so creating it ENABLED now means the
// campaign is immediately serve-ready the moment a human flips the ad
// group/ad/campaign to ENABLED, with no separate targeting-activation step.
// Negative criteria carry no status; see adGroupCriterionCreate.
//
// Audience criteria rely on the ad group create having already set its
// targetingSetting to observation-only for the AUDIENCE dimension (see
// targetingSetting's doc comment in campaign.go, and its use in
// adgroup_ad.go) — this function does not re-check that here. The Display cascade
// is the one other caller, and it sends only the income/parental exclusions and its
// demographic targeting (#499), whose bidOnly false is declared by
// displayAdGroupTargetingSetting.
//
// This is the highest-risk unverified assumption in this slice, mirroring the
// AdGroupAd composite-resourceName flag from GA-3: verify against a live
// account that a Search campaign's audience criteria actually honor
// bidOnly=true as observation-only rather than still narrowing reach, before
// relying on audience segments to expand rather than restrict delivery. The same
// is true of the observed demographics (#497): neither the demographic bidOnly
// restrictions nor the positive ageRange/gender/incomeRange criteria on a Search ad
// group have been checked with a validateOnly probe yet.
//
// Duplicate-criterion classification is unverified for this resource (unlike
// the budget/campaign/ad-group DUPLICATE_NAME family): any 4xx here —
// including a possible duplicate-criterion rejection on a retry — is reported
// as a straightforward failure, not reconciled by a duplicate predicate.
func (c *Client) createAdGroupTargeting(ctx context.Context, adGroupResource, adGroupID string, set adGroupCriteriaSet) (adGroupCriteriaIDs, error) {
	if set.count() == 0 {
		return adGroupCriteriaIDs{}, nil
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return adGroupCriteriaIDs{}, fmt.Errorf("google-ads ad group criteria aborted before any request (context already done; ad group %s has no criteria yet): %w", adGroupID, ctxErr)
	}

	ops := make([]mutateOperation, 0, set.count())
	for _, kw := range set.keywords {
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup: adGroupResource,
			Status:  StatusEnabled,
			Keyword: &keywordInfo{Text: kw.Text, MatchType: kw.MatchType},
		}})
	}
	for _, seg := range set.audienceSegments {
		// Every segment reaching here is a userList: validateAudienceSegments (the sole
		// producer of this slice) rejects customAudiences resource names outright, and
		// any other shape fails audienceCriterionField. So there is no oneof to branch
		// on. The earlier switch here was dead in its customAudience arm and, worse,
		// silently emitted a criterion with NO oneof set for an unrecognized field —
		// a 4xx after the budget/campaign/ad group/ad already exist.
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup:  adGroupResource,
			Status:   StatusEnabled,
			UserList: &userListInfo{UserList: seg},
		}})
	}
	// The exclusions below are NEGATIVE, and that flag is the whole meaning of each: an
	// omitted `negative` would TARGET the query, income range or parental status the
	// caller asked to keep out.
	for _, kw := range set.negativeKeywords {
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup:  adGroupResource,
			Negative: true,
			Keyword:  &keywordInfo{Text: kw.Text, MatchType: kw.MatchType},
		}})
	}
	for i := range set.demographics.incomeRanges {
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup:     adGroupResource,
			Negative:    true,
			IncomeRange: &set.demographics.incomeRanges[i],
		}})
	}
	for i := range set.demographics.parentalStatuses {
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup:        adGroupResource,
			Negative:       true,
			ParentalStatus: &set.demographics.parentalStatuses[i],
		}})
	}
	// The observed demographics (#497) are POSITIVE and ENABLED: the ad group's
	// targetingSetting declares their dimensions bidOnly, so they report and take bid
	// adjustments without narrowing reach (see createAdGroupAndAd).
	for i := range set.observed.ageRanges {
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup:  adGroupResource,
			Status:   StatusEnabled,
			AgeRange: &set.observed.ageRanges[i],
		}})
	}
	for i := range set.observed.genders {
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup: adGroupResource,
			Status:  StatusEnabled,
			Gender:  &set.observed.genders[i],
		}})
	}
	for i := range set.observed.incomeRanges {
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup:     adGroupResource,
			Status:      StatusEnabled,
			IncomeRange: &set.observed.incomeRanges[i],
		}})
	}
	// The Display demographic targeting (#499) is POSITIVE and ENABLED: the ad group's
	// targetingSetting declares each targeted dimension bidOnly false, so these criteria
	// restrict who sees the ad (see displayAdGroupTargetingSetting).
	for i := range set.targeted.ageRanges {
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup: adGroupResource, Status: StatusEnabled, AgeRange: &set.targeted.ageRanges[i],
		}})
	}
	for i := range set.targeted.genders {
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup: adGroupResource, Status: StatusEnabled, Gender: &set.targeted.genders[i],
		}})
	}
	for i := range set.targeted.incomeRanges {
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup: adGroupResource, Status: StatusEnabled, IncomeRange: &set.targeted.incomeRanges[i],
		}})
	}
	for i := range set.targeted.parentalStatuses {
		ops = append(ops, mutateOperation{Create: adGroupCriterionCreate{
			AdGroup: adGroupResource, Status: StatusEnabled, ParentalStatus: &set.targeted.parentalStatuses[i],
		}})
	}

	resp, mErr := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if mErr != nil {
		if createOutcomeAmbiguous(mErr) {
			return adGroupCriteriaIDs{}, fmt.Errorf("google-ads ad group criteria UNCONFIRMED (ad group %s; criteria may exist — verify in Google Ads before retrying): %w", adGroupID, mErr)
		}
		return adGroupCriteriaIDs{}, fmt.Errorf("google-ads ad group criteria failed (ad group %s created; it has none of its %d requested keyword, audience, negative keyword, exclusion, observed or targeted demographic criteria): %w", adGroupID, len(ops), mErr)
	}

	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return adGroupCriteriaIDs{}, fmt.Errorf("google-ads ad group criteria UNCONFIRMED (ad group %s; 2xx with a malformed/short mutate response — criteria may exist — verify in Google Ads before retrying)", adGroupID)
	}

	all := make([]string, 0, len(ops))
	seen := newResultIDSet(len(mr.Results))
	for i, r := range mr.Results {
		returnedAdGroupID, critID := c.adGroupCriterionID(r.ResourceName)
		if critID == "" || returnedAdGroupID == "" {
			return adGroupCriteriaIDs{}, fmt.Errorf("google-ads ad group criteria UNCONFIRMED (ad group %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", adGroupID, r.ResourceName, i)
		}
		// The adGroupCriterion resourceName's ad-group-id half must match the ad group this
		// criterion was created under — a mismatch means the response doesn't describe the
		// criterion this call just created (a malformed/substituted resourceName), so the
		// returned criterion ID cannot be trusted enough to persist.
		if returnedAdGroupID != adGroupID {
			return adGroupCriteriaIDs{}, fmt.Errorf("google-ads ad group criteria UNCONFIRMED (ad group %s; adGroupCriterion resource name %q reports a different ad group id %q — verify in Google Ads before retrying)", adGroupID, r.ResourceName, returnedAdGroupID)
		}
		if dup := seen.repeat(critID, i); dup != "" {
			return adGroupCriteriaIDs{}, fmt.Errorf("google-ads ad group criteria UNCONFIRMED "+
				"(ad group %s; %s — verify in Google Ads before retrying)", adGroupID, dup)
		}
		all = append(all, critID)
	}
	// Results are index-aligned with the operations, so the ids split back into kinds by
	// the same order the operations were built in. Each kind is capped at its own length so
	// an append to one can never write into the next.
	k, a, n := len(set.keywords), len(set.audienceSegments), len(set.negativeKeywords)
	d := k + a + n + set.demographics.count()
	o := d + set.observed.count()
	return adGroupCriteriaIDs{
		keywords:         nilIfEmpty(all[:k:k]),
		audiences:        nilIfEmpty(all[k : k+a : k+a]),
		negativeKeywords: nilIfEmpty(all[k+a : k+a+n : k+a+n]),
		demographics:     nilIfEmpty(all[k+a+n : d : d]),
		observed:         nilIfEmpty(all[d:o:o]),
		targeted:         nilIfEmpty(all[o:]),
	}, nil
}

// nilIfEmpty returns nil for an empty slice, so a criteria kind that was not requested
// reads as absent rather than as an empty list.
func nilIfEmpty(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// createCampaignNegativeKeywords attaches NEGATIVE keyword criteria to a
// just-created SEARCH campaign as a single campaignCriteria:mutate call, one
// operation per negative. Batched into one call so the whole exclusion list
// shares one atomic outcome (partialFailure stays false, as everywhere else in
// this client): a half-applied negative list is worse than none, because an
// operator looking at the campaign sees exclusions in place and has no reason to
// suspect the rest were dropped.
//
// CAMPAIGN level, not ad group, and that is a choice rather than a convenience.
// A negative keyword expresses campaign-wide intent — queries this event should
// never pay for — so it must keep applying when someone adds a second ad group
// later, which an ad-group-level exclusion would not. The Search path already
// owns a campaignCriteria:mutate call for geo, so the level costs nothing new.
//
// It is deliberately NOT folded into that geo call. createCampaignGeoTargeting's
// failure messages name the consequence of ITS criteria going missing ("it has NO
// location criteria and would serve worldwide if enabled"); a shared mutate would
// report that sentence for a dropped negative keyword, which is simply false, and
// would make either list's failure discard the other.
//
// Called AFTER the campaign create and reported as a partial-result failure by
// the caller: the campaign exists and is PAUSED regardless, so a failure here
// must never be a (nil, err) that discards the claim.
//
// Duplicate-criterion classification is unverified for this resource, as it is
// for the ad-group criteria above: any definite 4xx here is reported as a
// straightforward failure rather than reconciled by a duplicate predicate.
// The mutate-and-verify itself lives in mutateCampaignNegativeKeywords
// (negative_keywords.go), shared with the EDIT path's AddCampaignNegativeKeywords.
// The request and the acknowledgement checks are identical on both paths; only the
// sentence describing what a definite failure LEFT BEHIND differs, which is the
// argument passed below — the create path's "no negative keywords at all" is simply
// untrue of a campaign that already had some.
func (c *Client) createCampaignNegativeKeywords(ctx context.Context, campaignResource, campaignID string, negatives []Keyword) ([]string, error) {
	return c.mutateCampaignNegativeKeywords(ctx, campaignResource, campaignID, negatives,
		"the campaign was created but has NO negative keywords and would be eligible for every query its positive keywords match if enabled")
}
