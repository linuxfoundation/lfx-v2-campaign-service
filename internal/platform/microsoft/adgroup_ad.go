// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Ad group + ad creation (MS-2.5): complete the Campaign -> AdGroup -> Ad tree
//
// CreateCampaign creates the campaign (MS-2), then this file finishes the
// hierarchy so the result is a usable PAUSED campaign rather than an empty shell —
// mirroring the reddit/meta clients, whose CreateCampaign creates all three levels.
// Everything stays PAUSED so nothing serves until a human enables it.
//
// The same two Microsoft transport facts from campaign.go apply at every level:
// PartialErrors-on-200 (a per-entity failure is a 200 with a null id slot + a
// PartialError, inspected via firstEntityID) and duplicate-names-allowed (so each
// level is find-or-create by its deterministic name before a create).
// ---------------------------------------------------------------------------

const (
	// adGroupStatusPaused creates the ad group PAUSED. Microsoft uses "Paused" for
	// AdGroup.Status; an Ad has no independent runnable status beyond its EditorialStatus,
	// so the ad is created under this PAUSED ad group (nothing serves) — there is no
	// separate ad-status constant.
	adGroupStatusPaused = "Paused"

	// maxAdGroupNameRunes bounds the composed ad-group name. Microsoft limits
	// AdGroup.Name to 256 characters; validated in runes before the create.
	maxAdGroupNameRunes = 256

	// Text Ad field limits (characters). Microsoft's Expanded Text Ad limits: Title
	// (headline) 30, Text (description) 90. Composed copy is bounded to these before
	// the create so an over-limit value is rejected up front, not after the ad group
	// already exists.
	maxAdTitleRunes = 30
	maxAdTextRunes  = 90

	// adTypeText is the Ad.Type this slice creates. A Text Ad is the dependency-free
	// choice for a PAUSED Search shell (no image/asset upload needed).
	adTypeText = "Text"
)

// msAdGroup is one AdGroup in the POST /AdGroups body. Only the fields required for a
// PAUSED ad-group shell are set; targeting/bids use account defaults, the conservative
// choice for a broker-created shell.
type msAdGroup struct {
	Name   string `json:"Name"`
	Status string `json:"Status"`
}

// createAdGroupsRequest is the POST /AdGroups body. The v13 AddAdGroups operation
// REQUIRES CampaignId at the top level (a sibling to AdGroups) — the target campaign is
// NOT in the URL. Response is an index-aligned id slice + PartialErrors (a null slot =
// that entity failed).
type createAdGroupsRequest struct {
	CampaignId json.Number `json:"CampaignId"`
	AdGroups   []msAdGroup `json:"AdGroups"`
}

type createAdGroupsResponse struct {
	AdGroupIds    []*json.Number `json:"AdGroupIds"`
	PartialErrors []msErrorItem  `json:"PartialErrors"`
}

// queryAdGroupsRequest is the POST /AdGroups/QueryByCampaignId body used by
// findAdGroupByName — the v13 GetAdGroupsByCampaignId REST operation is a POST with the
// CampaignId in the body, not a GET.
type queryAdGroupsRequest struct {
	CampaignId json.Number `json:"CampaignId"`
}

// queryAdGroupsResponse is the (subset of the) QueryByCampaignId response.
type queryAdGroupsResponse struct {
	AdGroups []struct {
		Id   *json.Number `json:"Id"`
		Name string       `json:"Name"`
	} `json:"AdGroups"`
}

// msTextAd is one Text Ad in the POST /Ads body. FinalUrls is the ad destination;
// Title/Text are the visible copy. Microsoft requires a final URL. Type "Text"
// identifies the derived Text Ad.
type msTextAd struct {
	Type      string   `json:"Type"`
	Title     string   `json:"Title"`
	Text      string   `json:"Text"`
	FinalUrls []string `json:"FinalUrls"`
}

// createAdsRequest is the POST /Ads body. The v13 AddAds operation REQUIRES AdGroupId
// at the top level (a sibling to Ads) — the target ad group is NOT in the URL.
type createAdsRequest struct {
	AdGroupId json.Number `json:"AdGroupId"`
	Ads       []msTextAd  `json:"Ads"`
}

