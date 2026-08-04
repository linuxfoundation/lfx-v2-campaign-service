// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Ad group + responsive search ad creation (GA-3): adGroups:mutate ->
// adGroupAds:mutate. This makes the campaign->ad group->ad hierarchy real,
// matching the shape of the reddit and microsoft adapters. Keyword/audience
// targeting on the resulting ad group is GA-4 (see targeting.go) — this file
// calls into it after the ad is created.
// ---------------------------------------------------------------------------

const (
	// adGroupTypeSearchStandard is the only ad group type this client creates
	// today — the standard type for a Search-network ad group.
	adGroupTypeSearchStandard = "SEARCH_STANDARD"

	// maxAdGroupNameBytes mirrors the budget's limit: AdGroup.name is 1..255
	// UTF-8 bytes (trimmed), same unit/limit as CampaignBudget.name.
	maxAdGroupNameBytes = 255

	// errCodeDuplicateAdGroupName is Google's AdGroupError code when an ad group
	// name already exists within the campaign — the ad-group analogue of
	// errCodeDuplicateBudgetName/errCodeDuplicateCampaignName. A retry with the
	// same deterministic name (NameSuffix) hits this instead of double-creating.
	errCodeDuplicateAdGroupName = "DUPLICATE_ADGROUP_NAME"

	// Responsive Search Ad content bounds (Google Ads v23 System Limits). Unlike
	// Microsoft, Google does NOT halve these for double-width (CJK/emoji) text —
	// the limits are plain rune counts.
	minHeadlines        = 3
	maxHeadlines        = 15
	maxHeadlineRunes    = 30
	minDescriptions     = 2
	maxDescriptions     = 4
	maxDescriptionRunes = 90
)

// adGroupCreate is the create payload for adGroups:mutate.
type adGroupCreate struct {
	Name     string `json:"name"`
	Campaign string `json:"campaign"`
	Status   string `json:"status"`
	Type     string `json:"type"`
}

// adGroupStatusUpdate is the update payload for adGroups:mutate (status-only toggle,
// mirrors campaignStatusUpdate).
type adGroupStatusUpdate struct {
	ResourceName string `json:"resourceName"`
	Status       string `json:"status"`
}

// adTextAsset is one headline/description entry in a responsiveSearchAd.
type adTextAsset struct {
	Text string `json:"text"`
}

// responsiveSearchAd is the ad-type payload for an Ad create.
type responsiveSearchAd struct {
	Headlines    []adTextAsset `json:"headlines"`
	Descriptions []adTextAsset `json:"descriptions"`
}

// adCreate is the "ad" object nested in an adGroupAd create.
type adCreate struct {
	FinalUrls          []string            `json:"finalUrls"`
	ResponsiveSearchAd *responsiveSearchAd `json:"responsiveSearchAd,omitempty"`
}

// adGroupAdCreate is the create payload for adGroupAds:mutate.
type adGroupAdCreate struct {
	AdGroup string   `json:"adGroup"`
	Status  string   `json:"status"`
	Ad      adCreate `json:"ad"`
}

// adGroupAdStatusUpdate is the update payload for adGroupAds:mutate (status-only
// toggle, mirrors campaignStatusUpdate).
type adGroupAdStatusUpdate struct {
	ResourceName string `json:"resourceName"`
	Status       string `json:"status"`
}

// isDuplicateAdGroupNameErr reports whether err is Google's AdGroupError
// DUPLICATE_ADGROUP_NAME rejection on a definite 4xx (excluding 429), mirroring
// isDuplicateBudgetNameErr/isDuplicateCampaignNameErr.
func isDuplicateAdGroupNameErr(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) &&
		isDefiniteClientError(ae) &&
		ae.hasErrorCode(errCodeDuplicateAdGroupName)
}

