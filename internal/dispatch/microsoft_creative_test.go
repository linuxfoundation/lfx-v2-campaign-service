// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

const (
	msAssetID      = "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b"
	msOtherBriefID = "brief-2"
	msLeakText     = "UPSTREAM-MESSAGE-TEXT-must-not-leak"
)

func msTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// scopedCreativeAssets serves an asset ONLY to the (project, brief) that owns it, as the real
// repository's scoped lookup does, and counts the BYTE reads so a test can prove a refused
// asset was never loaded.
type scopedCreativeAssets struct {
	projectID, briefID, assetID string
	asset                       *model.CreativeAsset
	sizeOverride                int64
	getErr                      error

	mu       sync.Mutex
	getCalls int
}

func (s *scopedCreativeAssets) owns(projectID, briefID, assetID string) bool {
	return projectID == s.projectID && briefID == s.briefID && assetID == s.assetID
}

func (s *scopedCreativeAssets) GetAsset(_ context.Context, projectID, briefID, assetID string) (*model.CreativeAsset, error) {
	s.mu.Lock()
	s.getCalls++
	s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	if !s.owns(projectID, briefID, assetID) {
		return nil, domain.ErrNotFound
	}
	return s.asset, nil
}

func (s *scopedCreativeAssets) GetAssetSize(_ context.Context, projectID, briefID, assetID string) (int64, error) {
	if !s.owns(projectID, briefID, assetID) {
		return 0, domain.ErrNotFound
	}
	if s.sizeOverride != 0 {
		return s.sizeOverride, nil
	}
	return int64(len(s.asset.Bytes)), nil
}

func (s *scopedCreativeAssets) loads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCalls
}

func ownedPNGAsset(t *testing.T) *scopedCreativeAssets {
	brief := testBrief()
	return &scopedCreativeAssets{
		projectID: brief.ProjectID, briefID: brief.ID, assetID: msAssetID,
		asset: &model.CreativeAsset{ID: msAssetID, MimeType: model.MimeTypePNG, Bytes: msTestPNG(t, 703, 368)},
	}
}

// msImageAPI is a fake Microsoft API covering the campaign tree AND the image-extension
// steps. It is STATEFUL across dispatches: once an extension is associated, the association
// query reports it, which is what a retry against the real service would see.
type msImageAPI struct {
	mu       sync.Mutex
	calls    []string
	bodies   map[string]string
	attached bool
	// override answers a path suffix with a fixed status/body instead of the default.
	override map[string]struct {
		status int
		body   string
	}
}

func (a *msImageAPI) count(suffix string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		if strings.HasSuffix(c, suffix) {
			n++
		}
	}
	return n
}

func (a *msImageAPI) total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func newMSImageDispatcher(t *testing.T, api *msImageAPI, assets creativeAssetReader) *MicrosoftDispatcher {
	t.Helper()
	if api.bodies == nil {
		api.bodies = map[string]string{}
	}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p := r.URL.Path[strings.Index(r.URL.Path, "/v13/")+len("/v13/"):]
		api.mu.Lock()
		api.calls = append(api.calls, p)
		api.bodies[p] = string(b)
		ov, hasOv := api.override[p]
		attached := api.attached
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if hasOv {
			w.WriteHeader(ov.status)
			_, _ = io.WriteString(w, ov.body)
			return
		}
		switch p {
		case "Campaigns/QueryByAccountId":
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case "AdGroups/QueryByCampaignId":
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case "Ads/QueryByAdGroupId":
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case "Campaigns":
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case "AdGroups":
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case "Ads":
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		case "AdExtensionsAssociations/Query":
			if attached {
				_, _ = io.WriteString(w, `{"AdExtensionAssociationCollection":[{"AdExtensionAssociations":[{"AdExtension":{"Id":7771,"Type":"ImageAdExtension"},"AssociationType":"Campaign","EntityId":321}]}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"AdExtensionAssociationCollection":[null]}`)
		case "Media":
			_, _ = io.WriteString(w, `{"MediaIds":[5551]}`)
		case "AdExtensions":
			_, _ = io.WriteString(w, `{"AdExtensionIdentities":[{"Id":7771,"Version":1}],"NestedPartialErrors":[]}`)
		case "AdExtensionsAssociations/Set":
			api.mu.Lock()
			api.attached = true
			api.mu.Unlock()
			_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL))
	d.SetCreativeAssetRepo(assets)
	return d
}

