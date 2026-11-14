// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// assertScheduleRefusal checks err is the pre-write refusal: ErrCampaignGroupScheduleConflict,
// ErrCreateConfigInvalid (a 400) and a PreflightRefusal whose operator text says what to do.
func assertScheduleRefusal(t *testing.T, err error, wantText string) {
	t.Helper()
	if !errors.Is(err, ErrCampaignGroupScheduleConflict) {
		t.Fatalf("err = %v, want ErrCampaignGroupScheduleConflict", err)
	}
	if !errors.Is(err, domain.ErrCreateConfigInvalid) {
		t.Errorf("err = %v, want it to carry ErrCreateConfigInvalid", err)
	}
	msg, ok := domain.AsPreflightRefusal(err)
	if !ok {
		t.Fatalf("err = %v, want a PreflightRefusal (nothing was written)", err)
	}
	if !strings.Contains(msg, wantText) {
		t.Errorf("operator message = %q, want it to contain %q", msg, wantText)
	}
}

// A daily-capped lifetime campaign (#584) has no end of its own, so the shared group must end
// exactly at the campaign's end: a reused group ending later, or never, is refused with nothing
// written; one ending earlier is widened to exactly the campaign's end.
func TestEnsureCampaignGroupCoversEnd_CappedNeedsTheGroupToEndAtItsEnd(t *testing.T) {
	const endMs = int64(4_000_000_000_000)
	cases := []struct {
		name      string
		end       *int64
		wantErr   string
		wantWrite bool
	}{
		{"group ends later: refused", i64(5_000_000_000_000), "has an end of", false},
		{"group has no end: refused", nil, "has no end", false},
		{"group ends exactly at the campaign end: accepted", i64(endMs), "", false},
		{"group ends earlier: widened to exactly the campaign end", i64(3_000_000_000_000), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &groupScheduleServer{end: tc.end}
			wrote, err := g.client(t).ensureCampaignGroupCoversEnd(context.Background(), "123456789", "555", endMs, true)
			if tc.wantErr != "" {
				assertScheduleRefusal(t, err, tc.wantErr)
			} else if err != nil {
				t.Fatalf("ensureCampaignGroupCoversEnd: %v", err)
			}
			if wrote != tc.wantWrite || len(g.patches) != map[bool]int{true: 1, false: 0}[tc.wantWrite] {
				t.Fatalf("wrote=%v with %d writes, want %v", wrote, len(g.patches), tc.wantWrite)
			}
			if tc.wantWrite && *g.currentEnd() != endMs {
				t.Fatalf("group end = %d, want %d", *g.currentEnd(), endMs)
			}
		})
	}
}

// After this call's own write, another writer moving the group's end past a capped campaign's is
// still refused, but as a plain error: a write was sent, so it is not a PreflightRefusal.
func TestEnsureCampaignGroupCoversEnd_CappedRefusalAfterOwnWriteIsNotPreflight(t *testing.T) {
	g := &groupScheduleServer{end: i64(3_000_000_000_000)}
	g.afterPatch = func(g *groupScheduleServer) {
		g.mu.Lock()
		g.end = i64(5_000_000_000_000)
		g.mu.Unlock()
	}
	wrote, err := g.client(t).ensureCampaignGroupCoversEnd(context.Background(), "123456789", "555", 4_000_000_000_000, true)
	if !wrote || !errors.Is(err, ErrCampaignGroupScheduleConflict) {
		t.Fatalf("wrote=%v err=%v, want a write and ErrCampaignGroupScheduleConflict", wrote, err)
	}
	if _, ok := domain.AsPreflightRefusal(err); ok {
		t.Fatalf("err = %v is a PreflightRefusal, but a write was sent", err)
	}
}

