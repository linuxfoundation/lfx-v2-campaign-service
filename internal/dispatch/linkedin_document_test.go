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
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// liDocumentAssetID is the stored PDF of the document fixture.
const liDocumentAssetID = "4d0c5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f"

var liTestPDF = []byte("%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n")

// liDocumentAPI is a fake LinkedIn REST API for the campaign tree, the Documents API and a lead form.
type liDocumentAPI struct {
	srv *httptest.Server

	mu        sync.Mutex
	calls     []string
	campaigns []map[string]any
	posts     []map[string]any
	creatives []map[string]any
}

func newLIDocumentAPI(t *testing.T) *liDocumentAPI {
	t.Helper()
	a := &liDocumentAPI{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		a.calls = append(a.calls, r.Method+" "+r.URL.Path)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/documents":
			_, _ = io.WriteString(w, `{"value":{"uploadUrl":"`+a.srv.URL+`/dms-uploads/DOC1/0?ut=sig","document":"urn:li:document:DOC1"}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/documents/"):
			_, _ = io.WriteString(w, `{"status":"AVAILABLE"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/leadForms/"):
			_, _ = io.WriteString(w, `{"id":12345,"state":"PUBLISHED","owner":{"sponsoredAccount":"urn:li:sponsoredAccount:123456789"}}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			a.campaigns = append(a.campaigns, body)
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			a.posts = append(a.posts, body)
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			a.creatives = append(a.creatives, body)
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *liDocumentAPI) total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func newLIDocumentDispatcher(api *liDocumentAPI, assets creativeAssetReader, reserver *AssetReserver) *LinkedInDispatcher {
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(api.srv.URL), linkedin.WithClock(clock),
		linkedin.WithImagePolling(time.Millisecond, time.Second), linkedin.WithDocumentPolling(time.Millisecond, time.Second))
	if assets != nil {
		d.SetCreativeAssetRepo(assets)
	}
	d.SetAssetReserver(reserver)
	return d
}

// liDocumentConfig builds a format "document" linkedInConfig with any extra top-level fields.
func liDocumentConfig(extra string) json.RawMessage {
	if extra != "" {
		extra = "," + extra
	}
	return json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"format":"document","documentAssetId":"` + liDocumentAssetID + `",
		"documentTitle":"KubeCon 2099 sponsor prospectus","documentIntroText":"Sponsor KubeCon 2099"` + extra + `}}`)
}

// liDocumentAssets holds the stored PDF of the test brief.
func liDocumentAssets() *scopedCreativeAssets {
	brief := testBrief()
	return &scopedCreativeAssets{
		projectID: brief.ProjectID, briefID: brief.ID, assetID: liDocumentAssetID,
		asset: &model.CreativeAsset{ID: liDocumentAssetID, MimeType: model.MimeTypePDF, Bytes: liTestPDF},
	}
}

// TestLinkedInDocument_DispatchEndToEnd drives PreflightCreate and Dispatch with the flag on: an
// ENGAGEMENT campaign (decision 2026-11-25) sent without a format, the PDF uploaded through the
// Documents API, one dark post carrying it and one creative, recorded as a clean create, with the
// snapshot holding the canonical asset id under documentAssetId.
func TestLinkedInDocument_DispatchEndToEnd(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsDocumentEnabled, "true")
	api := newLIDocumentAPI(t)
	d := newLIDocumentDispatcher(api, liDocumentAssets(), nil)
	cfg := json.RawMessage(strings.Replace(string(liDocumentConfig("")), liDocumentAssetID, strings.ToUpper(liDocumentAssetID), 1))

	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, cfg); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("PreflightCreate on a valid document = %v, want ErrPreflightNotApplicable", err)
	}
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want %q", camp.Status, campaignStatusCreated)
	}
	if len(api.campaigns) != 1 || api.campaigns[0]["objectiveType"] != linkedin.ObjectiveEngagement || api.campaigns[0]["optimizationTargetType"] != linkedin.OptimizationTargetMaxReach {
		t.Fatalf("campaign bodies = %v, want one ENGAGEMENT / MAX_REACH campaign", api.campaigns)
	}
	if _, set := api.campaigns[0]["format"]; set {
		t.Errorf("campaign sent format %v", api.campaigns[0]["format"])
	}
	if len(api.posts) != 1 || len(api.creatives) != 1 {
		t.Fatalf("posts %d, creatives %d; want 1 and 1", len(api.posts), len(api.creatives))
	}
	content, _ := api.posts[0]["content"].(map[string]any)
	if media, _ := content["media"].(map[string]any); media["id"] != "urn:li:document:DOC1" {
		t.Errorf("post content = %v", content)
	}
	crContent, _ := api.creatives[0]["content"].(map[string]any)
	if docAd, _ := crContent["documentAd"].(map[string]any); docAd["reference"] != "urn:li:share:300" {
		t.Errorf("creative content = %v, want content.documentAd.reference", crContent)
	}
	snap := string(camp.ConfigSnapshot)
	if !strings.Contains(snap, `"documentAssetId":"`+liDocumentAssetID+`"`) {
		t.Errorf("snapshot must store the canonical asset id under documentAssetId: %s", snap)
	}
	var res struct {
		DocumentURNs map[string]string `json:"documentUrns"`
	}
	if err := json.Unmarshal(camp.Result, &res); err != nil || res.DocumentURNs[liDocumentAssetID] != "urn:li:document:DOC1" {
		t.Errorf("result documentUrns = %v (err %v)", res.DocumentURNs, err)
	}
}

