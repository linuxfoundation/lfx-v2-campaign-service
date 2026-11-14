// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Multiple ad groups at create (#563, gap 12): microsoftConfig.adGroups, up to MaxAdGroups ad
// groups under one campaign, each with its own name suffix, keywords and responsive search ad
// copy. The per-group negatives and remarketing lists are attached by the dispatcher after the
// tree exists, as the single-group ones are.
//
// THE SINGLE-GROUP PATH IS UNCHANGED: with no AdGroups, CreateCampaign calls createAdGroupAndAd
// exactly as before, composes the same ad group name and returns the same result (AdGroups is
// omitted from its JSON).
//
// PARTIAL RULES (owner decision on multi-ad creates: degraded success, no rollback):
//   - the FIRST group is today's ad group: its failure is today's failure (an error with the
//     campaign partial, UNCONFIRMED or rejected), because no ad group exists yet;
//   - a LATER group that fails leaves the earlier ones in place: the create succeeds, the group
//     is recorded with Status AdGroupStatusFailed and its error, and the dispatcher marks the
//     campaign created_degraded with a step naming every failed group. Remaining groups are still
//     attempted unless the context has ended (then they are AdGroupStatusNotAttempted).
//
// The result's top-level AdGroupID / AdID / KeywordIDs are the FIRST group's, so every reader
// written for one ad group keeps working on it; the dispatcher's toggle, bid and keyword levers
// read AdGroups for the others.
// ---------------------------------------------------------------------------

const (
	// FieldAdGroups is the microsoftConfig key, named in refusals.
	FieldAdGroups = "adGroups"
	// MaxAdGroups bounds one create. Each group is up to four sequential writes (ad group, ad,
	// keywords, then the dispatcher's extras) inside the dispatch budget.
	MaxAdGroups = 5
	// MaxAdGroupNameSuffixRunes bounds a group's name suffix; the composed name is also held to
	// Microsoft's 256-character ad group name limit.
	MaxAdGroupNameSuffixRunes = 40

	// AdGroupStatusCreated / Failed / NotAttempted are AdGroupResult.Status values.
	AdGroupStatusCreated      = "created"
	AdGroupStatusFailed       = "failed"
	AdGroupStatusNotAttempted = "not_attempted"
)

// AdGroupInput is one requested ad group. Keywords and copy follow the single-group rules
// (validateKeywords, validateAdCopy, validatePaths); empty copy is auto-composed from the event.
type AdGroupInput struct {
	NameSuffix   string
	Keywords     []Keyword
	Headlines    []string
	Descriptions []string
	Path1        string
	Path2        string
}

// AdGroupResult is one ad group's outcome, in request order. The negatives and remarketing
// lists are filled in by the dispatcher after the create.
type AdGroupResult struct {
	NameSuffix    string   `json:"nameSuffix"`
	AdGroupName   string   `json:"adGroupName,omitempty"`
	AdGroupID     string   `json:"adGroupId,omitempty"`
	AdGroupReused bool     `json:"adGroupReused,omitempty"`
	AdID          string   `json:"adId,omitempty"`
	KeywordIDs    []string `json:"keywordIds,omitempty"`
	Status        string   `json:"status"`
	// Error is the group's failure, service-authored text, when Status is not created.
	Error            string                   `json:"error,omitempty"`
	NegativeKeywords []NegativeKeywordOutcome `json:"negativeKeywords,omitempty"`
	Audiences        []AudienceOutcome        `json:"audiences,omitempty"`
}

// FailedAdGroups names, in request order, every requested ad group that was not created
// ("entry N (suffix)"). Empty on the single-group path.
func (r *CampaignResult) FailedAdGroups() []string {
	var out []string
	for i, g := range r.AdGroups {
		if g.Status != AdGroupStatusCreated {
			out = append(out, fmt.Sprintf("entry %d (%s)", i+1, g.NameSuffix))
		}
	}
	return out
}

// AdGroupError is a refusal of one adGroups entry; Entry is 1-based.
type AdGroupError struct {
	Entry int
	Field string
	Msg   string
}

func (e *AdGroupError) Error() string {
	if e.Entry == 0 {
		return FieldAdGroups + ": " + e.Msg
	}
	return fmt.Sprintf("%s entry %d: %s: %s", FieldAdGroups, e.Entry, e.Field, e.Msg)
}