// Widening a group moves the end of every campaign in it that has none of its own. A group
// holding a daily-capped lifetime campaign (both budgets, no end) is therefore not widened; one
// holding only ordinary campaigns is widened exactly as before.
func TestEnsureCampaignGroupCoversEnd_WideningChecksForCappedCampaigns(t *testing.T) {
	const endMs = int64(4_000_000_000_000)
	const capped = `{"id":"urn:li:sponsoredCampaign:901","status":"ACTIVE","campaignGroup":"urn:li:sponsoredCampaignGroup:555","totalBudget":{"amount":"100.00","currencyCode":"USD"},"dailyBudget":{"amount":"20.00","currencyCode":"USD"},"runSchedule":{"start":1000}}`
	const lifetimeWithEnd = `{"id":902,"status":"ACTIVE","campaignGroup":"urn:li:sponsoredCampaignGroup:555","totalBudget":{"amount":"100.00","currencyCode":"USD"},"runSchedule":{"start":1000,"end":3000000000000}}`
	const bothWithEnd = `{"id":903,"status":"PAUSED","campaignGroup":"urn:li:sponsoredCampaignGroup:555","totalBudget":{"amount":"100.00","currencyCode":"USD"},"dailyBudget":{"amount":"20.00","currencyCode":"USD"},"runSchedule":{"start":1000,"end":3000000000000}}`
	const cappedCanceled = `{"id":904,"status":"CANCELED","campaignGroup":"urn:li:sponsoredCampaignGroup:555","totalBudget":{"amount":"100.00","currencyCode":"USD"},"dailyBudget":{"amount":"20.00","currencyCode":"USD"},"runSchedule":{"start":1000}}`
	page := func(els ...string) string {
		return `{"elements":[` + strings.Join(els, ",") + `],"metadata":{"nextPageToken":""}}`
	}
	cases := []struct {
		name         string
		campaigns    string
		searchStatus int
		wantRefusal  bool
		wantErr      string
		wantWrite    bool
	}{
		{"holds a capped campaign: refused, group untouched", page(lifetimeWithEnd, capped), 0, true, "", false},
		{"only ordinary campaigns: widened", page(lifetimeWithEnd, bothWithEnd, cappedCanceled), 0, false, "", true},
		{"empty group: widened", page(), 0, false, "", true},
		{"search fails: error, group untouched", "", http.StatusInternalServerError, false, "could not be checked", false},
		{"search ignores the group filter: error, group untouched", page(`{"id":905,"status":"ACTIVE","campaignGroup":"urn:li:sponsoredCampaignGroup:777"}`), 0, false, "another group", false},
		{"page with no metadata: error, group untouched", `{"elements":[]}`, 0, false, "no metadata", false},
		{"page with a duplicate key: error, group untouched", `{"elements":[],"elements":[],"metadata":{}}`, 0, false, "campaigns", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &groupScheduleServer{end: i64(3_000_000_000_000), campaigns: tc.campaigns, searchStatus: tc.searchStatus}
			wrote, err := g.client(t).ensureCampaignGroupCoversEnd(context.Background(), "123456789", "555", endMs, false)
			switch {
			case tc.wantRefusal:
				assertScheduleRefusal(t, err, `campaign "901"`)
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("ensureCampaignGroupCoversEnd: %v", err)
			}
			if wrote != tc.wantWrite || len(g.patches) != map[bool]int{true: 1, false: 0}[tc.wantWrite] {
				t.Fatalf("wrote=%v with %d group writes, want %v", wrote, len(g.patches), tc.wantWrite)
			}
			if len(g.searches) != 1 {
				t.Fatalf("searches = %d, want exactly one", len(g.searches))
			}
			if want := "search=(campaignGroup:(values:List(urn%3Ali%3AsponsoredCampaignGroup%3A555)))"; !strings.Contains(g.searches[0], want) {
				t.Errorf("search query = %q, want it to contain %q", g.searches[0], want)
			}
		})
	}
}

// A group that already covers the campaign is not searched: the check costs a request only when a
// widening write is about to happen.
func TestEnsureCampaignGroupCoversEnd_NoSearchWithoutAWrite(t *testing.T) {
	g := &groupScheduleServer{end: i64(5_000_000_000_000)}
	if _, err := g.client(t).ensureCampaignGroupCoversEnd(context.Background(), "123456789", "555", 4_000_000_000_000, false); err != nil {
		t.Fatalf("ensureCampaignGroupCoversEnd: %v", err)
	}
	if len(g.searches) != 0 {
		t.Fatalf("searches = %d, want none", len(g.searches))
	}
}

// End to end: a daily-capped lifetime create that reuses the event's group refuses with NO result
// and no campaign POST when the group ends later or never, and proceeds when the group ends
// exactly at its end. A group this call creates carries the campaign's end and needs no check.
func TestCreateCampaign_CappedLifetimeReusedGroupEndGuard(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reuse     bool
		groupBody string
		wantErr   string
		wantPosts []string
	}{
		{"reused group ends later: refused", true, `{"id":100,"runSchedule":{"start":1000,"end":99999999999999}}`, "has an end of", nil},
		{"reused group has no end: refused", true, `{"id":100,"runSchedule":{"start":1000}}`, "has no end", nil},
		{"reused group ends at the campaign end: created", true, "", "", []string{"create-campaign"}},
		{"group created by this call: created", false, "", "", []string{"create-group", "create-campaign"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := budgetCreateInput(1000, true, 20)
			endMs, err := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithClock(fixedClock())).toMs(in.EndDate, true)
			if err != nil {
				t.Fatalf("toMs: %v", err)
			}
			groupBody := tc.groupBody
			if groupBody == "" {
				groupBody = `{"id":100,"runSchedule":{"start":1000,"end":` + strconv.FormatInt(endMs, 10) + `}}`
			}
			var mu sync.Mutex
			var posts []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "adCampaignGroups/100"):
					_, _ = io.WriteString(w, groupBody)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaignGroups") && tc.reuse:
					_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | CNCF","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:100"}],"metadata":{"nextPageToken":""}}`)
				case r.Method == http.MethodGet:
					_, _ = io.WriteString(w, `{"elements":[],"metadata":{"nextPageToken":""}}`)
				case strings.HasSuffix(r.URL.Path, "adCampaignGroups/100"):
					posts = append(posts, "extend-group")
					w.WriteHeader(http.StatusNoContent)
				case strings.HasSuffix(r.URL.Path, "adCampaignGroups"):
					posts = append(posts, "create-group")
					_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
				case strings.HasSuffix(r.URL.Path, "adCampaigns"):
					posts = append(posts, "create-campaign")
					w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
					_, _ = io.WriteString(w, `{}`)
				case strings.Contains(r.URL.Path, "posts"):
					_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
				case strings.Contains(r.URL.Path, "creatives"):
					_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
				default:
					http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
				}
			}))
			defer srv.Close()

			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			res, err := c.CreateCampaign(context.Background(), in)
			if tc.wantErr != "" {
				assertScheduleRefusal(t, err, tc.wantErr)
				if res != nil {
					t.Fatalf("want no result (nothing was created), got %+v", res)
				}
			} else if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(posts, ",") != strings.Join(tc.wantPosts, ",") {
				t.Fatalf("mutating requests = %v, want %v", posts, tc.wantPosts)
			}
		})
	}
}
