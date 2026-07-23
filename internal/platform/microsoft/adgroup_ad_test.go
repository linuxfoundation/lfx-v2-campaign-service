// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

// ---- full-tree happy path --------------------------------------------------

func TestCreateCampaign_CreatesFullHierarchyPaused(t *testing.T) {
	var camp createCampaignsRequest
	var group createAdGroupsRequest
	var ad createAdsRequest
	api := &campaignsAPI{postSeen: &camp, adGroupSeen: &group, adSeen: &ad}
	c := newAPIClient(t, api.handler(t))

	res, err := c.CreateCampaign(context.Background(), validInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	// All three levels created, ids surfaced.
	if res.CampaignID != "321" || res.AdGroupID != "654" || res.AdID != "987" {
		t.Errorf("ids = campaign %q adgroup %q ad %q, want 321/654/987", res.CampaignID, res.AdGroupID, res.AdID)
	}
	if res.AdGroupName == "" || !strings.Contains(res.AdGroupName, "Ad Group") {
		t.Errorf("AdGroupName = %q, want a composed 'LFX | Ad Group | ...' name", res.AdGroupName)
	}
	// Ad group is PAUSED, SearchStandard (responsive-search-ad-capable), with a language.
	if group.AdGroups[0].Status != adGroupStatusPaused {
		t.Errorf("ad group Status = %q, want %q", group.AdGroups[0].Status, adGroupStatusPaused)
	}
	if group.AdGroups[0].AdGroupType != adGroupTypeSearchStandard {
		t.Errorf("ad group AdGroupType = %q, want %q", group.AdGroups[0].AdGroupType, adGroupTypeSearchStandard)
	}
	if group.AdGroups[0].Language == "" {
		t.Error("ad group Language is empty; Add requires a language when the campaign sets none")
	}
	// Ad is a PAUSED responsive search ad: the polymorphic Type discriminator, >=3 headline
	// assets, >=2 description assets, the destination + UTM params.
	got := ad.Ads[0]
	if got.Type != adTypeResponsiveSearch {
		t.Errorf("ad Type = %q, want %q (the AddAds polymorphic discriminator)", got.Type, adTypeResponsiveSearch)
	}
	if got.Status != adStatusPaused {
		t.Errorf("ad Status = %q, want %q (Ad.Status defaults to Active on Add)", got.Status, adStatusPaused)
	}
	if len(got.Headlines) < minAdHeadlines || len(got.Headlines) > maxAdHeadlines {
		t.Errorf("ad Headlines count = %d, want %d-%d", len(got.Headlines), minAdHeadlines, maxAdHeadlines)
	}
	if len(got.Descriptions) < minAdDescriptions || len(got.Descriptions) > maxAdDescriptions {
		t.Errorf("ad Descriptions count = %d, want %d-%d", len(got.Descriptions), minAdDescriptions, maxAdDescriptions)
	}
	for _, h := range got.Headlines {
		if h.Asset.Type != "TextAsset" || h.Asset.Text == "" {
			t.Errorf("headline asset malformed: %+v", h)
		}
	}
	if len(got.FinalUrls) != 1 {
		t.Fatalf("ad FinalUrls = %v, want exactly one", got.FinalUrls)
	}
	u, perr := url.Parse(got.FinalUrls[0])
	if perr != nil {
		t.Fatalf("ad FinalUrl not a URL: %v", perr)
	}
	q := u.Query()
	if q.Get("utm_source") != "microsoft" || q.Get("utm_medium") != "cpc" || q.Get("utm_campaign") != "kubecon" {
		t.Errorf("FinalUrl UTM params wrong: %s", got.FinalUrls[0])
	}
}

func TestCreateCampaign_HonorsExplicitAdCopy(t *testing.T) {
	var ad createAdsRequest
	api := &campaignsAPI{adSeen: &ad}
	c := newAPIClient(t, api.handler(t))
	in := validInput()
	// Two caller headlines (below the min of 3) + two descriptions. The caller values must
	// appear first and in order; composeAdCopy pads the headlines up to the required minimum.
	in.Headlines = []string{"Register for KubeCon EU", "KubeCon + CloudNativeCon"}
	in.Descriptions = []string{"Join cloud native practitioners this spring.", "Sessions, keynotes, and more."}
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	hs := ad.Ads[0].Headlines
	if len(hs) < minAdHeadlines {
		t.Fatalf("headlines padded below minimum: %d", len(hs))
	}
	if hs[0].Asset.Text != in.Headlines[0] || hs[1].Asset.Text != in.Headlines[1] {
		t.Errorf("caller headlines not first/in-order: %q, %q", hs[0].Asset.Text, hs[1].Asset.Text)
	}
	ds := ad.Ads[0].Descriptions
	if ds[0].Asset.Text != in.Descriptions[0] || ds[1].Asset.Text != in.Descriptions[1] {
		t.Errorf("caller descriptions not first/in-order: %q, %q", ds[0].Asset.Text, ds[1].Asset.Text)
	}
}

// ---- idempotency at ad-group and ad levels ---------------------------------

func TestCreateCampaign_ReusesExistingAdGroupAndAd(t *testing.T) {
	in := validInput()
	adGroupName := composeAdGroupName(in)
	finalURL := buildAdFinalURL(in)
	api := &campaignsAPI{
		adGroupGetBody: `{"AdGroups":[{"Id":111,"Name":` + jsonString(adGroupName) + `}]}`,
		adGetBody:      `{"Ads":[{"Id":222,"FinalUrls":[` + jsonString(finalURL) + `]}]}`,
	}
	adGroupPostReached, adPostReached := false, false
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		// The CREATE routes are the bare /AdGroups and /Ads; the lookups are
		// /AdGroups/QueryByCampaignId and /Ads/QueryByAdGroupId, so a "/AdGroups" /
		// "/Ads" suffix matches only the create and never the query.
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/AdGroups") {
			adGroupPostReached = true
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Ads") {
			adPostReached = true
		}
		base(w, r)
	})
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if adGroupPostReached {
		t.Error("ad group create POST issued despite an existing ad group by name")
	}
	if adPostReached {
		t.Error("ad create POST issued despite an existing ad with the same destination")
	}
	if res.AdGroupID != "111" || res.AdID != "222" {
		t.Errorf("AdGroupID=%q AdID=%q, want the existing 111/222", res.AdGroupID, res.AdID)
	}
}