type createAdsResponse struct {
	AdIds         []*json.Number `json:"AdIds"`
	PartialErrors []msErrorItem  `json:"PartialErrors"`
}

// queryAdsRequest is the POST /Ads/QueryByAdGroupId body used by findTextAdByFinalURL.
type queryAdsRequest struct {
	AdGroupId json.Number `json:"AdGroupId"`
}

// createAdGroupAndAd completes the hierarchy under an already-created/found campaign:
// it find-or-creates a PAUSED ad group, then creates a PAUSED Text Ad under it. Each
// step accumulates its ids into the result so an ambiguous failure at a later step
// leaves the whole tree reconcilable, never orphaned anonymously.
//
// campaignPartial() returns the result carrying everything known so far (campaign id +
// name); this function extends it with the ad-group and ad ids/names as they land.
func (c *Client) createAdGroupAndAd(
	ctx context.Context,
	in CampaignInput,
	campaignID string,
	alreadyExisted bool,
	steps *[]string,
	campaignPartial func() *CampaignResult,
) (*CampaignResult, error) {
	// The ad destination URL (in.RegistrationURL) is validated up front in CreateCampaign,
	// BEFORE the campaign create, so a bad URL fails cleanly without orphaning a PAUSED
	// campaign or ad group. No re-validation here: the input hasn't changed, and repeating
	// it would only risk the two checks drifting apart.

	adGroupName := composeAdGroupName(in)
	if err := validateEntityName("ad group", adGroupName, utf8.RuneCountInString(adGroupName), maxAdGroupNameRunes, "characters"); err != nil {
		return campaignPartial(), fmt.Errorf("microsoft-ads ad group name invalid (campaign %s created): %w", campaignID, err)
	}

	// adGroupPartial carries the campaign id/name + the ad-group name (and, once known,
	// its id) so an ambiguous ad-group/ad failure is reconcilable.
	adGroupPartial := func() *CampaignResult {
		r := campaignPartial()
		r.AdGroupName = adGroupName
		return r
	}

	// Step 3: find-or-create the ad group under the campaign. The lookup is a read
	// (idempotent), the create is a mutation (not retried on 429). A cancellation
	// during the lookup is a clean abort (nothing new created), but the CAMPAIGN
	// already exists, so it is surfaced as a reconcilable partial rather than (nil,err).
	adGroupID, existed, err := c.findOrCreateAdGroup(ctx, campaignID, adGroupName)
	if err != nil {
		// createOutcomeAmbiguous covers transport/5xx/mutating-429; errNoID covers a
		// malformed 2xx that returned no id (the ad group MAY have been created). Both are
		// UNCONFIRMED: verify before retry so a duplicate isn't stacked.
		if createOutcomeAmbiguous(err) || errors.Is(err, errNoID) {
			return adGroupPartial(), fmt.Errorf("microsoft-ads ad group creation UNCONFIRMED (campaign %s created; %q may exist — verify before retrying): %w", campaignID, adGroupName, err)
		}
		if errors.Is(err, errPartialFailure) {
			return adGroupPartial(), fmt.Errorf("microsoft-ads ad group creation rejected (campaign %s created): %w", campaignID, err)
		}
		return adGroupPartial(), fmt.Errorf("microsoft-ads ad group creation failed (campaign %s created): %w", campaignID, err)
	}
	if existed {
		*steps = append(*steps, fmt.Sprintf("Ad group already exists by name: %s (not re-created)", adGroupID))
	} else {
		*steps = append(*steps, fmt.Sprintf("Ad group created: %s (PAUSED)", adGroupID))
	}

	adGroupWithIDPartial := func() *CampaignResult {
		r := adGroupPartial()
		r.AdGroupID = adGroupID
		return r
	}

	// Step 4: create the Text Ad under the ad group. Microsoft ALLOWS duplicate ads,
	// but an ad carries no stable human name to find-by, so idempotency here is by the
	// (Title, Text, FinalUrl) triple: look for an existing ad with the same destination
	// before creating, so a retry doesn't stack duplicate ads. All PAUSED via the ad
	// group.
	title, text := composeAdCopy(in)
	finalURL := buildAdFinalURL(in)

	adID, existed, err := c.findOrCreateTextAd(ctx, adGroupID, title, text, finalURL)
	if err != nil {
		// Same classification as the ad group: errNoID (malformed 2xx, no id) joins the
		// ambiguous set — the ad MAY have been created, so UNCONFIRMED not failed.
		if createOutcomeAmbiguous(err) || errors.Is(err, errNoID) {
			return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad creation UNCONFIRMED (campaign %s + ad group %s created; an ad may exist — verify before retrying): %w", campaignID, adGroupID, err)
		}
		if errors.Is(err, errPartialFailure) {
			return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad creation rejected (campaign %s + ad group %s created): %w", campaignID, adGroupID, err)
		}
		return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad creation failed (campaign %s + ad group %s created): %w", campaignID, adGroupID, err)
	}
	if existed {
		*steps = append(*steps, fmt.Sprintf("Ad already exists (%s) with the same destination (not re-created)", adID))
	} else {
		*steps = append(*steps, fmt.Sprintf("Ad created: %s (PAUSED, Text)", adID))
	}

	r := adGroupWithIDPartial()
	r.AdID = adID
	r.AlreadyExisted = alreadyExisted
	r.Steps = *steps
	return r, nil
}

