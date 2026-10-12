// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Lead form extensions (LFXV2-2665)
//
// The seventh member of the asset oneof, and the one assets_extended.go named as
// deliberately left to its own change. Structurally it is the same two mutates
// every other extension uses — assets:mutate, then campaignAssets:mutate with an
// AssetFieldType — and assets.go owns all of that. What is different enough to
// justify its own file is the shape: a lead form is not a line of text with a
// link, it is a FORM, with a field list, a post-submit screen and a privacy
// policy, and every one of those has its own refusal.
//
// Two things about it are unlike every sibling extension:
//
//   - It CHANGES WHERE THE LEAD GOES. A sitelink sends the user to the
//     advertiser's site; a lead form collects the user's name and email inside
//     Google and holds them for retrieval. Attaching one to a campaign whose
//     brief assumed site registrations silently changes what "a conversion"
//     means for that campaign, which is why the privacy-policy URL is required
//     by Google rather than optional, and why this client refuses a form with no
//     fields rather than creating an empty one.
//   - Only ONE may be linked per campaign. Google documents a single lead form
//     per campaign (or per ad group), so a second would be created as an
//     account-level asset and then refused at the LINK — leaving exactly the
//     "litter rather than a leak" this package already names. Refusing the
//     second locally costs nothing a caller could have had.
//
// The lead form's optional BACKGROUND IMAGE is a named gap, for the reason the
// IMAGE extension is: it carries bytes, so supporting it would give the Search
// cascade a network fetch phase it does not have. A form without one renders
// with Google's default background; it is not unservable.
// ---------------------------------------------------------------------------

const (
	// Google links one lead form per campaign. This is an UPSTREAM limit, not a
	// payload bound — see the file comment.
	maxLeadFormExtensions = 1
	// Google's documented per-field limits, in runes for the reason every other
	// extension counts runes: see assets.go.
	maxLeadFormBusinessNameRunes     = 25
	maxLeadFormHeadlineRunes         = 30
	maxLeadFormDescriptionRunes      = 200
	maxLeadFormCTADescriptionRunes   = 30
	maxLeadFormPostHeadlineRunes     = 25
	maxLeadFormPostDescriptionRunes  = 200
	maxLeadFormCustomDisclosureRunes = 200
	// A payload sanity bound, deliberately set FAR ABOVE Google's own ceiling so it
	// can never be the thing that refuses a form Google would have accepted. The
	// real ceiling is upstream and structural: fields are de-duplicated on the
	// input type just below, so a form cannot hold more distinct fields than
	// LeadFormFieldUserInputType has members, and that enum has well under half
	// this many. What is left for this bound to catch is the only case the
	// de-duplication cannot — a malformed brief sending thousands of DISTINCT
	// shape-matching strings, which `enumShapeRE` admits because it anchors the
	// shape and not the vocabulary.
	//
	// It was 12, which is roughly where a human stops wanting to fill in a form and
	// nowhere near where Google stops accepting one. That made it an over-refusal:
	// a thirteen-field brief Google would have taken, stopped here instead. A bound
	// chosen for payload sanity must be set where only nonsense reaches it.
	maxLeadFormFields = 64
)

const assetFieldLeadForm = "LEAD_FORM"

// LeadFormExtension is a lead form shown with the ad, collecting the fields it
// names inside Google rather than sending the user to a landing page.
//
// Everything Google requires is required here: a form missing any of them is
// rejected at the assets:mutate, which on this cascade happens AFTER the budget
// and the campaign exist.
type LeadFormExtension struct {
	// BusinessName, Headline and Description are what the user sees before
	// deciding to open the form. All required.
	BusinessName string
	Headline     string
	Description  string
	// CallToActionType is the button label enum (SIGN_UP, LEARN_MORE, …) and
	// CallToActionDescription the line beside it. Both required.
	CallToActionType        string
	CallToActionDescription string
	// PrivacyPolicyURL is required BY GOOGLE, not by this client's preference: a
	// form collecting personal data without one is refused upstream. It is NOT
	// UTM-tagged — see validateServableURL for why.
	PrivacyPolicyURL string
	// Fields are the input-type enums the form asks for (FULL_NAME, EMAIL,
	// PHONE_NUMBER, …), in the order they are shown. At least one.
	Fields []string
	// PostSubmit* are the thank-you screen. Optional as a group: Google renders
	// its own default when they are absent. PostSubmitCallToActionType is the
	// button on that screen (VISIT_SITE, DOWNLOAD, …).
	PostSubmitHeadline         string
	PostSubmitDescription      string
	PostSubmitCallToActionType string
	// DesiredIntent is LOW_INTENT or HIGH_INTENT — whether Google optimises for
	// volume or for qualified leads. Optional; absent means Google's default.
	DesiredIntent string
	// CustomDisclosure is extra legal text shown under the form. Optional, and
	// only permitted on accounts Google has allow-listed for it, which this
	// client cannot check — an account without the allow-list is refused at the
	// mutate.
	CustomDisclosure string
}

