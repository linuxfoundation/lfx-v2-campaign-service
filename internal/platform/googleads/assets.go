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
)

// ---------------------------------------------------------------------------
// Ad extensions, as assets (LFXV2-2665)
//
// A Search campaign created by this service carried no extensions at all: no
// sitelinks, no callouts, no structured snippets. That is not a cosmetic gap.
// Extensions add lines to the ad, and Google's own auction gives extension-rich
// ads a higher Ad Rank at the same bid — a campaign without them pays more per
// click for less of the SERP.
//
// Google models every extension as an ASSET, created account-wide, then LINKED
// to the campaign. So this is two mutates and they cannot be collapsed: the link
// operation needs the asset resource names the first mutate returns.
//
// SEARCH ONLY, like the criteria in campaign_criteria.go. Demand Gen takes its
// creative assets on the ad itself, not as campaign-level extensions, so the
// whole set is refused on that channel at preflight. Lift it per asset type as
// each is confirmed live.
// ---------------------------------------------------------------------------

// Extension caps. Google permits up to 20 sitelinks and 20 callouts per
// campaign, so these are the upstream ceilings rather than a tighter broker
// opinion — nothing Google would accept is refused here. Structured snippets
// are capped at one per header and Google publishes ~13 headers, so 10 is a
// bound on the payload, not on anything a real campaign wants.
const (
	maxSitelinks          = 20
	maxCallouts           = 20
	maxStructuredSnippets = 10
)

// Per-asset text bounds, in Google's documented character limits.
//
// These are RUNE counts, not the double-width character WEIGHT the RSA copy in
// ad_copy.go uses. Google applies the same CJK doubling to extension text, so a
// 25-rune Japanese sitelink will be refused upstream — that is deliberate
// under-refusal: counting weight here would refuse text Google might accept for
// a mixed-script string, and over-refusal is the costlier mistake. The RSA path
// may silently TRUNCATE over-long copy because it generates its own; extension
// text is written by a human for a reason, so it is refused rather than cut.
const (
	maxSitelinkTextRunes        = 25
	maxSitelinkDescriptionRunes = 35
	maxCalloutTextRunes         = 25
	maxSnippetHeaderRunes       = 25
	maxSnippetValueRunes        = 25
	minSnippetValues            = 3
	maxSnippetValues            = 10
)

// Asset field types, as Google's AssetFieldType enum.
const (
	assetFieldSitelink          = "SITELINK"
	assetFieldCallout           = "CALLOUT"
	assetFieldStructuredSnippet = "STRUCTURED_SNIPPET"
)

// Sitelink is one extra link under the ad: a label, its destination, and an
// optional two-line description.
//
// FinalURL is required. A sitelink with no destination is not a lesser sitelink,
// it is an unservable one, and Google refuses it — but only after the campaign
// exists, which is why it is checked at preflight here.
type Sitelink struct {
	Text string
	// Description1 and Description2 are all-or-nothing: Google renders the two
	// description lines together and rejects an asset carrying only one.
	Description1 string
	Description2 string
	FinalURL     string
}

// StructuredSnippet is one "Header: value, value, value" line under the ad.
type StructuredSnippet struct {
	// Header must be one of Google's predefined snippet headers FOR THE
	// CAMPAIGN'S LANGUAGE — "Brands" in English, "Marcas" in Spanish, and so on
	// down a list Google publishes per locale. This client therefore validates
	// the header's SHAPE and not its membership: a hardcoded English list would
	// refuse every correct non-English header, and refusing a create Google would
	// have accepted is the more expensive error. Same reasoning as the numeric
	// geo-target constant ids in geo.go.
	Header string
	Values []string
}

// ---------------------------------------------------------------------------
// Wire payloads
// ---------------------------------------------------------------------------

type sitelinkAsset struct {
	LinkText string `json:"linkText"`
	// Both omitempty: an absent description line is Google's own default, and an
	// empty string is not the same thing — it is rejected as blank text.
	Description1 string `json:"description1,omitempty"`
	Description2 string `json:"description2,omitempty"`
}

