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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// liveConn is a project's active connection to one ad platform, with readable credentials.
type liveConn struct{ conn model.Connection }

func (c liveConn) Get(context.Context, string, model.Provider) (*model.Connection, error) {
	cc := c.conn
	return &cc, nil
}

func (liveConn) Disconnected(context.Context, string, model.Provider) (bool, error) {
	return false, nil
}

// liveAdWire is a fake Reddit or Meta API that records the campaign names it is asked to create
// and mints a distinct campaign id per create.
type liveAdWire struct {
	mu        sync.Mutex
	campaigns []string
	// idBase keeps this test's campaign ids apart from any other run's in the shared schema,
	// where (platform, platform_campaign_id[, account]) is unique.
	idBase int64
}

func (w *liveAdWire) names() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.campaigns...)
}

func (w *liveAdWire) record(name string) int64 {
	w.campaigns = append(w.campaigns, name)
	return w.idBase + int64(len(w.campaigns))
}

func (w *liveAdWire) redditDispatcher(t *testing.T) service.PlatformDispatcher {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		defer w.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			var req struct {
				Data struct {
					Name string `json:"name"`
				} `json:"data"`
			}
			_ = json.Unmarshal(body, &req)
			_ = json.NewEncoder(rw).Encode(map[string]any{"data": map[string]any{"id": fmt.Sprintf("cmp_%d", w.record(req.Data.Name))}})
		case strings.Contains(r.URL.Path, "ad_groups"):
			_ = json.NewEncoder(rw).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			_ = json.NewEncoder(rw).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	conn := liveConn{model.Connection{
		Provider: model.ProviderRedditAds, AccountID: "t2_live", Status: model.StatusActive,
		EncryptedCredentials: []byte(`{"ClientID":"cid","ClientSecret":"sec","RefreshToken":"rt"}`),
		ProviderConfig:       map[string]string{"conversion_pixel_id": "t2_pixel"},
	}}
	return dispatch.NewRedditDispatcher(conn, livePlaintext{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }))
}