// ValidateAdGroupNameSuffixes checks the suffixes alone, without contacting Microsoft: each
// trimmed of nothing (sent as given), 1-MaxAdGroupNameSuffixRunes characters, at least one letter
// or digit, no control character and no "|" (the name delimiter), and no two equal ignoring case
// (Microsoft's ad group names are unique case-insensitively within a campaign).
func ValidateAdGroupNameSuffixes(suffixes []string) error {
	seen := make(map[string]int, len(suffixes))
	for i, s := range suffixes {
		entry := i + 1
		switch {
		case strings.TrimSpace(s) != s || s == "":
			return &AdGroupError{Entry: entry, Field: "nameSuffix", Msg: "is required, without leading or trailing spaces"}
		case utf8.RuneCountInString(s) > MaxAdGroupNameSuffixRunes:
			return &AdGroupError{Entry: entry, Field: "nameSuffix", Msg: fmt.Sprintf("is %d characters; at most %d", utf8.RuneCountInString(s), MaxAdGroupNameSuffixRunes)}
		case strings.IndexFunc(s, unicode.IsControl) >= 0 || strings.Contains(s, "|"):
			return &AdGroupError{Entry: entry, Field: "nameSuffix", Msg: "must not contain a control character or \"|\""}
		case !hasWord(s):
			return &AdGroupError{Entry: entry, Field: "nameSuffix", Msg: "must contain a letter or digit"}
		}
		key := strings.ToLower(s)
		if first, dup := seen[key]; dup {
			return &AdGroupError{Entry: entry, Field: "nameSuffix", Msg: fmt.Sprintf("repeats entry %d's suffix (ad group names are unique ignoring case)", first)}
		}
		seen[key] = entry
	}
	return nil
}

// ValidateKeywords is validateKeywords for a caller outside the client (the dispatcher's
// synchronous check of adGroups[].keywords), so a create and the 400 cannot disagree.
func ValidateKeywords(keywords []Keyword) error {
	_, err := validateKeywords(keywords)
	return err
}

// validatedAdGroup is one AdGroupInput after CreateCampaign's up-front checks.
type validatedAdGroup struct {
	in       AdGroupInput
	keywords []Keyword
}

// validateAdGroups checks every requested group before anything is created: the suffixes, each
// composed name against Microsoft's limit, each group's keywords (at least one: an extra ad group
// with nothing to match is inert) and its copy and paths. authority is the display domain the
// paths extend. Pure; a refusal names the entry.
func validateAdGroups(in CampaignInput, authority string) ([]validatedAdGroup, error) {
	if len(in.AdGroups) == 0 {
		return nil, nil
	}
	if len(in.AdGroups) > MaxAdGroups {
		return nil, &AdGroupError{Msg: fmt.Sprintf("holds %d ad groups; at most %d", len(in.AdGroups), MaxAdGroups)}
	}
	suffixes := make([]string, len(in.AdGroups))
	for i, g := range in.AdGroups {
		suffixes[i] = g.NameSuffix
	}
	if err := ValidateAdGroupNameSuffixes(suffixes); err != nil {
		return nil, err
	}
	out := make([]validatedAdGroup, len(in.AdGroups))
	for i, g := range in.AdGroups {
		entry := i + 1
		gin := in
		gin.adGroupNameSuffix = g.NameSuffix
		name := composeAdGroupName(gin)
		if err := validateEntityName("ad group", name, utf8.RuneCountInString(name), maxAdGroupNameRunes, "characters"); err != nil {
			return nil, &AdGroupError{Entry: entry, Field: "nameSuffix", Msg: err.Error()}
		}
		if len(g.Keywords) == 0 {
			return nil, &AdGroupError{Entry: entry, Field: "keywords", Msg: "at least one keyword is required; an ad group with none can never serve"}
		}
		kws, err := validateKeywords(g.Keywords)
		if err != nil {
			return nil, &AdGroupError{Entry: entry, Field: "keywords", Msg: strings.TrimPrefix(err.Error(), "microsoft-ads: ")}
		}
		if err := ValidateCallerAdCopy(g.Headlines, g.Descriptions, g.Path1, g.Path2); err != nil {
			return nil, &AdGroupError{Entry: entry, Field: "ad copy", Msg: err.Error()}
		}
		p1, p2, _ := validatePaths(g.Path1, g.Path2)
		if p1 != "" {
			shown := displayURL(authority, p1, p2)
			limit := maxDisplayDomainRunes
			if hasDoubleWidth(shown) {
				limit = maxDisplayDomainRunesWide
			}
			if n := utf8.RuneCountInString(shown); n > limit {
				return nil, &AdGroupError{Entry: entry, Field: "path1", Msg: fmt.Sprintf("the display URL (domain plus path1/path2) is %d characters, exceeding the %d limit; shorten path1/path2", n, limit)}
			}
		}
		v := g
		v.Path1, v.Path2 = p1, p2
		out[i] = validatedAdGroup{in: v, keywords: kws}
	}
	return out, nil
}