type calloutAsset struct {
	CalloutText string `json:"calloutText"`
}

type structuredSnippetAsset struct {
	Header string   `json:"header"`
	Values []string `json:"values"`
}

// assetCreate models the whole Asset resource. Each arm past FinalURLs is a
// separate member of Google's asset oneof, so exactly one is ever set.
type assetCreate struct {
	FinalURLs              []string                `json:"finalUrls,omitempty"`
	SitelinkAsset          *sitelinkAsset          `json:"sitelinkAsset,omitempty"`
	CalloutAsset           *calloutAsset           `json:"calloutAsset,omitempty"`
	StructuredSnippetAsset *structuredSnippetAsset `json:"structuredSnippetAsset,omitempty"`
}

// campaignAssetCreate links an already-created asset to the campaign. FieldType
// says which extension slot it fills, and Google treats the same asset linked
// under a different field type as a different extension — it is not derivable
// from the asset, so it is carried positionally alongside it in assetPlan.
type campaignAssetCreate struct {
	Campaign  string `json:"campaign"`
	Asset     string `json:"asset"`
	FieldType string `json:"fieldType"`
}

// ---------------------------------------------------------------------------
// The plan
// ---------------------------------------------------------------------------

// assetPlan is every extension asked for, validated and in the order it will be
// sent. assets and fieldTypes are POSITIONALLY paired — index i of one describes
// index i of the other — because the link mutate needs both and the field type
// cannot be read back off the created asset.
type assetPlan struct {
	assets     []assetCreate
	fieldTypes []string
	sitelinks  int
	callouts   int
	snippets   int
}

func (p assetPlan) empty() bool { return len(p.assets) == 0 }

func (p assetPlan) count() int { return len(p.assets) }

// validateAssetPlan resolves every extension input into the operations that will
// be sent, WITHOUT sending anything. CreateCampaign calls it inside
// preflightCampaignKind, before the budget mutate, so an over-long callout or a
// sitelink with no destination fails while nothing has been paid for.
func validateAssetPlan(kind string, in CampaignInput) (assetPlan, error) {
	asked := len(in.Sitelinks) + len(in.Callouts) + len(in.StructuredSnippets)
	if asked == 0 {
		return assetPlan{}, nil
	}
	// Refused as a channel capability, not as a value judgement: Demand Gen does
	// not take campaign-level extension assets, so sending them would fail
	// upstream after the campaign exists.
	if kind != campaignKindSearch {
		return assetPlan{}, fmt.Errorf("google-ads ad extensions (sitelinks, callouts, structured snippets) are supported on Search campaigns only, not on %s", kind)
	}

	var plan assetPlan

	sitelinks, err := validateSitelinks(in)
	if err != nil {
		return assetPlan{}, err
	}
	for i := range sitelinks {
		plan.assets = append(plan.assets, sitelinks[i])
		plan.fieldTypes = append(plan.fieldTypes, assetFieldSitelink)
	}
	plan.sitelinks = len(sitelinks)

	callouts, err := validateCallouts(in.Callouts)
	if err != nil {
		return assetPlan{}, err
	}
	for i := range callouts {
		plan.assets = append(plan.assets, callouts[i])
		plan.fieldTypes = append(plan.fieldTypes, assetFieldCallout)
	}
	plan.callouts = len(callouts)

	snippets, err := validateStructuredSnippets(in.StructuredSnippets)
	if err != nil {
		return assetPlan{}, err
	}
	for i := range snippets {
		plan.assets = append(plan.assets, snippets[i])
		plan.fieldTypes = append(plan.fieldTypes, assetFieldStructuredSnippet)
	}
	plan.snippets = len(snippets)

	return plan, nil
}