// findOrCreateAdGroup returns (id, existed, err). It first looks the ad group up by
// name under the campaign (a read), returning it if present; otherwise it POSTs the ad
// group to /AdGroups with the CampaignId in the body. Ad-group names are unique within a
// campaign, so the name lookup is the idempotency key (a stable name → a retry reuses
// the existing group).
func (c *Client) findOrCreateAdGroup(ctx context.Context, campaignID, name string) (id string, existed bool, err error) {
	if existingID, ferr := c.findAdGroupByName(ctx, campaignID, name); ferr != nil {
		return "", false, ferr
	} else if existingID != "" {
		return existingID, true, nil
	}
	req := createAdGroupsRequest{
		CampaignId: json.Number(campaignID),
		AdGroups:   []msAdGroup{{Name: name, Status: adGroupStatusPaused}},
	}
	body, err := c.doRequest(ctx, http.MethodPost, "AdGroups", req, false)
	if err != nil {
		return "", false, err
	}
	var resp createAdGroupsResponse
	newID, err := firstEntityID(body, "AdGroupIds", func(b []byte) ([]*json.Number, []msErrorItem, error) {
		if uErr := json.Unmarshal(b, &resp); uErr != nil {
			return nil, nil, uErr
		}
		return resp.AdGroupIds, resp.PartialErrors, nil
	})
	if err != nil {
		return "", false, err
	}
	return newID, false, nil
}

// findAdGroupByName returns the id of the ad group whose Name matches name (case-
// insensitively, per Microsoft's uniqueness comparison) under the campaign, or "" if
// none. It POSTs /AdGroups/QueryByCampaignId with the CampaignId in the body (the v13
// GetAdGroupsByCampaignId REST operation is a POST-with-body, not a GET). A READ
// (idempotent, retried on 429); the response carries the full set for the campaign in
// one response (not paged), so the single-shot read can't miss a match.
func (c *Client) findAdGroupByName(ctx context.Context, campaignID, name string) (string, error) {
	req := queryAdGroupsRequest{CampaignId: json.Number(campaignID)}
	body, err := c.doRequest(ctx, http.MethodPost, "AdGroups/QueryByCampaignId", req, true)
	if err != nil {
		return "", err
	}
	var resp queryAdGroupsResponse
	if uErr := json.Unmarshal(body, &resp); uErr != nil {
		return "", fmt.Errorf("decode AdGroups/QueryByCampaignId response: %w", uErr)
	}
	for _, g := range resp.AdGroups {
		if strings.EqualFold(g.Name, name) {
			if id := numberID(g.Id); id != "" {
				return id, nil
			}
		}
	}
	return "", nil
}

