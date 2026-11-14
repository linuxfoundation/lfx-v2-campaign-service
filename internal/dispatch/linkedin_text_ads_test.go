// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// linkedInTextConfig is a text create (#586) whose linkedInConfig carries the given extra
// fields; the base is a valid manual-bid text create with two text ads.
func linkedInTextConfig(extra string) json.RawMessage {
	cfg := `{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"bidding":{"strategy":"manual","bidAmount":5.04}`
	if extra != "" {
		cfg += "," + extra
	}
	return json.RawMessage(cfg + `}}`)
}

const validTextAds = `"format":"text","textAds":[
	{"headline":"KubeCon NA 2099","description":"Join cloud native engineers in Atlanta.","imageUrn":"urn:li:image:OWN1"},
	{"headline":"KubeCon NA 2099","description":"Early pricing ends Friday."}]`

func TestLinkedIn_TextAdsPreflight(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsTextAdsEnabled, "true")
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	auto := json.RawMessage(strings.Replace(string(linkedInTextConfig(validTextAds)), `"bidding":{"strategy":"manual","bidAmount":5.04}`, `"bidding":{"strategy":"automatic"}`, 1))
	noBid := json.RawMessage(strings.Replace(string(linkedInTextConfig(validTextAds)), `"bidding":{"strategy":"manual","bidAmount":5.04},`, ``, 1))
	for _, tc := range []struct {
		name string
		cfg  json.RawMessage
		want string // "" not applicable
	}{
		{"valid", linkedInTextConfig(validTextAds), ""},
		{"explicit single_image, no text ads", linkedInTextConfig(`"format":"single_image"`), ""},
		{"no format, no text ads", linkedInTextConfig(``), ""},
		{"automatic bidding", auto, "requires bidding"},
		{"omitted bidding is automatic", noBid, "requires bidding"},
		{"unknown format", linkedInTextConfig(`"format":"video"`), "format must be"},
		{"format as a number", linkedInTextConfig(`"format":1`), "format must be the string"},
		{"format null", linkedInTextConfig(`"format":null`), "format must be the string"},
		{"text ads without format", linkedInTextConfig(`"textAds":[{"headline":"h","description":"d"}]`), "only with format"},
		{"empty text ads without format", linkedInTextConfig(`"textAds":[]`), "only with format"},
		{"text with no text ads", linkedInTextConfig(`"format":"text"`), "at least one entry"},
		{"text with variants", linkedInTextConfig(validTextAds + `,"variants":[{"introText":"i","headline":"h"}]`), "omit variants"},
		{"long headline", linkedInTextConfig(`"format":"text","textAds":[{"headline":"` + strings.Repeat("h", 26) + `","description":"d"}]`), "headline is 26 characters"},
		{"long description", linkedInTextConfig(`"format":"text","textAds":[{"headline":"h","description":"` + strings.Repeat("d", 76) + `"}]`), "description is 76 characters"},
		{"text ads as an object", linkedInTextConfig(`"format":"text","textAds":{"headline":"h","description":"d"}`), "must be an array of objects"},
		{"text ads null", linkedInTextConfig(`"format":"text","textAds":null`), "must be an array of objects"},
		{"a number headline", linkedInTextConfig(`"format":"text","textAds":[{"headline":7,"description":"d"}]`), "must be an array of objects"},
		{"a null description", linkedInTextConfig(`"format":"text","textAds":[{"headline":"h","description":null}]`), "must be an array of objects"},
		{"a misspelled key", linkedInTextConfig(`"format":"text","textAds":[{"headline":"h","descripton":"d"}]`), "must be an array of objects"},
		{"a string entry", linkedInTextConfig(`"format":"text","textAds":["h"]`), "must be an array of objects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, tc.cfg)
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

func TestLinkedIn_TextAdsRefusedWhileTheFlagIsOff(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsTextAdsEnabled, "TRUE")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
	cfg := linkedInTextConfig(validTextAds)

	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, cfg)
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), constants.EnvLinkedInAdsTextAdsEnabled) {
		t.Fatalf("PreflightCreate = %v, want a 400 naming %s", err, constants.EnvLinkedInAdsTextAdsEnabled)
	}
	_, err = d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !isNoUpstreamCreate(err) {
		t.Fatalf("Dispatch = %v, want a not-created config refusal", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d LinkedIn requests for a refused create", n)
	}
	// The default create is not this flag's business.
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, linkedInTextConfig(``)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("default create with the flag off: PreflightCreate = %v, want not applicable", err)
	}
}

// liTextAPI is a fake LinkedIn API recording the campaign and creative bodies of a create.
type liTextAPI struct {
	srv       *httptest.Server
	mu        sync.Mutex
	calls     []string
	campaign  map[string]any
	creatives []map[string]any
}