// validateSitelinks checks each sitelink and builds its asset. The destination
// is tagged exactly like the ad's own final URL — a sitelink click IS an ad
// click, and an untagged one would land in the attribution data as organic
// traffic while the campaign paid for it.
func validateSitelinks(in CampaignInput) ([]assetCreate, error) {
	if len(in.Sitelinks) > maxSitelinks {
		return nil, fmt.Errorf("google-ads campaign accepts at most %d sitelinks, got %d", maxSitelinks, len(in.Sitelinks))
	}
	out := make([]assetCreate, 0, len(in.Sitelinks))
	seen := make(map[string]struct{}, len(in.Sitelinks))
	for i, s := range in.Sitelinks {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			return nil, fmt.Errorf("google-ads sitelink %d has no link text", i)
		}
		if n := utf8.RuneCountInString(text); n > maxSitelinkTextRunes {
			return nil, fmt.Errorf("google-ads sitelink %d link text is %d characters, exceeding the %d limit", i, n, maxSitelinkTextRunes)
		}
		// Google renders sitelinks by their link text and refuses two with the
		// same one on a campaign — and a caller who wrote it twice meant one.
		key := strings.ToLower(text)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("google-ads sitelink %q is listed more than once", text)
		}
		seen[key] = struct{}{}

		desc1 := strings.TrimSpace(s.Description1)
		desc2 := strings.TrimSpace(s.Description2)
		if (desc1 == "") != (desc2 == "") {
			return nil, fmt.Errorf("google-ads sitelink %q needs both description lines or neither (got one)", text)
		}
		for _, d := range []string{desc1, desc2} {
			if n := utf8.RuneCountInString(d); n > maxSitelinkDescriptionRunes {
				return nil, fmt.Errorf("google-ads sitelink %q has a description line of %d characters, exceeding the %d limit", text, n, maxSitelinkDescriptionRunes)
			}
		}

		finalURL, err := buildTaggedFinalURL(fmt.Sprintf("sitelink %q destination URL", text), s.FinalURL, in.EventSlug, in.EventName, in.Project, in.NameSuffix)
		if err != nil {
			return nil, fmt.Errorf("google-ads sitelink %d is unservable: %w", i, err)
		}
		if n := len(finalURL); n > maxFinalURLBytes {
			return nil, fmt.Errorf("google-ads sitelink %q destination URL is %d bytes, exceeding the %d limit", text, n, maxFinalURLBytes)
		}

		out = append(out, assetCreate{
			FinalURLs: []string{finalURL},
			SitelinkAsset: &sitelinkAsset{
				LinkText:     text,
				Description1: desc1,
				Description2: desc2,
			},
		})
	}
	return out, nil
}

// validateCallouts checks each callout and builds its asset. A callout carries
// no link — it is a short claim appended to the ad — so there is no URL here.
func validateCallouts(callouts []string) ([]assetCreate, error) {
	if len(callouts) > maxCallouts {
		return nil, fmt.Errorf("google-ads campaign accepts at most %d callouts, got %d", maxCallouts, len(callouts))
	}
	out := make([]assetCreate, 0, len(callouts))
	seen := make(map[string]struct{}, len(callouts))
	for i, raw := range callouts {
		text := strings.TrimSpace(raw)
		if text == "" {
			return nil, fmt.Errorf("google-ads callout %d is empty", i)
		}
		if n := utf8.RuneCountInString(text); n > maxCalloutTextRunes {
			return nil, fmt.Errorf("google-ads callout %q is %d characters, exceeding the %d limit", text, n, maxCalloutTextRunes)
		}
		key := strings.ToLower(text)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("google-ads callout %q is listed more than once", text)
		}
		seen[key] = struct{}{}
		out = append(out, assetCreate{CalloutAsset: &calloutAsset{CalloutText: text}})
	}
	return out, nil
}

