// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fullPMaxCreative is an asset group that passes every local rule, for tests that
// want to vary exactly one thing. The counts are the MINIMA Performance Max
// requires — three headlines, one long headline, two descriptions, both marketing
// shapes, one logo — so a test that removes anything lands on the bound it means to.
func fullPMaxCreative() PerformanceMaxCreative {
	return PerformanceMaxCreative{
		MarketingImages:       []string{"https://cdn.example.org/m1.png"},
		SquareMarketingImages: []string{"https://cdn.example.org/s1.png"},
		LogoImages:            []string{"https://cdn.example.org/logo.png"},
		Headlines:             []string{"Join us at KubeCon", "Three days in Amsterdam", "Meet the maintainers"},
		LongHeadlines:         []string{"KubeCon + CloudNativeCon Europe 2026, Amsterdam"},
		Descriptions:          []string{"Talks, workshops and hallway track.", "Register now and save."},
		BusinessName:          "Linux Foundation",
	}
}

// pmaxInput is demandgen_test.go's minimal input with an asset group attached, so
// these tests vary only the creative. The campaign-level fields are channel-agnostic.
func pmaxInput(c PerformanceMaxCreative) CampaignInput {
	in := demandGenInput()
	in.PerformanceMaxCreative = c
	return in
}

// ---------------------------------------------------------------------------
// validatePerformanceMaxCreative — the pure half
// ---------------------------------------------------------------------------

// An absent asset group must stay valid. A Performance Max campaign whose asset
// group was built by hand in the Google Ads UI is a real campaign this service must
// be able to ADOPT, and ValidateCampaignInputKind runs this same preflight — so
// refusing an empty creative would refuse adoption of exactly those campaigns.
func TestValidatePerformanceMaxCreative_AbsentIsAccepted(t *testing.T) {
	plan, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, demandGenInput())
	if err != nil {
		t.Fatalf("an empty asset group must be accepted: %v", err)
	}
	if plan.present {
		t.Error("an empty asset group must not produce a present plan")
	}
}

// The asset group is a Performance Max capability, so it is REFUSED on the other two
// channels rather than accepted and dropped — the mirror of the way the Demand Gen
// creative is refused off Demand Gen and the extension assets off Search.
func TestValidatePerformanceMaxCreative_RefusedOnOtherChannels(t *testing.T) {
	for _, kind := range []string{campaignKindSearch, campaignKindDemandGen} {
		t.Run(kind, func(t *testing.T) {
			_, err := validatePerformanceMaxCreative(kind, pmaxInput(fullPMaxCreative()))
			if err == nil {
				t.Fatalf("a Performance Max asset group must be refused on %s", kind)
			}
			if !strings.Contains(err.Error(), kind) {
				t.Errorf("the refusal must name the channel it was refused on, got: %v", err)
			}
		})
	}
}