func (w *liveAdWire) metaDispatcher(t *testing.T) service.PlatformDispatcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rw.Header().Set("Content-Type", "application/json")
		w.mu.Lock()
		defer w.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(rw, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			var req struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(body, &req)
			_, _ = io.WriteString(rw, fmt.Sprintf(`{"id":"%d"}`, w.record(req.Name)))
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(rw, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(rw, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(rw, `{"id":"ad_1"}`)
		default:
			http.Error(rw, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	conn := liveConn{model.Connection{
		Provider: model.ProviderMetaAds, AccountID: "act_777", Status: model.StatusActive,
		EncryptedCredentials: []byte(`{"AccessToken":"tok"}`),
		ProviderConfig:       map[string]string{"page_id": "987654321"},
	}}
	return dispatch.NewMetaDispatcher(conn, livePlaintext{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
}

// TestLiveRedditMetaSecondCampaignThroughTheOrchestrator (cs-3; AC 10, 11 and 12 for Reddit and
// Meta) drives the real orchestrator over the real repositories and the REAL Reddit and Meta
// dispatchers, each against a fake API:
//  1. a labelled first create claims slot 1 and sends the 8-segment labelled name, no slot token;
//  2. a retry of slot 1 carrying a DIFFERENT stage is a reuse: nothing is sent, and the row keeps
//     its stage, tactic and exact name;
//  3. a new_version request that saw slot 1 creates slot 2 under a name slot 1 does not hold;
//  4. its replay is a reuse and sends nothing.
//
// Exactly two creates reach the platform and two campaigns are live.
func TestLiveRedditMetaSecondCampaignThroughTheOrchestrator(t *testing.T) {
	for _, tc := range []struct {
		provider model.Provider
		config   string
		disp     func(*liveAdWire, *testing.T) service.PlatformDispatcher
	}{
		{model.ProviderRedditAds,
			`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"]}}`,
			(*liveAdWire).redditDispatcher},
		{model.ProviderMetaAds,
			`{"metaConfig":{"budget":2500,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"],"currencyOffset":100,` +
				`"variants":[{"headline":"KubeCon 2099","primaryText":"Join us","description":"Cloud native event"}]}}`,
			(*liveAdWire).metaDispatcher},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			ctx := context.Background()
			pool := dbtest.Pool(t)
			pgPool := &postgres.Pool{Pool: pool}
			campaigns := postgres.NewCampaignRepo(pgPool)
			jobs := postgres.NewJobRepo(pgPool)
			// A short project slug: Reddit caps the Project segment at 40 runes, which the shared
			// helper's test-unique slugs exceed. Unique per run, because the package shares one
			// schema and campaign_briefs is unique on (project_id, event_slug).
			project := fmt.Sprintf("cs3-%s-%x", tc.provider[:4], time.Now().UnixNano())
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
			wire := &liveAdWire{idBase: 120_000_000_000_000 + time.Now().UnixNano()%1_000_000_000*100}
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
			labels := func(s model.FunnelStage, tc model.Tactic) model.CampaignLabels {
				return model.CampaignLabels{FunnelStage: liveStage(s), Tactic: liveTactic(tc)}
			}
			slot1 := "Events | KubeCon NA 2026 | NA | Traffic | Intent prospecting | Social | " + project + " | ToFU"
			slot2 := "Events | KubeCon NA 2026 | NA | Traffic | Intent retargeting #2 | Social | " + project + " | MoFU"

			// 1. The first, labelled campaign.
			first := run(service.StartOptions{Labels: labels(model.FunnelStageTop, model.TacticProspecting)})
			if first["ok"] != true || first["slot_version"] != float64(1) || first["reused"] == true || first["funnel_stage"] != "top" {
				t.Fatalf("first create = %v, want slot 1 created, labelled top", first)
			}
			// 2. A retry of slot 1 with a different stage.
			retry := run(service.StartOptions{Labels: labels(model.FunnelStageLower, model.TacticKeyword)})
			if retry["slot_version"] != float64(1) || retry["reused"] != true || retry["funnel_stage"] != "top" || retry["tactic"] != "prospecting" {
				t.Fatalf("retry = %v, want slot 1 reused with its stored top/prospecting", retry)
			}
			// 3. The deliberate second campaign, and 4. its replay.
			second := run(service.StartOptions{NewVersion: true, ExpectedSlotVersions: map[model.Provider]int{p: 1}, Labels: labels(model.FunnelStageMid, model.TacticRetargeting)})
			if second["ok"] != true || second["slot_version"] != float64(2) || second["reused"] == true || second["funnel_stage"] != "mid" {
				t.Fatalf("new_version = %v, want slot 2 created, labelled mid", second)
			}
			replay := run(service.StartOptions{NewVersion: true, ExpectedSlotVersions: map[model.Provider]int{p: 1}, Labels: labels(model.FunnelStageMid, model.TacticRetargeting)})
			if replay["slot_version"] != float64(2) || replay["reused"] != true {
				t.Fatalf("replay = %v, want slot 2 returned with reused=true", replay)
			}

			if got, want := wire.names(), []string{slot1, slot2}; strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("campaign creates sent = %q, want exactly %q", got, want)
			}
			if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 2 {
				t.Fatalf("live campaigns = %d, want 2", live)
			}
			for slot, want := range map[int]string{1: slot1, 2: slot2} {
				var name, stage string
				if err := pool.QueryRow(ctx,
					`SELECT campaign_name, COALESCE(funnel_stage, '') FROM campaigns WHERE brief_id=$1 AND platform=$2 AND slot_version=$3 AND status <> 'deleted'`,
					briefID, string(p), slot).Scan(&name, &stage); err != nil {
					t.Fatalf("read slot %d: %v", slot, err)
				}
				if name != want {
					t.Errorf("slot %d stored name %q, want %q", slot, name, want)
				}
				if strings.Count(name, " | ") != 7 {
					t.Errorf("slot %d stored name %q is not 8 segments", slot, name)
				}
				if wantStage := map[int]string{1: "top", 2: "mid"}[slot]; stage != wantStage {
					t.Errorf("slot %d stored stage %q, want %q", slot, stage, wantStage)
				}
			}
		})
	}
}
