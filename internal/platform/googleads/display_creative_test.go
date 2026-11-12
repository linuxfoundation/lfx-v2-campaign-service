// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// validateDisplayCreative — the pure half
// ---------------------------------------------------------------------------

// fullDisplayCreative passes every local rule, so a test can vary exactly one thing.
func fullDisplayCreative() DisplayCreative {
	return DisplayCreative{
		MarketingImages:       []string{"https://cdn.example.org/m1.png"},
		SquareMarketingImages: []string{"https://cdn.example.org/sq.png"},
		Headlines:             []string{"Join us at KubeCon"},
		LongHeadline:          "KubeCon + CloudNativeCon Europe 2026, Amsterdam",
		Descriptions:          []string{"Three days of talks, workshops and hallway track."},
		BusinessName:          "Linux Foundation",
	}
}

func displayCreativeInput(d DisplayCreative) CampaignInput {
	in := demandGenInput()
	in.DisplayCreative = d
	return in
}

func validateDisplay(t *testing.T, d DisplayCreative) (displayCreativePlan, error) {
	t.Helper()
	return validateDisplayCreative(campaignKindDisplay, displayCreativeInput(d))
}

// An absent creative stays valid: the documented campaign-and-ad-group-but-no-ad shape
// is also what adoption of an upstream-built ad looks like, and refusing it would break
// rows already in the database.
func TestValidateDisplayCreative_AbsentIsAccepted(t *testing.T) {
	plan, err := validateDisplayCreative(campaignKindDisplay, demandGenInput())
	if err != nil {
		t.Fatalf("an empty creative must be accepted: %v", err)
	}
	if plan.present {
		t.Error("an empty creative must not produce a present plan")
	}
}

// The creative is a DISPLAY capability, so it is REFUSED off Display rather than
// silently dropped — the refuse-don't-drop rule every sibling validator follows.
func TestValidateDisplayCreative_RefusedOffDisplay(t *testing.T) {
	for _, kind := range []string{campaignKindSearch, campaignKindDemandGen, campaignKindPerformanceMax, campaignKindVideo} {
		t.Run(kind, func(t *testing.T) {
			_, err := validateDisplayCreative(kind, displayCreativeInput(fullDisplayCreative()))
			if err == nil {
				t.Fatalf("a Display creative on %s must be refused, not dropped", kind)
			}
			if !strings.Contains(err.Error(), kind) {
				t.Errorf("the refusal must name the channel it was sent on: %v", err)
			}
		})
	}
}

