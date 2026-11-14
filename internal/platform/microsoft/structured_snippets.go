// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/textfold"
)

// ---------------------------------------------------------------------------
// Structured snippet ad extensions at create (#563, gap 13).
//
// A Search campaign created here may carry campaign-level StructuredSnippetAdExtension entries,
// attached right after the campaign is confirmed through the sitelink/callout transport
// (text_extensions.go): one association read for the type, then one AddAdExtensions and one
// SetAdExtensionsAssociations per snippet, each failure classified definite or UNCONFIRMED.
//
//	{Type "StructuredSnippetAdExtension", Header, Values [..]}
//	https://learn.microsoft.com/en-us/advertising/campaign-management-service/structuredsnippetadextension?view=bingads-13
//
// Limits, from the v13 reference:
//   - Header: required, one of Microsoft's predefined headers. The English set is held here as a
//     closed enum (matched ignoring case, sent in Microsoft's spelling), because a header outside
//     it is refused upstream only after the campaign exists.
//   - Values: required, between 3 and 10, each at most 25 characters (the reduced limit applies to
//     double-width text, as for callouts).
//
// This service caps a campaign at 10 snippets and refuses a repeated header or a repeated value
// within a snippet, as googleAdsConfig.structuredSnippets does: a duplicate never adds a line.
// ---------------------------------------------------------------------------

const (
	adExtensionTypeStructuredSnippet = "StructuredSnippetAdExtension"

	// FieldStructuredSnippets is the microsoftConfig key, named in refusals.
	FieldStructuredSnippets = "structuredSnippets"
	// MaxStructuredSnippets is this service's per-campaign cap.
	MaxStructuredSnippets = 10
	// MinSnippetValues / MaxSnippetValues are Microsoft's per-snippet value counts.
	MinSnippetValues = 3
	MaxSnippetValues = 10

	maxSnippetValueRunes     = 25
	maxSnippetValueWideRunes = 12
)

// structuredSnippetHeaders is Microsoft's English header set, in Microsoft's spelling.
var structuredSnippetHeaders = []string{
	"Amenities", "Brands", "Courses", "Degree programs", "Destinations", "Featured hotels",
	"Insurance coverage", "Models", "Neighborhoods", "Service catalog", "Shows", "Styles", "Types",
}

// StructuredSnippet is one requested "Header: value, value, value" extension.
type StructuredSnippet struct {
	Header string
	Values []string
}

// canonicalSnippetHeader returns Microsoft's spelling of h (trimmed, matched ignoring case and
// inner-space runs), or "" when h is not a supported header.
func canonicalSnippetHeader(h string) string {
	key := strings.ToLower(strings.Join(strings.Fields(h), " "))
	for _, c := range structuredSnippetHeaders {
		if strings.ToLower(c) == key {
			return c
		}
	}
	return ""
}

// ValidateStructuredSnippets checks the list WITHOUT contacting Microsoft and returns it with
// headers in Microsoft's spelling and values trimmed, index-aligned with the input. An empty list
// is valid (nothing to attach). Every refusal is a *TextExtensionError naming the field and a
// 0-based position, as the sitelink refusals do.
func ValidateStructuredSnippets(in []StructuredSnippet) ([]StructuredSnippet, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxStructuredSnippets {
		return nil, textExtErr(FieldStructuredSnippets, "at most %d structured snippets are accepted, got %d", MaxStructuredSnippets, len(in))
	}
	out := make([]StructuredSnippet, 0, len(in))
	headers := make(map[string]struct{}, len(in))
	for i, s := range in {
		raw, err := extensionText(s.Header)
		if err != nil {
			return nil, textExtErr(FieldStructuredSnippets, "[%d].header %s", i, err)
		}
		header := canonicalSnippetHeader(raw)
		if header == "" {
			return nil, textExtErr(FieldStructuredSnippets, "[%d].header must be one of %s", i, strings.Join(structuredSnippetHeaders, ", "))
		}
		if _, dup := headers[header]; dup {
			return nil, textExtErr(FieldStructuredSnippets, "[%d].header repeats an earlier snippet's header", i)
		}
		headers[header] = struct{}{}
		if n := len(s.Values); n < MinSnippetValues || n > MaxSnippetValues {
			return nil, textExtErr(FieldStructuredSnippets, "[%d].values must hold between %d and %d values, got %d", i, MinSnippetValues, MaxSnippetValues, n)
		}
		values := make([]string, 0, len(s.Values))
		seen := make(map[string]struct{}, len(s.Values))
		for j, v := range s.Values {
			text, err := extensionText(v)
			if err != nil {
				return nil, textExtErr(FieldStructuredSnippets, "[%d].values[%d] %s", i, j, err)
			}
			limit := maxSnippetValueRunes
			if hasDoubleWidth(text) {
				limit = maxSnippetValueWideRunes
			}
			if n := utf8.RuneCountInString(text); n > limit {
				return nil, textExtErr(FieldStructuredSnippets, "[%d].values[%d] is %d characters; the limit is %d", i, j, n, limit)
			}
			key := textfold.Key(text)
			if _, dup := seen[key]; dup {
				return nil, textExtErr(FieldStructuredSnippets, "[%d].values[%d] repeats an earlier value", i, j)
			}
			seen[key] = struct{}{}
			values = append(values, text)
		}
		out = append(out, StructuredSnippet{Header: header, Values: values})
	}
	return out, nil
}

// AttachCampaignStructuredSnippets validates the list, then attaches each snippet to the
// campaign, unless the campaign already has a structured snippet (a retry that reused it, or a
// human), when none is added. It never stops at one snippet's failure: each is reported in Steps
// and counted, and the result is never nil. The error is non-nil only for a refusal before any
// request (bad ids or input), when nothing was sent.
func (c *Client) AttachCampaignStructuredSnippets(ctx context.Context, campaignID string, snippets []StructuredSnippet) (*TextExtensionsResult, error) {
	r := &TextExtensionsResult{}
	if err := c.validateAccountIDs(); err != nil {
		return r, err
	}
	if !idRE.MatchString(campaignID) {
		return r, fmt.Errorf("microsoft-ads structured snippets: campaign id %q is not a Microsoft id", clipID(campaignID))
	}
	valid, err := ValidateStructuredSnippets(snippets)
	if err != nil {
		return r, err
	}
	if len(valid) == 0 {
		return r, nil
	}
	exts := make([]msTextAdExtension, len(valid))
	for i, s := range valid {
		exts[i] = msTextAdExtension{Type: adExtensionTypeStructuredSnippet, Header: s.Header, Values: s.Values}
	}
	r.StructuredSnippetAdExtensionIDs = c.attachTextExtensionsOfType(ctx, r, json.Number(c.account.AccountID), json.Number(campaignID),
		adExtensionTypeStructuredSnippet, "structured snippet", exts)
	return r, nil
}
