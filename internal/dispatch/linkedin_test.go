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
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

const goodLinkedInCreds = `{"AccessToken":"tok"}`

func activeLinkedInConn(creds string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderLinkedInAds,
		AccountID:            "123456789",
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"org_id": "987654321"},
		Status:               model.StatusActive,
	}
}

// ---- pre-create paths -----------------------------------------------------

func TestLinkedIn_PreCreateErrorsReleaseClaim(t *testing.T) {
	cases := []struct {
		name string
		repo connReader
		enc  domain.Encryptor
	}{
		{"missing connection", fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}},
		{"no stored credentials", fakeConnReader{conn: &model.Connection{Provider: model.ProviderLinkedInAds, Status: model.StatusActive}}, identityEncryptor{}},
		{"decrypt fails", fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, errEncryptor{}},
		{"empty access token", fakeConnReader{conn: activeLinkedInConn(`{"AccessToken":""}`)}, identityEncryptor{}},
		{"inactive connection", fakeConnReader{conn: &model.Connection{Provider: model.ProviderLinkedInAds, AccountID: "1", EncryptedCredentials: []byte(goodLinkedInCreds), ProviderConfig: map[string]string{"org_id": "o"}, Status: model.StatusInactive}}, identityEncryptor{}},
		{"missing org id", fakeConnReader{conn: &model.Connection{Provider: model.ProviderLinkedInAds, AccountID: "1", EncryptedCredentials: []byte(goodLinkedInCreds), Status: model.StatusActive}}, identityEncryptor{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewLinkedInDispatcher(tc.repo, tc.enc)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, nil)
			var nuc interface{ NoUpstreamCreate() bool }
			if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("a pre-create failure must be NoUpstreamCreate, got %T: %v", err, err)
			}
		})
	}
}

func TestLinkedIn_BadConfigIsPreCreate(t *testing.T) {
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{})
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, json.RawMessage(`{bad`))
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a malformed config must be a pre-create error, got %T: %v", err, err)
	}
}

// ---- happy path through an httptest linkedin API --------------------------

func TestLinkedIn_DispatchSuccessMapsResult(t *testing.T) {
	// Minimal LinkedIn REST API: search GETs return empty (force create), then
	// group/campaign/post/creative creates return ids. Mirrors the client's own
	// happy-path harness, just enough to yield a CampaignID for the adapter to map.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock),
	)
	cfg := json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "200" {
		t.Fatalf("adapter must map the upstream campaign id (numeric), got %+v", camp)
	}
	if camp.CampaignName == "" || len(camp.Result) == 0 {
		t.Error("campaign name + result blob should be populated")
	}
}

func TestLinkedIn_ForeignAccountIDRejected(t *testing.T) {
	// A caller adAccountId that doesn't match the connection's account must be
	// rejected up front (pre-create) — appending it to the allowlist would defeat the
	// client's cross-tenant fail-closed check.
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{})
	cfg := json.RawMessage(`{"linkedInConfig":{"adAccountId":"999999999","targetingProfile":"x","targetingProfiles":[{"id":"x","label":"X"}]}}`)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a foreign adAccountId must be rejected pre-create, got %T: %v", err, err)
	}
}

func TestLinkedIn_AmbiguousCreateRetainsClaim(t *testing.T) {
	// A 5xx on the campaign-group create is ambiguous → the linkedin client returns a
	// non-nil partial (empty CampaignID). The adapter must retain the claim.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway) // ambiguous 5xx on a create POST
	}))
	defer srv.Close()
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock),
	)
	cfg := json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err == nil {
		t.Fatal("expected an error from an ambiguous create")
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
		t.Error("an ambiguous create must NOT be NoUpstreamCreate — the claim must be retained")
	}
	if camp == nil {
		t.Error("an ambiguous create must return a non-nil campaign for orphan recording")
	}
}

func TestLinkedIn_GroupCreatedButCampaignFails_RecordsGroupOrphan(t *testing.T) {
	// The campaign GROUP is created, then the campaign create 5xx's. The client
	// returns a non-nil result with CampaignGroupID set + empty CampaignID. The
	// adapter must retain the claim AND capture the group orphan (as group:<id>) so
	// it's reconcilable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"elements":[]}`)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:500"}`) // group created
		case strings.Contains(r.URL.Path, "adCampaigns"):
			w.WriteHeader(http.StatusBadGateway) // campaign create fails (group already exists)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock),
	)
	cfg := json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err == nil {
		t.Fatal("expected an error")
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
		t.Error("a group-created failure must retain the claim, not release it")
	}
	if camp == nil || camp.PlatformCampaignID != "group:500" {
		t.Errorf("the group orphan must be captured as group:<id>, got %+v", camp)
	}
}