// findOrCreateTextAd returns (id, existed, err). It looks for an existing Text Ad in
// the ad group whose FinalUrls already contains finalURL (ads have no stable name, so
// the destination URL is the idempotency key), returning it if present; otherwise it
// creates a PAUSED Text Ad.
func (c *Client) findOrCreateTextAd(ctx context.Context, adGroupID, title, text, finalURL string) (id string, existed bool, err error) {
	if existingID, ferr := c.findTextAdByFinalURL(ctx, adGroupID, finalURL); ferr != nil {
		return "", false, ferr
	} else if existingID != "" {
		return existingID, true, nil
	}
	req := createAdsRequest{
		AdGroupId: json.Number(adGroupID),
		Ads: []msTextAd{{
			Type:      adTypeText,
			Title:     title,
			Text:      text,
			FinalUrls: []string{finalURL},
		}},
	}
	body, err := c.doRequest(ctx, http.MethodPost, "Ads", req, false)
	if err != nil {
		return "", false, err
	}
	var resp createAdsResponse
	newID, err := firstEntityID(body, "AdIds", func(b []byte) ([]*json.Number, []msErrorItem, error) {
		if uErr := json.Unmarshal(b, &resp); uErr != nil {
			return nil, nil, uErr
		}
		return resp.AdIds, resp.PartialErrors, nil
	})
	if err != nil {
		return "", false, err
	}
	return newID, false, nil
}

// queryAdsResponse is the (subset of the) Ads/QueryByAdGroupId response used to match an
// existing ad by its destination for idempotency.
type queryAdsResponse struct {
	Ads []struct {
		Id        *json.Number `json:"Id"`
		FinalUrls []string     `json:"FinalUrls"`
	} `json:"Ads"`
}

// findTextAdByFinalURL returns the id of an ad in the group whose FinalUrls contains
// finalURL, or "" if none. It POSTs /Ads/QueryByAdGroupId with the AdGroupId in the body
// (the v13 GetAdsByAdGroupId REST operation is a POST-with-body, not a GET). A READ
// (idempotent). Matching on the destination keeps a retry from stacking duplicate ads
// (ads have no stable name to key on).
func (c *Client) findTextAdByFinalURL(ctx context.Context, adGroupID, finalURL string) (string, error) {
	req := queryAdsRequest{AdGroupId: json.Number(adGroupID)}
	body, err := c.doRequest(ctx, http.MethodPost, "Ads/QueryByAdGroupId", req, true)
	if err != nil {
		return "", err
	}
	var resp queryAdsResponse
	if uErr := json.Unmarshal(body, &resp); uErr != nil {
		return "", fmt.Errorf("decode Ads/QueryByAdGroupId response: %w", uErr)
	}
	for _, ad := range resp.Ads {
		if ad.Id == nil {
			continue
		}
		for _, u := range ad.FinalUrls {
			if u == finalURL {
				if id := numberID(ad.Id); id != "" {
					return id, nil
				}
			}
		}
	}
	return "", nil
}

// errNoID marks a 2xx create response that carried neither a usable id NOR a
// PartialError explaining a rejection — a malformed success. The mutation MAY have
// committed upstream, so createAdGroupAndAd classifies it as UNCONFIRMED (verify before
// retry), never as a clean failure. firstCampaignID's caller reaches the same UNCONFIRMED
// outcome via an explicit else; the ad-group/ad path keys off this sentinel because its
// call sites branch on createOutcomeAmbiguous first.
var errNoID = errors.New("create response carried no id")

// firstEntityID decodes a create-entities 200 body via extract (which returns the
// id slice + PartialErrors) and returns the created id. It mirrors firstCampaignID's
// contract exactly: a valid first id is success; a null id slot WITH an ACTUAL
// PartialError is a definite rejection (errPartialFailure); anything else (no id, no
// real error) is a malformed 200 → errNoID, which the caller treats as UNCONFIRMED.
// Extracted so the ad-group and ad creates share one classification path.
func firstEntityID(body []byte, idField string, extract func([]byte) ([]*json.Number, []msErrorItem, error)) (string, error) {
	ids, partials, err := extract(body)
	if err != nil {
		return "", fmt.Errorf("decode %s response: %w", idField, err)
	}
	if len(ids) > 0 && ids[0] != nil {
		if id := numberID(ids[0]); id != "" {
			return id, nil
		}
	}
	// No valid id. Only an ACTUAL PartialError is a definite rejection; a PartialErrors
	// slice of nothing but null placeholders (position-aligned with the id slots) does
	// NOT explain a failure, so it must fall through to UNCONFIRMED — mirroring
	// firstCampaignID. len(partials) would wrongly treat a null-only slice as a rejection.
	if partialErrorsHaveAny(partials) {
		return "", fmt.Errorf("%w: %s", errPartialFailure, partialErrorCodes(partials))
	}
	return "", fmt.Errorf("%s %w", idField, errNoID)
}

