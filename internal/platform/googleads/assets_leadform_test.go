// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"strings"
	"testing"
)

// sampleLeadForm is a fully-populated, valid lead form. Every refusal test below
// starts from this and breaks exactly one thing, so a test that fails is
// pointing at the field it names and not at an incidentally invalid fixture.
func sampleLeadForm() LeadFormExtension {
	return LeadFormExtension{
		BusinessName:               "Linux Foundation",
		Headline:                   "Register for KubeCon",
		Description:                "Join 12,000 engineers for four days of cloud native talks and workshops.",
		CallToActionType:           "SIGN_UP",
		CallToActionDescription:    "Save your seat today",
		PrivacyPolicyURL:           "https://www.linuxfoundation.org/legal/privacy",
		Fields:                     []string{"FULL_NAME", "EMAIL", "COMPANY_NAME"},
		PostSubmitHeadline:         "You are registered",
		PostSubmitDescription:      "We have emailed your confirmation and the schedule.",
		PostSubmitCallToActionType: "VISIT_SITE",
		DesiredIntent:              "HIGH_INTENT",
		CustomDisclosure:           "Your details are shared with the event organiser.",
	}
}

func planLeadForm(t *testing.T, f LeadFormExtension) *leadFormAsset {
	t.Helper()
	assets, err := validateLeadFormExtensions(CampaignInput{LeadForms: []LeadFormExtension{f}})
	if err != nil {
		t.Fatalf("validateLeadFormExtensions: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("got %d assets, want 1", len(assets))
	}
	if assets[0].LeadFormAsset == nil {
		t.Fatal("asset carries no LeadFormAsset")
	}
	return assets[0].LeadFormAsset
}

func refuseLeadForm(t *testing.T, f LeadFormExtension, wantFragment string) {
	t.Helper()
	_, err := validateLeadFormExtensions(CampaignInput{LeadForms: []LeadFormExtension{f}})
	if err == nil {
		t.Fatalf("expected a refusal mentioning %q, got none", wantFragment)
	}
	if !strings.Contains(err.Error(), wantFragment) {
		t.Errorf("error = %q, want it to mention %q", err, wantFragment)
	}
}

func TestValidateLeadFormExtensions_MapsEveryField(t *testing.T) {
	in := sampleLeadForm()
	asset := planLeadForm(t, in)

	if asset.BusinessName != in.BusinessName {
		t.Errorf("businessName = %q", asset.BusinessName)
	}
	if asset.Headline != in.Headline {
		t.Errorf("headline = %q", asset.Headline)
	}
	if asset.Description != in.Description {
		t.Errorf("description = %q", asset.Description)
	}
	if asset.CallToActionType != in.CallToActionType {
		t.Errorf("callToActionType = %q", asset.CallToActionType)
	}
	if asset.CallToActionDescription != in.CallToActionDescription {
		t.Errorf("callToActionDescription = %q", asset.CallToActionDescription)
	}
	if asset.PrivacyPolicyURL != in.PrivacyPolicyURL {
		t.Errorf("privacyPolicyUrl = %q, want %q", asset.PrivacyPolicyURL, in.PrivacyPolicyURL)
	}
	if len(asset.Fields) != 3 ||
		asset.Fields[0].InputType != "FULL_NAME" ||
		asset.Fields[1].InputType != "EMAIL" ||
		asset.Fields[2].InputType != "COMPANY_NAME" {
		t.Errorf("fields = %+v, want the three input types in order", asset.Fields)
	}
	if asset.PostSubmitHeadline != in.PostSubmitHeadline {
		t.Errorf("postSubmitHeadline = %q", asset.PostSubmitHeadline)
	}
	if asset.PostSubmitDescription != in.PostSubmitDescription {
		t.Errorf("postSubmitDescription = %q", asset.PostSubmitDescription)
	}
	if asset.PostSubmitCallToActionType != in.PostSubmitCallToActionType {
		t.Errorf("postSubmitCallToActionType = %q", asset.PostSubmitCallToActionType)
	}
	if asset.DesiredIntent != in.DesiredIntent {
		t.Errorf("desiredIntent = %q", asset.DesiredIntent)
	}
	if asset.CustomDisclosure != in.CustomDisclosure {
		t.Errorf("customDisclosure = %q", asset.CustomDisclosure)
	}
}

// Google links ONE lead form per campaign. A second would be created as an
// account-level asset and only then refused at the link, leaving paid litter.
func TestValidateLeadFormExtensions_RefusesASecondForm(t *testing.T) {
	_, err := validateLeadFormExtensions(CampaignInput{
		LeadForms: []LeadFormExtension{sampleLeadForm(), sampleLeadForm()},
	})
	if err == nil {
		t.Fatal("a second lead form must be refused")
	}
	if !strings.Contains(err.Error(), "refused at the link") {
		t.Errorf("error = %q, want it to name the link-time failure it is pre-empting", err)
	}
}

func TestValidateLeadFormExtensions_NoFormsIsNoAssets(t *testing.T) {
	assets, err := validateLeadFormExtensions(CampaignInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assets) != 0 {
		t.Errorf("got %d assets, want none", len(assets))
	}
}

func TestValidateLeadFormExtensions_RefusesEveryMissingRequiredField(t *testing.T) {
	cases := map[string]struct {
		mutate func(f *LeadFormExtension)
		want   string
	}{
		"business name":            {func(f *LeadFormExtension) { f.BusinessName = "   " }, "no business name"},
		"headline":                 {func(f *LeadFormExtension) { f.Headline = "" }, "no headline"},
		"description":              {func(f *LeadFormExtension) { f.Description = "" }, "no description"},
		"call-to-action desc":      {func(f *LeadFormExtension) { f.CallToActionDescription = "" }, "no call-to-action description"},
		"call-to-action type":      {func(f *LeadFormExtension) { f.CallToActionType = "" }, "no call-to-action type"},
		"privacy policy":           {func(f *LeadFormExtension) { f.PrivacyPolicyURL = "" }, "unservable"},
		"at least one form field":  {func(f *LeadFormExtension) { f.Fields = nil }, "cannot generate a lead"},
		"empty string form field":  {func(f *LeadFormExtension) { f.Fields = []string{"EMAIL", "  "} }, "field 1 is empty"},
		"well-shaped field needed": {func(f *LeadFormExtension) { f.Fields = []string{"email address"} }, "is not a field name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := sampleLeadForm()
			tc.mutate(&f)
			refuseLeadForm(t, f, tc.want)
		})
	}
}

