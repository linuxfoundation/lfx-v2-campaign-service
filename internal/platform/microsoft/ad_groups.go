// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/redact"
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
	Error string `json:"error,omitempty"`
	// Unconfirmed marks a failed group whose ad group create (or a later step) was ambiguous
	// (timeout, 5xx, unreadable success) while no ad group id was confirmed: an ad group may
	// exist upstream that this row cannot address. The levers that must reach every group (the
	// bid write) refuse until it is reconciled.
	Unconfirmed      bool                     `json:"unconfirmed,omitempty"`
	NegativeKeywords []NegativeKeywordOutcome `json:"negativeKeywords,omitempty"`
	Audiences        []AudienceOutcome        `json:"audiences,omitempty"`
}

// FailedAdGroups names, in request order, every requested ad group that was not created and
// does NOT exist upstream as an ad group of its own ("entry N (suffix)"): no ad group id was
// confirmed, or the one recorded is an earlier entry's (it resolved into that group). A failed
// entry whose own ad group Microsoft confirmed is IncompleteAdGroups', not this. Empty on the
// single-group path.
func (r *CampaignResult) FailedAdGroups() []string {
	var out []string
	for i, g := range r.AdGroups {
		if g.Status != AdGroupStatusCreated && !r.ownAdGroupConfirmed(i) {
			out = append(out, fmt.Sprintf("entry %d (%s)", i+1, g.NameSuffix))
		}
	}
	return out
}

// IncompleteAdGroups names, in request order, every failed entry whose OWN ad group Microsoft
// confirmed before a later step (the ad, or the keywords) failed: the ad group exists upstream,
// so the operator must finish it there, not create it. Each names the ad group id and the step
// to finish ("entry N (suffix): ad group ID exists but its ad was not created"). The create runs
// ad group, then ad, then keywords, so a recorded ad id means the keyword step failed.
func (r *CampaignResult) IncompleteAdGroups() []string {
	var out []string
	for i, g := range r.AdGroups {
		if g.Status == AdGroupStatusCreated || !r.ownAdGroupConfirmed(i) {
			continue
		}
		missing := "its ad (and so its keywords) was not created"
		if g.AdID != "" {
			missing = "its keywords were not all created"
		}
		out = append(out, fmt.Sprintf("entry %d (%s): ad group %s exists but %s", i+1, g.NameSuffix, g.AdGroupID, missing))
	}
	return out
}

// ownAdGroupConfirmed reports whether entry i records an ad group id no earlier entry recorded,
// i.e. Microsoft confirmed an ad group that belongs to this entry alone.
func (r *CampaignResult) ownAdGroupConfirmed(i int) bool {
	id := strings.TrimSpace(r.AdGroups[i].AdGroupID)
	if id == "" {
		return false
	}
	for j := 0; j < i; j++ {
		if strings.TrimSpace(r.AdGroups[j].AdGroupID) == id {
			return false
		}
	}
	return true
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
// or digit, no control character and no "|" (the name delimiter), no URL or credentials (nothing
// redact.SnapshotText would change and no "scheme://" or "www." host, urlShapedSuffixRE, since
// the suffix is stored and logged as given), and no two that compose the
// same ad group name: compared after sanitizeNamePart (the normalization the composed name gets,
// which collapses whitespace runs and Unicode spaces) and with strings.EqualFold, the comparison
// lookupNamedEntity finds an existing ad group by (Microsoft's ad group names are unique
// case-insensitively within a campaign). Two suffixes differing only there would otherwise send
// the second group into the first.
func ValidateAdGroupNameSuffixes(suffixes []string) error {
	type seenKey struct {
		key   string
		entry int
	}
	seen := make([]seenKey, 0, len(suffixes))
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
		case redact.SnapshotText(s) != s || urlShapedSuffixRE.MatchString(s):
			// The suffix is persisted (the result, the steps and the composed ad group name) and
			// logged verbatim, so one the snapshot redactor would change, such as a URL or
			// credentials, is refused rather than stored. The redactor keeps scheme + host, so
			// it leaves "https://x.co" unchanged; urlShapedSuffixRE refuses that shape too.
			return &AdGroupError{Entry: entry, Field: "nameSuffix", Msg: "must not contain a URL or credentials; a suffix is a label"}
		}
		key := sanitizeNamePart(s)
		for _, prev := range seen {
			if strings.EqualFold(prev.key, key) {
				return &AdGroupError{Entry: entry, Field: "nameSuffix", Msg: fmt.Sprintf("repeats entry %d's suffix (ad group names are unique ignoring case and spacing)", prev.entry)}
			}
		}
		seen = append(seen, seenKey{key: key, entry: entry})
	}
	return nil
}

