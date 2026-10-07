// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// lookupTestClient serves every Graph request with status/body and records the request URI. The
// recorded slice is guarded: the handler runs on the server's goroutine.
func lookupTestClient(t *testing.T, status int, body string) (*Client, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(
		Credentials{AccessToken: "tok"},
		AccountConfig{AccountID: "act_777"},
		WithBaseURL(srv.URL),
		WithClock(func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) }),
		withRetryBaseDelay(time.Millisecond),
	)
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

const metaLookupBody = `{"id":"120200000000001","name":"KubeCon EU — Leads","status":"PAUSED","effective_status":"PAUSED","account_id":"777","objective":"OUTCOME_LEADS","daily_budget":"5000","bid_strategy":"LOWEST_COST_WITHOUT_CAP"}`

func TestMetaGetCampaign_ReadsTheNodeWithTheAdoptionFields(t *testing.T) {
	c, seen := lookupTestClient(t, http.StatusOK, metaLookupBody)
	ref, err := c.GetCampaign(context.Background(), "120200000000001")
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	if ref == nil || ref.ID != "120200000000001" || ref.Name != "KubeCon EU — Leads" || ref.Status != StatusPaused ||
		ref.AccountID != "act_777" || ref.Objective != "OUTCOME_LEADS" || ref.EffectiveStatus != "PAUSED" {
		t.Fatalf("ref = %+v", ref)
	}
	got := seen()
	want := "GET /120200000000001?fields=id,name,status,effective_status,account_id,objective,daily_budget,lifetime_budget,bid_strategy"
	if len(got) != 1 || got[0] != want {
		t.Errorf("requests = %q, want exactly [%q]", got, want)
	}
}

func TestMetaGetCampaign_Outcomes(t *testing.T) {
	const notFound = `{"error":{"message":"Unsupported get request. Object with ID '120200000000001' does not exist","type":"GraphMethodException","code":100,"error_subcode":33,"fbtrace_id":"x"}}`
	cases := []struct {
		name    string
		status  int
		body    string
		absent  bool
		wantErr bool
		account string
	}{
		{name: "active", status: 200, body: `{"id":"120200000000001","name":"n","status":"ACTIVE","account_id":"act_777"}`, account: "act_777"},
		{name: "graph 100/33 is absent", status: 400, body: notFound, absent: true},
		{name: "deleted is absent", status: 200, body: `{"id":"120200000000001","name":"n","status":"DELETED","account_id":"777"}`, absent: true},
		{name: "archived is absent", status: 200, body: `{"id":"120200000000001","name":"n","status":"ARCHIVED","account_id":"777"}`, absent: true},
		// The node is in ANOTHER account: the client reports that account faithfully, and the
		// dispatcher is what refuses it.
		{name: "another account is reported, not hidden", status: 200, body: `{"id":"120200000000001","name":"n","status":"ACTIVE","account_id":"888"}`, account: "act_888"},
		{name: "code 100 without subcode 33 is unverifiable", status: 400, body: `{"error":{"message":"Tried accessing nonexisting field (objective)","type":"OAuthException","code":100}}`, wantErr: true},
		{name: "an auth failure is unverifiable", status: 400, body: `{"error":{"message":"Error validating access token","type":"OAuthException","code":190}}`, wantErr: true},
		{name: "unknown status is unverifiable", status: 200, body: `{"id":"120200000000001","name":"n","status":"IN_PROCESS","account_id":"777"}`, wantErr: true},
		{name: "missing status is unverifiable", status: 200, body: `{"id":"120200000000001","name":"n","account_id":"777"}`, wantErr: true},
		{name: "missing account is unverifiable", status: 200, body: `{"id":"120200000000001","name":"n","status":"ACTIVE"}`, wantErr: true},
		{name: "malformed account is unverifiable", status: 200, body: `{"id":"120200000000001","name":"n","status":"ACTIVE","account_id":"act_abc"}`, wantErr: true},
		{name: "missing name is unverifiable", status: 200, body: `{"id":"120200000000001","status":"ACTIVE","account_id":"777"}`, wantErr: true},
		{name: "another node is unverifiable", status: 200, body: `{"id":"120200000000002","name":"n","status":"ACTIVE","account_id":"777"}`, wantErr: true},
		{name: "a duplicated id is unverifiable", status: 200, body: `{"id":"120200000000002","id":"120200000000001","name":"n","status":"ACTIVE","account_id":"777"}`, wantErr: true},
		{name: "a malformed body is unverifiable", status: 200, body: `{"id":`, wantErr: true},
		{name: "a non-object body is unverifiable", status: 200, body: `[1,2]`, wantErr: true},
		{name: "5xx is unverifiable", status: 500, body: `{}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := lookupTestClient(t, tc.status, tc.body)
			ref, err := c.GetCampaign(context.Background(), "120200000000001")
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
					t.Fatalf("want a ref under %s, got %+v, %v", tc.account, ref, err)
				}
			}
		})
	}
}

// A throttle that never clears — Meta's 429 or its far commoner HTTP-400 rate-limit code — is an
// unverified answer, never an absence.
func TestMetaGetCampaign_ExhaustedThrottleIsAnErrorNotAbsence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"429", http.StatusTooManyRequests, `{}`},
		{"rate-limit code", http.StatusBadRequest, `{"error":{"message":"User request limit reached","type":"OAuthException","code":17}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, seen := lookupTestClient(t, tc.status, tc.body)
			ref, err := c.GetCampaign(context.Background(), "120200000000001")
			if err == nil || ref != nil {
				t.Fatalf("want an error, got %+v, %v", ref, err)
			}
			if n := len(seen()); n < 2 {
				t.Errorf("the read was sent %d time(s); a throttled GET is retried", n)
			}
		})
	}
}

func TestMetaGetCampaign_MalformedIDRefusedBeforeAnyRequest(t *testing.T) {
	c, seen := lookupTestClient(t, http.StatusInternalServerError, `{}`)
	for _, id := range []string{"", "0", "0123", "abc", "act_777", " 123", "123 ", "12/34", "123456789012345678901"} {
		ref, err := c.GetCampaign(context.Background(), id)
		if !errors.Is(err, ErrNotACampaignID) || ref != nil {
			t.Errorf("GetCampaign(%q) = %+v, %v; want ErrNotACampaignID", id, ref, err)
		}
	}
	if n := len(seen()); n != 0 {
		t.Errorf("a malformed id reached Meta %d time(s)", n)
	}
}
