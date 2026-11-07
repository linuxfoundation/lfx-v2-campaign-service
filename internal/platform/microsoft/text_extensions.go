// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/textfold"
)

// ---------------------------------------------------------------------------
// Sitelink and callout ad extensions at create (#563, gap 3).
//
// A Search campaign created here may carry campaign-level SitelinkAdExtension and
// CalloutAdExtension entries, attached right after the campaign is confirmed. The transport is
// the image extension's (image_extension.go), one extension at a time, so every response is
// classified by the same single-item rules and the same definite-vs-UNCONFIRMED split:
//
//  1. AdExtensionsAssociations/Query, once per TYPE, filtered to that type on this campaign.
//     A campaign that already has an extension of the type (a retry that reused the campaign,
//     or a human) gets none of that type added, so a retry never doubles them. A lookup that
//     cannot answer adds nothing of that type: it cannot tell a first attach from a retry.
//  2. AddAdExtensions — POST AdExtensions with ONE extension:
//     {Type "SitelinkAdExtension", DisplayText, Description1?, Description2?, FinalUrls [url]}
//     or {Type "CalloutAdExtension", Text}.
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/sitelinkadextension?view=bingads-13
//     https://learn.microsoft.com/en-us/advertising/campaign-management-service/calloutadextension?view=bingads-13
//  3. SetAdExtensionsAssociations — POST AdExtensionsAssociations/Set, AssociationType
//     "Campaign", for that one extension.
//
// One call per extension rather than one batch is deliberate: every classifier in this client
// that can tell "rejected" from "may exist" is built for one item at Index 0, and a batch
// would need positional attribution of AdExtensionIdentities and NestedPartialErrors that
// nothing here has been verified against. The cost is at most 2 × 40 small calls.
//
// Limits, from the v13 references above:
//   - Sitelink DisplayText: required; 25 characters when Description1/2 is set, else 35.
//     "If any Traditional Chinese characters are included, the limits are 11 ... and 15".
//   - Sitelink Description1/Description2: 35 each (15 with Traditional Chinese), and each
//     requires the other.
//   - Sitelink FinalUrls: required, http/https, at most 2,048 characters.
//   - "Each entity (account, campaign, or ad group) can be associated with up to 20 sitelink
//     ad extensions."
//   - Callout Text: 1–25 characters (12 with Traditional Chinese).
//   - "You must associate between 2 and 20 callout ad extensions per entity ... If you
//     associate one or fewer callout extensions ... no callout text will serve."
//
// The reduced limits are applied to any double-width text (hasDoubleWidth), as the RSA copy
// caps already are: Microsoft names Traditional Chinese, and a conservative cap never sends an
// over-length extension.
// ---------------------------------------------------------------------------

const (
	adExtensionTypeSitelink = "SitelinkAdExtension"
	adExtensionTypeCallout  = "CalloutAdExtension"

	// MaxSitelinks / MinCallouts / MaxCallouts are the per-campaign association limits.
	MaxSitelinks = 20
	MinCallouts  = 2
	MaxCallouts  = 20

	maxSitelinkTextRunes         = 35
	maxSitelinkTextWithDescRunes = 25
	maxSitelinkDescRunes         = 35
	maxSitelinkTextWideRunes     = 15
	maxSitelinkTextWithDescWide  = 11
	maxSitelinkDescWideRunes     = 15
	maxSitelinkURLChars          = 2048
	maxCalloutRunes              = 25
	maxCalloutWideRunes          = 12

	// FieldSitelinks / FieldCallouts are the microsoftConfig keys, named in refusals.
	FieldSitelinks = "sitelinks"
	FieldCallouts  = "callouts"
)

// Sitelink is one requested sitelink extension.
type Sitelink struct {
	Text         string
	Description1 string
	Description2 string
	FinalURL     string
}

// TextExtensionError is a refusal of one input list, naming the microsoftConfig field.
type TextExtensionError struct {
	Field string
	Msg   string
}

func (e *TextExtensionError) Error() string {
	return fmt.Sprintf("microsoft-ads: %s: %s", e.Field, e.Msg)
}

