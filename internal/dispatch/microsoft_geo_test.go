// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// The dispatch-layer plumbing assertion for geo targeting (LFXV2-3279), mirroring
// googleads_geo_test.go. microsoftServers' handler rejects any path it does not recognise, so
// the geo routes need their own fixture — and having one is the point: without a test at THIS
// layer, a `geoTargets` field that never reached microsoft.CampaignInput (a dropped or swapped
// assignment in Dispatch) would stay green, because every platform-client test constructs
// CampaignInput directly and so cannot see the dispatcher's mapping.

// msGeoCapture records what the fake Microsoft API was asked for.
type msGeoCapture struct {
	mu sync.Mutex
	// criterionBodies holds each raw POST /CampaignCriterions body.
	criterionBodies []string
	// sawGeoFileQuery records whether the locations file was looked up at all.
	sawGeoFileQuery bool
}

// msGeoServers wires the OAuth endpoint, an API server covering the full create hierarchy PLUS
// the geo routes, and a separate host serving the locations CSV — mirroring production, where
// the FileUrl points at storage rather than at the API.
//
// The CSV uses Microsoft's published version 2.0 header verbatim rather than one derived from
// the parser's own column constants: a fixture built from the code's idea of the format cannot
// falsify that idea.
func msGeoServers(t *testing.T) ([]microsoft.Option, *msGeoCapture) {
	t.Helper()
	cap := &msGeoCapture{}

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w,
			"Location Id,Bing Display Name,Location Type,Replaces,Status,AdWords Location Id\n"+
				"200,United Kingdom,Country,,Active,2826\n"+
				"172,India,Country,,Active,2356\n"+
				"190,United States,Country,,Active,2840\n")
	}))
	t.Cleanup(fileSrv.Close)

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/GeoLocationsFileUrl/Query"):
			cap.mu.Lock()
			cap.sawGeoFileQuery = true
			cap.mu.Unlock()
			_, _ = io.WriteString(w, `{"FileUrl":"`+fileSrv.URL+`/geolocations.csv",`+
				`"FileUrlExpiryTimeUtc":"2026-08-19T12:15:00Z","LastModifiedTimeUtc":"2026-06-05T18:43:00Z"}`)
		case strings.HasSuffix(p, "/CampaignCriterions"):
			body, _ := io.ReadAll(r.Body)
			cap.mu.Lock()
			cap.criterionBodies = append(cap.criterionBodies, string(body))
			cap.mu.Unlock()
			_, _ = io.WriteString(w, `{"CampaignCriterionIds":[9001,9002],"NestedPartialErrors":[]}`)
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case strings.HasSuffix(p, "/Ads/QueryByAdGroupId"):
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case strings.HasSuffix(p, "/Campaigns"):
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/AdGroups"):
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Ads"):
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Keywords"):
			_, _ = io.WriteString(w, `{"KeywordIds":[701],"PartialErrors":[]}`)
		default:
			// t.Error, never t.Fatal: FailNow is only valid on the test goroutine, and in a
			// handler it would kill just this goroutine and leave the exchange half-done.
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)

	return []microsoft.Option{
		microsoft.WithTokenURL(tokenSrv.URL),
		microsoft.WithBaseURL(apiSrv.URL),
	}, cap
}

// geoCriterionBody is the decoded POST /CampaignCriterions body, spelled independently of the
// client's own unexported request type so a rename there cannot quietly make this test assert
// a different shape.
type geoCriterionBody struct {
	CampaignCriterions []struct {
		CampaignID json.Number `json:"CampaignId"`
		Criterion  struct {
			Type       string      `json:"Type"`
			LocationID json.Number `json:"LocationId"`
		} `json:"Criterion"`
		Type string `json:"Type"`
	} `json:"CampaignCriterions"`
	CriterionType string `json:"CriterionType"`
}

