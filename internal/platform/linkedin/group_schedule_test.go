// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// groupScheduleServer is a stateful campaign group 555: GET serves its current end (or body,
// when set) and a PARTIAL_UPDATE applies the nested runSchedule.end it carries. afterPatch runs
// after each applied write (another replica's write landing in between); patchStatus fails it.
type groupScheduleServer struct {
	mu          sync.Mutex
	body        string
	end         *int64
	getDelay    time.Duration
	patchStatus int
	patches     []map[string]any
	restliHdr   []string
	afterPatch  func(g *groupScheduleServer)
	// campaigns, when set, is the adCampaigns search page for the group (default: empty);
	// searchStatus fails that search; searches records each search's query.
	campaigns    string
	searchStatus int
	searches     []string
}

func i64(v int64) *int64 { return &v }

func (g *groupScheduleServer) currentEnd() *int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.end
}

func (g *groupScheduleServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/adAccounts/123456789/adCampaigns") {
			g.mu.Lock()
			g.searches = append(g.searches, r.URL.RawQuery)
			body, status := g.campaigns, g.searchStatus
			g.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			if body == "" {
				body = `{"elements":[],"metadata":{}}`
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/adAccounts/123456789/adCampaignGroups/555") {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			g.mu.Lock()
			body := g.body
			if body == "" {
				if g.end == nil {
					body = `{"id":555,"runSchedule":{"start":1000}}`
				} else {
					body = fmt.Sprintf(`{"id":555,"runSchedule":{"start":1000,"end":%d}}`, *g.end)
				}
			}
			delay := g.getDelay
			g.mu.Unlock()
			time.Sleep(delay)
			_, _ = io.WriteString(w, body)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(raw, &p)
		g.mu.Lock()
		g.patches = append(g.patches, p)
		g.restliHdr = append(g.restliHdr, r.Header.Get("X-Restli-Method"))
		if g.patchStatus != 0 {
			g.mu.Unlock()
			w.WriteHeader(g.patchStatus)
			return
		}
		if rs, ok := p["patch"].(map[string]any)["runSchedule"].(map[string]any); ok {
			if set, ok := rs["$set"].(map[string]any); ok {
				if e, ok := set["end"].(float64); ok {
					g.end = i64(int64(e))
				}
			}
		}
		hook := g.afterPatch
		g.mu.Unlock()
		if hook != nil {
			hook(g)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *groupScheduleServer) client(t *testing.T) *Client {
	return NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(g.start(t).URL), WithClock(fixedClock()))
}

// Every campaign of an event shares one LinkedIn campaign group, whose schedule its creating call
// set. A later campaign that ends after the group would be refused by LinkedIn, so the reused
// group's end is widened first, by a nested $set of runSchedule.end alone (the group's start,
// usually past, is never re-sent), and the end is never moved earlier.
func TestEnsureCampaignGroupCoversEnd(t *testing.T) {
	const endMs = int64(4_000_000_000_000)
	cases := []struct {
		name      string
		end       *int64
		body      string
		wantWrite bool
		wantErr   string
	}{
		{"ends before the campaign: extended", i64(3_000_000_000_000), "", true, ""},
		{"ends exactly at the campaign end: untouched", i64(endMs), "", false, ""},
		{"ends after the campaign: never shortened", i64(5_000_000_000_000), "", false, ""},
		{"open-ended: covers anything", nil, "", false, ""},
		{"no schedule at all: nothing to widen", nil, `{"id":555}`, false, ""},
		{"an end with no start is still widened, the start never being sent", nil, `{"id":555,"runSchedule":{"end":3000000000000}}`, true, ""},
		{"URN id form is accepted", nil, `{"id":"urn:li:sponsoredCampaignGroup:555","runSchedule":{"start":1000,"end":3000000000000}}`, true, ""},
		{"a campaign URN with the group's number: refused", nil, `{"id":"urn:li:sponsoredCampaign:555","runSchedule":{"start":1000,"end":5000000000000}}`, false, "does not describe the requested group"},
		{"another group's body: refused", nil, `{"id":556,"runSchedule":{"start":1000,"end":3000000000000}}`, false, "does not describe the requested group"},
		{"duplicate key: refused", nil, `{"id":555,"runSchedule":{"start":1000,"end":5000000000000,"end":3000000000000}}`, false, "read campaign group 555"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &groupScheduleServer{end: tc.end, body: tc.body}
			// A fixed body never reflects the write, so the verify read would see the old end:
			// let the first write stand by serving the widened group afterwards.
			if tc.body != "" && tc.wantWrite {
				g.afterPatch = func(g *groupScheduleServer) {
					g.mu.Lock()
					g.body = `{"id":555,"runSchedule":{"start":1000,"end":4000000000000}}`
					g.mu.Unlock()
				}
			}
			wrote, err := g.client(t).ensureCampaignGroupCoversEnd(context.Background(), "123456789", "555", endMs, false)
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
			want := map[string]any{"patch": map[string]any{"runSchedule": map[string]any{"$set": map[string]any{"end": float64(endMs)}}}}
			if !reflect.DeepEqual(g.patches[0], want) {
				t.Errorf("patch = %v, want %v (runSchedule.end alone, no start)", g.patches[0], want)
			}
		})
	}
}