// TestLinkedInDocument_BriefWithoutRegistrationURL: a brief with no registration URL still dispatches
// a document ad, end to end; the post carries the PDF and no landing page.
func TestLinkedInDocument_BriefWithoutRegistrationURL(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsDocumentEnabled, "true")
	api := newLIDocumentAPI(t)
	d := newLIDocumentDispatcher(api, liDocumentAssets(), nil)
	brief := testBrief()
	brief.EventDetails = json.RawMessage(`{"eventName":"KubeCon NA 2026","project":"cncf"}`)
	camp, err := d.Dispatch(context.Background(), brief, model.ProviderLinkedInAds, liDocumentConfig(""))
	if err != nil || camp == nil || camp.Status != campaignStatusCreated {
		t.Fatalf("Dispatch: camp=%v err=%v; want a created document ad", camp, err)
	}
	if len(api.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(api.posts))
	}
}

// TestLinkedInDocument_LeadGen: objective "lead_generation" with a form makes a gated document — a
// LEAD_GENERATION campaign whose creative carries the form (decision 2026-11-25).
func TestLinkedInDocument_LeadGen(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsDocumentEnabled, "true")
	t.Setenv(constants.EnvLinkedInAdsLeadGenEnabled, "true")
	api := newLIDocumentAPI(t)
	d := newLIDocumentDispatcher(api, liDocumentAssets(), nil)
	cfg := liDocumentConfig(`"objective":"lead_generation","leadGenForm":"12345"`)
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, cfg); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("PreflightCreate = %v", err)
	}
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if api.campaigns[0]["objectiveType"] != linkedin.ObjectiveLeadGeneration {
		t.Errorf("objectiveType = %v, want LEAD_GENERATION", api.campaigns[0]["objectiveType"])
	}
	if cta, _ := api.creatives[0]["leadgenCallToAction"].(map[string]any); cta["destination"] != "urn:li:adForm:12345" {
		t.Errorf("creative = %v, want the form", api.creatives[0])
	}
}

