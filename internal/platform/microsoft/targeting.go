// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Keyword targeting + ad-group bid (MS-4). MS-2.5 created the Campaign -> AdGroup ->
// Responsive Search Ad hierarchy with ZERO keywords, so a Search ad group had nothing to
// match a query against: the tree was structurally complete and commercially inert, and
// enabling it in the Bing UI would have served nothing. This closes that gap by attaching
// keywords to the ad group createAdGroupAndAd just built, and by setting that ad group's
// CpcBid on create.
//
// Shape mirrors googleads/targeting.go (validate up front → one batched create → parse the
// index-aligned ids), because the orchestrator builds one input shape per platform and a
// gratuitously different structure here would be a second thing to learn. The WIRE contract
// is genuinely different, though, and NOT by analogy to the sibling:
//
//   - Keywords are their own resource: POST CampaignManagement/v13/Keywords (the historical
//     AddKeywords operation, still the v13 path). They are NOT AdGroupCriterions. That
//     endpoint exists and is spelled "Criterions", but its AdGroupCriterionType enum has no
//     Keyword member at all (Age, Audience, … Webpage), so keywords cannot be routed
//     through it. Verified against the AddKeywords and AdGroupCriterionType references.
//   - AdGroupId is a SIBLING of the Keywords array, exactly as AddAdGroups carries
//     CampaignId and AddAds carries AdGroupId. It is NOT a per-keyword field, and a
//     Keyword object has no AdGroupId member.
//   - A Keyword is FLAT: {Text, MatchType, Status, Bid?} with no Type discriminator and no
//     nested Criterion object. Unlike the polymorphic Ad body (msResponsiveSearchAd), there
//     is no subtype to select here.
//   - PartialErrors on 200, as every other create in this client: a per-entity failure is a
//     200 with a null id slot, not a non-2xx. Classification therefore reuses the existing
//     firstEntityID contract rather than inventing a second one. (AddKeywords returns a FLAT
//     PartialErrors array — the NestedPartialErrors shape belongs to the criterion endpoints
//     this client does not use.)
//
// GEO TARGETING IS NOT IN THIS FILE, but it EXISTS: it lives in geo.go (LFXV2-3279),
// because Microsoft takes location criteria at the CAMPAIGN level (POST /CampaignCriterions
// with CriterionType "Targets") while everything here is ad-group scoped. That split is the
// only reason the two are separate files.
//
// An earlier revision of this comment asserted geo targeting was IMPOSSIBLE without an
// invented ISO->LocationId table, on the grounds that "the v13 API accepts an ISO code for
// targeting NOWHERE" and that the ISO table in Microsoft's Geographical Location Codes guide
// is "explicitly scoped to account business addresses". Checked against the primary sources,
// the CONSTRAINT held but that PROOF did not. LocationCriterion.LocationId really is the only
// Add-writable element (DisplayName/LocationType/EnclosedLocationIds are all "Add: Read-only")
// and the locations file really has no ISO column, so an ISO code is genuinely not a
// targetable value. But the guide introduces its country table with "In some contexts the API
// requires a country code string e.g., for the business address of an AdvertiserAccount
// object" — an EXAMPLE, not a scope limit, so the table was never the barrier it was said to
// be. What it yields is a country NAME, and a name is not a LocationId; THAT is why the file
// must be ingested.
//
// geo.go therefore does what the deferral asked for: it fetches the locations file via
// POST /GeoLocationsFileUrl/Query, parses it by COLUMN NAME, drops non-Active rows, and
// caches the result with a refresh — so every LocationId that reaches a paid campaign comes
// from Microsoft at run time, and none is hardcoded here.
// See the MS-4 section of docs/knowledge/code/internal-platform-microsoft.md.
// ---------------------------------------------------------------------------