func TestEnsureCampaignGroupCoversEnd_WriteFailureIsAnError(t *testing.T) {
	g := &groupScheduleServer{end: i64(3_000_000_000_000), patchStatus: http.StatusBadRequest}
	wrote, err := g.client(t).ensureCampaignGroupCoversEnd(context.Background(), "123456789", "555", 4_000_000_000_000, false)
	if err == nil || wrote {
		t.Fatalf("wrote=%v err=%v, want an error and no claim of a write", wrote, err)
	}
}

// Another replica's write from a stale read can put back a shorter end right after ours. The
// verify read catches it and widens again; a group that keeps being shortened is an error after
// groupScheduleAttempts writes, never a "covered".
func TestEnsureCampaignGroupCoversEnd_ReWidensAfterAConcurrentShortening(t *testing.T) {
	const endMs = int64(4_000_000_000_000)
	shorten := func(g *groupScheduleServer) {
		g.mu.Lock()
		g.end = i64(3_500_000_000_000)
		g.mu.Unlock()
	}

	g := &groupScheduleServer{end: i64(3_000_000_000_000)}
	n := 0
	g.afterPatch = func(g *groupScheduleServer) {
		if n++; n == 1 {
			shorten(g)
		}
	}
	wrote, err := g.client(t).ensureCampaignGroupCoversEnd(context.Background(), "123456789", "555", endMs, false)
	if err != nil || !wrote {
		t.Fatalf("wrote=%v err=%v, want a re-widen to succeed", wrote, err)
	}
	if len(g.patches) != 2 || *g.currentEnd() != endMs {
		t.Fatalf("writes=%d end=%d, want 2 writes and end %d", len(g.patches), *g.currentEnd(), endMs)
	}

	// Shortened after the first two writes only: the third sticks, and the read after it must
	// report the group covered, not exhaustion.
	third := &groupScheduleServer{end: i64(3_000_000_000_000)}
	m := 0
	third.afterPatch = func(g *groupScheduleServer) {
		if m++; m <= 2 {
			shorten(g)
		}
	}
	if wrote, err := third.client(t).ensureCampaignGroupCoversEnd(context.Background(), "123456789", "555", endMs, false); err != nil || !wrote {
		t.Fatalf("wrote=%v err=%v, want the third write verified as covering", wrote, err)
	}
	if len(third.patches) != groupScheduleAttempts || *third.currentEnd() != endMs {
		t.Fatalf("writes=%d end=%d, want %d writes and end %d", len(third.patches), *third.currentEnd(), groupScheduleAttempts, endMs)
	}

	always := &groupScheduleServer{end: i64(3_000_000_000_000), afterPatch: shorten}
	if _, err := always.client(t).ensureCampaignGroupCoversEnd(context.Background(), "123456789", "555", endMs, false); err == nil || !strings.Contains(err.Error(), "keeps shortening") {
		t.Fatalf("err = %v, want the bounded-attempts error", err)
	}
	if len(always.patches) != groupScheduleAttempts {
		t.Fatalf("writes = %d, want %d", len(always.patches), groupScheduleAttempts)
	}
}

// Two creates for one event in this process widen the shared group at once (one ending March, one
// February). Whatever the interleaving, the group must end up covering the LATER one: the shorter
// write must never land after the longer one.
func TestEnsureCampaignGroupCoversEnd_ConcurrentCallersKeepTheLaterEnd(t *testing.T) {
	const feb, mar = int64(3_800_000_000_000), int64(4_000_000_000_000)
	for round := 0; round < 20; round++ {
		g := &groupScheduleServer{end: i64(3_000_000_000_000), getDelay: 5 * time.Millisecond}
		srv := g.start(t)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for k, end := range []int64{mar, feb} {
			wg.Add(1)
			go func(k int, end int64) {
				defer wg.Done()
				c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
				_, errs[k] = c.ensureCampaignGroupCoversEnd(context.Background(), "123456789", "555", end, false)
			}(k, end)
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("round %d: errs = %v", round, errs)
		}
		if got := *g.currentEnd(); got != mar {
			t.Fatalf("round %d: group ends %d, want the later end %d (writes %v)", round, got, mar, g.patches)
		}
		// Serialized, the second caller reads the first's end: at most one write each, and never
		// a write from a stale read that the first caller then has to undo (which takes three).
		if len(g.patches) > 2 {
			t.Fatalf("round %d: %d writes %v, want at most 2: a caller wrote from a stale read", round, len(g.patches), g.patches)
		}
	}
}

// End to end: a reused group that ends before the new campaign is extended BEFORE the campaign
// create, and a group whose schedule cannot be read creates no campaign and returns NO result: the
// group pre-existed, so this call created nothing, and a partial would be persisted as an orphan
// the orchestrator refuses to retry.
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
			extended := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "adCampaignGroups/100"):
					if extended {
						_, _ = io.WriteString(w, `{"id":100,"runSchedule":{"start":1000}}`)
						return
					}
					_, _ = io.WriteString(w, tc.groupBody)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaignGroups"):
					_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | tlf","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:100"}],"metadata":{"nextPageToken":""}}`)
				case r.Method == http.MethodGet:
					_, _ = io.WriteString(w, `{"elements":[],"metadata":{"nextPageToken":""}}`)
				case strings.HasSuffix(r.URL.Path, "adCampaignGroups/100"):
					extended = true
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
				if res != nil {
					t.Fatalf("want no result (nothing was created), got %+v", res)
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
