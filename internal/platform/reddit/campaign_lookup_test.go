// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

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
)

func TestRedditGetCampaign_ReadsTheAccountScopedCampaign(t *testing.T) {
	c, seen := budgetTestClient(t, http.StatusOK,
		`{"data":{"id":"t3_camp","name":"KubeCon EU — Traffic","configured_status":"PAUSED","effective_status":"PAUSED","ad_account_id":"t2_test"}}`)
	ref, err := c.GetCampaign(context.Background(), "t3_camp")
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	if ref == nil || ref.ID != "t3_camp" || ref.Name != "KubeCon EU — Traffic" || ref.Status != StatusPaused || ref.AdAccountID != "t2_test" {
		t.Fatalf("ref = %+v", ref)
	}
	if got := seen(); len(got) != 1 || got[0] != "GET /api/v3/ad_accounts/t2_test/campaigns/t3_camp " {
		t.Errorf("requests = %q, want exactly one GET of the account-scoped campaign", got)
	}
}

func TestRedditGetCampaign_Outcomes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		absent  bool
		wantErr bool
		account string
	}{
		{name: "active", status: 200, body: `{"data":{"id":"t3_camp","name":"n","configured_status":"ACTIVE","ad_account_id":"t2_test"}}`, account: "t2_test"},
		{name: "an unreported account is left to the path scope", status: 200, body: `{"data":{"id":"t3_camp","name":"n","configured_status":"ACTIVE"}}`},
		// Reported faithfully; the dispatcher refuses it.
		{name: "another account is reported, not hidden", status: 200, body: `{"data":{"id":"t3_camp","name":"n","configured_status":"ACTIVE","ad_account_id":"t2_other"}}`, account: "t2_other"},
		// Every request here gets the same answer, so the confirming account read 404s too: the
		// absence is unproven. The proven case is TestRedditGetCampaign_404IsAbsentOnlyOnceTheAccountIsConfirmed.
		{name: "404 with an unconfirmed account is unverifiable", status: 404, body: `{}`, wantErr: true},
		{name: "deleted is absent", status: 200, body: `{"data":{"id":"t3_camp","name":"n","configured_status":"DELETED"}}`, absent: true},
		{name: "archived is absent", status: 200, body: `{"data":{"id":"t3_camp","name":"n","configured_status":"ARCHIVED"}}`, absent: true},
		{name: "unknown status is unverifiable", status: 200, body: `{"data":{"id":"t3_camp","name":"n","configured_status":"PENDING"}}`, wantErr: true},
		{name: "missing status is unverifiable", status: 200, body: `{"data":{"id":"t3_camp","name":"n"}}`, wantErr: true},
		{name: "missing name is unverifiable", status: 200, body: `{"data":{"id":"t3_camp","configured_status":"ACTIVE"}}`, wantErr: true},
		{name: "another campaign is unverifiable", status: 200, body: `{"data":{"id":"t3_other","name":"n","configured_status":"ACTIVE"}}`, wantErr: true},
		{name: "a duplicated id is unverifiable", status: 200, body: `{"data":{"id":"t3_other","id":"t3_camp","name":"n","configured_status":"ACTIVE"}}`, wantErr: true},
		// The ENVELOPE is guarded, not only the decoded data: a duplicated "data" is resolved
		// last-wins before GetCampaign ever sees Data.
		{name: "a duplicated data key is unverifiable", status: 200, body: `{"data":{"id":"t3_other","name":"x","configured_status":"ACTIVE"},"data":{"id":"t3_camp","name":"n","configured_status":"ACTIVE"}}`, wantErr: true},
		{name: "data beside Data is unverifiable", status: 200, body: `{"Data":{"id":"t3_other","name":"x","configured_status":"ACTIVE"},"data":{"id":"t3_camp","name":"n","configured_status":"ACTIVE"}}`, wantErr: true},
		{name: "no data is unverifiable", status: 200, body: `{"data":null}`, wantErr: true},
		{name: "a malformed body is unverifiable", status: 200, body: `{"data":{"id":`, wantErr: true},
		{name: "a non-object campaign is unverifiable", status: 200, body: `{"data":[1]}`, wantErr: true},
		{name: "403 is unverifiable, not absent", status: 403, body: `{}`, wantErr: true},
		{name: "5xx is unverifiable", status: 502, body: `{}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := budgetTestClient(t, tc.status, tc.body, withRetryBaseDelay(time.Millisecond))
			ref, err := c.GetCampaign(context.Background(), "t3_camp")
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
				if err != nil || ref == nil || ref.AdAccountID != tc.account {
					t.Fatalf("want a ref reporting account %q, got %+v, %v", tc.account, ref, err)
				}
			}
		})
	}
}

func TestRedditGetCampaign_ExhaustedThrottleIsAnErrorNotAbsence(t *testing.T) {
	c, seen := budgetTestClient(t, http.StatusTooManyRequests, `{}`, withRetryBaseDelay(time.Millisecond))
	ref, err := c.GetCampaign(context.Background(), "t3_camp")
	if err == nil || ref != nil {
		t.Fatalf("want an error, got %+v, %v", ref, err)
	}
	if n := len(seen()); n < 2 {
		t.Errorf("the read was sent %d time(s); a throttled GET is retried", n)
	}
}

func TestRedditGetCampaign_MalformedIDRefusedBeforeAnyRequest(t *testing.T) {
	c, seen := budgetTestClient(t, http.StatusOK, `{"data":{}}`)
	long := make([]byte, maxCampaignIDLen+1)
	for i := range long {
		long[i] = 'a'
	}
	for _, id := range []string{"", " t3_camp", "t3_camp ", "t3/../x", "t3?x=1", "t3#f", "t3-camp", string(long)} {
		ref, err := c.GetCampaign(context.Background(), id)
		if !errors.Is(err, ErrInvalidCampaignID) || ref != nil {
			t.Errorf("GetCampaign(%q) = %+v, %v; want ErrInvalidCampaignID", id, ref, err)
		}
	}
	if n := len(seen()); n != 0 {
		t.Errorf("a malformed id reached Reddit %d time(s)", n)
	}
}

// lookupAccountStub answers the campaign read and the ad-account read separately, recording the
// order of every path. Guarded: the handler runs on the server goroutine.
func lookupAccountStub(t *testing.T, campStatus int, campBody string, acctStatus int, acctBody string) (*Client, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if strings.Contains(r.URL.Path, "/campaigns/") {
			w.WriteHeader(campStatus)
			_, _ = io.WriteString(w, campBody)
			return
		}
		w.WriteHeader(acctStatus)
		_, _ = io.WriteString(w, acctBody)
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	c := NewClient(testCreds, testAccount, WithBaseURL(api.URL+"/api/v3"), WithTokenURL(tok.URL),
		WithNowFunc(fixedRedditClock()), withRetryBaseDelay(time.Millisecond))
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// A campaign 404 is a PROVEN absence only once the connection's own ad account is confirmed
// readable: Reddit 404s a path whose account is inaccessible or revoked the same way, and a 404
// from that would tell an operator a possibly-live campaign is missing.
func TestRedditGetCampaign_404IsAbsentOnlyOnceTheAccountIsConfirmed(t *testing.T) {
	const acctOK = `{"data":{"id":"t2_test","name":"Test Account"}}`
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
		{"another account: unverifiable", http.StatusOK, `{"data":{"id":"t2_other"}}`, false},
		{"no account: unverifiable", http.StatusOK, `{"data":null}`, false},
		{"duplicated account id: unverifiable", http.StatusOK, `{"data":{"id":"t2_other","id":"t2_test"}}`, false},
		{"malformed account: unverifiable", http.StatusOK, `{"data":{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, seen := lookupAccountStub(t, http.StatusNotFound, `{}`, tc.acctStatus, tc.acctBody)
			ref, err := c.GetCampaign(context.Background(), "t3_camp")
			if tc.absent {
				if err != nil || ref != nil {
					t.Fatalf("want (nil, nil), got %+v, %v", ref, err)
				}
			} else if err == nil || ref != nil {
				t.Fatalf("want an unverifiable error, got %+v, %v", ref, err)
			}
			got := seen()
			if len(got) < 2 || got[0] != "GET /api/v3/ad_accounts/t2_test/campaigns/t3_camp" || got[1] != "GET /api/v3/ad_accounts/t2_test" {
				t.Errorf("requests = %q, want the campaign read and then the account read", got)
			}
		})
	}
}

// The confirming read happens ONLY after a campaign 404.
func TestRedditGetCampaign_NoAccountReadUnlessTheCampaign404s(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusOK, `{"data":{"id":"t3_camp","name":"n","configured_status":"ACTIVE"}}`},
		{http.StatusOK, `{"data":{"id":"t3_camp","name":"n","configured_status":"DELETED"}}`},
		{http.StatusForbidden, `{}`},
		{http.StatusInternalServerError, `{}`},
	} {
		c, seen := lookupAccountStub(t, tc.status, tc.body, http.StatusOK, `{"data":{"id":"t2_test"}}`)
		_, _ = c.GetCampaign(context.Background(), "t3_camp")
		for _, r := range seen() {
			if r == "GET /api/v3/ad_accounts/t2_test" {
				t.Errorf("campaign answered %d: the account was read, but only a campaign 404 needs proof", tc.status)
			}
		}
	}
}
