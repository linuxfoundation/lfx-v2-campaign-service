// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
		// Every request here gets the same answer, so the confirming account read 404s too: the
		// absence is unproven. The proven case is TestXGetCampaign_404IsAbsentOnlyOnceTheAccountIsConfirmed.
		{name: "404 with an unconfirmed account is unverifiable", status: 404, body: `{"errors":[{"code":"NOT_FOUND"}]}`, wantErr: true},
		{name: "deleted is absent", status: 200, body: `{"data":{"id":"cmp1","name":"n","entity_status":"PAUSED","deleted":true}}`, absent: true},
		{name: "unknown status is unverifiable", status: 200, body: `{"data":{"id":"cmp1","name":"n","entity_status":"EXPIRED"}}`, wantErr: true},
		{name: "missing status is unverifiable", status: 200, body: `{"data":{"id":"cmp1","name":"n"}}`, wantErr: true},
		{name: "missing name is unverifiable", status: 200, body: `{"data":{"id":"cmp1","entity_status":"ACTIVE"}}`, wantErr: true},
		{name: "another campaign is unverifiable", status: 200, body: `{"data":{"id":"cmp2","name":"n","entity_status":"ACTIVE"}}`, wantErr: true},
		{name: "a duplicated id is unverifiable", status: 200, body: `{"data":{"id":"cmp2","id":"cmp1","name":"n","entity_status":"ACTIVE"}}`, wantErr: true},
		{name: "a substituted name is unverifiable", status: 200, body: `{"data":{"id":"cmp1","name":"bad\uDC00","entity_status":"ACTIVE"}}`, wantErr: true},
		// The ENVELOPE is guarded, not only the decoded data: a duplicated "data" is resolved
		// last-wins before GetCampaign ever sees Data.
		{name: "a duplicated data key is unverifiable", status: 200, body: `{"data":{"id":"cmp2","name":"x","entity_status":"ACTIVE"},"data":{"id":"cmp1","name":"n","entity_status":"ACTIVE"}}`, wantErr: true},
		{name: "data beside Data is unverifiable", status: 200, body: `{"Data":{"id":"cmp2","name":"x","entity_status":"ACTIVE"},"data":{"id":"cmp1","name":"n","entity_status":"ACTIVE"}}`, wantErr: true},
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

// xAccountStub answers the campaign read and the account-root read separately, recording the
// order of every path. Guarded: the handler runs on the server goroutine.
func xAccountStub(t *testing.T, campStatus int, campBody string, acctStatus int, acctBody string) (*Client, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		status, body := acctStatus, acctBody
		if strings.Contains(r.URL.Path, "/campaigns/") {
			status, body = campStatus, campBody
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "3600") // past the wait cap: ends the read at once
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return newToggleTestClient(t, srv.URL), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// A campaign 404 is a PROVEN absence only once the connection's own ad account is confirmed
// readable: X 404s a path whose account is inaccessible the same way, and a 404 from that would
// tell an operator a possibly-live campaign is missing.
func TestXGetCampaign_404IsAbsentOnlyOnceTheAccountIsConfirmed(t *testing.T) {
	const acctOK = `{"data":{"id":"acc1","name":"Test"}}`
	for _, tc := range []struct {
		name       string
		acctStatus int
		acctBody   string
		absent     bool
	}{
		{"account readable: absent", http.StatusOK, acctOK, true},
		{"account 404: unverifiable", http.StatusNotFound, `{}`, false},
		{"account 403: unverifiable", http.StatusForbidden, `{}`, false},
		{"account 500: unverifiable", http.StatusInternalServerError, `{}`, false},
		{"account 429: unverifiable", http.StatusTooManyRequests, `{}`, false},
		{"another account: unverifiable", http.StatusOK, `{"data":{"id":"acc9"}}`, false},
		{"no account: unverifiable", http.StatusOK, `{"data":null}`, false},
		{"duplicated data: unverifiable", http.StatusOK, `{"data":{"id":"acc9"},"data":{"id":"acc1"}}`, false},
		{"malformed account: unverifiable", http.StatusOK, `{"data":{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, seen := xAccountStub(t, http.StatusNotFound, `{}`, tc.acctStatus, tc.acctBody)
			ref, err := c.GetCampaign(context.Background(), "cmp1")
			if tc.absent {
				if err != nil || ref != nil {
					t.Fatalf("want (nil, nil), got %+v, %v", ref, err)
				}
			} else if err == nil || ref != nil {
				t.Fatalf("want an unverifiable error, got %+v, %v", ref, err)
			}
			got := seen()
			if len(got) < 2 || got[0] != "GET /12/accounts/acc1/campaigns/cmp1" || got[1] != "GET /12/accounts/acc1" {
				t.Errorf("requests = %q, want the campaign read and then the account read", got)
			}
		})
	}
}

// The confirming read happens ONLY after a campaign 404.
func TestXGetCampaign_NoAccountReadUnlessTheCampaign404s(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusOK, `{"data":{"id":"cmp1","name":"n","entity_status":"ACTIVE"}}`},
		{http.StatusOK, `{"data":{"id":"cmp1","name":"n","entity_status":"PAUSED","deleted":true}}`},
		{http.StatusForbidden, `{}`},
		{http.StatusInternalServerError, `{}`},
	} {
		c, seen := xAccountStub(t, tc.status, tc.body, http.StatusOK, `{"data":{"id":"acc1"}}`)
		_, _ = c.GetCampaign(context.Background(), "cmp1")
		for _, r := range seen() {
			if r == "GET /12/accounts/acc1" {
				t.Errorf("campaign answered %d: the account was read, but only a campaign 404 needs proof", tc.status)
			}
		}
	}
}
