// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestXGetCampaign_ReadsTheAccountScopedCampaign(t *testing.T) {
	s, c := newBudgetServer(t, http.StatusOK,
		`{"data":{"id":"cmp1","name":"KubeCon EU — Awareness","entity_status":"PAUSED","deleted":false,"currency":"USD"},"request":{"params":{"account_id":"acc1"}}}`)
	ref, err := c.GetCampaign(context.Background(), "cmp1")
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	if ref == nil || ref.ID != "cmp1" || ref.Name != "KubeCon EU — Awareness" || ref.Status != StatusPaused || ref.AccountID != "" {
		t.Fatalf("ref = %+v", ref)
	}
	got := s.recorded()
	if len(got) != 1 || got[0].Method != http.MethodGet || got[0].Path != "/12/accounts/acc1/campaigns/cmp1" {
		t.Errorf("requests = %+v, want exactly one GET of the account-scoped campaign", got)
	}
}

func TestXGetCampaign_Outcomes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		absent  bool
		wantErr bool
		account string
	}{
		{name: "active", status: 200, body: `{"data":{"id":"cmp1","name":"n","entity_status":"ACTIVE"}}`},
		{name: "draft exists", status: 200, body: `{"data":{"id":"cmp1","name":"n","entity_status":"DRAFT"}}`},
		{name: "a reported account is passed on", status: 200, body: `{"data":{"id":"cmp1","name":"n","entity_status":"ACTIVE","account_id":"acc1"}}`, account: "acc1"},
		{name: "another account is reported, not hidden", status: 200, body: `{"data":{"id":"cmp1","name":"n","entity_status":"ACTIVE","account_id":"acc9"}}`, account: "acc9"},
		{name: "404 is absent", status: 404, body: `{"errors":[{"code":"NOT_FOUND"}]}`, absent: true},
		{name: "deleted is absent", status: 200, body: `{"data":{"id":"cmp1","name":"n","entity_status":"PAUSED","deleted":true}}`, absent: true},
		{name: "unknown status is unverifiable", status: 200, body: `{"data":{"id":"cmp1","name":"n","entity_status":"EXPIRED"}}`, wantErr: true},
		{name: "missing status is unverifiable", status: 200, body: `{"data":{"id":"cmp1","name":"n"}}`, wantErr: true},
		{name: "missing name is unverifiable", status: 200, body: `{"data":{"id":"cmp1","entity_status":"ACTIVE"}}`, wantErr: true},
		{name: "another campaign is unverifiable", status: 200, body: `{"data":{"id":"cmp2","name":"n","entity_status":"ACTIVE"}}`, wantErr: true},
		{name: "a duplicated id is unverifiable", status: 200, body: `{"data":{"id":"cmp2","id":"cmp1","name":"n","entity_status":"ACTIVE"}}`, wantErr: true},
		{name: "a substituted name is unverifiable", status: 200, body: `{"data":{"id":"cmp1","name":"bad\uDC00","entity_status":"ACTIVE"}}`, wantErr: true},
		{name: "no data is unverifiable", status: 200, body: `{"data":null}`, wantErr: true},
		{name: "a malformed body is unverifiable", status: 200, body: `{"data":{`, wantErr: true},
		{name: "401 is unverifiable, not absent", status: 401, body: `{}`, wantErr: true},
		{name: "5xx is unverifiable", status: 503, body: `{}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, c := newBudgetServer(t, tc.status, tc.body)
			ref, err := c.GetCampaign(context.Background(), "cmp1")
			switch {
			case tc.wantErr:
				if err == nil || ref != nil {
					t.Fatalf("want an error and no ref, got %+v, %v", ref, err)
				}
			case tc.absent:
				if err != nil || ref != nil {
					t.Fatalf("want (nil, nil), got %+v, %v", ref, err)
				}
			default:
				if err != nil || ref == nil || ref.AccountID != tc.account {
					t.Fatalf("want a ref reporting account %q, got %+v, %v", tc.account, ref, err)
				}
			}
		})
	}
}

// A throttle is an unverified answer, never an absence. X declares its reset, and a reset past
// the client's wait cap ends the read at once rather than burning the retries while still
// limited — so this pins the outcome without a wall-clock wait.
func TestXGetCampaign_ThrottleIsAnErrorNotAbsence(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c := newToggleTestClient(t, srv.URL)
	ref, err := c.GetCampaign(context.Background(), "cmp1")
	if err == nil || ref != nil {
		t.Fatalf("want an error, got %+v, %v", ref, err)
	}
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusTooManyRequests {
		t.Errorf("err = %v, want the 429 preserved", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the read was sent %d time(s), want 1: a reset past the wait cap is not slept on", n)
	}
}

func TestXGetCampaign_MalformedIDRefusedBeforeAnyRequest(t *testing.T) {
	s, c := newBudgetServer(t, http.StatusOK, `{"data":{}}`)
	for _, id := range []string{"", " cmp1", "cmp1 ", "cmp/1", "cmp_1", "cmp?1", strings.Repeat("a", maxCampaignIDLen+1)} {
		ref, err := c.GetCampaign(context.Background(), id)
		if !errors.Is(err, ErrInvalidCampaignID) || ref != nil {
			t.Errorf("GetCampaign(%q) = %+v, %v; want ErrInvalidCampaignID", id, ref, err)
		}
	}
	if n := len(s.recorded()); n != 0 {
		t.Errorf("a malformed id reached X %d time(s)", n)
	}
}