// createAdGroups builds every requested ad group under the confirmed campaign, in request order,
// under the partial rules at the top of this file. tgt carries the campaign-wide ad group
// settings (bid, flight, language, network); each group gets its own keywords.
func (c *Client) createAdGroups(
	ctx context.Context,
	in CampaignInput,
	groups []validatedAdGroup,
	campaignID string,
	alreadyExisted bool,
	steps *[]string,
	campaignPartial func() *CampaignResult,
	tgt targeting,
) (*CampaignResult, error) {
	results := make([]AdGroupResult, len(groups))
	for i, g := range groups {
		results[i] = AdGroupResult{NameSuffix: g.in.NameSuffix, Status: AdGroupStatusNotAttempted}
	}
	var first *CampaignResult
	bidApplied, flightApplied, untouched := true, true, true
	for i, g := range groups {
		if i > 0 && ctx.Err() != nil {
			*steps = append(*steps, fmt.Sprintf("Ad group %d of %d (%s) NOT attempted: the context ended first", i+1, len(groups), g.in.NameSuffix))
			bidApplied, flightApplied, untouched = false, false, false
			continue
		}
		*steps = append(*steps, fmt.Sprintf("Ad group %d of %d (%s):", i+1, len(groups), g.in.NameSuffix))
		gin := in
		gin.adGroupNameSuffix = g.in.NameSuffix
		gin.Keywords = g.in.Keywords
		gin.Headlines, gin.Descriptions = g.in.Headlines, g.in.Descriptions
		gin.Path1, gin.Path2 = g.in.Path1, g.in.Path2
		gt := tgt
		gt.keywords = g.keywords
		r, err := c.createAdGroupAndAd(ctx, gin, campaignID, alreadyExisted, steps, campaignPartial, gt)
		res := &results[i]
		if r != nil {
			res.AdGroupName, res.AdGroupID, res.AdGroupReused, res.AdID, res.KeywordIDs = r.AdGroupName, r.AdGroupID, r.AdGroupReused, r.AdID, r.KeywordIDs
		}
		if err != nil {
			res.Status, res.Error = AdGroupStatusFailed, err.Error()
			if i == 0 {
				// No ad group exists yet: today's failure, with the group recorded.
				if r == nil {
					r = campaignPartial()
				}
				r.AdGroups = results
				r.Steps = *steps
				return r, err
			}
			*steps = append(*steps, fmt.Sprintf("Ad group %d of %d (%s) NOT completed (the earlier ad groups stand): %v", i+1, len(groups), g.in.NameSuffix, err))
			bidApplied, flightApplied, untouched = false, false, false
			continue
		}
		res.Status = AdGroupStatusCreated
		bidApplied = bidApplied && r.BidApplied
		flightApplied = flightApplied && r.FlightApplied
		untouched = untouched && r.AlreadyExisted
		if i == 0 {
			first = r
		}
	}
	out := first
	out.AdGroups = results
	out.BidApplied = bidApplied
	out.FlightApplied = flightApplied
	out.AlreadyExisted = untouched
	out.Steps = *steps
	return out, nil
}

// UpdateAdGroupChildrenStatus sets ONE further ad group of a multi-ad-group campaign (#563 gap
// 12), its ad and its keywords to status, WITHOUT touching the campaign: the dispatcher's toggle
// calls it for every ad group after the first, before the campaign gate opens on ACTIVATE and
// after it closes on PAUSE, so the gate ordering of UpdateCampaignAndChildrenStatus still holds.
// Every id is checked before any call. The first call's error is returned as classified; once an
// entity has changed, a later failure is a partialCascadeError (Unconfirmed).
func (c *Client) UpdateAdGroupChildrenStatus(ctx context.Context, campaignID, adGroupID, adID string, keywordIDs []string, status string) error {
	if status != StatusActive && status != StatusPaused {
		return fmt.Errorf("microsoft-ads: unsupported status %q (want %s or %s)", status, StatusActive, StatusPaused)
	}
	cID, agID, aID := strings.TrimSpace(campaignID), strings.TrimSpace(adGroupID), strings.TrimSpace(adID)
	for _, id := range []struct{ label, val string }{{"campaign", cID}, {"ad group", agID}, {"ad", aID}} {
		if !idRE.MatchString(id.val) {
			return fmt.Errorf("microsoft-ads: %s id %q is not a numeric id", id.label, id.val)
		}
	}
	kws := make([]msKeywordStatus, 0, len(keywordIDs))
	for _, k := range keywordIDs {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if !idRE.MatchString(k) {
			return fmt.Errorf("microsoft-ads: keyword id %q is not a numeric id", k)
		}
		kws = append(kws, msKeywordStatus{Id: json.Number(k), Status: status})
	}
	if err := c.putStatus(ctx, "AdGroups", updateAdGroupsRequest{
		CampaignId: json.Number(cID),
		AdGroups:   []msAdGroupStatus{{Id: json.Number(agID), Status: status}},
	}, "ad group"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return &partialCascadeError{applied: "ad group " + agID, stage: "ad", err: err}
	}
	if err := c.putStatus(ctx, "Ads", updateAdsRequest{
		AdGroupId: json.Number(agID),
		Ads:       []msAdStatus{{Id: json.Number(aID), Status: status}},
	}, "ad"); err != nil {
		return &partialCascadeError{applied: "ad group " + agID, stage: "ad", err: err}
	}
	if len(kws) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return &partialCascadeError{applied: "ad group " + agID + " and ad", stage: "keywords", err: err}
	}
	if err := c.putStatus(ctx, "Keywords", updateKeywordsRequest{AdGroupId: json.Number(agID), Keywords: kws}, "keywords"); err != nil {
		return &partialCascadeError{applied: "ad group " + agID + " and ad", stage: "keywords", err: err}
	}
	return nil
}