func TestValidateLeadFormExtensions_RefusesOverLongText(t *testing.T) {
	cases := map[string]struct {
		mutate func(f *LeadFormExtension)
		want   string
	}{
		"business name": {func(f *LeadFormExtension) { f.BusinessName = strings.Repeat("a", maxLeadFormBusinessNameRunes+1) }, "business name is 26 characters"},
		"headline":      {func(f *LeadFormExtension) { f.Headline = strings.Repeat("a", maxLeadFormHeadlineRunes+1) }, "headline is 31 characters"},
		"description":   {func(f *LeadFormExtension) { f.Description = strings.Repeat("a", maxLeadFormDescriptionRunes+1) }, "description is 201 characters"},
		"cta description": {func(f *LeadFormExtension) {
			f.CallToActionDescription = strings.Repeat("a", maxLeadFormCTADescriptionRunes+1)
		}, "call-to-action description is 31 characters"},
		"post headline": {func(f *LeadFormExtension) { f.PostSubmitHeadline = strings.Repeat("a", maxLeadFormPostHeadlineRunes+1) }, "post-submit headline is 26 characters"},
		"post description": {func(f *LeadFormExtension) {
			f.PostSubmitDescription = strings.Repeat("a", maxLeadFormPostDescriptionRunes+1)
		}, "post-submit description is 201 characters"},
		"custom disclosure": {func(f *LeadFormExtension) {
			f.CustomDisclosure = strings.Repeat("a", maxLeadFormCustomDisclosureRunes+1)
		}, "custom disclosure is 201 characters"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := sampleLeadForm()
			tc.mutate(&f)
			refuseLeadForm(t, f, tc.want)
		})
	}
}