// leadFormField is one input on the form.
type leadFormField struct {
	InputType string `json:"inputType"`
}

// leadFormAsset is the wire shape of LeadFormAsset. Every optional key is
// omitempty: an absent post-submit screen must be ABSENT, not an empty string,
// which Google rejects as an invalid value rather than reading as "unset".
type leadFormAsset struct {
	BusinessName               string          `json:"businessName"`
	CallToActionType           string          `json:"callToActionType"`
	CallToActionDescription    string          `json:"callToActionDescription"`
	Headline                   string          `json:"headline"`
	Description                string          `json:"description"`
	PrivacyPolicyURL           string          `json:"privacyPolicyUrl"`
	Fields                     []leadFormField `json:"fields"`
	PostSubmitHeadline         string          `json:"postSubmitHeadline,omitempty"`
	PostSubmitDescription      string          `json:"postSubmitDescription,omitempty"`
	PostSubmitCallToActionType string          `json:"postSubmitCallToActionType,omitempty"`
	DesiredIntent              string          `json:"desiredIntent,omitempty"`
	CustomDisclosure           string          `json:"customDisclosure,omitempty"`
}

// validateLeadFormExtensions checks each lead form and builds its asset.
//
// PURE: no network, no clock. It runs inside preflightCampaignKind, before the
// budget mutate, like every other extension validator.
func validateLeadFormExtensions(in CampaignInput) ([]assetCreate, error) {
	forms := in.LeadForms
	if len(forms) > maxLeadFormExtensions {
		return nil, fmt.Errorf("google-ads campaign accepts %d lead form, got %d; Google links one lead form per campaign, and a second would be created as an asset and then refused at the link", maxLeadFormExtensions, len(forms))
	}
	out := make([]assetCreate, 0, len(forms))
	for i, f := range forms {
		asset := &leadFormAsset{}

		var err error
		if asset.BusinessName, err = requiredLeadFormText(i, "business name", f.BusinessName, maxLeadFormBusinessNameRunes); err != nil {
			return nil, err
		}
		if asset.Headline, err = requiredLeadFormText(i, "headline", f.Headline, maxLeadFormHeadlineRunes); err != nil {
			return nil, err
		}
		if asset.Description, err = requiredLeadFormText(i, "description", f.Description, maxLeadFormDescriptionRunes); err != nil {
			return nil, err
		}
		if asset.CallToActionDescription, err = requiredLeadFormText(i, "call-to-action description", f.CallToActionDescription, maxLeadFormCTADescriptionRunes); err != nil {
			return nil, err
		}

		cta := strings.TrimSpace(f.CallToActionType)
		if cta == "" {
			return nil, fmt.Errorf("google-ads lead form %d has no call-to-action type; Google needs the button label, such as SIGN_UP", i)
		}
		if !enumShapeRE.MatchString(cta) {
			return nil, fmt.Errorf("google-ads lead form %d call-to-action type %q is not a call-to-action name; Google spells these in upper case with underscores, such as SIGN_UP", i, capForError(f.CallToActionType))
		}
		asset.CallToActionType = cta

		// Required by Google. Validated but NOT tagged: a privacy policy is not
		// an ad destination, and tagging it would attribute a policy read as an
		// ad click.
		policy, err := validateServableURL(fmt.Sprintf("lead form %d privacy policy URL", i), f.PrivacyPolicyURL)
		if err != nil {
			return nil, fmt.Errorf("google-ads lead form %d is unservable: %w", i, err)
		}
		asset.PrivacyPolicyURL = policy.String()
		if n := len(asset.PrivacyPolicyURL); n > maxFinalURLBytes {
			return nil, fmt.Errorf("google-ads lead form %d privacy policy URL is %d bytes, exceeding the %d limit", i, n, maxFinalURLBytes)
		}

		if len(f.Fields) == 0 {
			return nil, fmt.Errorf("google-ads lead form %d asks for no fields; a form that collects nothing cannot generate a lead, and Google refuses it", i)
		}
		if len(f.Fields) > maxLeadFormFields {
			return nil, fmt.Errorf("google-ads lead form %d accepts at most %d fields in one request (a bound on this request's payload, not an upstream limit), got %d", i, maxLeadFormFields, len(f.Fields))
		}
		// De-duplicated on the input type itself: Google renders one input per
		// type, so a repeated EMAIL is not a second box, it is a form that fails
		// to validate upstream.
		seenField := make(map[string]struct{}, len(f.Fields))
		for j, raw := range f.Fields {
			input := strings.TrimSpace(raw)
			if input == "" {
				return nil, fmt.Errorf("google-ads lead form %d field %d is empty", i, j)
			}
			if !enumShapeRE.MatchString(input) {
				return nil, fmt.Errorf("google-ads lead form %d field %q is not a field name; Google spells these in upper case with underscores, such as FULL_NAME or EMAIL", i, capForError(raw))
			}
			if _, dup := seenField[input]; dup {
				// Capped like every other caller-echoing arm: `enumShapeRE` anchors the
				// SHAPE, not the length, so a 20k-character run of underscores reaches
				// here having matched.
				return nil, fmt.Errorf("google-ads lead form %d asks for %q twice; Google renders one input per field type", i, capForError(input))
			}
			seenField[input] = struct{}{}
			asset.Fields = append(asset.Fields, leadFormField{InputType: input})
		}

		// The post-submit screen is optional AS A GROUP but not field by field:
		// a headline with no description renders a half-written thank-you screen
		// the caller cannot see before it is live. This is the sitelink
		// description rule, for the same reason.
		postHeadline := strings.TrimSpace(f.PostSubmitHeadline)
		postDescription := strings.TrimSpace(f.PostSubmitDescription)
		if (postHeadline == "") != (postDescription == "") {
			return nil, fmt.Errorf("google-ads lead form %d needs both a post-submit headline and a post-submit description or neither; one without the other renders a half-written thank-you screen", i)
		}
		if postHeadline != "" {
			if n := utf8.RuneCountInString(postHeadline); n > maxLeadFormPostHeadlineRunes {
				return nil, fmt.Errorf("google-ads lead form %d post-submit headline is %d characters, exceeding the %d limit", i, n, maxLeadFormPostHeadlineRunes)
			}
			if n := utf8.RuneCountInString(postDescription); n > maxLeadFormPostDescriptionRunes {
				return nil, fmt.Errorf("google-ads lead form %d post-submit description is %d characters, exceeding the %d limit", i, n, maxLeadFormPostDescriptionRunes)
			}
			asset.PostSubmitHeadline = postHeadline
			asset.PostSubmitDescription = postDescription
		}
		// The post-submit button stands alone: Google renders it on its own
		// default screen too, so it is NOT part of the pair above.
		if postCTA := strings.TrimSpace(f.PostSubmitCallToActionType); postCTA != "" {
			if !enumShapeRE.MatchString(postCTA) {
				return nil, fmt.Errorf("google-ads lead form %d post-submit call-to-action type %q is not a call-to-action name; Google spells these in upper case with underscores, such as VISIT_SITE", i, capForError(f.PostSubmitCallToActionType))
			}
			asset.PostSubmitCallToActionType = postCTA
		}

		if intent := strings.TrimSpace(f.DesiredIntent); intent != "" {
			if !enumShapeRE.MatchString(intent) {
				return nil, fmt.Errorf("google-ads lead form %d desired intent %q is not an intent name; Google spells these in upper case with underscores, such as LOW_INTENT", i, capForError(f.DesiredIntent))
			}
			asset.DesiredIntent = intent
		}

		if disclosure := strings.TrimSpace(f.CustomDisclosure); disclosure != "" {
			if n := utf8.RuneCountInString(disclosure); n > maxLeadFormCustomDisclosureRunes {
				return nil, fmt.Errorf("google-ads lead form %d custom disclosure is %d characters, exceeding the %d limit", i, n, maxLeadFormCustomDisclosureRunes)
			}
			asset.CustomDisclosure = disclosure
		}

		out = append(out, assetCreate{LeadFormAsset: asset})
	}
	return out, nil
}

// requiredLeadFormText trims, refuses empty, and bounds by RUNE count — the
// count every extension in this package uses, and not the double-width weight
// ad_copy.go applies to generated RSA copy. See assets.go for why the two differ.
func requiredLeadFormText(i int, label, raw string, maxRunes int) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("google-ads lead form %d has no %s", i, label)
	}
	if n := utf8.RuneCountInString(s); n > maxRunes {
		return "", fmt.Errorf("google-ads lead form %d %s is %d characters, exceeding the %d limit", i, label, n, maxRunes)
	}
	return s, nil
}
