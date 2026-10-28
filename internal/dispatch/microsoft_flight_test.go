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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// msFlightCapture records the raw AddAdGroups body and whether any API request was made.
type msFlightCapture struct {
	mu          sync.Mutex
	adGroupBody string
	sawAPI      bool
}

func (c *msFlightCapture) snapshot() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.adGroupBody, c.sawAPI
}

// microsoftFlightServers is microsoftServers with the AddAdGroups body captured and the client
// clock pinned to 2026-01-01 UTC, so "today" is deterministic for the past-date rule.
func microsoftFlightServers(t *testing.T) ([]microsoft.Option, *msFlightCapture) {
	t.Helper()
	cap := &msFlightCapture{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.mu.Lock()
		cap.sawAPI = true
		cap.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case strings.HasSuffix(p, "/Ads/QueryByAdGroupId"):
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case strings.HasSuffix(p, "/Campaigns"):
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/AdGroups"):
			b, _ := io.ReadAll(r.Body)
			cap.mu.Lock()
			cap.adGroupBody = string(b)
			cap.mu.Unlock()
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Ads"):
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	return []microsoft.Option{
		microsoft.WithTokenURL(tokenSrv.URL),
		microsoft.WithBaseURL(apiSrv.URL),
		microsoft.WithClock(func() time.Time { return now }),
	}, cap
}

// TestMicrosoft_FlightDatesReachAdGroupAndRow: microsoftConfig.startDate/endDate (the metaConfig
// keys and format) reach the AddAdGroups body as Date objects, and are recorded on the row and in
// the config snapshot the way Meta's are.
func TestMicrosoft_FlightDatesReachAdGroupAndRow(t *testing.T) {
	opts, cap := microsoftFlightServers(t)
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"microsoftConfig":{"budget":50,"startDate":"2026-02-01","endDate":"2026-02-28"}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	body, _ := cap.snapshot()
	for _, want := range []string{`"StartDate":{"Month":2,"Day":1,"Year":2026}`, `"EndDate":{"Month":2,"Day":28,"Year":2026}`} {
		if !strings.Contains(body, want) {
			t.Errorf("AddAdGroups body lacks %s: %s", want, body)
		}
	}
	if camp.StartDate == nil || camp.StartDate.Format("2006-01-02") != "2026-02-01" {
		t.Errorf("row StartDate = %v, want 2026-02-01", camp.StartDate)
	}
	if camp.EndDate == nil || camp.EndDate.Format("2006-01-02") != "2026-02-28" {
		t.Errorf("row EndDate = %v, want 2026-02-28", camp.EndDate)
	}
	var snap map[string]any
	if err := json.Unmarshal(camp.ConfigSnapshot, &snap); err != nil {
		t.Fatalf("config snapshot: %v", err)
	}
	if snap["startDate"] != "2026-02-01" || snap["endDate"] != "2026-02-28" {
		t.Errorf("snapshot dates = %v / %v, want 2026-02-01 / 2026-02-28", snap["startDate"], snap["endDate"])
	}
}

// TestMicrosoft_NoFlightDatesKeepsTodaysBehaviour: a config without dates sends no ad-group date
// and records none on the row — the create as it was before the fields existed.
func TestMicrosoft_NoFlightDatesKeepsTodaysBehaviour(t *testing.T) {
	opts, cap := microsoftFlightServers(t)
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, json.RawMessage(`{"microsoftConfig":{"budget":50}}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	body, _ := cap.snapshot()
	if body == "" {
		t.Fatal("ad group create not captured")
	}
	if strings.Contains(body, "StartDate") || strings.Contains(body, "EndDate") {
		t.Errorf("AddAdGroups body carries a flight date nobody asked for: %s", body)
	}
	if camp.StartDate != nil || camp.EndDate != nil {
		t.Errorf("row dates = %v / %v, want none", camp.StartDate, camp.EndDate)
	}
}

// TestMicrosoft_InvalidFlightDatesFailBeforeAnyUpstreamCall: an invalid date is a pre-create
// failure exactly like Meta's — the claim is released (NoUpstreamCreate), the refusal is
// operator-readable, and Microsoft is never called.
func TestMicrosoft_InvalidFlightDatesFailBeforeAnyUpstreamCall(t *testing.T) {
	cases := map[string]struct{ cfg, wantErr string }{
		"start in the past": {`{"budget":50,"startDate":"2025-12-31"}`, "is in the past"},
		"end before start":  {`{"budget":50,"startDate":"2026-03-01","endDate":"2026-02-01"}`, "must be after start date"},
		"bad format":        {`{"budget":50,"startDate":"03/01/2026"}`, "expected YYYY-MM-DD"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts, cap := microsoftFlightServers(t)
			d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, json.RawMessage(`{"microsoftConfig":`+tc.cfg+`}`))
			if err == nil {
				t.Fatal("expected a flight-date refusal")
			}
			if camp != nil {
				t.Errorf("an invalid date must return no campaign (claim released), got %+v", camp)
			}
			var nuc interface{ NoUpstreamCreate() bool }
			if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("an invalid date must be NoUpstreamCreate, got %T: %v", err, err)
			}
			msg, ok := domain.AsPreflightRefusal(err)
			if !ok || !strings.Contains(msg, tc.wantErr) {
				t.Errorf("refusal = %q (preflight %v), want it to contain %q", msg, ok, tc.wantErr)
			}
			if _, sawAPI := cap.snapshot(); sawAPI {
				t.Error("Microsoft was called although the flight dates were invalid")
			}
		})
	}
}