// adGroupAdID splits an adGroupAd resourceName into its ad group id and ad id.
// Unlike most other Google Ads resources, AdGroupAd uses a COMPOSITE trailing
// segment "{adGroupId}~{adId}" (e.g. "customers/1/adGroupAds/111~222") rather
// than a single numeric id. AdGroupCriterion (GA-4, targeting.go) shares this
// same composite shape, so the split logic lives in compositeResourceID.
func adGroupAdID(resourceName string) (adGroupID, adID string) {
	return compositeResourceID(resourceName)
}

// compositeResourceID splits a resourceName's trailing "{parentId}~{id}"
// segment — the shape AdGroupAd and AdGroupCriterion resource names use,
// unlike every single-id resource this package otherwise handles via
// resourceID. Returns ("", "") if the resource name is empty or the trailing
// segment isn't in that shape.
func compositeResourceID(resourceName string) (parentID, id string) {
	trailing := resourceID(resourceName)
	if trailing == "" {
		return "", ""
	}
	parts := strings.SplitN(trailing, "~", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ""
	}
	return parts[0], parts[1]
}

// createAdGroupAndAd extends a just-created campaign with a PAUSED ad group and a
// PAUSED responsive search ad. Both are created with a single mutate call each
// (no idempotency key on either), so the same ambiguous/duplicate classification
// used for the budget/campaign applies. Unlike the budget/campaign duplicate
// branches, a DUPLICATE_ADGROUP_NAME on retry does NOT look up the existing ad
// group's id — it is reported the same way the budget/campaign duplicates are,
// "already exists, reconcile by name" — so a retry after an ambiguous or
// duplicate ad-group outcome does not re-attempt the ad create either (there is
// no id to attach it to). That mirrors this package's existing choice to prefer
// simple create-then-catch over find-then-create for named resources; a
// campaign left in that state needs manual reconciliation, same as a
// duplicate-budget or duplicate-campaign orphan today.
//
// res is mutated in place (AdGroupName/AdGroupID/AdID/Steps) so the caller's
// existing partial-result plumbing (campaignNamePartial-derived) carries
// whatever was created even when this returns an error.
func (c *Client) createAdGroupAndAd(ctx context.Context, in CampaignInput, campaignResource, campaignID string, res *CampaignResult) error {
	// Validate the destination URL and ad copy BEFORE any ad-group mutate: a bad
	// input here must not leave an orphaned ad group with no ad, mirroring the
	// budget/campaign name validation that runs before their own mutates.
	finalURL, err := buildAdFinalURL(in.RegistrationURL, in.EventSlug, in.EventName, in.Project, in.NameSuffix)
	if err != nil {
		return fmt.Errorf("google-ads ad group/ad creation aborted before any request (invalid destination URL): %w", err)
	}
	headlines, descriptions, err := composeAdCopy(in.Headlines, in.Descriptions, in.EventName, in.Project)
	if err != nil {
		return fmt.Errorf("google-ads ad group/ad creation aborted before any request (invalid ad copy): %w", err)
	}

	adGroupName := composeName("Ad Group", in)
	if err := validateEntityName("ad group", adGroupName, len(adGroupName), maxAdGroupNameBytes, "UTF-8 bytes"); err != nil {
		return err
	}
	// Validate GA-4 targeting input up front too, alongside the URL/copy/name
	// checks above: a bad keyword or audience-segment value must not leave a
	// created ad group + ad with a failed targeting step the caller has to
	// puzzle out separately.
	keywords, err := validateKeywords(in.Keywords)
	if err != nil {
		return fmt.Errorf("google-ads ad group/ad creation aborted before any request (invalid keyword input): %w", err)
	}
	audienceSegments, err := validateAudienceSegments(in.AudienceSegments)
	if err != nil {
		return fmt.Errorf("google-ads ad group/ad creation aborted before any request (invalid audience segment input): %w", err)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("google-ads ad group creation aborted before any request (context already done): %w", ctxErr)
	}

	adGroupReq := mutateRequest{Operations: []mutateOperation{{Create: adGroupCreate{
		Name:     adGroupName,
		Campaign: campaignResource,
		Status:   StatusPaused,
		Type:     adGroupTypeSearchStandard,
	}}}}
	adGroupResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroups:mutate"), adGroupReq, false)
	if err != nil {
		switch {
		case isDuplicateAdGroupNameErr(err):
			return fmt.Errorf("google-ads ad group %q already exists (DUPLICATE_ADGROUP_NAME) — a prior attempt likely created it; verify in Google Ads before retrying: %w", adGroupName, err)
		case createOutcomeAmbiguous(err):
			return fmt.Errorf("google-ads ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", adGroupName, err)
		default:
			return fmt.Errorf("google-ads ad group creation failed (campaign %s created): %w", campaignID, err)
		}
	}
	adGroupResource, adGroupID, err := firstResourceName(adGroupResp)
	if err != nil {
		return fmt.Errorf("google-ads ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", adGroupName, err)
	}
	res.AdGroupName = adGroupName
	res.AdGroupID = adGroupID
	res.Steps = append(res.Steps, fmt.Sprintf("Ad group created: %s (PAUSED, %s)", adGroupID, adGroupTypeSearchStandard))

	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("google-ads ad creation aborted after ad group %s created (context done before ad create; the ad group has no ad yet): %w", adGroupID, ctxErr)
	}

	adReq := mutateRequest{Operations: []mutateOperation{{Create: adGroupAdCreate{
		AdGroup: adGroupResource,
		Status:  StatusPaused,
		Ad: adCreate{
			FinalUrls: []string{finalURL},
			ResponsiveSearchAd: &responsiveSearchAd{
				Headlines:    textAssets(headlines),
				Descriptions: textAssets(descriptions),
			},
		},
	}}}}
	adResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupAds:mutate"), adReq, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; ad may exist — verify in Google Ads before retrying): %w", adGroupID, err)
		}
		// Ads carry no unique name/duplicate-error code (Google allows duplicate ad
		// content within an ad group), so a definite 4xx here is a straightforward
		// rejection, not a possible prior-attempt collision.
		return fmt.Errorf("google-ads ad creation failed (ad group %s created): %w", adGroupID, err)
	}
	adResource, _, err := firstResourceName(adResp)
	if err != nil {
		return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; 2xx with no/malformed resource name — an ad may exist — verify in Google Ads before retrying): %w", adGroupID, err)
	}
	_, adID := adGroupAdID(adResource)
	if adID == "" {
		return fmt.Errorf("google-ads ad creation UNCONFIRMED (ad group %s created; malformed adGroupAd resource name %q — verify in Google Ads before retrying)", adGroupID, adResource)
	}
	res.AdID = adID
	res.Steps = append(res.Steps, fmt.Sprintf("Responsive search ad created: %s (PAUSED, %d headlines, %d descriptions)", adID, len(headlines), len(descriptions)))

	if len(keywords) == 0 && len(audienceSegments) == 0 {
		return nil
	}
	keywordIDs, audienceIDs, err := c.createAdGroupTargeting(ctx, adGroupResource, adGroupID, keywords, audienceSegments)
	if err != nil {
		return err
	}
	res.KeywordCriteriaIDs = keywordIDs
	res.AudienceCriteriaIDs = audienceIDs
	res.Steps = append(res.Steps, fmt.Sprintf("Keyword/audience targeting attached: %d keyword(s), %d audience segment(s)", len(keywordIDs), len(audienceIDs)))
	return nil
}