func TestValidatePerformanceMaxCreative_HappyPath(t *testing.T) {
	plan, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, pmaxInput(fullPMaxCreative()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !plan.present {
		t.Fatal("a supplied asset group must produce a present plan")
	}
	// Positional parity with performanceMaxImageSlots is what pairs each URL with its
	// shape rules and its ASSET FIELD TYPE; a plan that lost it would link the logo
	// under MARKETING_IMAGE and the marketing image under LOGO.
	if len(plan.urls) != len(performanceMaxImageSlots) {
		t.Fatalf("plan.urls has %d slots, want %d", len(plan.urls), len(performanceMaxImageSlots))
	}
	if len(plan.urls[pmaxSlotMarketing]) != 1 || len(plan.urls[pmaxSlotSquare]) != 1 || len(plan.urls[pmaxSlotLogo]) != 1 {
		t.Errorf("urls landed in the wrong slots: %v", plan.urls)
	}
	if got := plan.imageCount(); got != 3 {
		t.Errorf("imageCount() = %d, want 3", got)
	}
	if plan.businessName != "Linux Foundation" {
		t.Errorf("businessName = %q", plan.businessName)
	}
}

func TestValidatePerformanceMaxCreative_Rejections(t *testing.T) {
	long := strings.Repeat("a", 200)
	fill := func(prefix string, n int) []string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, "https://e.org/"+prefix+string(rune('a'+i))+".png")
		}
		return out
	}
	cases := []struct {
		name    string
		mutate  func(*PerformanceMaxCreative)
		wantSub string
	}{
		// Both marketing shapes are independently REQUIRED here. On Demand Gen the
		// landscape and portrait shapes are reciprocal — either satisfies the other —
		// and assuming that rule travels is the specific mistake these two cases pin.
		{"no landscape marketing image", func(c *PerformanceMaxCreative) { c.MarketingImages = nil }, "at least one marketing image"},
		{"no square marketing image", func(c *PerformanceMaxCreative) { c.SquareMarketingImages = nil }, "at least one square marketing image"},
		{"no logo", func(c *PerformanceMaxCreative) { c.LogoImages = nil }, "at least 1 square logo"},
		{"six logos", func(c *PerformanceMaxCreative) { c.LogoImages = fill("l", 6) }, "at most 5 square logos"},
		{"six landscape logos", func(c *PerformanceMaxCreative) { c.LandscapeLogoImages = fill("w", 6) }, "at most 5 landscape logos"},
		{"two headlines", func(c *PerformanceMaxCreative) {
			c.Headlines = []string{"one", "two"}
		}, "at least 3 headline"},
		{"sixteen headlines", func(c *PerformanceMaxCreative) {
			c.Headlines = strings.Split("a b c d e f g h i j k l m n o p", " ")
		}, "at most 15 headlines"},
		{"no long headline", func(c *PerformanceMaxCreative) { c.LongHeadlines = nil }, "at least 1 long headline"},
		{"six long headlines", func(c *PerformanceMaxCreative) {
			c.LongHeadlines = []string{"a", "b", "c", "d", "e", "f"}
		}, "at most 5 long headlines"},
		{"one description", func(c *PerformanceMaxCreative) {
			c.Descriptions = []string{"only one"}
		}, "at least 2 description"},
		{"six descriptions", func(c *PerformanceMaxCreative) {
			c.Descriptions = []string{"a", "b", "c", "d", "e", "f"}
		}, "at most 5 descriptions"},
		{"over-wide headline", func(c *PerformanceMaxCreative) { c.Headlines = []string{long, "b", "c"} }, "display width"},
		{"duplicate headline", func(c *PerformanceMaxCreative) {
			c.Headlines = []string{"Join us", "JOIN US", "third"}
		}, "more than once"},
		{"no business name", func(c *PerformanceMaxCreative) { c.BusinessName = "  " }, "business name"},
		{"over-wide business name", func(c *PerformanceMaxCreative) { c.BusinessName = long }, "display width"},
		{"http image URL", func(c *PerformanceMaxCreative) {
			c.MarketingImages = []string{"http://cdn.example.org/m1.png"}
		}, "https"},
		{"duplicate image URL", func(c *PerformanceMaxCreative) {
			c.SquareMarketingImages = []string{"https://e.org/a.png", "https://e.org/a.png"}
		}, "more than once"},
		{"YouTube URL instead of an id", func(c *PerformanceMaxCreative) {
			c.YouTubeVideoIDs = []string{"https://youtu.be/dQw4w9WgXcQ"}
		}, "looks like a URL"},
		{"duplicate YouTube id", func(c *PerformanceMaxCreative) {
			c.YouTubeVideoIDs = []string{"abc123", "abc123"}
		}, "more than once"},
		{"six YouTube videos", func(c *PerformanceMaxCreative) {
			c.YouTubeVideoIDs = []string{"a1", "b2", "c3", "d4", "e5", "f6"}
		}, "at most 5 YouTube videos"},
		{"over-long asset group name", func(c *PerformanceMaxCreative) {
			c.AssetGroupName = strings.Repeat("a", maxAssetGroupNameRunes+1)
		}, "exceeding the"},
		{"display path with a slash", func(c *PerformanceMaxCreative) { c.Path1 = "events/eu" }, "display path"},
		{"over-long display path", func(c *PerformanceMaxCreative) { c.Path1 = "sixteencharacter" }, "display path1"},
		// path2 renders only after path1, so on its own it renders nothing at all.
		{"path2 without path1", func(c *PerformanceMaxCreative) { c.Path2 = "eu" }, "without path1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fullPMaxCreative()
			tc.mutate(&c)
			_, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, pmaxInput(c))
			if err == nil {
				t.Fatalf("expected a refusal mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// The counts are Performance Max's own, and the trap is that the display WIDTHS
// coincide with the RSA and Demand Gen ones (30 / 90) — so a validator wired to the
// wrong channel's count constants would pass every width-based assertion. One
// headline is legal on Demand Gen and must still be refused here.
func TestValidatePerformanceMaxCreative_CountsAreNotDemandGensCounts(t *testing.T) {
	if minDemandGenHeadlines >= minPerformanceMaxHeadlines {
		t.Fatalf("test premise broken: Demand Gen needs %d headlines, Performance Max %d", minDemandGenHeadlines, minPerformanceMaxHeadlines)
	}
	c := fullPMaxCreative()
	c.Headlines = []string{"Join us at KubeCon"} // legal for Demand Gen
	if _, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, pmaxInput(c)); err == nil {
		t.Error("one headline is legal for Demand Gen and must still be refused for Performance Max")
	}
}

// At least one description must fit the SHORT slot Google renders on constrained
// surfaces, and the check is "any of them" rather than "the first of them": the
// caller's ordering has never been a contract this package states, so reading a
// position as a field type would refuse a perfectly valid list.
func TestValidatePerformanceMaxCreative_ShortDescriptionIsPositionIndependent(t *testing.T) {
	long := strings.Repeat("x", maxPerformanceMaxShortDescriptionWeight+1)
	short := strings.Repeat("y", maxPerformanceMaxShortDescriptionWeight)

	c := fullPMaxCreative()
	c.Descriptions = []string{long, short} // the short one written second
	if _, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, pmaxInput(c)); err != nil {
		t.Errorf("a short description written second must satisfy the rule: %v", err)
	}

	c.Descriptions = []string{long, long + "z"}
	_, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, pmaxInput(c))
	if err == nil {
		t.Fatal("descriptions that are all too long for the short slot must be refused")
	}
	if !strings.Contains(err.Error(), "display width of 60 or less") {
		t.Errorf("the refusal must name the short-slot limit, got: %v", err)
	}
}