// Rune counts, not bytes: a 25-character CJK business name is 75 bytes and must
// be accepted, exactly as every other extension in this package counts.
func TestValidateLeadFormExtensions_BoundsAreRunesNotBytes(t *testing.T) {
	f := sampleLeadForm()
	f.BusinessName = strings.Repeat("基", maxLeadFormBusinessNameRunes)
	if asset := planLeadForm(t, f); asset.BusinessName != f.BusinessName {
		t.Errorf("businessName = %q, want the full %d-rune name", asset.BusinessName, maxLeadFormBusinessNameRunes)
	}
}

func TestValidateLeadFormExtensions_RefusesDuplicateFieldTypes(t *testing.T) {
	f := sampleLeadForm()
	f.Fields = []string{"EMAIL", "FULL_NAME", "EMAIL"}
	refuseLeadForm(t, f, "twice")
}

func TestValidateLeadFormExtensions_RefusesTooManyFields(t *testing.T) {
	f := sampleLeadForm()
	f.Fields = make([]string, 0, maxLeadFormFields+1)
	for i := 0; i <= maxLeadFormFields; i++ {
		f.Fields = append(f.Fields, "FIELD_"+string(rune('A'+i)))
	}
	refuseLeadForm(t, f, "at most 12 fields")
}

// The post-submit screen is optional as a GROUP, not field by field: a headline
// with no description renders a half-written thank-you screen.
func TestValidateLeadFormExtensions_PostSubmitPairIsAllOrNothing(t *testing.T) {
	t.Run("headline alone", func(t *testing.T) {
		f := sampleLeadForm()
		f.PostSubmitDescription = ""
		refuseLeadForm(t, f, "or neither")
	})
	t.Run("description alone", func(t *testing.T) {
		f := sampleLeadForm()
		f.PostSubmitHeadline = ""
		refuseLeadForm(t, f, "or neither")
	})
	t.Run("neither is fine", func(t *testing.T) {
		f := sampleLeadForm()
		f.PostSubmitHeadline = ""
		f.PostSubmitDescription = ""
		asset := planLeadForm(t, f)
		if asset.PostSubmitHeadline != "" || asset.PostSubmitDescription != "" {
			t.Errorf("post-submit screen = %q/%q, want both absent", asset.PostSubmitHeadline, asset.PostSubmitDescription)
		}
		// The button stands alone — Google renders it on its own default screen.
		if asset.PostSubmitCallToActionType != "VISIT_SITE" {
			t.Errorf("postSubmitCallToActionType = %q, want it kept without the pair", asset.PostSubmitCallToActionType)
		}
	})
}

// Shape, not membership — the structured-snippet precedent. Google revises these
// enums, and a local allow-list would refuse values Google accepts.
func TestValidateLeadFormExtensions_AcceptsUnknownButWellShapedEnums(t *testing.T) {
	f := sampleLeadForm()
	f.CallToActionType = "BOOK_A_STAND"
	f.PostSubmitCallToActionType = "JOIN_THE_SLACK"
	f.DesiredIntent = "MODERATE_INTENT"
	f.Fields = []string{"FAVOURITE_DISTRO"}

	asset := planLeadForm(t, f)
	if asset.CallToActionType != "BOOK_A_STAND" {
		t.Errorf("callToActionType = %q", asset.CallToActionType)
	}
	if asset.PostSubmitCallToActionType != "JOIN_THE_SLACK" {
		t.Errorf("postSubmitCallToActionType = %q", asset.PostSubmitCallToActionType)
	}
	if asset.DesiredIntent != "MODERATE_INTENT" {
		t.Errorf("desiredIntent = %q", asset.DesiredIntent)
	}
	if len(asset.Fields) != 1 || asset.Fields[0].InputType != "FAVOURITE_DISTRO" {
		t.Errorf("fields = %+v", asset.Fields)
	}
}