// `geoTargets` in the dispatch config must reach the outbound CampaignCriterions request as
// resolved Microsoft LocationIds. Distinctive per-country ids (GB=200, IN=172) mean a mapping
// that collapsed both to one country, or swapped their order, fails here rather than passing
// silently.
func TestMicrosoft_ConfigGeoTargetsReachCampaignCriterions(t *testing.T) {
	opts, cap := msGeoServers(t)
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"microsoftConfig":{"budget":50,"keywords":[{"text":"kubernetes","matchType":"Exact"}],"geoTargets":["GB","IN"]}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if !cap.sawGeoFileQuery {
		t.Error("the locations file was never requested — geo codes were not resolved against Microsoft's file")
	}
	if len(cap.criterionBodies) == 0 {
		t.Fatal("no /CampaignCriterions request was sent — cfg.GeoTargets never reached the client")
	}

	var body geoCriterionBody
	if err := json.Unmarshal([]byte(cap.criterionBodies[0]), &body); err != nil {
		t.Fatalf("decode criterion body: %v (%s)", err, cap.criterionBodies[0])
	}
	// CriterionType MUST be "Targets" for location criteria; "Location" is a READ-path value
	// and is rejected on Add.
	if body.CriterionType != "Targets" {
		t.Errorf("CriterionType = %q, want %q", body.CriterionType, "Targets")
	}
	if len(body.CampaignCriterions) != 2 {
		t.Fatalf("got %d location criteria, want 2: %s", len(body.CampaignCriterions), cap.criterionBodies[0])
	}
	want := []string{"200", "172"} // GB, IN — caller order
	for i, cc := range body.CampaignCriterions {
		if cc.Criterion.LocationID.String() != want[i] {
			t.Errorf("criterion %d: LocationId = %q, want %q", i, cc.Criterion.LocationID.String(), want[i])
		}
		if cc.Criterion.Type != "LocationCriterion" {
			t.Errorf("criterion %d: Criterion.Type = %q, want LocationCriterion", i, cc.Criterion.Type)
		}
		if cc.Type != "BiddableCampaignCriterion" {
			t.Errorf("criterion %d: Type = %q, want BiddableCampaignCriterion", i, cc.Type)
		}
		// Campaign LEVEL: each criterion carries the created campaign's id.
		if cc.CampaignID.String() != "321" {
			t.Errorf("criterion %d: CampaignId = %q, want the created campaign 321", i, cc.CampaignID.String())
		}
	}
}

// A config with no geoTargets must send no criteria request and must not even fetch the
// locations file: the pre-LFXV2-3279 behaviour is preserved for every caller predating the
// field.
func TestMicrosoft_NoGeoTargetsSendsNoCriterionRequest(t *testing.T) {
	opts, cap := msGeoServers(t)
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"microsoftConfig":{"budget":50,"keywords":[{"text":"kubernetes","matchType":"Exact"}]}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.criterionBodies) != 0 {
		t.Errorf("no geo targets were supplied, so no /CampaignCriterions request may be sent: %v", cap.criterionBodies)
	}
	if cap.sawGeoFileQuery {
		t.Error("no geo targets were supplied, so the locations file must not be fetched")
	}
}

// An unresolvable geo code must fail the dispatch BEFORE the campaign is created. Asserting
// only the error would pass against an implementation that created the campaign first and
// validated afterwards — the orphaned-paid-campaign defect this ordering exists to prevent —
// so this asserts that NO mutating call was issued.
func TestMicrosoft_UnresolvableGeoTargetCreatesNothing(t *testing.T) {
	var mu sync.Mutex
	var mutating []string

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	// The file carries only GB, so "US" is a supported ISO code that cannot resolve.
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w,
			"Location Id,Bing Display Name,Location Type,Replaces,Status,AdWords Location Id\n"+
				"200,United Kingdom,Country,,Active,2826\n")
	}))
	t.Cleanup(fileSrv.Close)

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/GeoLocationsFileUrl/Query"):
			_, _ = io.WriteString(w, `{"FileUrl":"`+fileSrv.URL+`/geolocations.csv",`+
				`"FileUrlExpiryTimeUtc":"2026-08-19T12:15:00Z","LastModifiedTimeUtc":"2026-06-05T18:43:00Z"}`)
		// The lookups must return their SHAPED "absent" responses, not a bare `{}`. A bare
		// object makes findCampaignByName fail for its own reasons and the create is then
		// never reached — which would let a reordering mutant escape this test through a
		// lookup failure rather than through the guard it is meant to pin.
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case strings.HasSuffix(p, "/Ads/QueryByAdGroupId"):
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		default:
			// Anything else is a CREATE. Record it: reaching here at all is the defect.
			mu.Lock()
			mutating = append(mutating, p)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		}
	}))
	t.Cleanup(apiSrv.Close)

	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL))
	cfg := json.RawMessage(`{"microsoftConfig":{"budget":50,"keywords":[{"text":"kubernetes","matchType":"Exact"}],"geoTargets":["GB","US"]}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, cfg); err == nil {
		t.Fatal("an unresolvable geo target must fail the dispatch")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(mutating) != 0 {
		t.Fatalf("NO mutating call may be issued when a geo target cannot be resolved; got %v", mutating)
	}
}