// urlShapedSuffixRE matches a suffix that holds a URL redact.SnapshotText leaves as written: any
// "scheme://" (an origin-only "https://x.co" is unchanged by the redactor) or a "www." host. A
// bare host alone ("example.com") is not matched: it cannot be told apart from a label such as
// "Node.js", and one with a path, query or credentials is already changed by SnapshotText.
var urlShapedSuffixRE = regexp.MustCompile(`(?i)://|\bwww\.`)

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
			if n, limit, over := displayURLOverflow(authority, p1, p2); over {
				return nil, &AdGroupError{Entry: entry, Field: "path1", Msg: fmt.Sprintf("the display URL (domain plus path1/path2) is %d characters, exceeding the %d limit; shorten path1/path2", n, limit)}
			}
		}
		v := g
		v.Path1, v.Path2 = p1, p2
		out[i] = validatedAdGroup{in: v, keywords: kws}
	}
	return out, nil
}

// createAdGroupStep is the per-group create createAdGroups calls; a variable only so a test can
// stand in a step that returns no result.
var createAdGroupStep = (*Client).createAdGroupAndAd

// SiblingAdGroupName recomposes the name createAdGroups gave the ad group with suffix from a
// sibling group of the same create: its recorded name and suffix. Every group's name is the
// shared prefix (project and event) plus its own sanitized suffix last, so the sibling's suffix is
// swapped for this one. For a row recorded before every group's name was (a later group whose
// step returned no result recorded none). ok is false when the sibling's name does not end in its
// suffix or the result is not a valid ad group name.
func SiblingAdGroupName(siblingName, siblingSuffix, suffix string) (string, bool) {
	tail := " | " + sanitizeNamePart(siblingSuffix)
	s := sanitizeNamePart(suffix)
	if sanitizeNamePart(siblingSuffix) == "" || s == "" || !strings.HasSuffix(siblingName, tail) {
		return "", false
	}
	name := strings.TrimSuffix(siblingName, tail) + " | " + s
	if validateEntityName("ad group", name, utf8.RuneCountInString(name), maxAdGroupNameRunes, "characters") != nil {
		return "", false
	}
	return name, true
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
	// Every group's ad group name is composed up front, exactly as createAdGroupAndAd composes it
	// for the create, and recorded whatever the step returns: the bid write reconciles a later
	// group left UNCONFIRMED by looking it up by this name (FindAdGroupByName refuses an empty
	// one), so a step that returned no result, or one without the name, must not leave it blank.
	results := make([]AdGroupResult, len(groups))
	for i, g := range groups {
		gin := in
		gin.adGroupNameSuffix = g.in.NameSuffix
		results[i] = AdGroupResult{NameSuffix: g.in.NameSuffix, AdGroupName: composeAdGroupName(gin), Status: AdGroupStatusNotAttempted}
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
		r, err := createAdGroupStep(c, ctx, gin, campaignID, alreadyExisted, steps, campaignPartial, gt)
		res := &results[i]
		if r != nil {
			res.AdGroupID, res.AdGroupReused, res.AdID, res.KeywordIDs = r.AdGroupID, r.AdGroupReused, r.AdID, r.KeywordIDs
			if r.AdGroupName != "" {
				res.AdGroupName = r.AdGroupName
			}
		}
		if err == nil && i > 0 {
			// Defence in depth behind ValidateAdGroupNameSuffixes: a later group that resolved to an
			// earlier group's ad group was built into it, so it is not a group of its own and must
			// not be recorded as created (the levers would address that ad group twice).
			for j := 0; j < i; j++ {
				if results[j].AdGroupID != "" && results[j].AdGroupID == res.AdGroupID {
					err = fmt.Errorf("microsoft-ads: ad group %d resolved to ad group %d's ad group %s; the suffixes compose the same name", i+1, j+1, clipID(res.AdGroupID))
					break
				}
			}
		}
		if err != nil {
			res.Status, res.Error = AdGroupStatusFailed, err.Error()
			res.Unconfirmed = res.AdGroupID == "" && IsOutcomeUnconfirmed(err)
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