func newLITextAPI(t *testing.T) *liTextAPI {
	t.Helper()
	a := &liTextAPI{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		a.calls = append(a.calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images"):
			_, _ = io.WriteString(w, `{"value":{"uploadUrl":"`+a.srv.URL+`/dms-uploads/IMG1/0?ut=sig","image":"urn:li:image:IMG1"}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/"):
			_, _ = io.WriteString(w, `{"status":"AVAILABLE"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			_ = json.Unmarshal(raw, &a.campaign)
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "creatives"):
			var b map[string]any
			_ = json.Unmarshal(raw, &b)
			a.creatives = append(a.creatives, b)
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func newLITextDispatcher(api *liTextAPI, assets creativeAssetReader) *LinkedInDispatcher {
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(api.srv.URL), linkedin.WithClock(clock), linkedin.WithImagePolling(time.Millisecond, time.Second))
	if assets != nil {
		d.SetCreativeAssetRepo(assets)
	}
	return d
}

// TestLinkedIn_TextAdsEndToEnd drives Dispatch through the LinkedIn client against a fake API
// and pins the wire bodies (#586): a TEXT_AD campaign on the manual plan, no dark post, a
// textAd creative per entry with the stored asset's uploaded image, a created row whose
// strategy and bid are recorded, and a snapshot carrying the canonical format and text ads.
func TestLinkedIn_TextAdsEndToEnd(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsTextAdsEnabled, "true")
	api := newLITextAPI(t)
	d := newLITextDispatcher(api, liPNGAsset(t, 100, 100))
	cfg := linkedInTextConfig(`"format":"text","textAds":[
		{"headline":"KubeCon NA 2099","description":"Join cloud native engineers.","imageUrn":"urn:li:image:OWN1"},
		{"headline":"KubeCon NA 2099","description":"Early pricing ends Friday.","imageAssetId":"` + strings.ToUpper(msAssetID) + `"}]`)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated || camp.PlatformCampaignID != "200" {
		t.Errorf("row = %q/%q, want created/200", camp.Status, camp.PlatformCampaignID)
	}
	if camp.BiddingStrategy == nil || *camp.BiddingStrategy != model.BiddingManual {
		t.Errorf("bidding strategy = %v, want manual recorded", camp.BiddingStrategy)
	}
	if !strings.Contains(camp.CampaignName, "| Conversions | Prospecting | Text |") {
		t.Errorf("campaign name = %q, want the Text Ad Format segment", camp.CampaignName)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, c := range api.calls {
		if strings.Contains(c, "/posts") {
			t.Errorf("a text create sent %s", c)
		}
	}
	if api.campaign["type"] != "TEXT_AD" || api.campaign["objectiveType"] != "WEBSITE_CONVERSION" ||
		api.campaign["costType"] != "CPC" || api.campaign["optimizationTargetType"] != "NONE" {
		t.Errorf("campaign body = %v", api.campaign)
	}
	if len(api.creatives) != 2 {
		t.Fatalf("creatives = %d, want 2", len(api.creatives))
	}
	images := []string{"urn:li:image:OWN1", "urn:li:image:IMG1"}
	for i, cr := range api.creatives {
		ta, _ := cr["content"].(map[string]any)["textAd"].(map[string]any)
		if ta == nil || ta["headline"] != "KubeCon NA 2099" || ta["image"] != images[i] || !strings.HasPrefix(ta["landingPage"].(string), "https://") {
			t.Errorf("creative %d textAd = %v", i, ta)
		}
		if cr["intendedStatus"] != "DRAFT" {
			t.Errorf("creative %d intendedStatus = %v, want DRAFT", i, cr["intendedStatus"])
		}
	}
	var snap linkedinConfig
	if err := json.Unmarshal(camp.ConfigSnapshot, &snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Format != linkedin.FormatText || len(snap.TextAds) != 2 || snap.TextAds[1].ImageAssetID != msAssetID {
		t.Errorf("snapshot = format %q, text ads %+v; want text, 2, the canonical asset id", snap.Format, snap.TextAds)
	}
}

// TestLinkedIn_DefaultFormatSnapshotUnchanged: an explicit "single_image" is stored as the
// omitted default, so a default create's snapshot and name are what they were before #586.
func TestLinkedIn_DefaultFormatSnapshotUnchanged(t *testing.T) {
	d := newLIImageDispatcher(newLIImageAPI(t), nil, nil)
	cfg := json.RawMessage(strings.Replace(string(liImageConfig(liVariant(""))), `"budgetUsd":100`, `"format":"single_image","budgetUsd":100`, 1))
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if strings.Contains(string(camp.ConfigSnapshot), `"format"`) || strings.Contains(string(camp.ConfigSnapshot), `"textAds"`) {
		t.Errorf("snapshot = %s, want no format or textAds", camp.ConfigSnapshot)
	}
	if !strings.Contains(camp.CampaignName, "| Static |") {
		t.Errorf("default name = %q, want the Static segment", camp.CampaignName)
	}
}

// TestLinkedIn_TextAdStoredImageRefusedBeforeAnythingIsCreated: a stored asset that is not a
// 100x100 text ad image, or that does not exist for the brief, fails pre-create with nothing
// sent, and a refusal that names the referencing field names textAds — the field the asset came
// from on a text create, which may not carry variants at all.
func TestLinkedIn_TextAdStoredImageRefusedBeforeAnythingIsCreated(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsTextAdsEnabled, "true")
	const unknownAssetID = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	for _, tc := range []struct {
		name    string
		assetID string
		want    string
	}{
		{"not 100x100", msAssetID, "require 100x100"},
		{"asset does not exist", unknownAssetID, "linkedInConfig.textAds reference creative asset " + unknownAssetID + ", which does not exist for this brief"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newLITextAPI(t)
			d := newLITextDispatcher(api, liPNGAsset(t, 1200, 627))
			cfg := linkedInTextConfig(`"format":"text","textAds":[{"headline":"h","description":"d","imageAssetId":"` + tc.assetID + `"}]`)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
			if !isNoUpstreamCreate(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Dispatch = %v, want a not-created refusal containing %q", err, tc.want)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if len(api.calls) != 0 {
				t.Errorf("requests sent: %v", api.calls)
			}
		})
	}
}