func TestValidateLeadFormExtensions_RefusesMisshapenEnums(t *testing.T) {
	cases := map[string]struct {
		mutate func(f *LeadFormExtension)
		want   string
	}{
		"call to action":      {func(f *LeadFormExtension) { f.CallToActionType = "Sign up" }, "is not a call-to-action name"},
		"post call to action": {func(f *LeadFormExtension) { f.PostSubmitCallToActionType = "visit-site" }, "is not a call-to-action name"},
		"desired intent":      {func(f *LeadFormExtension) { f.DesiredIntent = "high intent" }, "is not an intent name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := sampleLeadForm()
			tc.mutate(&f)
			refuseLeadForm(t, f, tc.want)
		})
	}
}

// The privacy-policy URL goes through the same servable-URL checks every ad
// destination does, and the refusal must never echo the URL back — it can carry
// a signed query string, and this error is persisted and logged.
func TestValidateLeadFormExtensions_RefusesAndRedactsAnUnservablePrivacyPolicy(t *testing.T) {
	cases := map[string]string{
		// Synthetic, on an .example host, and the embedded userinfo is the case under
		// test: strip it and this row stops testing anything.
		// secretlint-disable-next-line
		"embedded userinfo": "https://alice:s3cr3t@policy.example.org/privacy",
		"wrong scheme":      "ftp://policy.example.org/privacy",
		"no host":           "https:///privacy",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			f := sampleLeadForm()
			f.PrivacyPolicyURL = raw
			_, err := validateLeadFormExtensions(CampaignInput{LeadForms: []LeadFormExtension{f}})
			if err == nil {
				t.Fatalf("%s must be refused", name)
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("error leaks the credential: %q", err)
			}
			if !strings.Contains(err.Error(), "unservable") {
				t.Errorf("error = %q, want it to say the form is unservable", err)
			}
		})
	}
}

// http is accepted as well as https — buildTaggedFinalURL has always taken both,
// and splitting validateServableURL out of it must not have narrowed that.
func TestValidateLeadFormExtensions_AcceptsPlainHTTPPrivacyPolicy(t *testing.T) {
	f := sampleLeadForm()
	f.PrivacyPolicyURL = "http://policy.example.org/privacy"
	if asset := planLeadForm(t, f); asset.PrivacyPolicyURL != f.PrivacyPolicyURL {
		t.Errorf("privacyPolicyUrl = %q, want the http URL kept", asset.PrivacyPolicyURL)
	}
}

// The privacy policy is NOT UTM-tagged: it is a link Google renders inside the
// form, not an ad destination, so tagging it would attribute a policy read as an
// ad click and could break a query the policy host parses itself.
func TestValidateLeadFormExtensions_DoesNotTagThePrivacyPolicy(t *testing.T) {
	f := sampleLeadForm()
	f.PrivacyPolicyURL = "https://policy.example.org/privacy?lang=en"
	asset := planLeadForm(t, f)
	if strings.Contains(asset.PrivacyPolicyURL, "utm_") {
		t.Errorf("privacyPolicyUrl = %q, want no utm_ parameters", asset.PrivacyPolicyURL)
	}
	if asset.PrivacyPolicyURL != f.PrivacyPolicyURL {
		t.Errorf("privacyPolicyUrl = %q, want it forwarded unchanged", asset.PrivacyPolicyURL)
	}
}

func TestValidateLeadFormExtensions_RefusesAnOverLongPrivacyPolicyURL(t *testing.T) {
	f := sampleLeadForm()
	f.PrivacyPolicyURL = "https://policy.example.org/privacy?q=" + strings.Repeat("a", maxFinalURLBytes)
	refuseLeadForm(t, f, "exceeding the")
}