func msImageConfig(extra string) json.RawMessage {
	return json.RawMessage(`{"microsoftConfig":{"budget":50` + extra + `}}`)
}

type msResultBlob struct {
	ImageMediaID       string   `json:"imageMediaId"`
	ImageAdExtensionID string   `json:"imageAdExtensionId"`
	Steps              []string `json:"steps"`
}

func decodeMSResult(t *testing.T, camp *model.Campaign) msResultBlob {
	t.Helper()
	var b msResultBlob
	if err := json.Unmarshal(camp.Result, &b); err != nil {
		t.Fatalf("result blob: %v", err)
	}
	return b
}

func TestMicrosoftImage_DispatchAttachesStoredAsset(t *testing.T) {
	api := &msImageAPI{}
	assets := ownedPNGAsset(t)
	d := newMSImageDispatcher(t, api, assets)
	d.SetAssetReserver(NewAssetReserver(MaxConcurrentVariantAssetBytes, 0))

	// A braced spelling of the id: it must be canonicalized before lookup and snapshot.
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds,
		msImageConfig(`,"imageAssetId":"{`+strings.ToUpper(msAssetID)+`}","imageAltText":"KubeCon NA"`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	blob := decodeMSResult(t, camp)
	if blob.ImageMediaID != "5551" || blob.ImageAdExtensionID != "7771" {
		t.Errorf("the upload must be recorded in the result, got %+v", blob)
	}
	var media struct{ Media []struct{ Data string } }
	_ = json.Unmarshal([]byte(api.bodies["Media"]), &media)
	if len(media.Media) != 1 {
		t.Fatalf("AddMedia body = %s", api.bodies["Media"])
	}
	if dec, _ := base64.StdEncoding.DecodeString(media.Media[0].Data); !bytes.Equal(dec, assets.asset.Bytes) {
		t.Error("AddMedia must carry the stored asset's bytes")
	}
	if !strings.Contains(api.bodies["AdExtensions"], `"AlternativeText":"KubeCon NA"`) {
		t.Errorf("the caller's alt text must reach the extension: %s", api.bodies["AdExtensions"])
	}
	var snap microsoftConfig
	if err := json.Unmarshal(camp.ConfigSnapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.ImageAssetID != msAssetID {
		t.Errorf("snapshot imageAssetId = %q, want the canonical %q", snap.ImageAssetID, msAssetID)
	}
	// The reservation was released at the end of the dispatch: the whole budget is free.
	if !d.assets.sem.TryAcquire(d.assets.budget) {
		t.Error("the asset reservation must be released when the dispatch returns")
	}
}

func TestMicrosoftImage_DefaultAltTextIsTheEventName(t *testing.T) {
	api := &msImageAPI{}
	d := newMSImageDispatcher(t, api, ownedPNGAsset(t))
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds,
		msImageConfig(`,"imageAssetId":"`+msAssetID+`"`)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(api.bodies["AdExtensions"], `"AlternativeText":"KubeCon NA 2026"`) {
		t.Errorf("omitted alt text must default to the event name: %s", api.bodies["AdExtensions"])
	}
}

func TestMicrosoftImage_DispatchWithoutAssetTouchesNoImageEndpoint(t *testing.T) {
	api := &msImageAPI{}
	assets := ownedPNGAsset(t)
	d := newMSImageDispatcher(t, api, assets)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msImageConfig(""))
	if err != nil || camp.Status != campaignStatusCreated {
		t.Fatalf("Dispatch = %v, %v", camp, err)
	}
	for _, p := range []string{"AdExtensionsAssociations/Query", "Media", "AdExtensions", "AdExtensionsAssociations/Set"} {
		if n := api.count(p); n != 0 {
			t.Errorf("%s called %d times on an image-less dispatch", p, n)
		}
	}
	if assets.loads() != 0 {
		t.Error("no asset may be read when none is requested")
	}
	if blob := decodeMSResult(t, camp); blob.ImageMediaID != "" || blob.ImageAdExtensionID != "" {
		t.Errorf("no image ids on an image-less dispatch, got %+v", blob)
	}
}

// Every refusal below happens BEFORE any upstream call: the claim is released
// (NoUpstreamCreate) and the fake API sees nothing.
func TestMicrosoftImage_PreCreateRefusals(t *testing.T) {
	foreign := ownedPNGAsset(t)
	foreign.briefID = msOtherBriefID // the asset belongs to ANOTHER brief

	oversized := ownedPNGAsset(t)
	oversized.sizeOverride = microsoft.MaxImageBytes + 1

	tooSmall := ownedPNGAsset(t)
	tooSmall.asset.Bytes = msTestPNG(t, 39, 20)

	undecodable := ownedPNGAsset(t)
	undecodable.asset.Bytes = []byte("definitely not a png")

	empty := ownedPNGAsset(t)
	empty.asset.Bytes = nil
	empty.sizeOverride = -1 // GetAssetSize must not look at Bytes for this case

	dbDown := ownedPNGAsset(t)
	dbDown.getErr = errors.New("db down")

	cases := []struct {
		name       string
		assets     creativeAssetReader
		cfg        string
		wantInErr  string
		wantNoLoad bool
	}{
		{"asset owned by another brief", foreign, `,"imageAssetId":"` + msAssetID + `"`, "does not exist for this brief", true},
		{"oversized asset refused on its size", oversized, `,"imageAssetId":"` + msAssetID + `"`, "at most 76800 bytes", true},
		{"below the minimum dimensions", tooSmall, `,"imageAssetId":"` + msAssetID + `"`, "at least 40x20", false},
		{"undecodable bytes", undecodable, `,"imageAssetId":"` + msAssetID + `"`, "could not be decoded", false},
		{"no stored bytes", empty, `,"imageAssetId":"` + msAssetID + `"`, "no stored image bytes", false},
		{"store error", dbDown, `,"imageAssetId":"` + msAssetID + `"`, "load creative asset", false},
		{"malformed asset id", ownedPNGAsset(t), `,"imageAssetId":"not-a-uuid"`, "not a valid asset id", true},
		{"alt text over 35", ownedPNGAsset(t), `,"imageAssetId":"` + msAssetID + `","imageAltText":"` + strings.Repeat("a", 36) + `"`, "the limit is 35", false},
		{"alt text without an image", ownedPNGAsset(t), `,"imageAltText":"orphan"`, "without imageAssetId", true},
		{"store not configured", nil, `,"imageAssetId":"` + msAssetID + `"`, "not configured", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &msImageAPI{}
			var d *MicrosoftDispatcher
			if tc.assets == nil {
				d = newMSImageDispatcher(t, api, nil)
			} else {
				d = newMSImageDispatcher(t, api, tc.assets)
			}
			reserver := NewAssetReserver(MaxConcurrentVariantAssetBytes, 0)
			d.SetAssetReserver(reserver)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msImageConfig(tc.cfg))
			if err == nil || camp != nil {
				t.Fatalf("want a pre-create refusal, got %v, %v", camp, err)
			}
			if !dispatchErrIsPreCreateForTest(err) {
				t.Errorf("a refusal before any create must release the claim (NoUpstreamCreate): %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantInErr)
			}
			if n := api.total(); n != 0 {
				t.Errorf("no upstream call may precede the image validation, got %d: %v", n, api.calls)
			}
			if sa, ok := tc.assets.(*scopedCreativeAssets); ok && tc.wantNoLoad && sa.loads() != 0 {
				t.Errorf("the asset bytes must not be loaded for this refusal (%d loads)", sa.loads())
			}
			if !reserver.sem.TryAcquire(reserver.budget) {
				t.Error("a failed resolve must release its reservation")
			}
		})
	}
}

