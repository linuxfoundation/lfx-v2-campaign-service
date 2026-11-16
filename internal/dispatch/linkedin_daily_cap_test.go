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

// linkedInDailyCapConfig is a valid LinkedIn create config with the given budget fields spliced
// in, e.g. `"budgetUsd":1000,"lifetimeBudget":true,"dailyCapUsd":20`.
func linkedInDailyCapConfig(budget string) json.RawMessage {
	return json.RawMessage(`{"linkedInConfig":{` + budget + `,
		"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
}

// TestLinkedIn_DailyCapPreflight pins #584's synchronous 400s: a cap that is mistyped, null,
// beside a daily budget, under LinkedIn's daily minimum, or above the lifetime total is refused
// before the create job exists.
func TestLinkedIn_DailyCapPreflight(t *testing.T) {
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct{ name, budget, want string }{
		{"valid", `"budgetUsd":1000,"lifetimeBudget":true,"dailyCapUsd":20`, ""},
		{"no cap", `"budgetUsd":1000,"lifetimeBudget":true`, ""},
		{"null", `"budgetUsd":1000,"lifetimeBudget":true,"dailyCapUsd":null`, "must be a number"},
		{"a string", `"budgetUsd":1000,"lifetimeBudget":true,"dailyCapUsd":"20"`, "must be a number"},
		{"an explicit zero", `"budgetUsd":1000,"lifetimeBudget":true,"dailyCapUsd":0`, "greater than zero"},
		{"beside a daily budget", `"budgetUsd":50,"dailyCapUsd":20`, "applies only to a lifetime budget"},
		{"under the daily minimum", `"budgetUsd":1000,"lifetimeBudget":true,"dailyCapUsd":5`, "below LinkedIn's minimum"},
		{"above the total", `"budgetUsd":1000,"lifetimeBudget":true,"dailyCapUsd":1500`, "is more than the lifetime budget"},
		// Copilot (PR #607): a total under LinkedIn's $100 lifetime minimum must not let a cap
		// above it through to an async job failure.
		{"above a total under the lifetime minimum", `"budgetUsd":50,"lifetimeBudget":true,"dailyCapUsd":150`, "daily cap 150.00 is more than the lifetime budget 50.00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, linkedInDailyCapConfig(tc.budget))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, "linkedInConfig.dailyCapUsd") || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// TestLinkedIn_DailyCapRefusedInDispatch drives the same refusals through Dispatch, the path a
// job takes. Without Dispatch's own check, a null cap would decode as "no cap" and create an
// uncapped campaign; it must instead be refused with nothing sent upstream.
func TestLinkedIn_DailyCapRefusedInDispatch(t *testing.T) {
	for _, budget := range []string{
		`"budgetUsd":1000,"lifetimeBudget":true,"dailyCapUsd":null`,
		`"budgetUsd":1000,"lifetimeBudget":true,"dailyCapUsd":1500`,
	} {
		t.Run(budget, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer srv.Close()
			d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))

			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInDailyCapConfig(budget))
			var nuc interface{ NoUpstreamCreate() bool }
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Fatalf("Dispatch = %v, want a not-created config refusal", err)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%d LinkedIn requests for a refused create", n)
			}
		})
	}
}

// TestLinkedIn_DailyCapReachesTheCampaign pins the wiring end to end: linkedInConfig.dailyCapUsd
// becomes the campaign's dailyBudget beside its totalBudget, and the stored snapshot records it.
func TestLinkedIn_DailyCapReachesTheCampaign(t *testing.T) {
	var mu sync.Mutex
	var campaignBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			_ = json.Unmarshal(b, &campaignBody)
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
		linkedInDailyCapConfig(`"budgetUsd":1000,"lifetimeBudget":true,"dailyCapUsd":20`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	total, _ := campaignBody["totalBudget"].(map[string]any)
	daily, _ := campaignBody["dailyBudget"].(map[string]any)
	mu.Unlock()
	if total["amount"] != "1000.00" || daily["amount"] != "20.00" {
		t.Fatalf("campaign budgets = total %v, daily %v; want 1000.00 and the 20.00 cap", total, daily)
	}
	if !strings.Contains(string(camp.ConfigSnapshot), `"dailyCapUsd":20`) {
		t.Errorf("config snapshot does not record the cap: %s", camp.ConfigSnapshot)
	}
}