// textAssets wraps plain strings as the {"text": ...} shape RSA headlines/
// descriptions require.
func textAssets(ss []string) []adTextAsset {
	out := make([]adTextAsset, len(ss))
	for i, s := range ss {
		out[i] = adTextAsset{Text: s}
	}
	return out
}

// composeAdCopy resolves the caller-supplied headlines/descriptions (if any)
// into a valid Responsive Search Ad content set: each entry trimmed and
// rune-capped to its limit, empties dropped, and duplicates (after trimming)
// removed — Google rejects both an over-limit asset and a duplicate one within
// the same ad. If fewer than the minimum survive, deterministic placeholders
// derived from eventName are appended (never removed) until the minimum is
// met; the result is also capped at the maximum count. An eventName so long
// none of its truncations are useful should not happen in practice (EventName
// is capped upstream via the campaign name validation), but a caller supplying
// zero usable text and an empty eventName is a hard error — there is nothing
// to advertise.
func composeAdCopy(callerHeadlines, callerDescriptions []string, eventName, project string) (headlines, descriptions []string, err error) {
	headlines = boundedUniqueCopy(callerHeadlines, maxHeadlineRunes, maxHeadlines)
	descriptions = boundedUniqueCopy(callerDescriptions, maxDescriptionRunes, maxDescriptions)

	headlines = padUnique(headlines, defaultHeadlines(eventName), maxHeadlineRunes, minHeadlines, maxHeadlines)
	descriptions = padUnique(descriptions, defaultDescriptions(eventName, project), maxDescriptionRunes, minDescriptions, maxDescriptions)

	if len(headlines) < minHeadlines {
		return nil, nil, fmt.Errorf("google-ads ad requires at least %d usable headline(s), got %d (need a non-empty eventName or caller-supplied headlines)", minHeadlines, len(headlines))
	}
	if len(descriptions) < minDescriptions {
		return nil, nil, fmt.Errorf("google-ads ad requires at least %d usable description(s), got %d (need a non-empty eventName/project or caller-supplied descriptions)", minDescriptions, len(descriptions))
	}
	return headlines, descriptions, nil
}