// The 20-image ceiling is Google's own COMBINED total across the three marketing
// shapes. Checked across them rather than per shape: three arrays of 19 is 57 images
// and satisfies every per-array reading of the rule.
func TestValidatePerformanceMaxCreative_MarketingCeilingIsCombined(t *testing.T) {
	fill := func(prefix string, n int) []string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, "https://e.org/"+prefix+string(rune('a'+i))+".png")
		}
		return out
	}
	c := fullPMaxCreative()
	c.MarketingImages = fill("m", 7)
	c.SquareMarketingImages = fill("s", 7)
	c.PortraitImages = fill("p", 7) // 21 combined, none over 20 alone
	if _, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, pmaxInput(c)); err == nil {
		t.Fatal("21 marketing images across the three shapes must be refused")
	}
	c.PortraitImages = fill("p", 6) // exactly 20
	if _, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, pmaxInput(c)); err != nil {
		t.Fatalf("exactly 20 marketing images must be accepted: %v", err)
	}
}

// Logos sit OUTSIDE the marketing ceiling. Folding them in would refuse a
// 20-marketing-image asset group that also carries the logo Google REQUIRES — an
// over-refusal of an asset group Google accepts.
func TestValidatePerformanceMaxCreative_LogosAreOutsideTheMarketingCeiling(t *testing.T) {
	c := fullPMaxCreative()
	c.SquareMarketingImages = make([]string, 0, maxPerformanceMaxMarketingImages-1)
	for i := 0; i < maxPerformanceMaxMarketingImages-1; i++ {
		c.SquareMarketingImages = append(c.SquareMarketingImages, "https://e.org/s"+string(rune('a'+i))+".png")
	}
	c.LogoImages = []string{"https://e.org/l1.png", "https://e.org/l2.png"}
	c.LandscapeLogoImages = []string{"https://e.org/w1.png", "https://e.org/w2.png"}
	if _, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, pmaxInput(c)); err != nil {
		t.Fatalf("20 marketing images plus 4 logos must be accepted: %v", err)
	}
}