const (
	// maxKeywordTextRunes is Microsoft's documented Keyword.Text limit: 100 characters.
	// (Google Ads caps the same field at 80, so this is NOT a copy of the sibling's number.)
	maxKeywordTextRunes = 100

	// maxKeywords bounds caller input so one AddKeywords call (and its log/error output)
	// stays a sane size. This is NOT Microsoft's per-ad-group ceiling, which is far higher —
	// it is a sanity cap on this broker's input.
	//
	// 60 matches the google-ads sibling deliberately. That value was not chosen freehand: it
	// was raised from 20 after the product's own AI brief generator was observed emitting
	// ~38 keywords, which a 20-cap refused outright, blocking every default paid create. The
	// same generator feeds the Microsoft path, so a tighter cap here would reproduce a bug
	// the sibling has already paid for.
	maxKeywords = 60

	// Keyword match types. Microsoft's MatchType enum for keywords is exactly these three,
	// spelled in PascalCase — NOT the google-ads SCREAMING_CASE, so a value copied across
	// from the sibling would be rejected. (The enum's other members, e.g. ContentContextual,
	// are not valid for keywords in v13.)
	MatchTypeExact  = "Exact"
	MatchTypePhrase = "Phrase"
	MatchTypeBroad  = "Broad"

	// keywordStatusPaused creates every keyword PAUSED.
	//
	// This is a DELIBERATE divergence from googleads/targeting.go, which creates criteria
	// ENABLED on the argument that the paused ancestors are gate enough. That argument does
	// not survive this repo's standing constraint — a created campaign must spend nothing
	// until a human enables it — because it makes the keyword list the one part of the tree
	// no human ever has to look at. A keyword left Enabled starts spending the moment the
	// campaign and ad group are enabled, which is the documented next step, so a list this
	// broker assembled from a brief would go live without ever being reviewed as a list.
	//
	// The cost of PAUSED is one bulk-enable in the Bing UI, on keywords the operator should
	// be reading anyway. The cost of ENABLED is unreviewed spend. The service-driven enable
	// is not made harder by this: the toggle path activates keywords alongside the ad group
	// and ad (see UpdateCampaignAndChildrenStatus).
	keywordStatusPaused = "Paused"

	// minCpcBid / maxCpcBid bound the ad-group CpcBid this client will send, in whole units
	// of the ACCOUNT's currency — a plain decimal with NO micros conversion, the same unit
	// rule msCampaign.DailyBudget follows. These are sanity bounds on caller input, not
	// Microsoft's limits: Microsoft applies a currency-dependent minimum server-side.
	minCpcBid = 0.01
	maxCpcBid = 1_000.0
)

// Keyword is a single positive Search keyword. Text and MatchType are both required; see
// validateKeywords for the exact rules.
type Keyword struct {
	Text      string
	MatchType string
}

// msBid is Microsoft's money object for a bid: a bare decimal Amount in the ACCOUNT's
// currency. NO micros conversion (Google Ads uses micros; Microsoft does not).
type msBid struct {
	Amount float64 `json:"Amount"`
}

// msKeyword is one Keyword in the POST /Keywords body. Text and MatchType are Add:Required;
// Status is sent explicitly (see keywordStatusPaused).
//
// Bid is OMITTED (a nil pointer with omitempty). Microsoft documents that a keyword with no
// bid of its own uses the AD GROUP's CpcBid, which createAdGroupAndAd now sets — so one bid
// lives in one place and a human reviewing the group sees a single number rather than N
// invented ones. FinalUrls is likewise omitted: a keyword-level destination would override
// the ad's, and the ad already carries the validated, UTM-stamped registration URL.
type msKeyword struct {
	Text      string `json:"Text"`
	MatchType string `json:"MatchType"`
	Status    string `json:"Status"`
	Bid       *msBid `json:"Bid,omitempty"`
}

// createKeywordsRequest is the POST /Keywords body. The v13 AddKeywords operation carries
// AdGroupId at the TOP level (a sibling to Keywords) — the target ad group is NOT in the
// URL and NOT a field of the Keyword object, matching AddAdGroups/AddAds.
//
// ReturnInheritedBidStrategyTypes is sent as false for the same reason
// createAdGroupsRequest sends it: the docs list it as a request element with no optional
// note ("unless otherwise noted... all request elements are required").
type createKeywordsRequest struct {
	AdGroupId                       json.Number `json:"AdGroupId"`
	Keywords                        []msKeyword `json:"Keywords"`
	ReturnInheritedBidStrategyTypes bool        `json:"ReturnInheritedBidStrategyTypes"`
}