func TestCreateCampaign_AdLookupMatchWithNoIDIsUnconfirmed(t *testing.T) {
	// The ad lookup finds an ad whose destination MATCHES the target but whose Id is null.
	// The ad almost certainly exists, so treating it as absent and POSTing /Ads would stack
	// a DUPLICATE responsive search ad (v13 permits duplicates). The step must instead be
	// UNCONFIRMED (verify before retry) and issue NO ad create.
	in := validInput()
	adGroupName := composeAdGroupName(in)
	finalURL := buildAdFinalURL(in)
	api := &campaignsAPI{
		adGroupGetBody: `{"AdGroups":[{"Id":111,"Name":` + jsonString(adGroupName) + `}]}`,
		adGetBody:      `{"Ads":[{"Id":null,"FinalUrls":[` + jsonString(finalURL) + `]}]}`,
	}
	adPostReached := false
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Ads") {
			adPostReached = true
		}
		base(w, r)
	})
	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an UNCONFIRMED error when the matching ad has no usable id")
	}
	if adPostReached {
		t.Error("ad create POST issued despite a destination-matching ad (would duplicate)")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a destination-match with an unusable id must be UNCONFIRMED, got: %v", err)
	}
	if res == nil || res.AdGroupID != "111" {
		t.Fatalf("expected a partial carrying the created ad group 111, got %+v", res)
	}
	if res.AdID != "" {
		t.Errorf("AdID = %q, want empty on an unconfirmed ad step", res.AdID)
	}
}