// The group is named from the EVENT when the caller names none, which is what keeps
// it findable next to the campaign it belongs to. A test pins the default because a
// changed suffix silently renames every group this service creates.
func TestValidatePerformanceMaxCreative_AssetGroupNameDefaultsToTheEvent(t *testing.T) {
	plan, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, pmaxInput(fullPMaxCreative()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "KubeCon Europe 2026" + performanceMaxAssetGroupSuffix; plan.assetGroupName != want {
		t.Errorf("assetGroupName = %q, want %q", plan.assetGroupName, want)
	}

	c := fullPMaxCreative()
	c.AssetGroupName = "  Hand-named group  "
	plan, err = validatePerformanceMaxCreative(campaignKindPerformanceMax, pmaxInput(c))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.assetGroupName != "Hand-named group" {
		t.Errorf("a supplied name must win (trimmed), got %q", plan.assetGroupName)
	}
}

// A YouTube id's LENGTH is deliberately not pinned to 11: that is the length every
// id has had, not a documented guarantee, and refusing a longer one would refuse a
// video Google would have accepted. Over-refusal is the failure mode these guards
// exist to avoid.
func TestValidateYouTubeVideoIDs_DoesNotPinTheLength(t *testing.T) {
	got, err := validateYouTubeVideoIDs([]string{"dQw4w9WgXcQ", "a-longer_id-than-eleven"})
	if err != nil {
		t.Fatalf("an id longer than 11 characters must be accepted: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %v, want both ids", got)
	}
	if _, err := validateYouTubeVideoIDs([]string{"has space"}); err == nil {
		t.Error("a space is not a YouTube id character and must be refused")
	}
}

// ---------------------------------------------------------------------------
// preflight integration
// ---------------------------------------------------------------------------

// The asset group must be resolved by preflightCampaignKind ITSELF, which is what
// makes ValidateCampaignInputKind refuse exactly what the create cascade refuses.
func TestPreflightCampaignKind_ResolvesTheAssetGroup(t *testing.T) {
	c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))
	pf, err := c.preflightCampaignKind(campaignKindPerformanceMax, pmaxInput(fullPMaxCreative()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pf.pmax.present {
		t.Error("the preflight did not resolve the asset group")
	}
	bad := fullPMaxCreative()
	bad.LogoImages = nil
	if _, err := c.preflightCampaignKind(campaignKindPerformanceMax, pmaxInput(bad)); err == nil {
		t.Error("an asset group with no logo must fail the preflight, before any mutate")
	}
}

// Adoption runs the same preflight and sends nothing at all, so it must refuse and
// accept exactly what the create path does.
func TestValidateCampaignInputKind_RefusesTheSameAssetGroup(t *testing.T) {
	c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))
	bad := fullPMaxCreative()
	bad.SquareMarketingImages = nil
	if err := c.ValidateCampaignInputKind(campaignKindPerformanceMax, pmaxInput(bad)); err == nil {
		t.Error("adoption must refuse an asset group the create path refuses")
	}
	if err := c.ValidateCampaignInputKind(campaignKindPerformanceMax, pmaxInput(fullPMaxCreative())); err != nil {
		t.Errorf("adoption must accept an asset group the create path accepts: %v", err)
	}
}

// ---------------------------------------------------------------------------
// buildPerformanceMaxAssets
// ---------------------------------------------------------------------------