func textExtErr(field, format string, args ...any) error {
	return &TextExtensionError{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// ValidateTextExtensions checks the sitelink and callout lists WITHOUT contacting Microsoft and
// returns them trimmed, index-aligned with the input. Both lists empty is valid (nothing to
// attach). Every refusal is a *TextExtensionError.
func ValidateTextExtensions(sitelinks []Sitelink, callouts []string) ([]Sitelink, []string, error) {
	outLinks, err := validateSitelinks(sitelinks)
	if err != nil {
		return nil, nil, err
	}
	outCallouts, err := validateCallouts(callouts)
	if err != nil {
		return nil, nil, err
	}
	return outLinks, outCallouts, nil
}

func validateSitelinks(in []Sitelink) ([]Sitelink, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxSitelinks {
		return nil, textExtErr(FieldSitelinks, "at most %d sitelinks can be associated with a campaign, got %d", MaxSitelinks, len(in))
	}
	out := make([]Sitelink, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, s := range in {
		text, err := extensionText(s.Text)
		if err != nil {
			return nil, textExtErr(FieldSitelinks, "[%d].text %s", i, err)
		}
		d1, err := optionalExtensionText(s.Description1)
		if err != nil {
			return nil, textExtErr(FieldSitelinks, "[%d].description1 %s", i, err)
		}
		d2, err := optionalExtensionText(s.Description2)
		if err != nil {
			return nil, textExtErr(FieldSitelinks, "[%d].description2 %s", i, err)
		}
		if (d1 == "") != (d2 == "") {
			return nil, textExtErr(FieldSitelinks, "[%d]: description1 and description2 must be set together or both omitted", i)
		}
		limit := maxSitelinkTextRunes
		switch {
		case d1 != "" && hasDoubleWidth(text):
			limit = maxSitelinkTextWithDescWide
		case d1 != "":
			limit = maxSitelinkTextWithDescRunes
		case hasDoubleWidth(text):
			limit = maxSitelinkTextWideRunes
		}
		if n := utf8.RuneCountInString(text); n > limit {
			return nil, textExtErr(FieldSitelinks, "[%d].text is %d characters; the limit is %d", i, n, limit)
		}
		for j, d := range []string{d1, d2} {
			dl := maxSitelinkDescRunes
			if hasDoubleWidth(d) {
				dl = maxSitelinkDescWideRunes
			}
			if n := utf8.RuneCountInString(d); n > dl {
				return nil, textExtErr(FieldSitelinks, "[%d].description%d is %d characters; the limit is %d", i, j+1, n, dl)
			}
		}
		u, err := sitelinkURL(s.FinalURL)
		if err != nil {
			return nil, textExtErr(FieldSitelinks, "[%d].finalUrl %s", i, err)
		}
		key := textfold.Key(text)
		if _, dup := seen[key]; dup {
			return nil, textExtErr(FieldSitelinks, "[%d].text repeats an earlier sitelink", i)
		}
		seen[key] = struct{}{}
		out = append(out, Sitelink{Text: text, Description1: d1, Description2: d2, FinalURL: u})
	}
	return out, nil
}

func validateCallouts(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) < MinCallouts {
		return nil, textExtErr(FieldCallouts, "at least %d callouts are required (Microsoft serves none with fewer), got %d", MinCallouts, len(in))
	}
	if len(in) > MaxCallouts {
		return nil, textExtErr(FieldCallouts, "at most %d callouts can be associated with a campaign, got %d", MaxCallouts, len(in))
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, raw := range in {
		text, err := extensionText(raw)
		if err != nil {
			return nil, textExtErr(FieldCallouts, "[%d] %s", i, err)
		}
		limit := maxCalloutRunes
		if hasDoubleWidth(text) {
			limit = maxCalloutWideRunes
		}
		if n := utf8.RuneCountInString(text); n > limit {
			return nil, textExtErr(FieldCallouts, "[%d] is %d characters; the limit is %d", i, n, limit)
		}
		key := textfold.Key(text)
		if _, dup := seen[key]; dup {
			return nil, textExtErr(FieldCallouts, "[%d] repeats an earlier callout", i)
		}
		seen[key] = struct{}{}
		out = append(out, text)
	}
	return out, nil
}

// extensionText trims s and refuses an empty value or one carrying a control character
// (checked on the raw value, so a trailing newline cannot hide behind the trim).
func extensionText(s string) (string, error) {
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("contains a control character")
	}
	t := strings.TrimSpace(s)
	if t == "" {
		return "", fmt.Errorf("is empty")
	}
	return t, nil
}

// optionalExtensionText is extensionText where an empty (or whitespace-only) value is absent.
func optionalExtensionText(s string) (string, error) {
	if strings.TrimSpace(s) == "" && strings.IndexFunc(s, unicode.IsControl) < 0 {
		return "", nil
	}
	return extensionText(s)
}

// sitelinkURL accepts an absolute http/https URL with a host, at most 2,048 characters, and
// returns it trimmed but otherwise as given. The URL itself is never quoted in an error.
func sitelinkURL(raw string) (string, error) {
	// Control characters are checked on the RAW value, as extensionText does, so a leading or
	// trailing one cannot hide behind the trim below.
	if strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("contains a control character")
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("is required")
	}
	// Microsoft's limit is in characters, so a non-ASCII path is counted by code point.
	if n := utf8.RuneCountInString(s); n > maxSitelinkURLChars {
		return "", fmt.Errorf("is %d characters; the limit is %d", n, maxSitelinkURLChars)
	}
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return "", fmt.Errorf("contains whitespace or a control character")
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("must be an absolute http or https URL")
	}
	// Credentials in the URL would be forwarded to Microsoft and served in the ad; the ad
	// destination validator (validateAdURL) refuses them for the same reason.
	if u.User != nil {
		return "", fmt.Errorf("must not carry user credentials")
	}
	return s, nil
}