func TestCreateCampaign_AdGroupBodyCarriesRequiredFields(t *testing.T) {
	// AddAdGroups lists ReturnInheritedBidStrategyTypes as a required body element (reserved,
	// but no optional note), so it must be present in the JSON even as false. Assert it is
	// serialized (not omitted) alongside AdGroupType + Language.
	var group createAdGroupsRequest
	var rawBody string
	api := &campaignsAPI{}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/AdGroups") {
			b, _ := io.ReadAll(r.Body)
			rawBody = string(b)
			_ = json.Unmarshal(b, &group)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
			return
		}
		base(w, r)
	})
	if _, err := c.CreateCampaign(context.Background(), validInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if !strings.Contains(rawBody, "ReturnInheritedBidStrategyTypes") {
		t.Errorf("AddAdGroups body omits the required ReturnInheritedBidStrategyTypes field: %s", rawBody)
	}
	if group.AdGroups[0].AdGroupType != adGroupTypeSearchStandard || group.AdGroups[0].Language == "" {
		t.Errorf("ad group missing AdGroupType/Language: %+v", group.AdGroups[0])
	}
}

func TestComposeAdCopy_DoubleWidthUsesReducedLimit(t *testing.T) {
	// All-CJK content: Microsoft's documented cap for double-width-language headlines is 15
	// final chars (not 30), so a long CJK headline must truncate to 15. This asserts the
	// all-wide case, which matches the doc exactly. (The client applies that reduced cap
	// whenever ANY wide char is present — a deliberately conservative approximation of the
	// language-scoped rule, since v13 publishes no per-character weighted formula; see
	// hasDoubleWidth. So it never emits an over-length asset, at the cost of occasionally
	// truncating mixed ASCII+wide copy a little short.)
	hs, _ := composeAdCopy(CampaignInput{EventName: strings.Repeat("字", 40), Project: "字"})
	for _, h := range hs {
		if hasDoubleWidth(h) && utf8.RuneCountInString(h) > maxAdHeadlineRunesWide {
			t.Errorf("double-width headline %q kept %d runes, want <=%d", h, utf8.RuneCountInString(h), maxAdHeadlineRunesWide)
		}
	}
	// hasDoubleWidth sanity: false for ASCII, true for CJK and emoji.
	if hasDoubleWidth("Register Today") {
		t.Error("ASCII must not be flagged double-width")
	}
	if !hasDoubleWidth("字") {
		t.Error("CJK must be flagged double-width")
	}
	if !hasDoubleWidth("🎉") {
		t.Error("emoji must count as double-width")
	}
}