// boundedUniqueCopy trims each candidate, rune-truncates it to maxRunes, drops
// empties, de-duplicates (case-sensitive, post-truncation), and caps the
// result at maxCount entries.
func boundedUniqueCopy(candidates []string, maxRunes, maxCount int) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range candidates {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		s = truncateRunes(s, maxRunes)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
		if len(out) >= maxCount {
			break
		}
	}
	return out
}

// padUnique appends entries from fallback (already-ordered candidates) to base
// until base reaches min entries or fallback is exhausted, skipping any
// fallback entry that duplicates one already present (post-truncation). The
// result is capped at max.
func padUnique(base, fallback []string, maxRunes, min, max int) []string {
	seen := map[string]struct{}{}
	for _, s := range base {
		seen[s] = struct{}{}
	}
	for _, s := range fallback {
		if len(base) >= min || len(base) >= max {
			break
		}
		s = truncateRunes(strings.TrimSpace(s), maxRunes)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		base = append(base, s)
	}
	return base
}

// truncateRunes cuts s to at most n runes (not bytes), so a multibyte
// character is never split mid-encoding.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// defaultHeadlines derives deterministic placeholder headlines from the event
// name so a caller that supplies none still gets a valid, non-generic-sounding
// ad. Order matters: padUnique consumes these in order until the minimum is
// reached.
func defaultHeadlines(eventName string) []string {
	eventName = strings.TrimSpace(eventName)
	if eventName == "" {
		return nil
	}
	return []string{
		eventName,
		"Register for " + eventName,
		"Join " + eventName + " Today",
		"Save Your Spot Now",
		"Learn More & Register",
	}
}

// defaultDescriptions mirrors defaultHeadlines for the description slots.
func defaultDescriptions(eventName, project string) []string {
	eventName = strings.TrimSpace(eventName)
	project = strings.TrimSpace(project)
	if eventName == "" {
		return nil
	}
	var out []string
	if project != "" {
		out = append(out, fmt.Sprintf("%s is happening soon, hosted by %s. Reserve your spot today.", eventName, project))
	}
	out = append(out,
		fmt.Sprintf("Don't miss %s. Register now to secure your place.", eventName),
		"Connect with the community. Registration is open now.",
	)
	return out
}

