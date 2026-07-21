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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

const goodMetaCreds = `{"accessToken":"tok"}`

func activeMetaConn(creds string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderMetaAds,
		AccountID:            "act_777",
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"page_id": "987654321"},
		Status:               model.StatusActive,
	}
}

// ---- pre-create paths -----------------------------------------------------

func TestMeta_PreCreateErrorsReleaseClaim(t *testing.T) {
	cases := []struct {
		name string
		repo connReader
		enc  domain.Encryptor
	}{
		{"missing connection", fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}},
		{"no stored credentials", fakeConnReader{conn: &model.Connection{Provider: model.ProviderMetaAds, Status: model.StatusActive}}, identityEncryptor{}},
		{"decrypt fails", fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, errEncryptor{}},
		{"empty access token", fakeConnReader{conn: activeMetaConn(`{"accessToken":""}`)}, identityEncryptor{}},
		{"inactive connection", fakeConnReader{conn: &model.Connection{Provider: model.ProviderMetaAds, AccountID: "act_1", EncryptedCredentials: []byte(goodMetaCreds), ProviderConfig: map[string]string{"page_id": "p"}, Status: model.StatusInactive}}, identityEncryptor{}},
		{"missing page id", fakeConnReader{conn: &model.Connection{Provider: model.ProviderMetaAds, AccountID: "act_1", EncryptedCredentials: []byte(goodMetaCreds), Status: model.StatusActive}}, identityEncryptor{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewMetaDispatcher(tc.repo, tc.enc)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, nil)
			var nuc interface{ NoUpstreamCreate() bool }
			if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("a pre-create failure must be NoUpstreamCreate, got %T: %v", err, err)
			}
		})
	}
}

func TestMeta_BadConfigIsPreCreate(t *testing.T) {
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{})
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, json.RawMessage(`{bad`))
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a malformed config must be a pre-create error, got %T: %v", err, err)
	}
}

// ---- happy path through an httptest meta API ------------------------------

func TestMeta_DispatchSuccessMapsResult(t *testing.T) {
	var creativeCount, adCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"camp_123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"adset_456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			n := atomic.AddInt32(&creativeCount, 1)
			_, _ = io.WriteString(w, `{"id":"creative_`+strconv.Itoa(int(n))+`"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			n := atomic.AddInt32(&adCount, 1)
			_, _ = io.WriteString(w, `{"id":"ad_`+strconv.Itoa(int(n))+`"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(clock),
	)
	// CurrencyOffset set → the preflight skips currency-code derivation.
	cfg := json.RawMessage(`{
		"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic",
		"geoTargets":["US"],"currencyOffset":100,
		"variants":[{"headline":"KubeCon 2099","primaryText":"Join us — it's great","description":"Cloud native event"}]
	}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "camp_123" {
		t.Fatalf("adapter must map the upstream campaign id, got %+v", camp)
	}
	if camp.CampaignName == "" || len(camp.Result) == 0 {
		t.Error("campaign name + result blob should be populated")
	}
}