// The asset ORDER is load-bearing, not cosmetic: the assets:mutate response is
// POSITIONAL — result i describes operation i — and the link mutate pairs each
// returned resource name with the field type recorded at the same index. An order
// that drifted from the field-type list would link every asset under the wrong type,
// and Google would accept it: a headline linked as a DESCRIPTION is a valid link.
func TestBuildPerformanceMaxAssets_OrderAndFieldTypes(t *testing.T) {
	plan, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, pmaxInput(func() PerformanceMaxCreative {
		c := fullPMaxCreative()
		c.YouTubeVideoIDs = []string{"vid1"}
		return c
	}()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Two images, in slot order: the marketing image (slot 0) then the logo (slot 3).
	images := []fetchedImage{
		{slot: pmaxSlotMarketing, url: "https://cdn.example.org/m1.png", data: []byte("m")},
		{slot: pmaxSlotLogo, url: "https://cdn.example.org/logo.png", data: []byte("l")},
	}

	got := buildPerformanceMaxAssets(plan, images)
	want := []string{
		assetFieldHeadline, assetFieldHeadline, assetFieldHeadline,
		assetFieldLongHeadline,
		assetFieldDescription, assetFieldDescription,
		assetFieldBusinessName,
		assetFieldMarketingImage, assetFieldLogo,
		assetFieldYouTubeVideo,
	}
	if len(got) != len(want) {
		t.Fatalf("built %d assets, want %d", len(got), len(want))
	}
	for i, field := range want {
		if got[i].fieldType != field {
			t.Errorf("asset %d has field type %q, want %q", i, got[i].fieldType, field)
		}
	}
	// Each asset carries exactly one of the three oneof members, and the member must
	// match the field type: a text asset linked as an image is rejected at the link
	// mutate, which runs after the campaign, the assets and the group all exist.
	for i, a := range got {
		set := 0
		for _, isSet := range []bool{a.create.TextAsset != nil, a.create.ImageAsset != nil, a.create.YouTubeVideoAsset != nil} {
			if isSet {
				set++
			}
		}
		if set != 1 {
			t.Errorf("asset %d sets %d of the three oneof members, want exactly 1", i, set)
		}
	}
	if got[0].create.TextAsset == nil || got[0].create.TextAsset.Text != "Join us at KubeCon" {
		t.Errorf("the first asset must be the first headline, got %+v", got[0].create)
	}
	if got[6].create.TextAsset == nil || got[6].create.TextAsset.Text != "Linux Foundation" {
		t.Errorf("the business name must follow the descriptions, got %+v", got[6].create)
	}
	if got[7].create.ImageAsset == nil {
		t.Errorf("an image asset must carry bytes, got %+v", got[7].create)
	}
	if got[9].create.YouTubeVideoAsset == nil || got[9].create.YouTubeVideoAsset.YouTubeVideoID != "vid1" {
		t.Errorf("the video must come last, got %+v", got[9].create)
	}
}

// An image asset carries NO name, and in particular never the source URL.
//
// The obvious label — slot plus source URL — ships the caller's creative URL to
// Google as a permanent, human-visible asset label in the shared Foundation ad
// account. That is the same value sanitizeSnapshotURL reduces to scheme+host before
// this service may write it to its own database, so shipping it whole to a third
// party redacts going in and exports going out. The URL is also unbounded and
// caller-controlled, in a payload sent AFTER the campaign exists.
//
// Pinned on the URL's QUERY specifically: a signed CDN URL is the case where the
// query string IS the credential.
func TestBuildPerformanceMaxAssets_ImageAssetsCarryNoName(t *testing.T) {
	plan, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, pmaxInput(fullPMaxCreative()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const signed = "https://cdn.example.org/m1.png?sig=SECRETSIGNATURE&Expires=99"
	images := []fetchedImage{
		{slot: pmaxSlotMarketing, url: signed, data: []byte("m")},
		{slot: pmaxSlotLogo, url: "https://cdn.example.org/logo.png", data: []byte("l")},
	}

	for i, a := range buildPerformanceMaxAssets(plan, images) {
		if a.create.ImageAsset == nil {
			continue
		}
		if a.create.Name != "" {
			t.Errorf("image asset %d carries a name %q; Google names it, and any name we build from caller input is a leak or an unbounded string", i, a.create.Name)
		}
		if strings.Contains(a.create.Name, "SECRETSIGNATURE") {
			t.Errorf("image asset %d carries the signed query string from the source URL", i)
		}
	}
}

// An absent asset group builds NO assets, which is what keeps the campaign-only
// cascade identical to the one a campaign with no creative has always taken.
func TestBuildPerformanceMaxAssets_EmptyPlanBuildsNothing(t *testing.T) {
	if got := buildPerformanceMaxAssets(performanceMaxPlan{}, nil); len(got) != 0 {
		t.Errorf("an absent plan must build no assets, got %d", len(got))
	}
}