// validateStructuredSnippets checks each snippet and builds its asset. Values
// are de-duplicated before the minimum is counted: three values of which two are
// the same renders as two, which is below what Google accepts.
func validateStructuredSnippets(snippets []StructuredSnippet) ([]assetCreate, error) {
	if len(snippets) > maxStructuredSnippets {
		return nil, fmt.Errorf("google-ads campaign accepts at most %d structured snippets, got %d", maxStructuredSnippets, len(snippets))
	}
	out := make([]assetCreate, 0, len(snippets))
	seenHeaders := make(map[string]struct{}, len(snippets))
	for i, s := range snippets {
		header := strings.TrimSpace(s.Header)
		if header == "" {
			return nil, fmt.Errorf("google-ads structured snippet %d has no header", i)
		}
		if n := utf8.RuneCountInString(header); n > maxSnippetHeaderRunes {
			return nil, fmt.Errorf("google-ads structured snippet %d header is %d characters, exceeding the %d limit", i, n, maxSnippetHeaderRunes)
		}
		// One snippet per header: Google serves a single line per header, so the
		// second would be dropped — and which one is dropped is not the caller's
		// choice to lose silently.
		key := strings.ToLower(header)
		if _, dup := seenHeaders[key]; dup {
			return nil, fmt.Errorf("google-ads structured snippet header %q is listed more than once", header)
		}
		seenHeaders[key] = struct{}{}

		values := make([]string, 0, len(s.Values))
		seenValues := make(map[string]struct{}, len(s.Values))
		for _, raw := range s.Values {
			v := strings.TrimSpace(raw)
			if v == "" {
				continue
			}
			if n := utf8.RuneCountInString(v); n > maxSnippetValueRunes {
				return nil, fmt.Errorf("google-ads structured snippet %q has a value of %d characters, exceeding the %d limit", header, n, maxSnippetValueRunes)
			}
			if _, dup := seenValues[strings.ToLower(v)]; dup {
				continue
			}
			seenValues[strings.ToLower(v)] = struct{}{}
			values = append(values, v)
		}
		if len(values) < minSnippetValues {
			return nil, fmt.Errorf("google-ads structured snippet %q needs at least %d distinct values, got %d", header, minSnippetValues, len(values))
		}
		if len(values) > maxSnippetValues {
			return nil, fmt.Errorf("google-ads structured snippet %q has %d values, exceeding the %d limit", header, len(values), maxSnippetValues)
		}
		out = append(out, assetCreate{StructuredSnippetAsset: &structuredSnippetAsset{Header: header, Values: values}})
	}
	return out, nil
}