// composeAdGroupName builds a deterministic ad-group name from the input, mirroring
// composeName's sanitization so a retry composes the same name (the idempotency key).
func composeAdGroupName(in CampaignInput) string {
	parts := []string{"LFX", "Ad Group"}
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

// composeAdCopy returns the (Title, Text) for the Text Ad, bounded to Microsoft's
// limits. Caller-supplied Headline/Description win; otherwise the copy is derived from
// the sanitized EventName as a safe PAUSED placeholder a human edits before enabling.
// Both are rune-truncated to their field limit so an over-limit value can't fail the
// paid create (the ad is PAUSED, so a truncated placeholder is acceptable).
func composeAdCopy(in CampaignInput) (title, text string) {
	event := sanitizeNamePart(in.EventName)
	title = strings.TrimSpace(in.Headline)
	if title == "" {
		title = event
	}
	text = strings.TrimSpace(in.Description)
	if text == "" {
		text = "Learn more about " + event
	}
	return truncateRunes(title, maxAdTitleRunes), truncateRunes(text, maxAdTextRunes)
}

// truncateRunes returns at most n runes of s (never splitting a multibyte rune).
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// buildAdFinalURL returns the ad's destination: the registration URL with the LFX
// utm_* attribution params SET (replacing only those keys, preserving every other
// query param). Falls back to the raw URL if it can't be parsed (validateAdURL has
// already rejected a genuinely malformed URL, so this is defensive).
func buildAdFinalURL(in CampaignInput) string {
	base := strings.TrimSpace(in.RegistrationURL)
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set("utm_source", "microsoft")
	q.Set("utm_medium", "cpc")
	if slug := sanitizeNamePart(in.EventSlug); slug != "" {
		q.Set("utm_campaign", slug)
	} else if e := sanitizeNamePart(in.EventName); e != "" {
		q.Set("utm_campaign", e)
	}
	if p := sanitizeNamePart(in.Project); p != "" {
		q.Set("utm_content", p)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// validateAdURL rejects an empty/malformed ad destination BEFORE any mutating call.
// https/http only, absolute, no embedded userinfo (an ad destination never needs URL
// credentials, and forwarding them would leak a secret), and a well-formed query (a
// malformed %-escape would be silently dropped by u.Query() in buildAdFinalURL,
// changing the destination). Mirrors the reddit client's validateRegistrationURL.
func validateAdURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("registration URL is required to create the ad")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		// Do NOT wrap the url.Parse error: a *url.Error embeds the full raw URL,
		// re-exposing any userinfo/query secret even though we redacted the %q arg.
		return fmt.Errorf("registration URL %q is not a valid URL", redactAdURL(raw))
	}
	if !u.IsAbs() || u.Hostname() == "" {
		return fmt.Errorf("registration URL %q must be absolute (include scheme and host)", redactAdURL(raw))
	}
	if _, qerr := url.ParseQuery(u.RawQuery); qerr != nil {
		return fmt.Errorf("registration URL %q has a malformed query string", redactAdURL(raw))
	}
	if u.User != nil {
		return fmt.Errorf("registration URL must not contain embedded credentials (userinfo)")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("registration URL %q must use an http or https scheme, got %q", redactAdURL(raw), u.Scheme)
	}
}

// redactAdURL returns a URL safe to echo in an error: scheme://host/path only (query,
// fragment, and userinfo dropped). Mirrors the reddit/meta redactURL.
func redactAdURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if u, err := url.Parse(trimmed); err == nil && u.IsAbs() && u.Host != "" {
		redacted := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
		return redacted.String()
	}
	if i := strings.IndexAny(trimmed, "?#"); i >= 0 {
		trimmed = trimmed[:i]
	}
	if strings.Contains(trimmed, "@") {
		return "[unparseable-url-redacted]"
	}
	return truncate(trimmed, maxErrorBodyChars)
}
