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
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// linkedInBudgetConfig is a complete linkedInConfig whose budget fields are the given JSON
// fragment, e.g. `"budgetUsd":1000,"lifetimeBudget":true,"dailyBudgetUsd":20`.
func linkedInBudgetConfig(budget string) json.RawMessage {
	return json.RawMessage(`{"linkedInConfig":{` + budget + `,
		"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
}

// TestLinkedIn_BudgetPairPreflight pins #584's synchronous 400s: every rule on a daily cap is
// refused before any job exists, and a config without one is left to the existing job-time check.
func TestLinkedIn_BudgetPairPreflight(t *testing.T) {
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct{ name, budget, want string }{
		{"lifetime with a daily cap", `"budgetUsd":1000,"lifetimeBudget":true,"dailyBudgetUsd":20`, ""},
		{"cap equal to the total", `"budgetUsd":100,"lifetimeBudget":true,"dailyBudgetUsd":100`, ""},
		{"lifetime only", `"budgetUsd":1000,"lifetimeBudget":true`, ""},
		{"daily only", `"budgetUsd":50`, ""},
		{"zero cap reads as none", `"budgetUsd":1000,"lifetimeBudget":true,"dailyBudgetUsd":0`, ""},
		{"null cap reads as none", `"budgetUsd":1000,"lifetimeBudget":true,"dailyBudgetUsd":null`, ""},
		{"cap without lifetimeBudget", `"budgetUsd":50,"dailyBudgetUsd":20`, "needs lifetimeBudget: true"},
		{"cap above the total", `"budgetUsd":150,"lifetimeBudget":true,"dailyBudgetUsd":200`, "above the lifetime budget"},
		{"cap below LinkedIn's daily minimum", `"budgetUsd":1000,"lifetimeBudget":true,"dailyBudgetUsd":5`, "minimum of $10"},
		{"total below LinkedIn's lifetime minimum", `"budgetUsd":90,"lifetimeBudget":true,"dailyBudgetUsd":20`, "minimum of $100"},
		{"negative cap", `"budgetUsd":1000,"lifetimeBudget":true,"dailyBudgetUsd":-20`, "greater than zero"},
		{"string cap", `"budgetUsd":1000,"lifetimeBudget":true,"dailyBudgetUsd":"20"`, "must be a number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, linkedInBudgetConfig(tc.budget))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// TestLinkedIn_BudgetPairDispatchRefusesBeforeAnyRequest: Dispatch applies the same rules with
// nothing sent upstream, so a caller that skipped the synchronous check still creates nothing.
func TestLinkedIn_BudgetPairDispatchRefusesBeforeAnyRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))

	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds,
		linkedInBudgetConfig(`"budgetUsd":150,"lifetimeBudget":true,"dailyBudgetUsd":200`))
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Fatalf("Dispatch = %v, want a not-created config refusal", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d LinkedIn requests for a refused create", n)
	}
}

// TestLinkedIn_BudgetPairReachesTheWire is the end-to-end wiring check: linkedInConfig's
// dailyBudgetUsd reaches the campaign POST as dailyBudget beside totalBudget, and the row records
// the lifetime total as its budget with the cap kept in the config snapshot.
func TestLinkedIn_BudgetPairReachesTheWire(t *testing.T) {
	var mu sync.Mutex
	var campaignRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			mu.Lock()
			campaignRaw = string(raw)
			mu.Unlock()
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
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock))

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds,
		linkedInBudgetConfig(`"budgetUsd":1000,"lifetimeBudget":true,"dailyBudgetUsd":20`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	raw := campaignRaw
	mu.Unlock()
	for _, want := range []string{
		`"totalBudget":{"amount":"1000.00","currencyCode":"USD"}`,
		`"dailyBudget":{"amount":"20.00","currencyCode":"USD"}`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("campaign POST body lacks %s:\n%s", want, raw)
		}
	}
	if camp.BudgetAmount == nil || *camp.BudgetAmount != 1000 || camp.BudgetType == nil || *camp.BudgetType != model.BudgetLifetime {
		t.Errorf("row budget = %v %v, want 1000 lifetime", camp.BudgetAmount, camp.BudgetType)
	}
	if !strings.Contains(string(camp.ConfigSnapshot), `"dailyBudgetUsd":20`) {
		t.Errorf("config snapshot lacks the daily cap: %s", camp.ConfigSnapshot)
	}
}