// dispatchErrIsPreCreateForTest mirrors the orchestrator's claim-release test.
func dispatchErrIsPreCreateForTest(err error) bool {
	var nuc interface{ NoUpstreamCreate() bool }
	return errors.As(err, &nuc) && nuc.NoUpstreamCreate()
}

// The reservation is taken before the bytes are loaded: an asset the budget cannot admit is
// refused as retryable capacity and never read.
func TestMicrosoftImage_ReservationRefusalNeverLoads(t *testing.T) {
	api := &msImageAPI{}
	assets := ownedPNGAsset(t)
	d := newMSImageDispatcher(t, api, assets)
	d.SetAssetReserver(NewAssetReserver(int64(len(assets.asset.Bytes))-1, 0))
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msImageConfig(`,"imageAssetId":"`+msAssetID+`"`))
	if err == nil || !strings.Contains(err.Error(), "memory concurrently available") {
		t.Fatalf("want a capacity refusal, got %v", err)
	}
	if assets.loads() != 0 || api.total() != 0 {
		t.Errorf("nothing may be loaded or sent: loads %d, calls %d", assets.loads(), api.total())
	}
}

// A retry that reaches the image step again (the campaign reused by name) must not upload
// the image a second time: the association lookup finds the extension the first run attached.
func TestMicrosoftImage_RetryDoesNotUploadTwice(t *testing.T) {
	api := &msImageAPI{}
	d := newMSImageDispatcher(t, api, ownedPNGAsset(t))
	cfg := msImageConfig(`,"imageAssetId":"` + msAssetID + `"`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, cfg); err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, cfg)
	if err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}
	if n := api.count("Media"); n != 1 {
		t.Errorf("AddMedia sent %d times across two dispatches, want exactly 1", n)
	}
	if n := api.count("AdExtensions"); n != 1 {
		t.Errorf("AddAdExtensions sent %d times, want exactly 1", n)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("a found extension is success, status = %q", camp.Status)
	}
	blob := decodeMSResult(t, camp)
	if blob.ImageAdExtensionID != "7771" || blob.ImageMediaID != "" {
		t.Errorf("retry must record the found extension and no new media, got %+v", blob)
	}
}

