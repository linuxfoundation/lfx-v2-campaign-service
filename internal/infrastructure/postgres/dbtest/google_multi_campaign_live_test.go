// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/dispatch"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// liveGoogleConn is the project's Google Ads connection, with readable credentials.
type liveGoogleConn struct{}

func (liveGoogleConn) Get(context.Context, string, model.Provider) (*model.Connection, error) {
	return &model.Connection{
		Provider: model.ProviderGoogleAds, AccountID: "1234567890", Status: model.StatusActive,
		EncryptedCredentials: []byte(`{"ClientID":"cid","ClientSecret":"csec","DeveloperToken":"dev","RefreshToken":"rt"}`),
	}, nil
}

func (liveGoogleConn) Disconnected(context.Context, string, model.Provider) (bool, error) {
	return false, nil
}

type livePlaintext struct{}

func (livePlaintext) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (livePlaintext) Decrypt(c []byte) ([]byte, error) { return c, nil }

// liveGoogleWire is a fake Google Ads API that records the budget and campaign names it is asked
// to create and mints a distinct campaign id per create.
type liveGoogleWire struct {
	mu        sync.Mutex
	budgets   []string
	campaigns []string
	// idBase keeps this test's campaign ids apart from any other run's in the shared schema,
	// where (platform, platform_campaign_id) is unique.
	idBase int64
}

func (w *liveGoogleWire) names() (budgets, campaigns []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.budgets...), append([]string(nil), w.campaigns...)
}

func (w *liveGoogleWire) options(t *testing.T) []googleads.Option {
	t.Helper()
	createName := func(body []byte) string {
		var req struct {
			Operations []struct {
				Create struct {
					Name string `json:"name"`
				} `json:"create"`
			} `json:"operations"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.Operations) == 0 {
			return ""
		}
		return req.Operations[0].Create.Name
	}
	token := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(rw, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(token.Close)
	api := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rw.Header().Set("Content-Type", "application/json")
		w.mu.Lock()
		defer w.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			_, _ = io.WriteString(rw, `{"results":[]}`)
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			w.budgets = append(w.budgets, createName(body))
			_, _ = io.WriteString(rw, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/`+strconv.Itoa(100+len(w.budgets))+`"}]}`)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			w.campaigns = append(w.campaigns, createName(body))
			_, _ = io.WriteString(rw, `{"results":[{"resourceName":"customers/1234567890/campaigns/`+strconv.FormatInt(w.idBase+int64(len(w.campaigns)), 10)+`"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			_, _ = io.WriteString(rw, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(rw, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/333~444"}]}`)
		default:
			http.Error(rw, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	return []googleads.Option{googleads.WithTokenURL(token.URL), googleads.WithBaseURL(api.URL)}
}

// TestLiveGoogleSecondCampaignThroughTheOrchestrator (cs-2; AC 10, 11 and 12 for Google) drives
// the real orchestrator over the real repositories and the REAL Google Ads dispatcher, against a
// fake Google API:
//  1. a labelled first create claims slot 1 and sends the 7-segment names with the bare brief id;
//  2. a retry of slot 1 carrying a DIFFERENT stage is a reuse: nothing is sent, and the row keeps
//     its stage and its exact name;
//  3. a new_version request that saw slot 1 creates slot 2 under names slot 1 does not hold —
//     campaign and budget alike;
//  4. its replay is a reuse and sends nothing.
//
// Exactly two creates reach Google and two campaigns are live.
func TestLiveGoogleSecondCampaignThroughTheOrchestrator(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	pgPool := &postgres.Pool{Pool: pool}
	campaigns := postgres.NewCampaignRepo(pgPool)
	jobs := postgres.NewJobRepo(pgPool)
	briefID, project := insertApprovedBrief(ctx, t, pool)
	if _, err := pool.Exec(ctx, `UPDATE campaign_briefs SET event_details = $2 WHERE id = $1`, briefID,
		`{"eventName":"KubeCon NA 2026","registrationUrl":"https://events.example/kc"}`); err != nil {
		t.Fatalf("set event details: %v", err)
	}
	brief, err := postgres.NewBriefRepo(pgPool).GetBrief(ctx, project, briefID)
	if err != nil {
		t.Fatalf("get brief: %v", err)
	}
	p := model.ProviderGoogleAds
	wire := &liveGoogleWire{idBase: time.Now().UnixNano() % 1_000_000_000 * 100}
	disp := dispatch.NewGoogleAdsDispatcher(liveGoogleConn{}, livePlaintext{}, wire.options(t)...)
	orch := service.NewOrchestrator(campaigns, jobs, map[model.Provider]service.PlatformDispatcher{p: disp})
	t.Cleanup(func() { _ = orch.Shutdown(context.Background(), 5*time.Second) })

	run := func(opts service.StartOptions) map[string]any {
		t.Helper()
		id, err := orch.StartWithOptions(ctx, brief, brief.Version, []model.Provider{p}, json.RawMessage(`{"googleAdsConfig":{"budget":50}}`), opts)
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
	slot1Camp := "LFX | Search Campaign | " + project + " | KubeCon NA 2026 | ToFU | prospecting | " + briefID
	slot1Budget := "LFX | Budget | " + project + " | KubeCon NA 2026 | ToFU | prospecting | " + briefID
	slot2Camp := "LFX | Search Campaign | " + project + " | KubeCon NA 2026 | MoFU | retargeting | " + briefID + "-2"
	slot2Budget := "LFX | Budget | " + project + " | KubeCon NA 2026 | MoFU | retargeting | " + briefID + "-2"

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

	budgets, camps := wire.names()
	if want := []string{slot1Camp, slot2Camp}; strings.Join(camps, "\n") != strings.Join(want, "\n") {
		t.Errorf("campaign creates sent = %q, want exactly %q", camps, want)
	}
	if want := []string{slot1Budget, slot2Budget}; strings.Join(budgets, "\n") != strings.Join(want, "\n") {
		t.Errorf("budget creates sent = %q, want exactly %q (unique per slot)", budgets, want)
	}
	if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 2 {
		t.Fatalf("live campaigns = %d, want 2", live)
	}
	for slot, want := range map[int]string{1: slot1Camp, 2: slot2Camp} {
		var name, stage string
		if err := pool.QueryRow(ctx,
			`SELECT campaign_name, COALESCE(funnel_stage, '') FROM campaigns WHERE brief_id=$1 AND platform=$2 AND slot_version=$3 AND status <> 'deleted'`,
			briefID, string(p), slot).Scan(&name, &stage); err != nil {
			t.Fatalf("read slot %d: %v", slot, err)
		}
		if name != want {
			t.Errorf("slot %d stored name %q, want %q", slot, name, want)
		}
		if wantStage := map[int]string{1: "top", 2: "mid"}[slot]; stage != wantStage {
			t.Errorf("slot %d stored stage %q, want %q", slot, stage, wantStage)
		}
	}
}