func TestValidateDisplayCreative_HappyPath(t *testing.T) {
	d := fullDisplayCreative()
	d.LogoImages = []string{"https://cdn.example.org/logo.png"}
	d.SquareLogoImages = []string{"https://cdn.example.org/sqlogo.png"}
	d.CallToActionText = "Learn more"

	plan, err := validateDisplay(t, d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !plan.present {
		t.Fatal("a supplied creative must produce a present plan")
	}
	// urls is positionally parallel to displayImageSlots, and every rule that names a
	// slot addresses it by index — so a reordering here is a real defect.
	if len(plan.urls) != len(displayImageSlots) {
		t.Fatalf("urls = %d slots, want %d", len(plan.urls), len(displayImageSlots))
	}
	for i, want := range []string{"/m1.png", "/sq.png", "/logo.png", "/sqlogo.png"} {
		if len(plan.urls[i]) != 1 || !strings.HasSuffix(plan.urls[i][0], want) {
			t.Errorf("slot %d (%s) = %v, want the %s URL", i, displayImageSlots[i].label, plan.urls[i], want)
		}
	}
	if plan.longHeadline != d.LongHeadline {
		t.Errorf("longHeadline = %q, want %q", plan.longHeadline, d.LongHeadline)
	}
	if plan.businessName != "Linux Foundation" || plan.callToActionText != "Learn more" {
		t.Errorf("business name %q / cta %q", plan.businessName, plan.callToActionText)
	}
}

// Each slot label NAMES ITS CHANNEL. Three of the four differ in ratio or minimum from
// a slot of the same name on another channel, so an error reading only "logo image"
// would not tell an operator which creative they got wrong.
func TestDisplayImageSlots_LabelsNameTheChannel(t *testing.T) {
	for _, slot := range displayImageSlots {
		if !strings.HasPrefix(slot.label, "Display ") {
			t.Errorf("slot label %q does not name its channel", slot.label)
		}
	}
}

// BOTH marketing shapes are required, each on its own (#437). v23
// ResponsiveDisplayAdInfo says "At least one `marketing_image` is required" and "At least
// one square `marketing_image` is required"; the earlier reading copied Demand Gen's
// reciprocal wording and let either shape alone through, so Google refused the ad at
// adGroupAds:mutate — after the budget, the campaign and the ad group existed.
func TestValidateDisplayCreative_BothMarketingShapesAreRequired(t *testing.T) {
	if _, err := validateDisplay(t, fullDisplayCreative()); err != nil {
		t.Errorf("one image of each marketing shape must be accepted: %v", err)
	}

	cases := []struct {
		name  string
		strip func(*DisplayCreative)
		want  string
	}{
		{"landscape only", func(d *DisplayCreative) { d.SquareMarketingImages = nil }, "square marketing image (1:1)"},
		{"square only", func(d *DisplayCreative) { d.MarketingImages = nil }, "marketing image (1.91:1)"},
		{"neither", func(d *DisplayCreative) { d.MarketingImages, d.SquareMarketingImages = nil, nil }, "marketing image (1.91:1)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := fullDisplayCreative()
			tc.strip(&d)
			_, err := validateDisplay(t, d)
			if err == nil {
				t.Fatalf("a creative with %s must be refused before the budget mutate", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal must name the missing shape %q: %v", tc.want, err)
			}
		})
	}
}

// The marketing ceiling is COMBINED across the two arrays, which is how Google states
// it. Two arrays each under the cap but over it together is the case a per-array check
// would wave through, and it fails at the ad mutate — after three paid resources exist.
func TestValidateDisplayCreative_MarketingCapIsCombined(t *testing.T) {
	d := fullDisplayCreative()
	d.MarketingImages = urlsN("m", 8)
	d.SquareMarketingImages = urlsN("s", 8)

	_, err := validateDisplay(t, d)
	if err == nil {
		t.Fatalf("16 marketing images across both shapes must be refused at %d", maxDisplayMarketingImages)
	}
	if !strings.Contains(err.Error(), "across both shapes") {
		t.Errorf("the refusal must say the cap spans both arrays: %v", err)
	}

	atCap := fullDisplayCreative()
	atCap.MarketingImages = urlsN("m", 8)
	atCap.SquareMarketingImages = urlsN("s", 7)
	if _, err := validateDisplay(t, atCap); err != nil {
		t.Errorf("exactly %d marketing images must be accepted: %v", maxDisplayMarketingImages, err)
	}
}

// Logos share ONE ceiling across both arrays (#437) — v23 states "Combined with
// `square_logo_images`, the maximum is 5" on each field and no per-array limit — and have
// no floor at all, which is the explicit difference from Demand Gen, where Google requires
// a logo.
func TestValidateDisplayCreative_LogosAreCappedCombinedWithNoFloor(t *testing.T) {
	if _, err := validateDisplay(t, fullDisplayCreative()); err != nil {
		t.Errorf("Display logos are optional, unlike Demand Gen's: %v", err)
	}

	accepted := map[string][2]int{
		"five landscape":   {maxDisplayLogos, 0},
		"five square":      {0, maxDisplayLogos},
		"five across both": {3, 2},
	}
	for name, n := range accepted {
		t.Run(name, func(t *testing.T) {
			d := fullDisplayCreative()
			d.LogoImages = urlsN("l", n[0])
			d.SquareLogoImages = urlsN("q", n[1])
			if _, err := validateDisplay(t, d); err != nil {
				t.Errorf("%d + %d logos is within the combined ceiling of %d: %v", n[0], n[1], maxDisplayLogos, err)
			}
		})
	}

	refused := map[string][2]int{
		"three and three": {3, 3},
		"six landscape":   {maxDisplayLogos + 1, 0},
		"six square":      {0, maxDisplayLogos + 1},
	}
	for name, n := range refused {
		t.Run(name, func(t *testing.T) {
			d := fullDisplayCreative()
			d.LogoImages = urlsN("l", n[0])
			d.SquareLogoImages = urlsN("q", n[1])
			_, err := validateDisplay(t, d)
			if err == nil {
				t.Fatalf("%d + %d logos is over the combined ceiling of %d and must be refused", n[0], n[1], maxDisplayLogos)
			}
			if !strings.Contains(err.Error(), "across both shapes") {
				t.Errorf("the refusal must say the ceiling spans both logo arrays: %v", err)
			}
		})
	}
}

// The long headline is a SCALAR, not a list. It is required, bounded by display width,
// and must never be reported with a count error — the whole reason it is validated
// inline rather than through validateCreativeText.
func TestValidateDisplayCreative_LongHeadlineIsARequiredScalar(t *testing.T) {
	missing := fullDisplayCreative()
	missing.LongHeadline = "   "
	_, err := validateDisplay(t, missing)
	if err == nil {
		t.Fatal("a missing long headline must be refused")
	}
	if !strings.Contains(err.Error(), "long headline") {
		t.Errorf("the refusal must name the field: %v", err)
	}
	// "at least 1" / "at most 5" is validateCreativeText's vocabulary, and it is
	// nonsense for a field that cannot have a count.
	if strings.Contains(err.Error(), "at least") || strings.Contains(err.Error(), "at most") {
		t.Errorf("a scalar field must not be refused with a COUNT error: %v", err)
	}

	tooWide := fullDisplayCreative()
	tooWide.LongHeadline = strings.Repeat("a", maxDisplayLongHeadlineWeight+1)
	if _, err := validateDisplay(t, tooWide); err == nil {
		t.Errorf("a long headline wider than %d must be refused", maxDisplayLongHeadlineWeight)
	}

	atLimit := fullDisplayCreative()
	atLimit.LongHeadline = strings.Repeat("a", maxDisplayLongHeadlineWeight)
	if _, err := validateDisplay(t, atLimit); err != nil {
		t.Errorf("a long headline of exactly %d must be accepted: %v", maxDisplayLongHeadlineWeight, err)
	}
}

// Headlines and descriptions go through the shared list validator, so the only thing
// worth pinning here is that THIS channel's bounds are the ones applied.
func TestValidateDisplayCreative_TextBounds(t *testing.T) {
	tooMany := fullDisplayCreative()
	tooMany.Headlines = make([]string, 0, maxDisplayHeadlines+1)
	for i := 0; i <= maxDisplayHeadlines; i++ {
		tooMany.Headlines = append(tooMany.Headlines, "Headline")
	}
	if _, err := validateDisplay(t, tooMany); err == nil {
		t.Errorf("more than %d headlines must be refused", maxDisplayHeadlines)
	}

	noHeadlines := fullDisplayCreative()
	noHeadlines.Headlines = nil
	if _, err := validateDisplay(t, noHeadlines); err == nil {
		t.Error("a creative with no headlines must be refused")
	}

	noDescriptions := fullDisplayCreative()
	noDescriptions.Descriptions = nil
	if _, err := validateDisplay(t, noDescriptions); err == nil {
		t.Error("a creative with no descriptions must be refused")
	}

	wideHeadline := fullDisplayCreative()
	wideHeadline.Headlines = []string{strings.Repeat("a", maxDisplayHeadlineWeight+1)}
	if _, err := validateDisplay(t, wideHeadline); err == nil {
		t.Errorf("a headline wider than %d must be refused", maxDisplayHeadlineWeight)
	}
}

// The business name is required and bounded; the call to action is neither required
// nor, when absent, invented.
func TestValidateDisplayCreative_BusinessNameAndCallToAction(t *testing.T) {
	missing := fullDisplayCreative()
	missing.BusinessName = "  "
	if _, err := validateDisplay(t, missing); err == nil {
		t.Error("a missing business name must be refused")
	}

	wide := fullDisplayCreative()
	wide.BusinessName = strings.Repeat("a", maxDisplayBusinessNameWeight+1)
	if _, err := validateDisplay(t, wide); err == nil {
		t.Errorf("a business name wider than %d must be refused", maxDisplayBusinessNameWeight)
	}

	noCTA := fullDisplayCreative()
	plan, err := validateDisplay(t, noCTA)
	if err != nil {
		t.Fatalf("the call to action is optional: %v", err)
	}
	if plan.callToActionText != "" {
		t.Errorf("callToActionText = %q, want empty — Google supplies its own button text", plan.callToActionText)
	}

	refused := fullDisplayCreative()
	refused.CallToActionText = "Register"
	if _, err := validateDisplay(t, refused); err == nil || !strings.Contains(err.Error(), "not one Google accepts") {
		t.Errorf("err = %v, want Register refused before any write (#575)", err)
	}
}

// The URL shape rules are the shared validator's, and they must apply here: an http://
// URL cannot reach the fetch phase, because the fetch is the step that would follow it.
func TestValidateDisplayCreative_RefusesNonHTTPSImageURLs(t *testing.T) {
	d := fullDisplayCreative()
	d.MarketingImages = []string{"http://cdn.example.org/m1.png"}
	if _, err := validateDisplay(t, d); err == nil {
		t.Error("an http:// image URL must be refused")
	}
}

// displayCreativeStep reports what the ad was built from. It reads the plan's slot
// arrays by index, so it is the function a slot reordering breaks silently.
func TestDisplayCreativeStep(t *testing.T) {
	d := fullDisplayCreative()
	d.LogoImages = []string{"https://cdn.example.org/logo.png"}
	plan, err := validateDisplay(t, d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	step := displayCreativeStep(plan)
	for _, want := range []string{"2 marketing image(s)", "1 logo(s)", "1 headline(s)", "1 description(s)"} {
		if !strings.Contains(step, want) {
			t.Errorf("step %q is missing %q", step, want)
		}
	}
}

func TestDisplayAdGroupName(t *testing.T) {
	in := demandGenInput()
	got := displayAdGroupName(in)
	if !strings.HasSuffix(got, " - Display Network") {
		t.Errorf("displayAdGroupName = %q, want one naming the channel", got)
	}

	// The reason the suffix is " - Display Network" and not " - Display": Demand Gen
	// composes its ad group name the same way off the same EventName, and both channels
	// can now sit under one brief. Equal names defeat the name-based reconciliation
	// each relies on, so this inequality is the assertion that matters — a later
	// "simplify the suffix" edit has to fail here rather than in production.
	// Both sides come from the PRODUCTION compositions. A local literal for the Demand Gen
	// half would keep passing if demandgen.go's own suffix moved, which is the half of the
	// collision this test cannot see from here.
	if demandGenName := demandGenAdGroupName(in); got == demandGenName {
		t.Errorf("displayAdGroupName = %q collides with the Demand Gen ad group name", got)
	}

	// Sanitized, not merely trimmed. A control character inside EventName survives a
	// TrimSpace and is rejected by Google at `adGroups:mutate` — which runs AFTER the
	// budget and campaign are created and paid for.
	dirty := in
	dirty.EventName = "Kube\x00Con\tEU"
	if got := displayAdGroupName(dirty); got != "Kube Con EU - Display Network" {
		t.Errorf("displayAdGroupName with control characters = %q, want them folded to single spaces", got)
	}
}

// urlsN builds n distinct https image URLs, so a count rule is exercised without a
// duplicate-URL rule interfering.
func urlsN(prefix string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "https://cdn.example.org/"+prefix+string(rune('a'+i))+".png")
	}
	return out
}