// ---- wire shapes ------------------------------------------------------------

type msTextAdExtension struct {
	Type         string   `json:"Type"`
	DisplayText  string   `json:"DisplayText,omitempty"`
	Description1 string   `json:"Description1,omitempty"`
	Description2 string   `json:"Description2,omitempty"`
	FinalUrls    []string `json:"FinalUrls,omitempty"`
	Text         string   `json:"Text,omitempty"`
}

type addTextAdExtensionsRequest struct {
	AccountId    json.Number         `json:"AccountId"`
	AdExtensions []msTextAdExtension `json:"AdExtensions"`
}

// ---- the operation ----------------------------------------------------------

// TextExtensionsResult reports what AttachCampaignTextExtensions did. Ids are those this run
// created AND associated; Steps is the human-readable trail (ids and fixed text only).
type TextExtensionsResult struct {
	SitelinkAdExtensionIDs []string
	CalloutAdExtensionIDs  []string
	Steps                  []string
	// Failed counts extensions NOT attached (definite); Unconfirmed counts those that may be.
	// A type skipped because its lookup failed counts every requested item of it as Failed.
	Failed, Unconfirmed int
	// CalloutsBelowMinimum is set when the campaign is left with fewer than MinCallouts callouts
	// associated — after this run, or as a retry found it — so Microsoft serves none.
	CalloutsBelowMinimum bool
}

// Degraded reports whether any requested extension is not, or may not be, attached, or the
// campaign's callouts cannot serve.
func (r *TextExtensionsResult) Degraded() bool {
	return r.Failed > 0 || r.Unconfirmed > 0 || r.CalloutsBelowMinimum
}

// AttachCampaignTextExtensions validates the lists, then attaches each sitelink and callout to
// the campaign, skipping a type the campaign already has. It never stops at one extension's
// failure: each is reported in Steps and counted, and the result is never nil. The error is
// non-nil only for a refusal before any request (bad ids or input), when nothing was sent.
func (c *Client) AttachCampaignTextExtensions(ctx context.Context, campaignID string, sitelinks []Sitelink, callouts []string) (*TextExtensionsResult, error) {
	r := &TextExtensionsResult{}
	if err := c.validateAccountIDs(); err != nil {
		return r, err
	}
	if !idRE.MatchString(campaignID) {
		return r, fmt.Errorf("microsoft-ads text extensions: campaign id %q is not a Microsoft id", clipID(campaignID))
	}
	links, calls, err := ValidateTextExtensions(sitelinks, callouts)
	if err != nil {
		return r, err
	}
	account := json.Number(c.account.AccountID)
	campaign := json.Number(campaignID)

	if len(links) > 0 {
		exts := make([]msTextAdExtension, len(links))
		for i, s := range links {
			exts[i] = msTextAdExtension{Type: adExtensionTypeSitelink, DisplayText: s.Text, Description1: s.Description1, Description2: s.Description2, FinalUrls: []string{s.FinalURL}}
		}
		r.SitelinkAdExtensionIDs = c.attachTextExtensionsOfType(ctx, r, account, campaign, adExtensionTypeSitelink, "sitelink", exts)
	}
	if len(calls) > 0 {
		exts := make([]msTextAdExtension, len(calls))
		for i, t := range calls {
			exts[i] = msTextAdExtension{Type: adExtensionTypeCallout, Text: t}
		}
		r.CalloutAdExtensionIDs = c.attachTextExtensionsOfType(ctx, r, account, campaign, adExtensionTypeCallout, "callout", exts)
	}
	return r, nil
}