func TestCreateCampaign_AdsLookupSendsRequiredAdTypes(t *testing.T) {
	// GetAdsByAdGroupId REQUIRES an AdTypes array (only ReturnAdditionalFields is optional);
	// omitting it rejects the idempotency lookup before the ad create is reached. Assert the
	// lookup body carries AdTypes.
	var adQuery queryAdsRequest
	api := &campaignsAPI{}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Ads/QueryByAdGroupId") {
			if err := json.NewDecoder(r.Body).Decode(&adQuery); err != nil {
				t.Errorf("decode ads query body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Ads":[]}`)
			return
		}
		base(w, r)
	})
	if _, err := c.CreateCampaign(context.Background(), validInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if len(adQuery.AdTypes) == 0 {
		t.Fatal("ads lookup omitted the required AdTypes array")
	}
	if adQuery.AdTypes[0] != adTypeResponsiveSearch {
		t.Errorf("AdTypes[0] = %q, want %q", adQuery.AdTypes[0], adTypeResponsiveSearch)
	}
}

// ---- ad-group failure classification (carries campaign id) -----------------

func TestCreateCampaign_AdGroupPartialErrorCarriesCampaign(t *testing.T) {
	// The campaign succeeds; the ad-group create returns a 200 with a PartialError and
	// a null id → definite rejection. The partial must carry the created campaign id so
	// the caller can reconcile the orphaned campaign.
	api := &campaignsAPI{
		adGroupPostBody: `{"AdGroupIds":[null],"PartialErrors":[{"ErrorCode":"AdGroupServiceInvalidName"}]}`,
	}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an ad-group rejection error")
	}
	if res == nil || res.CampaignID != "321" {
		t.Fatalf("expected a partial carrying the campaign id 321, got %+v", res)
	}
	if res.AdGroupID != "" {
		t.Errorf("AdGroupID = %q, want empty on an ad-group rejection", res.AdGroupID)
	}
	if !strings.Contains(err.Error(), "AdGroupServiceInvalidName") {
		t.Errorf("error should surface the PartialError code, got: %v", err)
	}
}

func TestCreateCampaign_AdGroupNullPartialErrorIsUnconfirmed(t *testing.T) {
	// A 200 with a null id slot AND only a null PartialErrors placeholder (position-aligned,
	// no actual error) is a MALFORMED success, not a definite rejection: the ad group MAY
	// have been created. It must be UNCONFIRMED (verify before retry), carrying the campaign
	// id — mirroring TestCreateCampaign_NullPartialErrorIsUnconfirmed at the campaign level.
	api := &campaignsAPI{
		adGroupPostBody: `{"AdGroupIds":[null],"PartialErrors":[null]}`,
	}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an error on a malformed ad-group create")
	}
	if res == nil || res.CampaignID != "321" {
		t.Fatalf("expected a partial carrying campaign 321, got %+v", res)
	}
	if res.AdGroupID != "" {
		t.Errorf("AdGroupID = %q, want empty on an unconfirmed ad-group create", res.AdGroupID)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a null-only PartialErrors ad-group create must be UNCONFIRMED, not rejected: %v", err)
	}
}

func TestCreateCampaign_AdGroupUnparseable200IsUnconfirmed(t *testing.T) {
	// An unreadable 2xx ad-group create body (the create may have committed) is UNCONFIRMED,
	// not a clean failure — a blind retry could stack a duplicate ad group.
	api := &campaignsAPI{adGroupPostBody: `{not valid json`}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an error on an unparseable ad-group create body")
	}
	if res == nil || res.CampaignID != "321" {
		t.Fatalf("expected a partial carrying campaign 321, got %+v", res)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("an unparseable 2xx ad-group create must be UNCONFIRMED, got: %v", err)
	}
}

func TestCreateCampaign_ReusedCampaignFailedAdGroupNotAlreadyExisted(t *testing.T) {
	// Campaign is REUSED (found by name), but the ad-group create is rejected. The returned
	// partial must NOT report AlreadyExisted=true: this run attempted a lower level, so
	// "created nothing" is false — even though the campaign itself pre-existed.
	in := validInput()
	name := composeName(in)
	api := &campaignsAPI{
		getBody:         `{"Campaigns":[{"Id":999,"Name":` + jsonString(name) + `}]}`,
		adGroupPostBody: `{"AdGroupIds":[null],"PartialErrors":[{"ErrorCode":"AdGroupServiceInvalidName"}]}`,
	}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an ad-group rejection error")
	}
	if res == nil || res.CampaignID != "999" {
		t.Fatalf("expected a partial carrying the reused campaign 999, got %+v", res)
	}
	if res.AlreadyExisted {
		t.Error("AlreadyExisted = true on a failed ad-group step; want false (this run attempted a lower level)")
	}
}

func TestCreateCampaign_AdGroup5xxIsUnconfirmedCarriesCampaign(t *testing.T) {
	api := &campaignsAPI{adGroupStatus: http.StatusInternalServerError, adGroupPostBody: `{"Errors":[{"ErrorCode":"InternalError"}]}`}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an error on a 500 ad-group create")
	}
	if res == nil || res.CampaignID != "321" {
		t.Fatalf("expected a partial carrying campaign 321, got %+v", res)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a 5xx ad-group create should be UNCONFIRMED, got: %v", err)
	}
}

// ---- ad failure classification (carries campaign + ad group ids) -----------

func TestCreateCampaign_AdFailureCarriesCampaignAndAdGroup(t *testing.T) {
	api := &campaignsAPI{adStatus: http.StatusInternalServerError, adPostBody: `{"Errors":[{"ErrorCode":"InternalError"}]}`}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an error on a 500 ad create")
	}
	if res == nil || res.CampaignID != "321" || res.AdGroupID != "654" {
		t.Fatalf("expected a partial carrying campaign 321 + ad group 654, got %+v", res)
	}
	if res.AdID != "" {
		t.Errorf("AdID = %q, want empty on an ad failure", res.AdID)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a 5xx ad create should be UNCONFIRMED, got: %v", err)
	}
}

func TestCreateCampaign_ErrorTextSaysFoundForReusedCampaign(t *testing.T) {
	// When the campaign was matched by lookup (not created this run), a later-step failure's
	// error text must say "campaign <id> found", not "created" — otherwise a retry against an
	// existing hierarchy falsely attributes the campaign side effect to this run.
	in := validInput()
	name := composeName(in)
	api := &campaignsAPI{
		getBody:         `{"Campaigns":[{"Id":999,"Name":` + jsonString(name) + `}]}`,
		adGroupStatus:   http.StatusInternalServerError,
		adGroupPostBody: `{"Errors":[{"ErrorCode":"InternalError"}]}`,
	}
	c := newAPIClient(t, api.handler(t))
	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error on a 500 ad-group create")
	}
	if !strings.Contains(err.Error(), "campaign 999 found") {
		t.Errorf("error should say the campaign was FOUND (reused), got: %v", err)
	}
	if strings.Contains(err.Error(), "campaign 999 created") {
		t.Errorf("error must not claim the pre-existing campaign was created this run: %v", err)
	}
}

func TestCreateCampaign_DisplayDomainWideCapRejectsCJKHost(t *testing.T) {
	// A CJK IDN host of 34–67 runes passes the 67-char single-width display cap but is
	// rejected by the reduced 33-char double-width cap (conservative, matching the copy
	// limits), so the ad cannot fail to display after its parents were created.
	in := validInput()
	// 40 CJK runes as the host — well over 33, under 67.
	in.RegistrationURL = "https://" + strings.Repeat("字", 40) + ".example/register"
	c := newAPIClient(t, (&campaignsAPI{}).handler(t))
	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected a display-domain rejection for an oversized CJK host")
	}
	if !strings.Contains(err.Error(), "display domain") {
		t.Errorf("expected a display-domain error, got: %v", err)
	}
}

func TestCreateCampaign_AdNullPartialErrorIsUnconfirmed(t *testing.T) {
	// The ad-group create succeeds (654); the ad create returns a 200 with a null id slot
	// and only a null PartialErrors placeholder → malformed success. The ad MAY exist, so
	// UNCONFIRMED (not rejected), carrying the campaign + ad-group ids for reconcile.
	api := &campaignsAPI{
		adPostBody: `{"AdIds":[null],"PartialErrors":[null]}`,
	}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an error on a malformed ad create")
	}
	if res == nil || res.CampaignID != "321" || res.AdGroupID != "654" {
		t.Fatalf("expected a partial carrying campaign 321 + ad group 654, got %+v", res)
	}
	if res.AdID != "" {
		t.Errorf("AdID = %q, want empty on an unconfirmed ad create", res.AdID)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a null-only PartialErrors ad create must be UNCONFIRMED, not rejected: %v", err)
	}
}

// ---- ad destination validation ---------------------------------------------

func TestCreateCampaign_RejectsBadAdURL(t *testing.T) {
	// A bad destination URL is rejected UP FRONT, before the campaign create, so nothing
	// is created: a clean (nil, err), NOT a partial. This avoids orphaning a PAUSED
	// campaign (and ad group) behind a URL that was never going to work.
	// The userinfo case is built at runtime via url.UserPassword rather than written as a
	// "https://user:pass@host" literal, which would trip secretlint in CI (it flags any
	// credential-shaped literal). Mirrors the reddit client tests' runtime userinfo fixtures.
	userinfoURL := (&url.URL{
		Scheme: "https",
		User:   url.UserPassword("user", "pass"),
		Host:   "events.example.org",
		Path:   "/register",
	}).String()
	cases := map[string]string{
		"empty":     "",
		"no scheme": "events.example.org/register",
		"ftp":       "ftp://events.example.org/register",
		"userinfo":  userinfoURL,
	}
	for name, badURL := range cases {
		t.Run(name, func(t *testing.T) {
			var reached bool
			api := &campaignsAPI{}
			base := api.handler(t)
			c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
				reached = true // any API call means we failed to validate up front
				base(w, r)
			})
			in := validInput()
			in.RegistrationURL = badURL
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatalf("%s: expected an ad-URL validation error", name)
			}
			if res != nil {
				t.Errorf("%s: a bad URL must fail cleanly (nil result), got %+v", name, res)
			}
			if reached {
				t.Errorf("%s: no API call should be made — the URL is invalid up front", name)
			}
			// A userinfo URL error must not echo the password.
			if strings.Contains(err.Error(), "pass") {
				t.Errorf("%s: error leaked userinfo: %v", name, err)
			}
		})
	}
}

func TestCreateCampaign_RejectsOverLongDisplayDomain(t *testing.T) {
	// A registration URL whose HOST exceeds the 67-char display-domain limit passes the
	// 2,048-char FinalUrls check but Microsoft rejects the display domain at AddAds. It must
	// fail UP FRONT (nil, err, no API call) so a PAUSED campaign/ad group is never orphaned.
	var reached bool
	api := &campaignsAPI{}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		base(w, r)
	})
	in := validInput()
	// A syntactically valid hostname of 68 chars (label + ".example.org"), over the 67 limit.
	longHost := strings.Repeat("a", 68-len(".example.org")) + ".example.org"
	in.RegistrationURL = "https://" + longHost + "/register"

	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected a display-domain validation error")
	}
	if res != nil {
		t.Errorf("an over-long display domain must fail cleanly (nil result), got %+v", res)
	}
	if reached {
		t.Error("no API call should be made — the display domain is invalid up front")
	}
}

func TestCreateCampaign_RejectsBadAdCopy(t *testing.T) {
	// Over-count or over-long caller ad copy fails UP FRONT (before any API call), a clean
	// (nil, err) — the composed responsive search ad would otherwise be rejected by Microsoft.
	cases := map[string]func(*CampaignInput){
		"too many headlines":    func(in *CampaignInput) { in.Headlines = make([]string, maxAdHeadlines+1) },
		"too many descriptions": func(in *CampaignInput) { in.Descriptions = make([]string, maxAdDescriptions+1) },
		"over-long headline":    func(in *CampaignInput) { in.Headlines = []string{strings.Repeat("x", maxAdHeadlineRunes+1)} },
		"over-long description": func(in *CampaignInput) { in.Descriptions = []string{strings.Repeat("x", maxAdDescriptionRunes+1)} },
		"duplicate headline":    func(in *CampaignInput) { in.Headlines = []string{"Register", "register"} }, // case-insensitive dup
		"newline headline":      func(in *CampaignInput) { in.Headlines = []string{"first\nsecond"} },
		"wordless headline":     func(in *CampaignInput) { in.Headlines = []string{"!!!"} },
		// A CJK headline over the reduced 15-char double-width limit (but under the 30 single-width one).
		"over-long wide headline": func(in *CampaignInput) {
			in.Headlines = []string{strings.Repeat("字", maxAdHeadlineRunesWide+1)}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var reached bool
			api := &campaignsAPI{}
			base := api.handler(t)
			c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
				reached = true
				base(w, r)
			})
			in := validInput()
			mutate(&in)
			if _, err := c.CreateCampaign(context.Background(), in); err == nil {
				t.Fatalf("%s: expected an ad-copy validation error", name)
			}
			if reached {
				t.Errorf("%s: no API call should be made — copy is invalid up front", name)
			}
		})
	}
}

// ---- pure helpers ----------------------------------------------------------

func TestComposeAdCopy_BoundsCountsAndUniqueness(t *testing.T) {
	assertBounds := func(t *testing.T, hs, ds []string) {
		t.Helper()
		if len(hs) < minAdHeadlines || len(hs) > maxAdHeadlines {
			t.Errorf("headline count = %d, want %d-%d", len(hs), minAdHeadlines, maxAdHeadlines)
		}
		if len(ds) < minAdDescriptions || len(ds) > maxAdDescriptions {
			t.Errorf("description count = %d, want %d-%d", len(ds), minAdDescriptions, maxAdDescriptions)
		}
		assertUniqueBounded(t, hs, maxAdHeadlineRunes, "headline")
		assertUniqueBounded(t, ds, maxAdDescriptionRunes, "description")
	}

	// Auto-compose from a long event name: still meets the min counts, each entry truncated,
	// all unique. A long single event name must not collapse to fewer than the minimum.
	hs, ds := composeAdCopy(CampaignInput{EventName: strings.Repeat("A", 100), Project: "CNCF"})
	assertBounds(t, hs, ds)

	// Empty input still pads to the minimum with placeholders.
	hs, ds = composeAdCopy(CampaignInput{})
	assertBounds(t, hs, ds)

	// Over-max caller input is capped, and a truncated caller headline is bounded.
	many := make([]string, 20)
	for i := range many {
		many[i] = fmt.Sprintf("Headline %d", i)
	}
	hs, _ = composeAdCopy(CampaignInput{Headlines: many, EventName: "KubeCon"})
	if len(hs) != maxAdHeadlines {
		t.Errorf("over-max headlines not capped: got %d, want %d", len(hs), maxAdHeadlines)
	}
}

func TestComposeAdCopy_DropsWordlessAssets(t *testing.T) {
	// A punctuation-only EventName ("!!!") survives sanitizeNamePart non-empty and would
	// otherwise become a wordless headline candidate, which Microsoft rejects at AddAds. Every
	// composed asset must carry a word; the deterministic fallbacks/padding fill the minimum.
	hs, ds := composeAdCopy(CampaignInput{EventName: "!!!", Project: "CNCF"})
	for _, s := range append(append([]string{}, hs...), ds...) {
		if !hasWord(s) {
			t.Errorf("composed a wordless asset: %q", s)
		}
	}
	if len(hs) < minAdHeadlines {
		t.Errorf("headline count = %d, want >= %d", len(hs), minAdHeadlines)
	}
	if len(ds) < minAdDescriptions {
		t.Errorf("description count = %d, want >= %d", len(ds), minAdDescriptions)
	}
}

func assertUniqueBounded(t *testing.T, items []string, maxRunes int, kind string) {
	t.Helper()
	seen := map[string]struct{}{}
	for _, s := range items {
		if utf8.RuneCountInString(s) > maxRunes {
			t.Errorf("%s over limit (%d runes): %q", kind, utf8.RuneCountInString(s), s)
		}
		if s == "" {
			t.Errorf("%s is empty", kind)
		}
		key := strings.ToLower(s)
		if _, dup := seen[key]; dup {
			t.Errorf("%s duplicated (case-insensitive): %q", kind, s)
		}
		seen[key] = struct{}{}
	}
}

func TestBuildAdFinalURL_PreservesExistingQuery(t *testing.T) {
	in := validInput()
	in.RegistrationURL = "https://events.example.org/register?ref=partner&utm_source=old"
	got := buildAdFinalURL(in)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("built URL not parseable: %v", err)
	}
	q := u.Query()
	if q.Get("ref") != "partner" {
		t.Errorf("existing query param dropped: %s", got)
	}
	if q.Get("utm_source") != "microsoft" {
		t.Errorf("utm_source not overridden to microsoft: %s", got)
	}
}

func TestValidateAdURL(t *testing.T) {
	if err := validateAdURL("https://events.example.org/register"); err != nil {
		t.Errorf("a valid https URL must pass: %v", err)
	}
	for _, bad := range []string{"", "  ", "not a url", "//no-scheme", "javascript:alert(1)"} {
		if err := validateAdURL(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

// numberID's full contract (valid/zero/negative/fractional/exponent/nil) is covered by
// TestNumberID in campaign_test.go, alongside the numberID definition in campaign.go.

// checkAdCopyList and boundedUniqueCopy dedupe case-insensitively via
// strings.EqualFold, so Unicode case variants that share a fold collapse to one
// entry. The Greek final sigma "ς" and medial sigma "σ" are the canonical case:
// strings.ToLower keeps them distinct (a ToLower map key would treat them as two
// entries), while EqualFold treats them as equal. Under the old ToLower keying a
// case-insensitive duplicate would reach Microsoft, which then rejects the ad
// after its parents were already created.
func TestAdCopyDedupeIsUnicodeCaseFold(t *testing.T) {
	// "finish task οδός" vs "finish task οδόσ": identical but for final vs medial
	// sigma. EqualFold folds them together; ToLower does not.
	dupes := []string{"finish task οδός", "finish task οδόσ"}

	if err := checkAdCopyList("headline", dupes, maxAdHeadlines, maxAdHeadlineRunes, maxAdHeadlineRunesWide); err == nil {
		t.Fatalf("checkAdCopyList accepted a Unicode-case-fold duplicate: %v", dupes)
	}

	got := boundedUniqueCopy(dupes, maxAdHeadlineRunes, maxAdHeadlineRunesWide, 1, maxAdHeadlines)
	if len(got) != 1 {
		t.Fatalf("boundedUniqueCopy kept %d entries for a Unicode-case-fold duplicate, want 1: %q", len(got), got)
	}
}

// TestCreateCampaign_AdGroupLookupMatchWithNoIDIsUnconfirmed: an ad group whose unique name
// matches but whose Id is null must be UNCONFIRMED (verify before retry), not "absent" — else
// the client POSTs /AdGroups and creates a duplicate.
func TestCreateCampaign_AdGroupLookupMatchWithNoIDIsUnconfirmed(t *testing.T) {
	in := validInput()
	adGroupName := composeAdGroupName(in)
	adGroupPostReached := false
	api := &campaignsAPI{
		adGroupGetBody: `{"AdGroups":[{"Id":null,"Name":` + jsonString(adGroupName) + `}]}`,
	}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/AdGroups") {
			adGroupPostReached = true
		}
		base(w, r)
	})
	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an UNCONFIRMED error when the matching ad group has no usable id")
	}
	if adGroupPostReached {
		t.Error("ad group create POST issued despite a name-matching ad group (would duplicate)")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a name-match with an unusable id must be UNCONFIRMED, got: %v", err)
	}
}

// TestHasDoubleWidth_BMPEmoji: BMP emoji (❤️ ☀️ ✈️ — U+2600..27BF + the VS16 selector) must
// be treated as double-width so the reduced copy cap applies, not just supplementary-plane
// emoji (>= U+1F000).
func TestHasDoubleWidth_BMPEmoji(t *testing.T) {
	for _, s := range []string{"❤️", "☀", "✈️", "❤"} {
		if !hasDoubleWidth(s) {
			t.Errorf("hasDoubleWidth(%q) = false, want true (BMP emoji)", s)
		}
	}
	if hasDoubleWidth("Register Today") {
		t.Error("plain ASCII must not be flagged double-width")
	}
}

// TestCheckAdCopyList_WhitespaceOnlyEntryRejected: a whitespace-only caller entry carries no
// word and is rejected (per the CampaignInput contract), not silently dropped. A genuinely
// empty "" is still skippable (composeAdCopy pads).
func TestCheckAdCopyList_WhitespaceOnlyEntryRejected(t *testing.T) {
	err := checkAdCopyList("headline", []string{"Register Today", "   "}, maxAdHeadlines, maxAdHeadlineRunes, maxAdHeadlineRunesWide)
	if err == nil {
		t.Fatal("a whitespace-only entry must be rejected")
	}
	if !strings.Contains(err.Error(), "word") {
		t.Errorf("rejection should cite the missing word, got: %v", err)
	}
	// A genuinely empty string is fine (skipped, list padded elsewhere).
	if err := checkAdCopyList("headline", []string{"Register Today", ""}, maxAdHeadlines, maxAdHeadlineRunes, maxAdHeadlineRunesWide); err != nil {
		t.Errorf("an empty string entry must be skipped, not rejected: %v", err)
	}
}

// TestCreateCampaign_AdGroupLookupUnparseableBodyIsUnconfirmed: a 2xx ad-group lookup whose
// body fails to decode leaves it UNKNOWN whether the group exists, so it must be UNCONFIRMED
// (verify before retry), not a definite "creation failed" that could invite a duplicate.
func TestCreateCampaign_AdGroupLookupUnparseableBodyIsUnconfirmed(t *testing.T) {
	adGroupPostReached := false
	api := &campaignsAPI{
		adGroupGetBody: `{"AdGroups": not-json`, // 2xx but unparseable
	}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/AdGroups") {
			adGroupPostReached = true
		}
		base(w, r)
	})
	_, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an error on an unparseable ad-group lookup body")
	}
	if adGroupPostReached {
		t.Error("ad group create POST issued after an unparseable lookup (would risk a duplicate)")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("an unparseable 2xx lookup must be UNCONFIRMED, got: %v", err)
	}
}