// TestLinkedInDocument_RefusedBeforeAnythingIsCreated: the flag, the objective, the request shape
// and the asset are decided before any LinkedIn request, the claim and the reservation are
// released, and every shape refusal is also PreflightCreate's synchronous 400.
func TestLinkedInDocument_RefusedBeforeAnythingIsCreated(t *testing.T) {
	withAsset := func(mime string, bytes []byte) func() *scopedCreativeAssets {
		return func() *scopedCreativeAssets {
			a := liDocumentAssets()
			a.asset.MimeType, a.asset.Bytes = mime, bytes
			return a
		}
	}
	replace := func(old, repl string) json.RawMessage {
		return json.RawMessage(strings.Replace(string(liDocumentConfig("")), old, repl, 1))
	}
	cases := []struct {
		name      string
		flag      string
		cfg       json.RawMessage
		assets    func() *scopedCreativeAssets
		preflight bool
		want      string
	}{
		{"feature off", "", liDocumentConfig(""), liDocumentAssets, true, constants.EnvLinkedInAdsDocumentEnabled},
		{"feature misspelled", "TRUE", liDocumentConfig(""), liDocumentAssets, true, constants.EnvLinkedInAdsDocumentEnabled},
		{"objective traffic", "true", liDocumentConfig(`"objective":"traffic"`), liDocumentAssets, true, `no objective "traffic"`},
		{"objective conversions", "true", liDocumentConfig(`"objective":"conversions","bidding":{"strategy":"manual","bidAmount":5}`), liDocumentAssets, true, `no objective "conversions"`},
		{"manual bid", "true", liDocumentConfig(`"bidding":{"strategy":"manual","bidAmount":5}`), liDocumentAssets, true, "automatic bidding only"},
		{"variants beside the document", "true", liDocumentConfig(`"variants":[` + liVariant("") + `]`), liDocumentAssets, true, "omit variants"},
		{"malformed asset id", "true", replace(liDocumentAssetID, "not-a-uuid"), liDocumentAssets, true, "not a valid asset id"},
		{"title over 70", "true", replace("KubeCon 2099 sponsor prospectus", strings.Repeat("t", 71)), liDocumentAssets, true, "documentTitle is 71 characters"},
		{"numeric title", "true", replace(`"KubeCon 2099 sponsor prospectus"`, "7"), liDocumentAssets, true, "must be strings"},
		{"null asset id", "true", replace(`"`+liDocumentAssetID+`"`, "null"), liDocumentAssets, true, "must be strings"},
		{"document fields without the format", "true", json.RawMessage(strings.Replace(string(liDocumentConfig(`"variants":[`+liVariant("")+`]`)), `"format":"document",`, "", 1)), liDocumentAssets, true, `accepted only with format "document"`},
		{"asset of another brief", "true", liDocumentConfig(""), func() *scopedCreativeAssets {
			a := liDocumentAssets()
			a.briefID = "other-brief"
			return a
		}, false, "does not exist for this brief"},
		{"image named as the document", "true", liDocumentConfig(""), withAsset(model.MimeTypePNG, msTestPNG(t, 400, 400)), false, "which is an image; a LinkedIn document ad takes a PDF"},
		{"video named as the document", "true", liDocumentConfig(""), withAsset(model.MimeTypeMP4, liTestMP4(liVideoSize)), false, "which is a video; a LinkedIn document ad takes a PDF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsDocumentEnabled, tc.flag)
			api := newLIDocumentAPI(t)
			reserver := NewAssetReserver(10<<20, testAssetReserveWait)
			d := newLIDocumentDispatcher(api, tc.assets(), reserver)
			perr := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, tc.cfg)
			if tc.preflight {
				if !errors.Is(perr, domain.ErrCreateConfigInvalid) || !strings.Contains(perr.Error(), tc.want) {
					t.Errorf("PreflightCreate = %v, want a 400 containing %q", perr, tc.want)
				}
			} else if !errors.Is(perr, domain.ErrPreflightNotApplicable) {
				t.Errorf("PreflightCreate = %v, want ErrPreflightNotApplicable", perr)
			}
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, tc.cfg)
			if camp != nil || !isNoUpstreamCreate(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a NoUpstreamCreate refusal containing %q, got camp=%v err=%v", tc.want, camp, err)
			}
			if n := api.total(); n != 0 {
				t.Errorf("%d request(s) reached LinkedIn; a refused document must send none", n)
			}
			if rel, ok := reserver.reserve(context.Background(), 10<<20); !ok {
				t.Error("a refused dispatch leaked its asset reservation")
			} else {
				rel()
			}
		})
	}
}

// TestLinkedInDocument_PDFRefusedInImageAndVideoSlots: a stored PDF (#665) named as a variant's
// image or video is refused before any LinkedIn request.
func TestLinkedInDocument_PDFRefusedInImageAndVideoSlots(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsVideoEnabled, "true")
	pdf := func() *scopedCreativeAssets {
		a := liDocumentAssets()
		a.assetID, a.asset.ID = msAssetID, msAssetID
		// Above the video size floor, so the slot's type is what refuses it.
		a.asset.Bytes = append(append([]byte{}, liTestPDF...), make([]byte, linkedin.MinVideoBytes)...)
		return a
	}
	for _, tc := range []struct {
		name, variant, want string
	}{
		{"image slot", liVariant(`"imageAssetId":"` + msAssetID + `"`), "but it is a PDF document; an image is a PNG or JPEG"},
		{"video slot", liVariant(`"videoAssetId":"` + msAssetID + `"`), "but it is a PDF document; a video is an MP4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newLIDocumentAPI(t)
			d := newLIDocumentDispatcher(api, pdf(), nil)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, liImageConfig(tc.variant))
			if camp != nil || !isNoUpstreamCreate(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got camp=%v err=%v; want %q", camp, err, tc.want)
			}
			if n := api.total(); n != 0 {
				t.Errorf("%d request(s) reached LinkedIn", n)
			}
		})
	}
}