// assetStep renders the extension step sentence, naming only what was actually
// requested. A clause about callouts on a campaign with no callout assets is a
// lie about a paid resource.
func assetStep(plan assetPlan) string {
	parts := make([]string, 0, 3)
	if plan.sitelinks > 0 {
		parts = append(parts, fmt.Sprintf("%d sitelinks", plan.sitelinks))
	}
	if plan.callouts > 0 {
		parts = append(parts, fmt.Sprintf("%d callouts", plan.callouts))
	}
	if plan.snippets > 0 {
		parts = append(parts, fmt.Sprintf("%d structured snippets", plan.snippets))
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// The mutates
// ---------------------------------------------------------------------------

// createCampaignAssets creates every extension asset and links it to the
// campaign, returning (asset ids, campaign-asset ids).
//
// Two mutates, necessarily: the link operations reference the asset resource
// names the create returns, so they cannot be batched together. Each mutate is
// internally atomic for the reason the geo mutate is — a campaign carrying half
// its sitelinks is harder to reconcile than one carrying none, and the step
// sentence would be true of neither.
//
// The split has one consequence worth stating in the error: assets created by
// the first mutate but never linked by the second are left in the account. They
// are account-level, reusable, cost nothing and serve nothing until something
// links them, so they are litter rather than a leak — but a retry will create
// another set rather than adopt them.
func (c *Client) createCampaignAssets(ctx context.Context, campaignResource, campaignID string, plan assetPlan) (assetIDs, campaignAssetIDs []string, err error) {
	if plan.empty() {
		return nil, nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, fmt.Errorf("google-ads ad extensions aborted before any request (context already done; campaign %s has no extension assets yet): %w", campaignID, ctxErr)
	}

	assetOps := make([]mutateOperation, 0, plan.count())
	for i := range plan.assets {
		assetOps = append(assetOps, mutateOperation{Create: plan.assets[i]})
	}
	assetResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("assets:mutate"), mutateRequest{Operations: assetOps}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, nil, fmt.Errorf("google-ads ad extension assets UNCONFIRMED (campaign %s; assets may exist and may be unlinked — verify in Google Ads before retrying): %w", campaignID, err)
		}
		return nil, nil, fmt.Errorf("google-ads ad extension asset creation failed (campaign %s created; it has NO sitelinks, callouts or structured snippets): %w", campaignID, err)
	}

	var assetResults mutateResponse
	if uErr := json.Unmarshal(assetResp, &assetResults); uErr != nil || len(assetResults.Results) != len(assetOps) {
		return nil, nil, fmt.Errorf("google-ads ad extension assets UNCONFIRMED (campaign %s; 2xx with a malformed/short mutate response — assets may exist unlinked — verify in Google Ads before retrying)", campaignID)
	}

	assetResources := make([]string, 0, len(assetOps))
	ids := make([]string, 0, len(assetOps))
	for i, r := range assetResults.Results {
		id := c.assetID(r.ResourceName)
		if id == "" {
			return nil, nil, fmt.Errorf("google-ads ad extension assets UNCONFIRMED (campaign %s; malformed/wrong-kind/wrong-account asset resource name %q at index %d — verify in Google Ads before retrying)", campaignID, r.ResourceName, i)
		}
		// The resource name Google returned is what gets linked, never one
		// rebuilt from the id: a rebuilt name would paper over exactly the
		// mismatch the check above exists to catch.
		assetResources = append(assetResources, r.ResourceName)
		ids = append(ids, id)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ids, nil, fmt.Errorf("google-ads ad extension linking aborted before its request (context done; campaign %s has %d unlinked extension assets): %w", campaignID, len(ids), ctxErr)
	}

	linkOps := make([]mutateOperation, 0, len(assetResources))
	for i, resource := range assetResources {
		linkOps = append(linkOps, mutateOperation{Create: campaignAssetCreate{
			Campaign:  campaignResource,
			Asset:     resource,
			FieldType: plan.fieldTypes[i],
		}})
	}
	linkResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaignAssets:mutate"), mutateRequest{Operations: linkOps}, false)
	if err != nil {
		// ids are returned either way: the assets exist, and naming them is what
		// makes the partial reconcilable.
		if createOutcomeAmbiguous(err) {
			return ids, nil, fmt.Errorf("google-ads ad extension linking UNCONFIRMED (campaign %s; %d assets created, their links may exist — verify in Google Ads before retrying): %w", campaignID, len(ids), err)
		}
		return ids, nil, fmt.Errorf("google-ads ad extension linking failed (campaign %s created with %d unlinked extension assets; the campaign has NO sitelinks, callouts or structured snippets): %w", campaignID, len(ids), err)
	}

	var linkResults mutateResponse
	if uErr := json.Unmarshal(linkResp, &linkResults); uErr != nil || len(linkResults.Results) != len(linkOps) {
		return ids, nil, fmt.Errorf("google-ads ad extension linking UNCONFIRMED (campaign %s; 2xx with a malformed/short mutate response — some extensions may be linked — verify in Google Ads before retrying)", campaignID)
	}

	// Every component of the returned composite is checked against the operation at the
	// same index, not just the campaign. Results come back in operation order — the asset
	// mutate above already depends on that to pair ids with plan.fieldTypes — so index i
	// names the link operation i asked for, and a 2xx reporting `222~999~SITELINK` for an
	// operation that linked asset 700 as CALLOUT is not proof that link exists. Checking
	// only the campaign accepted exactly that, which is the gap this closes.
	linkIDs := make([]string, 0, len(linkOps))
	for i, r := range linkResults.Results {
		returnedCampaignID, assetID, returnedFieldType := c.campaignAssetID(r.ResourceName)
		if returnedCampaignID == "" || assetID == "" {
			return ids, nil, fmt.Errorf("google-ads ad extension linking UNCONFIRMED (campaign %s; malformed/wrong-kind/wrong-account campaignAsset resource name %q at index %d — verify in Google Ads before retrying)", campaignID, r.ResourceName, i)
		}
		if returnedCampaignID != campaignID {
			return ids, nil, fmt.Errorf("google-ads ad extension linking UNCONFIRMED (campaign %s; campaignAsset resource name %q reports a different campaign id %q — verify in Google Ads before retrying)", campaignID, r.ResourceName, returnedCampaignID)
		}
		if assetID != ids[i] {
			return ids, nil, fmt.Errorf("google-ads ad extension linking UNCONFIRMED (campaign %s; campaignAsset resource name %q at index %d reports asset %s but that operation linked asset %s — verify in Google Ads before retrying)", campaignID, r.ResourceName, i, assetID, ids[i])
		}
		// Compared case-insensitively on purpose. The value is Google's own enum name
		// echoed back, so a casing difference would be a change in how the API spells a
		// field type rather than the wrong link — and failing a real, correct create over
		// spelling is the over-refusal this guard must not commit. A different field type
		// is still caught, which is the whole point of the check.
		if !strings.EqualFold(returnedFieldType, plan.fieldTypes[i]) {
			return ids, nil, fmt.Errorf("google-ads ad extension linking UNCONFIRMED (campaign %s; campaignAsset resource name %q at index %d reports field type %s but that operation asked for %s — verify in Google Ads before retrying)", campaignID, r.ResourceName, i, returnedFieldType, plan.fieldTypes[i])
		}
		linkIDs = append(linkIDs, assetID)
	}
	return ids, linkIDs, nil
}

