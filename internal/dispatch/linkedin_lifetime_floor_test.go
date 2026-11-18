// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// linkedInFlightConfig is a valid LinkedIn create config over startDate..endDate with the given
// budget fields spliced in.
func linkedInFlightConfig(budget, start, end string) json.RawMessage {
	return json.RawMessage(`{"linkedInConfig":{` + budget + `,
		"startDate":"` + start + `","endDate":"` + end + `",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
}

// TestLinkedIn_LifetimeFloorPreflight pins #633's synchronous 400: an uncapped lifetime budget on
// a fixed schedule under max($100, $10 × days) is refused before the create job exists. A daily
// budget, and a lifetime budget with a #584 daily cap, are not held to it.
func TestLinkedIn_LifetimeFloorPreflight(t *testing.T) {
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	// 2099-01-01..2099-01-31 is 31 days, so the floor is $310.
	for _, tc := range []struct{ name, budget, end, want string }{
		{"31 days at $309", `"budgetUsd":309,"lifetimeBudget":true`, "2099-01-31", "minimum of $310 for a 31-day flight from 2099-01-01 through 2099-01-31"},
		{"31 days at $310", `"budgetUsd":310,"lifetimeBudget":true`, "2099-01-31", ""},
		{"rounds up to the floor", `"budgetUsd":309.996,"lifetimeBudget":true`, "2099-01-31", ""},
		{"rounds down below the floor", `"budgetUsd":309.994,"lifetimeBudget":true`, "2099-01-31", "lifetime budget 309.99 is below"},
		{"one day at $100", `"budgetUsd":100,"lifetimeBudget":true`, "2099-01-01", ""},
		{"one day under $100", `"budgetUsd":99,"lifetimeBudget":true`, "2099-01-01", "minimum of $100 for a 1-day flight"},
		{"a daily budget", `"budgetUsd":10`, "2099-01-31", ""},
		{"a capped lifetime budget", `"budgetUsd":150,"lifetimeBudget":true,"dailyCapUsd":10`, "2099-01-31", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, linkedInFlightConfig(tc.budget, "2099-01-01", tc.end))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, "linkedInConfig.budgetUsd") || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// TestLinkedIn_LifetimeFloorRefusedInDispatch drives the refusal through Dispatch, the path a
// job takes: nothing is sent upstream.
func TestLinkedIn_LifetimeFloorRefusedInDispatch(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))

	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds,
		linkedInFlightConfig(`"budgetUsd":100,"lifetimeBudget":true`, "2099-01-01", "2099-01-31"))
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Fatalf("Dispatch = %v, want a not-created config refusal", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d LinkedIn requests for a refused create", n)
	}
}

// TestLinkedInLifetimeFloor_PastStartCountsFromToday pins the preflight's day count for a start
// already begun: toMs sends today's now+buffer, so the flight is today through the end.
func TestLinkedInLifetimeFloor_PastStartCountsFromToday(t *testing.T) {
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	cfg := linkedinConfig{BudgetUSD: 310, LifetimeBudget: true, StartDate: "2026-11-01", EndDate: "2026-12-18"}
	if err := linkedinLifetimeFloor(cfg, now); err != nil {
		t.Fatalf("18 Nov–18 Dec is 31 days from today; $310 covers it: %v", err)
	}
	cfg.BudgetUSD = 100
	if err := linkedinLifetimeFloor(cfg, now); err == nil || !strings.Contains(err.Error(), "minimum of $310 for a 31-day flight from 2026-11-18") {
		t.Fatalf("930195766's shape ($100, 31 days) = %v, want refused naming $310", err)
	}
}

// liFixedSchedule is a lifetime campaign read with a runSchedule from start's 00:00 UTC to end's
// 23:59:59.999 UTC, the instants the create path sends.
func liFixedSchedule(t *testing.T, total, start, end string, withEnd bool) string {
	t.Helper()
	s, err := time.ParseInLocation("2006-01-02", start, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	e, err := time.ParseInLocation("2006-01-02", end, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	sched := `"start":` + strconv.FormatInt(s.UnixMilli(), 10)
	if withEnd {
		sched += `,"end":` + strconv.FormatInt(e.Add(24*time.Hour-time.Millisecond).UnixMilli(), 10)
	}
	return `{"id":777,"totalBudget":{"amount":"` + total + `","currencyCode":"USD"},"runSchedule":{` + sched + `}}`
}

// TestLinkedIn_WriteBudget_LifetimeFloor pins #633 on the edit path: a lifetime amount change on
// a fixed-schedule campaign must still cover $10 for every day of its flight, refused as a 400
// with nothing written; a continuous-schedule campaign is not held to it.
func TestLinkedIn_WriteBudget_LifetimeFloor(t *testing.T) {
	t.Run("under the floor is refused", func(t *testing.T) {
		d, calls := liBudgetDispatcher(t, liFixedSchedule(t, "1000.00", "2026-11-18", "2026-12-18", true), http.StatusNoContent)
		err := d.WriteBudget(context.Background(), "proj", model.ProviderLinkedInAds, liBudgetCampaign(),
			model.BudgetChange{Amount: 309, Type: model.BudgetLifetime})
		if !errors.Is(err, domain.ErrBudgetAmountRejected) || !strings.Contains(err.Error(), "minimum of $310 for a 31-day flight") {
			t.Fatalf("WriteBudget = %v, want a budget-amount refusal naming $310", err)
		}
		assertNoLinkedInWrite(t, calls())
	})
	t.Run("at the floor is written", func(t *testing.T) {
		d, calls := liBudgetDispatcher(t, liFixedSchedule(t, "1000.00", "2026-11-18", "2026-12-18", true), http.StatusNoContent)
		if err := d.WriteBudget(context.Background(), "proj", model.ProviderLinkedInAds, liBudgetCampaign(),
			model.BudgetChange{Amount: 310, Type: model.BudgetLifetime}); err != nil {
			t.Fatalf("WriteBudget: %v", err)
		}
		if w := liWrites(calls()); len(w) != 1 || !strings.Contains(w[0].Body, `"310.00"`) {
			t.Fatalf("want one totalBudget write of 310.00, got %+v", w)
		}
	})
	t.Run("a continuous schedule is not held to it", func(t *testing.T) {
		d, calls := liBudgetDispatcher(t, liFixedSchedule(t, "1000.00", "2026-11-18", "2026-12-18", false), http.StatusNoContent)
		if err := d.WriteBudget(context.Background(), "proj", model.ProviderLinkedInAds, liBudgetCampaign(),
			model.BudgetChange{Amount: 150, Type: model.BudgetLifetime}); err != nil {
			t.Fatalf("WriteBudget: %v", err)
		}
		if w := liWrites(calls()); len(w) != 1 {
			t.Fatalf("want one write, got %+v", w)
		}
	})
}