// buildAdFinalURL builds the ad's destination URL from the brief's
// registration URL, tagging it with UTM parameters for attribution. Existing
// query parameters on the registration URL are preserved; a utm_* key already
// present is left untouched rather than overwritten (mirrors the reddit/meta/
// twitter/microsoft clients' final-URL builders).
func buildAdFinalURL(registrationURL, eventSlug, eventName, project, nameSuffix string) (string, error) {
	registrationURL = strings.TrimSpace(registrationURL)
	if registrationURL == "" {
		return "", fmt.Errorf("registration URL is empty")
	}
	u, err := url.Parse(registrationURL)
	if err != nil {
		return "", fmt.Errorf("registration URL %q is not a valid URL: %w", registrationURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("registration URL %q must be http(s), got scheme %q", registrationURL, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("registration URL %q has no host", registrationURL)
	}

	campaign := sanitizeNamePart(eventSlug)
	if campaign == "" {
		campaign = sanitizeNamePart(eventName)
	}
	if campaign == "" {
		campaign = sanitizeNamePart(nameSuffix)
	}

	q := u.Query()
	setIfAbsent(q, "utm_source", "google")
	setIfAbsent(q, "utm_medium", "cpc")
	if campaign != "" {
		setIfAbsent(q, "utm_campaign", campaign)
	}
	if p := sanitizeNamePart(project); p != "" {
		setIfAbsent(q, "utm_content", p)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// setIfAbsent sets key=value in q only when key is not already present, so a
// registration URL that already carries its own utm_* tagging is not
// overwritten.
func setIfAbsent(q url.Values, key, value string) {
	if q.Has(key) {
		return
	}
	q.Set(key, value)
}

// UpdateAdGroupAndAdStatus toggles an ad group and its ad between ENABLED and
// PAUSED, mirroring UpdateCampaignStatus. Both mutates are sent as idempotent
// (bounded 429 retries are safe: re-applying the same status converges, same
// reasoning as UpdateCampaignStatus). Returns after the FIRST failure without
// attempting the second mutate — the caller (ToggleStatus) already orders
// campaign/ad-group/ad calls per the children-first-on-ACTIVATE /
// campaign-first-on-PAUSE contract, so a failed ad group update must not mask
// itself as "ad group ok, ad unknown".
func (c *Client) UpdateAdGroupAndAdStatus(ctx context.Context, adGroupID, adID, status string) error {
	if err := c.validateAccountIDs(); err != nil {
		return err
	}
	if status != StatusEnabled && status != StatusPaused {
		return fmt.Errorf("google-ads: unsupported ad group/ad status %q (want %s or %s)", status, StatusEnabled, StatusPaused)
	}
	adGroupID = strings.TrimSpace(adGroupID)
	adID = strings.TrimSpace(adID)
	if adGroupID == "" || adID == "" {
		return fmt.Errorf("google-ads: cannot update ad group/ad status: ad group id and ad id must both be set")
	}
	if !numericID(adGroupID) {
		return fmt.Errorf("google-ads: ad group id %q is not numeric", adGroupID)
	}
	if !numericID(adID) {
		return fmt.Errorf("google-ads: ad id %q is not numeric", adID)
	}

	adGroupReq := mutateRequest{Operations: []mutateOperation{{
		Update: adGroupStatusUpdate{
			ResourceName: "customers/" + c.account.CustomerID + "/adGroups/" + adGroupID,
			Status:       status,
		},
		UpdateMask: "status",
	}}}
	if _, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroups:mutate"), adGroupReq, true); err != nil {
		return fmt.Errorf("google-ads ad group %s status update to %s failed: %w", adGroupID, status, err)
	}

	adReq := mutateRequest{Operations: []mutateOperation{{
		Update: adGroupAdStatusUpdate{
			ResourceName: "customers/" + c.account.CustomerID + "/adGroupAds/" + adGroupID + "~" + adID,
			Status:       status,
		},
		UpdateMask: "status",
	}}}
	if _, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupAds:mutate"), adReq, true); err != nil {
		return fmt.Errorf("google-ads ad %s (ad group %s) status update to %s failed: %w", adID, adGroupID, status, err)
	}
	return nil
}

// numericID reports whether s is a non-empty run of ASCII digits — the same
// shape check UpdateCampaignStatus applies to a campaign id, reused here so an
// id interpolated into a resourceName can't alter the resource path.
func numericID(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}