// assetID applies to an Asset resource name the same four checks
// campaignCriterionID applies to a criterion: exactly four segments, the
// "assets" kind, THIS client's customer id, and a numeric trailing id. Returns
// "" for anything else — a 2xx naming another account's asset, another resource
// kind, or "garbage/4242" is not proof the asset this run asked for exists.
// parsedAssetIDs is every well-formed asset id in a mutate response, in order, skipping
// any resource name assetID refuses. It exists for the arms that return an UNCONFIRMED
// error over a response that nonetheless PARSED: the ids in it are the only handle an
// operator has on account-level assets that may already exist, so the error carries them
// rather than dropping them.
func (c *Client) parsedAssetIDs(resp mutateResponse) []string {
	out := make([]string, 0, len(resp.Results))
	for _, r := range resp.Results {
		if id := c.assetID(r.ResourceName); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func (c *Client) assetID(resourceName string) string {
	pathParts := strings.Split(resourceName, "/")
	if len(pathParts) != 4 || pathParts[0] != "customers" || pathParts[2] != "assets" {
		return ""
	}
	if pathParts[1] != c.account.CustomerID {
		return ""
	}
	if !numericID(pathParts[3]) {
		return ""
	}
	return pathParts[3]
}

// campaignAssetID parses a CampaignAsset resource name, whose trailing segment
// is a THREE-part composite "{campaignId}~{assetId}~{fieldType}" — not the
// two-part shape compositeResourceID handles, which is why that helper is not
// reused here. The field type is a non-numeric enum name, so it is checked for
// presence rather than parsed; the two ids are checked for the numeric shape
// every id in this package is checked for.
func (c *Client) campaignAssetID(resourceName string) (campaignID, assetID, fieldType string) {
	pathParts := strings.Split(resourceName, "/")
	if len(pathParts) != 4 || pathParts[0] != "customers" || pathParts[2] != "campaignAssets" {
		return "", "", ""
	}
	if pathParts[1] != c.account.CustomerID {
		return "", "", ""
	}
	parts := strings.Split(pathParts[3], "~")
	if len(parts) != 3 || !numericID(parts[0]) || !numericID(parts[1]) || strings.TrimSpace(parts[2]) == "" {
		return "", "", ""
	}
	// The field type is returned so the caller can check the link Google reports is the
	// link it asked for. It is the only one of the three components that is not numeric,
	// so it is the only one trimmed rather than shape-checked.
	return parts[0], parts[1], strings.TrimSpace(parts[2])
}