// createKeywordsResponse is the (subset of the) 200 response: an index-aligned id slice +
// a FLAT PartialErrors array, the same shape every other create in this client returns.
// Both arrays are BOUNDED so a malformed 8-MiB body cannot amplify into a huge allocation
// (see boundedNumberIDs / boundedErrorItems).
type createKeywordsResponse struct {
	KeywordIds    boundedKeywordIDs `json:"KeywordIds"`
	PartialErrors boundedErrorItems `json:"PartialErrors"`
}

// validateKeywords trims and validates each caller-supplied keyword, de-duplicating by
// (matchType, case-folded text). Returns (nil, nil) for an empty input — keywords are
// optional at this layer, and whether a campaign may be ACTIVATED without them is the
// dispatcher toggle guard's decision, not this function's.
//
// A bad entry is a HARD error rather than a silent drop. Unlike ad copy, which pads to a
// deterministic placeholder, there is no defensible stand-in for a keyword: substituting
// one would put a search term this broker invented onto a paid campaign in the caller's
// name. Failing loudly, before any create, is the only honest option.
func validateKeywords(keywords []Keyword) ([]Keyword, error) {
	if len(keywords) == 0 {
		return nil, nil
	}
	if len(keywords) > maxKeywords {
		return nil, fmt.Errorf("microsoft-ads: at most %d keywords are supported, got %d", maxKeywords, len(keywords))
	}
	seen := make(map[string]struct{}, len(keywords))
	out := make([]Keyword, 0, len(keywords))
	for _, kw := range keywords {
		text := strings.TrimSpace(kw.Text)
		if text == "" {
			return nil, fmt.Errorf("microsoft-ads: keyword text must not be empty")
		}
		// Measured in RUNES, matching Microsoft's character-based limit and every other length
		// check in this client — a byte count would reject a valid CJK keyword.
		if n := utf8.RuneCountInString(text); n > maxKeywordTextRunes {
			return nil, fmt.Errorf("microsoft-ads: keyword %q is %d characters, exceeding the %d limit",
				truncate(text, maxErrorBodyChars), n, maxKeywordTextRunes)
		}
		// Reject ANY control rune (not just \n/\r): a \t, \v, \f or NUL would otherwise reach
		// POST /Keywords verbatim and be rejected by Microsoft only AFTER the campaign, ad group
		// and ad exist. Checks kw.Text (PRE-trim) so a leading/trailing control char is caught
		// too, exactly as checkAdCopyList does.
		if strings.IndexFunc(kw.Text, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("microsoft-ads: keyword %q contains a control character", truncate(text, maxErrorBodyChars))
		}
		matchType, ok := canonicalMatchType(kw.MatchType)
		if !ok {
			return nil, fmt.Errorf("microsoft-ads: keyword %q has unsupported match type %q (want %s, %s, or %s)",
				truncate(text, maxErrorBodyChars), truncate(kw.MatchType, maxErrorBodyChars),
				MatchTypeExact, MatchTypePhrase, MatchTypeBroad)
		}
		// Microsoft treats keyword text case-insensitively for uniqueness within an ad group, so
		// the dedupe key case-folds the text while the SENT Text keeps its original casing.
		// Without the fold, "Kubernetes" and "kubernetes" both pass here as distinct and the
		// whole create is then rejected upstream as a duplicate.
		key := matchType + "\x00" + strings.ToLower(text)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Keyword{Text: text, MatchType: matchType})
	}
	return out, nil
}

// canonicalMatchType maps a caller's match type to Microsoft's PascalCase enum spelling,
// accepting any casing so a caller sending the google-ads SCREAMING_CASE ("EXACT") is not
// refused over a purely cosmetic difference. An unrecognised value is REFUSED rather than
// defaulted: silently defaulting a typo'd match type would broaden or narrow what a paid
// campaign matches, which is exactly the kind of guess that has to fail loudly.
func canonicalMatchType(in string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(in)) {
	case "exact":
		return MatchTypeExact, true
	case "phrase":
		return MatchTypePhrase, true
	case "broad":
		return MatchTypeBroad, true
	default:
		return "", false
	}
}

