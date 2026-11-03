// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// groupScheduleServer serves one campaign group's entity read with body and records every
// PARTIAL_UPDATE against it. patchStatus answers the write.
type groupScheduleServer struct {
	body        string
	patchStatus int
	patches     []map[string]any
	restliHdr   []string
}

func (g *groupScheduleServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/adAccounts/123456789/adCampaignGroups/555") {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, g.body)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(raw, &p)
		g.patches = append(g.patches, p)
		g.restliHdr = append(g.restliHdr, r.Header.Get("X-Restli-Method"))
		if g.patchStatus != 0 {
			w.WriteHeader(g.patchStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Every campaign of an event shares one LinkedIn campaign group, whose schedule its creating call
// set. A later campaign that ends after the group would be refused by LinkedIn, so the reused
// group's end is widened first: start re-sent unchanged, end never moved earlier.
func TestEnsureCampaignGroupCoversEnd(t *testing.T) {
	const endMs = int64(4_000_000_000_000)
	cases := []struct {
		name      string
		body      string
		wantWrite bool
		wantErr   string
	}{
		{"ends before the campaign: extended", `{"id":555,"runSchedule":{"start":1000,"end":3000000000000}}`, true, ""},
		{"ends exactly at the campaign end: untouched", `{"id":555,"runSchedule":{"start":1000,"end":4000000000000}}`, false, ""},
		{"ends after the campaign: never shortened", `{"id":555,"runSchedule":{"start":1000,"end":5000000000000}}`, false, ""},
		{"open-ended: covers anything", `{"id":555,"runSchedule":{"start":1000}}`, false, ""},
		{"no schedule at all: nothing to widen", `{"id":555}`, false, ""},
		{"URN id form is accepted", `{"id":"urn:li:sponsoredCampaignGroup:555","runSchedule":{"start":1000,"end":3000000000000}}`, true, ""},
		{"another group's body: refused", `{"id":556,"runSchedule":{"start":1000,"end":3000000000000}}`, false, "does not describe the requested group"},
		{"duplicate key: refused", `{"id":555,"runSchedule":{"start":1000,"end":5000000000000,"end":3000000000000}}`, false, "read campaign group 555"},
		{"end without a start: refused, never guessed", `{"id":555,"runSchedule":{"end":3000000000000}}`, false, "no start"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &groupScheduleServer{body: tc.body}
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(g.start(t).URL), WithClock(fixedClock()))
			wrote, err := c.ensureCampaignGroupCoversEnd(context.Background(), "123456789", "555", endMs)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("ensureCampaignGroupCoversEnd: %v", err)
			}
			if wrote != tc.wantWrite || len(g.patches) != map[bool]int{true: 1, false: 0}[tc.wantWrite] {
				t.Fatalf("wrote = %v with %d writes, want %v", wrote, len(g.patches), tc.wantWrite)
			}
			if !tc.wantWrite {
				return
			}
			if g.restliHdr[0] != "PARTIAL_UPDATE" {
				t.Errorf("X-Restli-Method = %q, want PARTIAL_UPDATE", g.restliHdr[0])
			}
			rs := g.patches[0]["patch"].(map[string]any)["$set"].(map[string]any)["runSchedule"].(map[string]any)
			if rs["start"] != float64(1000) || rs["end"] != float64(endMs) {
				t.Errorf("runSchedule written = %v, want start 1000 (unchanged), end %d", rs, endMs)
			}
		})
	}
}

func TestEnsureCampaignGroupCoversEnd_WriteFailureIsAnError(t *testing.T) {
	g := &groupScheduleServer{body: `{"id":555,"runSchedule":{"start":1000,"end":3000000000000}}`, patchStatus: http.StatusBadRequest}
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(g.start(t).URL), WithClock(fixedClock()))
	wrote, err := c.ensureCampaignGroupCoversEnd(context.Background(), "123456789", "555", 4_000_000_000_000)
	if err == nil || wrote {
		t.Fatalf("wrote=%v err=%v, want an error and no claim of a write", wrote, err)
	}
}

// End to end: a reused group that ends before the new campaign is extended BEFORE the campaign
// create, and a group whose schedule cannot be read creates no campaign but returns a partial
// result carrying the group (so the dispatcher keeps the claim).
func TestCreateCampaign_ReusedGroupScheduleCoversCampaign(t *testing.T) {
	for _, tc := range []struct {
		name        string
		groupBody   string
		wantExtend  bool
		wantErr     bool
		wantCreates int
	}{
		{"group ends early: extended, then campaign created", `{"id":100,"runSchedule":{"start":1000,"end":1000000}}`, true, false, 1},
		{"group open-ended: no write, campaign created", `{"id":100,"runSchedule":{"start":1000}}`, false, false, 1},
		{"group read untrustworthy: nothing created", `{"id":101,"runSchedule":{"start":1000}}`, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "adCampaignGroups/100"):
					_, _ = io.WriteString(w, tc.groupBody)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaignGroups"):
					_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | tlf","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:100"}],"metadata":{"nextPageToken":""}}`)
				case r.Method == http.MethodGet:
					_, _ = io.WriteString(w, `{"elements":[],"metadata":{"nextPageToken":""}}`)
				case strings.HasSuffix(r.URL.Path, "adCampaignGroups/100"):
					order = append(order, "extend-group")
					w.WriteHeader(http.StatusNoContent)
				case strings.HasSuffix(r.URL.Path, "adCampaigns"):
					order = append(order, "create-campaign")
					w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
					_, _ = io.WriteString(w, `{}`)
				case strings.Contains(r.URL.Path, "posts"):
					_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
				case strings.Contains(r.URL.Path, "creatives"):
					_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
				default:
					http.Error(w, "unexpected POST "+r.URL.Path, http.StatusBadRequest)
				}
			}))
			defer srv.Close()

			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			res, err := c.CreateCampaign(context.Background(), CampaignInput{
				EventName:        "KubeCon",
				Project:          "tlf",
				RegistrationURL:  "https://events.example.org/reg",
				BudgetUSD:        100,
				StartDate:        "2099-01-01",
				EndDate:          "2099-02-01",
				TargetingProfile: "cloud-native",
				GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
				Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
				ManualCPCBid:     1,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatal("CreateCampaign: want an error for an untrustworthy group read")
				}
				if res == nil || res.CampaignGroupID != "100" {
					t.Fatalf("want a partial result carrying group 100, got %+v", res)
				}
			} else if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			creates := 0
			for _, o := range order {
				if o == "create-campaign" {
					creates++
				}
			}
			if creates != tc.wantCreates {
				t.Fatalf("campaign creates = %d, want %d (order %v)", creates, tc.wantCreates, order)
			}
			if tc.wantExtend {
				if len(order) < 2 || order[0] != "extend-group" || order[1] != "create-campaign" {
					t.Fatalf("order = %v, want the group extended before the campaign create", order)
				}
			} else {
				for _, o := range order {
					if o == "extend-group" {
						t.Fatalf("order = %v, want no group write", order)
					}
				}
			}
		})
	}
}
