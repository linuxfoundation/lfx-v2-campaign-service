// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/dispatch"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// liveNamedWire is a STATEFUL fake LinkedIn or X API. Both clients reuse a campaign by its
// composed name on every create, so the fake remembers each campaign it creates and answers the
// by-name lookups with them, the way the platform does: a slot-2 name equal to slot 1's would be
// found and reused here, exactly as upstream.
type liveNamedWire struct {
	mu        sync.Mutex
	names     []string // campaign names created, in order; the campaign id is idBase+index+1
	children  []int64  // the campaign id each child (LinkedIn creative, X line item) was created under
	groups    []string
	lineItems map[string][]string // X: line item names by campaign id
	// idBase keeps this test's campaign ids apart from any other run's in the shared schema,
	// where (platform, platform_campaign_id[, account]) is unique.
	idBase int64
}

func (w *liveNamedWire) created() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.names...)
}

func (w *liveNamedWire) childCampaigns() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]int64(nil), w.children...)
}

func (w *liveNamedWire) linkedInDispatcher(t *testing.T) service.PlatformDispatcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rw.Header().Set("Content-Type", "application/json")
		w.mu.Lock()
		defer w.mu.Unlock()
		var req struct {
			Name     string `json:"name"`
			Campaign string `json:"campaign"`
		}
		_ = json.Unmarshal(body, &req)
		search := func(els []map[string]any) {
			_ = json.NewEncoder(rw).Encode(map[string]any{"elements": els, "metadata": map[string]any{}})
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaignGroups/"):
			// The single-group read before a reused group's campaign create: an open-ended
			// schedule covers every campaign, so no extension is written.
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_, _ = io.WriteString(rw, `{"id":`+id+`,"runSchedule":{"start":1}}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaignGroups"):
			els := []map[string]any{}
			for i, g := range w.groups {
				els = append(els, map[string]any{"id": json.Number(fmt.Sprint(100 + i)), "name": g, "status": "ACTIVE"})
			}
			search(els)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaigns"):
			els := []map[string]any{}
			for i, n := range w.names {
				els = append(els, map[string]any{"id": json.Number(fmt.Sprint(w.idBase + int64(i) + 1)), "name": n, "status": "PAUSED",
					"campaignGroup": "urn:li:sponsoredCampaignGroup:100"})
			}
			search(els)
		case r.Method == http.MethodGet:
			search([]map[string]any{})
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			w.groups = append(w.groups, req.Name)
			_, _ = io.WriteString(rw, fmt.Sprintf(`{"id":"urn:li:sponsoredCampaignGroup:%d"}`, 99+len(w.groups)))
		case strings.Contains(r.URL.Path, "adCampaigns"):
			w.names = append(w.names, req.Name)
			rw.Header().Set("x-restli-id", fmt.Sprintf("urn:li:sponsoredCampaign:%d", w.idBase+int64(len(w.names))))
			_, _ = io.WriteString(rw, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(rw, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			var id int64
			_, _ = fmt.Sscanf(req.Campaign, "urn:li:sponsoredCampaign:%d", &id)
			w.children = append(w.children, id)
			_, _ = io.WriteString(rw, fmt.Sprintf(`{"id":"urn:li:sponsoredCreative:%d"}`, 400+len(w.children)))
		default:
			http.Error(rw, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	conn := liveConn{model.Connection{
		Provider: model.ProviderLinkedInAds, AccountID: "123456789", Status: model.StatusActive,
		EncryptedCredentials: []byte(`{"AccessToken":"tok"}`),
		ProviderConfig:       map[string]string{"org_id": "987654321"},
	}}
	return dispatch.NewLinkedInDispatcher(conn, livePlaintext{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
}

func (w *liveNamedWire) twitterDispatcher(t *testing.T) service.PlatformDispatcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.lineItems == nil {
			w.lineItems = map[string][]string{}
		}
		q := r.URL.Query()
		list := func(els []map[string]any) { _ = json.NewEncoder(rw).Encode(map[string]any{"data": els}) }
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = io.WriteString(rw, `{"data":{"name":"LF Events"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/campaigns"):
			els := []map[string]any{}
			for i, n := range w.names {
				els = append(els, map[string]any{"id": fmt.Sprint(w.idBase + int64(i) + 1), "name": n, "funding_instrument_id": "fi1"})
			}
			list(els)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/line_items"):
			parent := q.Get("campaign_ids")
			els := []map[string]any{}
			for i, n := range w.lineItems[parent] {
				els = append(els, map[string]any{"id": fmt.Sprintf("li_%s_%d", parent, i), "name": n, "campaign_id": parent})
			}
			list(els)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			w.names = append(w.names, q.Get("name"))
			_, _ = io.WriteString(rw, fmt.Sprintf(`{"data":{"id":"%d"}}`, w.idBase+int64(len(w.names))))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/line_items"):
			parent := q.Get("campaign_id")
			var id int64
			_, _ = fmt.Sscan(parent, &id)
			w.children = append(w.children, id)
			w.lineItems[parent] = append(w.lineItems[parent], q.Get("name"))
			_, _ = io.WriteString(rw, fmt.Sprintf(`{"data":{"id":"li_%s_%d"}}`, parent, len(w.lineItems[parent])-1))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/promoted_tweets"):
			_, _ = io.WriteString(rw, `{"data":[{"id":"pt1"}]}`)
		default:
			http.Error(rw, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	conn := liveConn{model.Connection{
		Provider: model.ProviderTwitterAds, AccountID: "acc1", Status: model.StatusActive,
		EncryptedCredentials: []byte(`{"ConsumerKey":"ck","ConsumerSecret":"cs","AccessToken":"at","AccessTokenSecret":"ats"}`),
		ProviderConfig:       map[string]string{"funding_instrument_id": "fi1"},
	}}
	return dispatch.NewTwitterDispatcher(conn, livePlaintext{}, twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
}

// TestLiveLinkedInXSecondCampaignThroughTheOrchestrator (cs-4; AC 10, 11 and 12 for LinkedIn and
// X) drives the real orchestrator over the real repositories and the REAL LinkedIn and X
// dispatchers, each against a STATEFUL fake API whose by-name lookups see every campaign it has
// created:
//  1. a labelled first create claims slot 1 and sends the 8-segment labelled name, no slot token;
//  2. a retry of slot 1 carrying a DIFFERENT stage is a reuse: nothing is sent, and the row keeps
//     its stage, tactic and exact name;
//  3. a new_version request that saw slot 1 creates slot 2 under a name slot 1 does not hold, so
//     the client's by-name lookup does not find slot 1's live campaign: slot 2 gets its own
//     upstream campaign and its children (LinkedIn creative, X line item) go onto it;
//  4. its replay is a reuse and sends nothing.
//
// Exactly two creates reach the platform and two campaigns are live. It runs labelled and
// unlabelled; unlabelled, only the "#2" slot token keeps slot 2 from finding slot 1 by name.
func TestLiveLinkedInXSecondCampaignThroughTheOrchestrator(t *testing.T) {
	labels := func(s model.FunnelStage, tc model.Tactic) model.CampaignLabels {
		return model.CampaignLabels{FunnelStage: liveStage(s), Tactic: liveTactic(tc)}
	}
	for _, tc := range []struct {
		provider   model.Provider
		config     string
		disp       func(*liveNamedWire, *testing.T) service.PlatformDispatcher
		head, tail string
	}{
		{model.ProviderLinkedInAds,
			`{"linkedInConfig":{"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",` +
				`"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],"targetingProfile":"cloud-native",` +
				`"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"]}],` +
				`"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]}}`,
			(*liveNamedWire).linkedInDispatcher,
			"Events | KubeCon NA 2026 | LinkedIn | Traffic | ", " | Static | "},
		{model.ProviderTwitterAds,
			`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890"}}`,
			(*liveNamedWire).twitterDispatcher,
			"Events | KubeCon NA 2026 | Global | Awareness | ", " | Promoted Post | "},
	} {
		// Labelled, where slot 1 and slot 2 also differ by stage and tactic; and unlabelled,
		// where ONLY the "#2" slot token separates slot 2's name from slot 1's live campaign.
		for _, sc := range []struct {
			name             string
			slot1, slot2     model.CampaignLabels
			seg1, seg2       string // segment 5 of each slot
			funnel1, funnel2 string
			stage1, stage2   string
		}{
			{"labelled", labels(model.FunnelStageTop, model.TacticProspecting), labels(model.FunnelStageMid, model.TacticRetargeting),
				"Prospecting prospecting", "Prospecting retargeting #2", "ToFU", "MoFU", "top", "mid"},
			{"unlabelled", model.CampaignLabels{}, model.CampaignLabels{}, "Prospecting", "Prospecting #2", "MoFU", "MoFU", "", ""},
		} {
			t.Run(string(tc.provider)+"/"+sc.name, func(t *testing.T) {
				ctx := context.Background()
				pool := dbtest.Pool(t)
				pgPool := &postgres.Pool{Pool: pool}
				campaigns := postgres.NewCampaignRepo(pgPool)
				jobs := postgres.NewJobRepo(pgPool)
				// Unique per run, because the package shares one schema and campaign_briefs is unique
				// on (project_id, event_slug).
				project := fmt.Sprintf("cs4-%s-%x", tc.provider[:4], time.Now().UnixNano())
				var briefID string
				if err := pool.QueryRow(ctx, `
				INSERT INTO campaign_briefs (project_id, program_type, event_slug, status)
				VALUES ($1, 'events', $1, 'approved')
				RETURNING id`, project).Scan(&briefID); err != nil {
					t.Fatalf("insert approved brief: %v", err)
				}
				if _, err := pool.Exec(ctx, `UPDATE campaign_briefs SET event_details = $2 WHERE id = $1`, briefID,
					`{"eventName":"KubeCon NA 2026","registrationUrl":"https://events.example/kc"}`); err != nil {
					t.Fatalf("set event details: %v", err)
				}
				brief, err := postgres.NewBriefRepo(pgPool).GetBrief(ctx, project, briefID)
				if err != nil {
					t.Fatalf("get brief: %v", err)
				}
				p := tc.provider
				wire := &liveNamedWire{idBase: 130_000_000_000_000 + time.Now().UnixNano()%1_000_000_000*100}
				orch := service.NewOrchestrator(campaigns, jobs, map[model.Provider]service.PlatformDispatcher{p: tc.disp(wire, t)})
				t.Cleanup(func() { _ = orch.Shutdown(context.Background(), 5*time.Second) })

				run := func(opts service.StartOptions) map[string]any {
					t.Helper()
					id, err := orch.StartWithOptions(ctx, brief, brief.Version, []model.Provider{p}, json.RawMessage(tc.config), opts)
					if err != nil {
						t.Fatalf("start: %v", err)
					}
					deadline := time.Now().Add(10 * time.Second)
					for time.Now().Before(deadline) {
						j, err := jobs.GetJob(ctx, project, id)
						if err == nil && j.Status.Terminal() {
							var res []map[string]any
							if err := json.Unmarshal(j.Result, &res); err != nil || len(res) != 1 {
								t.Fatalf("decode %s: %v", j.Result, err)
							}
							return res[0]
						}
						time.Sleep(20 * time.Millisecond)
					}
					t.Fatal("job did not finish")
					return nil
				}
				slot1 := tc.head + sc.seg1 + tc.tail + project + " | " + sc.funnel1
				slot2 := tc.head + sc.seg2 + tc.tail + project + " | " + sc.funnel2

				// 1. The first, labelled campaign.
				first := run(service.StartOptions{Labels: sc.slot1})
				if first["ok"] != true || first["slot_version"] != float64(1) || first["reused"] == true || str(first["funnel_stage"]) != sc.stage1 {
					t.Fatalf("first create = %v, want slot 1 created, stage %q", first, sc.stage1)
				}
				// 2. A retry of slot 1 with a different stage.
				retry := run(service.StartOptions{Labels: labels(model.FunnelStageLower, model.TacticKeyword)})
				if retry["slot_version"] != float64(1) || retry["reused"] != true || str(retry["funnel_stage"]) != sc.stage1 {
					t.Fatalf("retry = %v, want slot 1 reused with its stored stage %q", retry, sc.stage1)
				}
				// 3. The deliberate second campaign, and 4. its replay.
				second := run(service.StartOptions{NewVersion: true, ExpectedSlotVersions: map[model.Provider]int{p: 1}, Labels: sc.slot2})
				if second["ok"] != true || second["slot_version"] != float64(2) || second["reused"] == true || str(second["funnel_stage"]) != sc.stage2 {
					t.Fatalf("new_version = %v, want slot 2 created, stage %q", second, sc.stage2)
				}
				replay := run(service.StartOptions{NewVersion: true, ExpectedSlotVersions: map[model.Provider]int{p: 1}, Labels: sc.slot2})
				if replay["slot_version"] != float64(2) || replay["reused"] != true {
					t.Fatalf("replay = %v, want slot 2 returned with reused=true", replay)
				}

				if got, want := wire.created(), []string{slot1, slot2}; strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("campaign creates sent = %q, want exactly %q", got, want)
				}
				if got, want := wire.childCampaigns(), []int64{wire.idBase + 1, wire.idBase + 2}; fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("children created under campaigns %v, want one per slot under its own campaign %v", got, want)
				}
				if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 2 {
					t.Fatalf("live campaigns = %d, want 2", live)
				}
				ids := map[string]bool{}
				for slot, want := range map[int]string{1: slot1, 2: slot2} {
					var name, stage, upstream string
					if err := pool.QueryRow(ctx,
						`SELECT campaign_name, COALESCE(funnel_stage, ''), COALESCE(platform_campaign_id, '') FROM campaigns WHERE brief_id=$1 AND platform=$2 AND slot_version=$3 AND status <> 'deleted'`,
						briefID, string(p), slot).Scan(&name, &stage, &upstream); err != nil {
						t.Fatalf("read slot %d: %v", slot, err)
					}
					if name != want {
						t.Errorf("slot %d stored name %q, want %q", slot, name, want)
					}
					if strings.Count(name, " | ") != 7 {
						t.Errorf("slot %d stored name %q is not 8 segments", slot, name)
					}
					if wantStage := map[int]string{1: sc.stage1, 2: sc.stage2}[slot]; stage != wantStage {
						t.Errorf("slot %d stored stage %q, want %q", slot, stage, wantStage)
					}
					ids[upstream] = true
				}
				if len(ids) != 2 || ids[""] {
					t.Errorf("stored upstream campaign ids %v, want two distinct ids (slot 2 bound to its own campaign)", ids)
				}
			})
		}
	}
}

// str reads an optional string field of a decoded job result ("" when absent).
func str(v any) string {
	s, _ := v.(string)
	return s
}