// validateCpcBid checks a caller-supplied ad-group bid. A zero value means UNSET and
// returns (0, false, nil): the bid field is then omitted from the ad-group create entirely
// and Microsoft applies its documented behaviour — "if you do not set a bid, it will be set
// to the minimum depending on your account's currency". That floor is a documented,
// serve-capable state, so this client does NOT invent a default bid to avoid it; inventing
// one would put a number nobody chose onto a paid campaign, and the currency-correct minimum
// is a better starting point than any constant this client could hardcode across every
// account currency. A caller who wants a real bid supplies one.
//
// NaN/Inf are rejected explicitly: NaN passes every ordered comparison, so a bare range
// check would let it through into the money field of a paid create. Mirrors the budget
// validation in CreateCampaign.
func validateCpcBid(bid float64) (amount float64, set bool, err error) {
	if bid == 0 {
		return 0, false, nil
	}
	if math.IsNaN(bid) || math.IsInf(bid, 0) {
		return 0, false, fmt.Errorf("microsoft-ads: ad group CpcBid must be a finite number, got %v", bid)
	}
	if bid < minCpcBid {
		return 0, false, fmt.Errorf("microsoft-ads: ad group CpcBid must be at least %.2f, got %.4f", minCpcBid, bid)
	}
	if bid > maxCpcBid {
		return 0, false, fmt.Errorf("microsoft-ads: ad group CpcBid %.2f exceeds the maximum %.0f", bid, maxCpcBid)
	}
	return bid, true, nil
}

