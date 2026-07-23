// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
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
	// Ad group is PAUSED.
	if group.AdGroups[0].Status != adGroupStatusPaused {
		t.Errorf("ad group Status = %q, want %q", group.AdGroups[0].Status, adGroupStatusPaused)
	}
	// Ad is a Text ad with the destination + UTM params, bounded copy.
	got := ad.Ads[0]
	if got.Type != adTypeText {
		t.Errorf("ad Type = %q, want %q", got.Type, adTypeText)
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
	if got.Title == "" || got.Text == "" {
		t.Errorf("ad copy empty: title=%q text=%q", got.Title, got.Text)
	}
}

func TestCreateCampaign_HonorsExplicitAdCopy(t *testing.T) {
	var ad createAdsRequest
	api := &campaignsAPI{adSeen: &ad}
	c := newAPIClient(t, api.handler(t))
	in := validInput()
	in.Headline = "Register for KubeCon EU"
	in.Description = "Join thousands of cloud native practitioners this spring."
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if ad.Ads[0].Title != in.Headline {
		t.Errorf("ad Title = %q, want the caller headline", ad.Ads[0].Title)
	}
	if ad.Ads[0].Text != in.Description {
		t.Errorf("ad Text = %q, want the caller description", ad.Ads[0].Text)
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
	cases := map[string]string{
		"empty":     "",
		"no scheme": "events.example.org/register",
		"ftp":       "ftp://events.example.org/register",
		"userinfo":  "https://user:pass@events.example.org/register",
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

// ---- pure helpers ----------------------------------------------------------

func TestComposeAdCopy_BoundsAndDefaults(t *testing.T) {
	// Long event name → title truncated to the limit, default text derived.
	in := CampaignInput{EventName: strings.Repeat("A", 100)}
	title, text := composeAdCopy(in)
	if len([]rune(title)) > maxAdTitleRunes {
		t.Errorf("title not truncated: %d runes", len([]rune(title)))
	}
	if len([]rune(text)) > maxAdTextRunes {
		t.Errorf("text not truncated: %d runes", len([]rune(text)))
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