// attachTextExtensionsOfType runs the find-first lookup for one type and then creates and
// associates each extension, recording outcomes on r. It returns the ids attached.
func (c *Client) attachTextExtensionsOfType(ctx context.Context, r *TextExtensionsResult, account, campaign json.Number, extType, label string, exts []msTextAdExtension) []string {
	existing, count, err := c.findCampaignExtension(ctx, account, campaign, extType, "a "+label)
	if err != nil {
		// Only a read was sent for this type, so nothing of it can have been attached.
		r.Failed += len(exts)
		r.Steps = append(r.Steps, fmt.Sprintf("%s extensions NOT attached: could not check the campaign for existing %s extensions, so none was added: %v", capitalize(label), label, err))
		return nil
	}
	if existing != "" {
		r.Steps = append(r.Steps, fmt.Sprintf("%s extensions already associated with the campaign (%d, e.g. %s): none added", capitalize(label), count, existing))
		// Still nothing is added — the earlier run's outcome is unknown item by item, and adding
		// would risk doubling — but a campaign holding one callout serves none, so say so.
		if extType == adExtensionTypeCallout && count < MinCallouts {
			r.CalloutsBelowMinimum = true
			r.Steps = append(r.Steps, fmt.Sprintf("Callout extensions: only %d associated with the campaign; Microsoft serves none with fewer than %d — add more in Microsoft Advertising", count, MinCallouts))
		}
		return nil
	}
	var ids []string
	for i, ext := range exts {
		id, err := c.addOneTextExtension(ctx, account, campaign, ext)
		switch {
		case err == nil:
			ids = append(ids, id)
		case IsOutcomeUnconfirmed(err):
			r.Unconfirmed++
			r.Steps = append(r.Steps, fmt.Sprintf("%s extension %d UNCONFIRMED (it may be attached — verify in Microsoft Advertising before re-attaching): %v", capitalize(label), i, err))
		default:
			r.Failed++
			r.Steps = append(r.Steps, fmt.Sprintf("%s extension %d NOT attached: %v", capitalize(label), i, err))
		}
	}
	if len(ids) > 0 {
		r.Steps = append(r.Steps, fmt.Sprintf("%s extensions associated with campaign %s: %s", capitalize(label), campaign, strings.Join(ids, ",")))
	}
	if extType == adExtensionTypeCallout && len(ids) < MinCallouts {
		r.CalloutsBelowMinimum = true
	}
	return ids
}

// addOneTextExtension creates one extension and associates it with the campaign. A failure is
// classified by classifyImageStep's rules (definite vs UNCONFIRMED).
func (c *Client) addOneTextExtension(ctx context.Context, account, campaign json.Number, ext msTextAdExtension) (string, error) {
	body, err := c.doRequest(ctx, http.MethodPost, "AdExtensions", addTextAdExtensionsRequest{
		AccountId:    account,
		AdExtensions: []msTextAdExtension{ext},
	}, false)
	var id string
	if err == nil {
		id, err = adExtensionIdentity(body)
	}
	if err != nil {
		return "", classifyImageStep("extension create", err)
	}
	if err := c.associateCampaignExtension(ctx, account, campaign, id); err != nil {
		return "", classifyImageStep(fmt.Sprintf("campaign association of extension %s", id), err)
	}
	return id, nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