// A failed attach on a CONFIRMED campaign degrades it — the campaign exists — and the step
// says whether the failure is definite or must be verified. Upstream text never reaches it.
func TestMicrosoftImage_UpstreamFailureDegrades(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		status   int
		body     string
		wantStep string
	}{
		{"media rejected", "Media", 400, `{"Code":3000,"Message":"` + msLeakText + `"}`, "Image extension NOT attached"},
		{"media ambiguous", "Media", 502, `{"Message":"` + msLeakText + `"}`, "Image extension UNCONFIRMED"},
		{"extension rejected", "AdExtensions", 200, `{"AdExtensionIdentities":[null],"NestedPartialErrors":[{"Code":3001,"ErrorCode":"InvalidImage","Message":"` + msLeakText + `"}]}`, "Image extension NOT attached"},
		{"association ambiguous", "AdExtensionsAssociations/Set", 500, `{}`, "Image extension UNCONFIRMED"},
		{"lookup failed", "AdExtensionsAssociations/Query", 503, `{"Message":"` + msLeakText + `"}`, "Image extension NOT attached"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &msImageAPI{override: map[string]struct {
				status int
				body   string
			}{tc.path: {tc.status, tc.body}}}
			d := newMSImageDispatcher(t, api, ownedPNGAsset(t))
			reserver := NewAssetReserver(MaxConcurrentVariantAssetBytes, 0)
			d.SetAssetReserver(reserver)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msImageConfig(`,"imageAssetId":"`+msAssetID+`"`))
			if err != nil {
				t.Fatalf("the campaign exists, so the job must not fail: %v", err)
			}
			if camp.PlatformCampaignID != "321" || camp.Status != campaignStatusCreatedDegraded {
				t.Errorf("campaign = %q status %q, want 321 created_degraded", camp.PlatformCampaignID, camp.Status)
			}
			blob := decodeMSResult(t, camp)
			joined := strings.Join(blob.Steps, "\n")
			if !strings.Contains(joined, tc.wantStep) {
				t.Errorf("steps must record %q, got:\n%s", tc.wantStep, joined)
			}
			if strings.Contains(string(camp.Result), msLeakText) {
				t.Error("upstream message text leaked into the persisted result")
			}
			if !reserver.sem.TryAcquire(reserver.budget) {
				t.Error("the reservation must be released after a degraded dispatch")
			}
		})
	}
}

// An UNCONFIRMED campaign create skips the image entirely: there is no confirmed campaign to
// attach to.
func TestMicrosoftImage_UnconfirmedCreateSkipsImage(t *testing.T) {
	api := &msImageAPI{override: map[string]struct {
		status int
		body   string
	}{"Campaigns": {502, `{}`}}}
	d := newMSImageDispatcher(t, api, ownedPNGAsset(t))
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msImageConfig(`,"imageAssetId":"`+msAssetID+`"`))
	if err == nil || camp == nil {
		t.Fatalf("want an UNCONFIRMED partial, got %v, %v", camp, err)
	}
	if n := api.count("Media") + api.count("AdExtensionsAssociations/Query"); n != 0 {
		t.Errorf("no image step may run after an unconfirmed create, got %d", n)
	}
}