// createKeywords attaches every validated keyword to the ad group as a SINGLE AddKeywords
// call, returning the created keyword ids in request order.
//
// Batched into one call rather than one per keyword so the whole set shares ONE outcome: N
// calls would leave a half-targeted ad group on any mid-sequence failure — exactly the
// partially-created state every other step in this client is written to avoid.
//
// NOT RETRIED ON 429. This is a MUTATING create with no idempotency key: Microsoft enforces
// no uniqueness on a keyword the way it does on a campaign or ad-group name, so a retried
// create ADDS a second copy of every keyword rather than reconciling onto the first.
// doRequest's idempotent=false encodes that, matching every other create here.
func (c *Client) createKeywords(ctx context.Context, adGroupID string, keywords []Keyword) ([]string, error) {
	if len(keywords) == 0 {
		return nil, nil
	}
	// A context already done costs nothing to check and saves a mutating request whose
	// outcome would then have to be UNCONFIRMED. Mirrors createAdGroupAndAd's pre-step guards.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("microsoft-ads keyword targeting aborted before any request (context already done; ad group %s has no keywords yet): %w", adGroupID, ctxErr)
	}

	msKeywords := make([]msKeyword, 0, len(keywords))
	for _, kw := range keywords {
		msKeywords = append(msKeywords, msKeyword{
			Text:      kw.Text,
			MatchType: kw.MatchType,
			Status:    keywordStatusPaused,
		})
	}

	body, err := c.doRequest(ctx, http.MethodPost, "Keywords", createKeywordsRequest{
		AdGroupId: json.Number(adGroupID),
		Keywords:  msKeywords,
	}, false)
	if err != nil {
		return nil, err
	}

	var resp createKeywordsResponse
	if uErr := json.Unmarshal(body, &resp); uErr != nil {
		// A 2xx whose body will not parse is UNCONFIRMED, not a clean failure: the keywords may
		// have been created and a blind retry would duplicate them. errNoID is the sentinel the
		// call site maps to UNCONFIRMED.
		return nil, fmt.Errorf("decode KeywordIds response (%v): %w", uErr, errNoID)
	}
	// A PartialError is a definite per-entity rejection, and AddKeywords is a batch: the response
	// is index-aligned with the request, so a rejection of one keyword sits alongside real ids for
	// the ones that succeeded. `[701, null, 703]` with one PartialError means TWO keywords exist
	// upstream, not none.
	//
	// Those ids must be carried out with the error. They are what the status cascade enables on
	// ACTIVATE, and what a reconciliation needs in order to avoid creating a second copy — a blind
	// retry of this batch would duplicate every keyword that did succeed.
	//
	// Gated on partialErrorsHaveAny so a null-only placeholder slice does not count, and
	// classified BEFORE the cardinality check below, because a rejected entry legitimately
	// returns fewer usable ids and must not be reported as the ambiguous short-response case.
	if partialErrorsHaveAny(resp.PartialErrors.Items) {
		created := make([]string, 0, len(resp.KeywordIds))
		for _, raw := range resp.KeywordIds {
			if id := numberID(raw); id != "" {
				created = append(created, id)
			}
		}
		// A DUPLICATE rejection is not a failure — it is Microsoft confirming the keyword is
		// already on the ad group. This client calls no keyword read, so a re-run against a
		// reused ad group cannot know which keywords are already attached and re-posts the whole
		// batch; the ones that exist come back as CampaignServiceDuplicateKeyword (1517) or
		// CampaignServiceKeywordAndMatchTypeCombinationAlreadyExists (1542), matched on the
		// NORMALIZED form (case, whitespace, accents and punctuation are folded). Treating
		// that as a rejection would report "keyword targeting rejected" for an ad group whose
		// targeting is exactly what was asked for.
		//
		// The ids of the pre-existing keywords are NOT recoverable by this client — a duplicate
		// entry gets a null id slot and this client calls no keyword read — so this reports the
		// duplicates rather than inventing ids for them. errDuplicateKeywords carries that
		// distinction to the caller, which decides what the tree's state means.
		//
		// The duplicate classification is gated on the error array being COMPLETE. It is
		// ALL-not-ANY precisely so a genuine rejection travelling alongside a duplicate keeps
		// the batch on the failure path, and that guarantee is only as good as the set the
		// predicate can see: boundedErrorItems retains maxDecodedErrorItems (16) entries while
		// this call sends up to maxKeywords (60), so a rejection past the cap was DISCARDED
		// during decode and the surviving prefix read as all-duplicate. Absence of a rejection
		// in a truncated array is not evidence there was none, so a truncated array is not
		// classifiable as duplicate-only and falls through to the rejection path below —
		// refusing beats a false success.
		if !resp.PartialErrors.Truncated && isDuplicateKeywordPartial(resp.PartialErrors.Items) {
			return created, fmt.Errorf("%w (%d of %d keywords were created; the rest already existed): %s",
				errDuplicateKeywords, len(created), len(msKeywords), partialErrorCodes(resp.PartialErrors.Items))
		}
		if len(created) == 0 {
			// Every entry was rejected: a clean failure, nothing to reconcile.
			return nil, fmt.Errorf("%w: %s", errPartialFailure, partialErrorCodes(resp.PartialErrors.Items))
		}
		return created, fmt.Errorf("%w (%d of %d keywords were created): %s",
			errPartialFailure, len(created), len(msKeywords), partialErrorCodes(resp.PartialErrors.Items))
	}

	// The id array is index-aligned with the request. A SHORT array is not a partial success to
	// be papered over: it means the response does not describe what was sent, so which keywords
	// exist upstream is unknown → UNCONFIRMED.
	//
	// FULL cardinality is required: one id per keyword sent, no lowering.
	//
	// This previously bounded `want` at maxDecodedErrorItems (16) because KeywordIds decoded
	// through boundedNumberIDs, whose retention limit is sized for a campaign create (one id) and
	// for error arrays. A 60-keyword response therefore decoded short BY DESIGN, the check
	// lowered its own expectation to match, and only 16 ids were persisted — so ACTIVATE enabled
	// 16 keywords and left the other 44 Paused on a campaign the operator believes is fully live.
	//
	// KeywordIds now decodes through boundedKeywordIDs, bounded by maxKeywords, so a valid
	// response is never short and a short one is a real defect worth refusing.
	want := len(msKeywords)
	if len(resp.KeywordIds) < want {
		return nil, fmt.Errorf("microsoft-ads keyword targeting returned %d ids for %d keywords (ad group %s): %w",
			len(resp.KeywordIds), len(msKeywords), adGroupID, errNoID)
	}

	ids := make([]string, 0, want)
	for i := 0; i < want; i++ {
		id := numberID(resp.KeywordIds[i])
		if id == "" {
			// A null/malformed id slot with NO PartialError explaining it is the malformed-200
			// case: the keyword may exist but cannot be identified → UNCONFIRMED.
			return nil, fmt.Errorf("microsoft-ads keyword targeting returned no usable id at index %d (ad group %s): %w", i, adGroupID, errNoID)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
