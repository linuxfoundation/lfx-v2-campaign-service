// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

const monitorAccountPath = "/api/v3/ad_accounts/t2_test"

// accountStub serves the monitor's campaign list and reports (monitorStub) plus the ad-account
// read the currency comes from.
type accountStub struct {
	*monitorStub
	status int
	body   string
	// block, when set, holds the account read open until it is closed or the request ends.
	block chan struct{}
	// retryAfter, when set, is sent as the account read's Retry-After header.
	retryAfter string
}

func (s *accountStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == monitorAccountPath {
		s.mu.Lock()
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		if s.block != nil {
			select {
			case <-s.block:
			case <-r.Context().Done():
			}
			return
		}
		if s.retryAfter != "" {
			w.Header().Set("Retry-After", s.retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.body))
		return
	}
	s.monitorStub.ServeHTTP(w, r)
}

// The Reddit monitor's currency is one identity-checked GET /ad_accounts/{id}: present when the
// body describes the requested account with a well-formed code; absent ("") when the field is
// absent or null, malformed, the body names another account, carries a duplicate key, or the
// read fails. None of those fails the monitor or costs its rows.
func TestListAccountCampaignsWithCurrency(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"present", http.StatusOK, `{"data":{"id":"t2_test","name":"Acct","currency":"EUR"}}`, "EUR"},
		{"absent", http.StatusOK, `{"data":{"id":"t2_test","name":"Acct"}}`, ""},
		{"null", http.StatusOK, `{"data":{"id":"t2_test","currency":null}}`, ""},
		{"malformed (lower case) dropped", http.StatusOK, `{"data":{"id":"t2_test","currency":"eur"}}`, ""},
		{"malformed (symbol) dropped", http.StatusOK, `{"data":{"id":"t2_test","currency":"€"}}`, ""},
		{"another account's answer", http.StatusOK, `{"data":{"id":"t2_other","currency":"EUR"}}`, ""},
		{"no id at all", http.StatusOK, `{"data":{"currency":"EUR"}}`, ""},
		{"duplicate key refused", http.StatusOK, `{"data":{"id":"t2_test","currency":"USD","Currency":"EUR"}}`, ""},
		{"the read fails", http.StatusInternalServerError, `{"error":"boom EUR"}`, ""},
		{"the account is forbidden", http.StatusForbidden, `{}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &accountStub{
				monitorStub: &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(2) + `}`, report: healthyReport},
				status:      tc.status, body: tc.body,
			}
			c := newMonitorClient(t, stub, fixedRedditClock())
			rows, currency, err := c.ListAccountCampaignsWithCurrency(context.Background(), testAccount.AccountID, 7)
			if err != nil {
				t.Fatalf("ListAccountCampaignsWithCurrency: %v", err)
			}
			if currency != tc.want {
				t.Errorf("currency = %q, want %q", currency, tc.want)
			}
			if len(rows) != 2 {
				t.Fatalf("got %d rows, want 2", len(rows))
			}
			for _, r := range rows {
				if r.FetchFailed {
					t.Errorf("row %+v FetchFailed: the currency read must not touch the rows", r)
				}
			}
			reads := 0
			for _, p := range stub.requestedPaths() {
				if p == http.MethodGet+" "+monitorAccountPath {
					reads++
				}
			}
			if reads != 1 {
				t.Errorf("ad-account reads = %d, want exactly 1", reads)
			}
		})
	}
}

// ListAccountCampaigns (no currency) makes no ad-account read: the extra call is paid only by
// the caller that asked for the currency.
func TestListAccountCampaigns_MakesNoAccountRead(t *testing.T) {
	stub := &accountStub{
		monitorStub: &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(1) + `}`, report: healthyReport},
		status:      http.StatusOK, body: `{"data":{"id":"t2_test","currency":"EUR"}}`,
	}
	c := newMonitorClient(t, stub, fixedRedditClock())
	if _, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7); err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	for _, p := range stub.requestedPaths() {
		if p == http.MethodGet+" "+monitorAccountPath {
			t.Errorf("ListAccountCampaigns read the ad account: %v", stub.requestedPaths())
		}
	}
}

// A failed currency read's error never carries upstream text or the account id.
func TestReadMonitorAccountCurrency_ErrorsCarryNoUpstreamText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"5xx", http.StatusInternalServerError, `{"error":"UPSTREAM-CANARY"}`},
		{"another account", http.StatusOK, `{"data":{"id":"UPSTREAM-CANARY","currency":"EUR"}}`},
		{"duplicate key", http.StatusOK, `{"data":{"id":"t2_test","id":"UPSTREAM-CANARY"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &accountStub{monitorStub: &monitorStub{t: t}, status: tc.status, body: tc.body}
			c := newMonitorClient(t, stub, fixedRedditClock())
			_, err := c.readMonitorAccountCurrency(context.Background(), testAccount.AccountID)
			if err == nil {
				t.Fatal("an untrustworthy answer was accepted")
			}
			for _, leak := range []string{"UPSTREAM-CANARY", testAccount.AccountID} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q carries %q", err, leak)
				}
			}
		})
	}
}

// The currency read is off the monitor's critical path: an account read that hangs, or answers
// 429 with a long Retry-After, costs the monitor at most monitorCurrencyTimeout (and the 429 is
// not waited out at all) — the rows come back measured and the currency is "".
func TestListAccountCampaignsWithCurrency_SlowOrThrottledAccountRead(t *testing.T) {
	for _, tc := range []struct {
		name       string
		block      bool
		status     int
		retryAfter string
		within     time.Duration
	}{
		{"the account read hangs", true, 0, "", monitorCurrencyTimeout + 2*time.Second},
		{"the account read is throttled for 30s", false, http.StatusTooManyRequests, "30", time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &accountStub{
				monitorStub: &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(3) + `}`, report: healthyReport},
				status:      tc.status, body: `{}`, retryAfter: tc.retryAfter,
			}
			if tc.block {
				stub.block = make(chan struct{})
				t.Cleanup(func() { close(stub.block) })
			}
			c := newMonitorClient(t, stub, fixedRedditClock())
			start := time.Now()
			rows, currency, err := c.ListAccountCampaignsWithCurrency(context.Background(), testAccount.AccountID, 7)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("ListAccountCampaignsWithCurrency: %v", err)
			}
			if currency != "" {
				t.Errorf("currency = %q, want \"\"", currency)
			}
			if len(rows) != 3 {
				t.Fatalf("got %d rows, want 3", len(rows))
			}
			for _, r := range rows {
				if r.FetchFailed {
					t.Errorf("row %+v FetchFailed: a slow currency read must not touch the rows", r)
				}
			}
			if elapsed > tc.within {
				t.Errorf("took %v, want under %v", elapsed, tc.within)
			}
			reads := 0
			for _, p := range stub.requestedPaths() {
				if p == http.MethodGet+" "+monitorAccountPath {
					reads++
				}
			}
			if reads != 1 {
				t.Errorf("ad-account reads = %d, want exactly 1 (a 429 is not retried)", reads)
			}
		})
	}
}
