// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

func liveStage(s model.FunnelStage) *model.FunnelStage { return &s }
func liveTactic(t model.Tactic) *model.Tactic          { return &t }

// TestLiveClaimWritesLabelsOnlyOnInsert (migration 000054): the claim INSERT writes funnel_stage
// and tactic; a retry claim of the same slot version carrying DIFFERENT labels conflicts and
// hands back the row with its STORED labels; and the upsert that finalizes the claim leaves them
// alone. Labels are immutable after the claim.
func TestLiveClaimWritesLabelsOnlyOnInsert(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)
	p := model.ProviderMicrosoftAds
	jobID := uuid.NewString()

	claimed, row, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p, model.VariantDefault, model.FirstSlotVersion, jobID, nil,
		model.CampaignLabels{FunnelStage: liveStage(model.FunnelStageTop), Tactic: liveTactic(model.TacticProspecting)})
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if row.FunnelStage == nil || *row.FunnelStage != model.FunnelStageTop || row.Tactic == nil || *row.Tactic != model.TacticProspecting {
		t.Fatalf("claimed row labels = %v/%v, want top/prospecting", row.FunnelStage, row.Tactic)
	}

	retried, again, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p, model.VariantDefault, model.FirstSlotVersion, uuid.NewString(), nil,
		model.CampaignLabels{FunnelStage: liveStage(model.FunnelStageLower), Tactic: liveTactic(model.TacticKeyword)})
	if err != nil || retried {
		t.Fatalf("retry claim: claimed=%v err=%v, want the existing row handed back", retried, err)
	}
	if again.FunnelStage == nil || *again.FunnelStage != model.FunnelStageTop || again.Tactic == nil || *again.Tactic != model.TacticProspecting {
		t.Fatalf("retry handed back labels %v/%v, want the stored top/prospecting", again.FunnelStage, again.Tactic)
	}

	done, err := repo.UpsertCampaign(ctx, &model.Campaign{
		ProjectID: project, BriefID: briefID, JobID: &jobID, Platform: p, Variant: model.VariantDefault,
		SlotVersion: model.FirstSlotVersion, CampaignName: dbtest.UniqueID(t, "campaign"),
		Status: model.CampaignStatusCreated, PlatformCampaignID: dbtest.UniqueID(t, "upstream"),
		// Different labels on the upsert: its conflict arm must ignore them.
		FunnelStage: liveStage(model.FunnelStageLower), Tactic: liveTactic(model.TacticKeyword),
	}, nil)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if done.FunnelStage == nil || *done.FunnelStage != model.FunnelStageTop || done.Tactic == nil || *done.Tactic != model.TacticProspecting {
		t.Fatalf("finalized row labels = %v/%v, want top/prospecting kept by the upsert", done.FunnelStage, done.Tactic)
	}

	unlabelled := createSlotVersion(ctx, t, repo, project, briefID, p, 2)
	if unlabelled.FunnelStage != nil || unlabelled.Tactic != nil {
		t.Fatalf("an unlabelled claim stored %v/%v, want NULL (legacy)", unlabelled.FunnelStage, unlabelled.Tactic)
	}
}

// TestLiveUpsertInsertArmKeepsTheClaimedLabels: when a labelled claim is cleared mid-flight and
// the slot is left empty, the dispatch's upsert takes its INSERT arm. The orchestrator hands it
// the claimed row's labels, and the recreated row must keep them — its upstream name was composed
// from that stage.
func TestLiveUpsertInsertArmKeepsTheClaimedLabels(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)
	p := model.ProviderMicrosoftAds
	jobID := uuid.NewString()
	labels := model.CampaignLabels{FunnelStage: liveStage(model.FunnelStageMid), Tactic: liveTactic(model.TacticCompetitor)}

	if claimed, _, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p, model.VariantDefault, model.FirstSlotVersion, jobID, nil, labels); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	// The operator clears the stuck claim; the slot is now empty.
	if err := repo.DeleteDispatchClaim(ctx, briefID, p, model.VariantDefault, model.FirstSlotVersion, jobID); err != nil {
		t.Fatalf("clear claim: %v", err)
	}
	row, err := repo.UpsertCampaign(ctx, &model.Campaign{
		ProjectID: project, BriefID: briefID, JobID: &jobID, Platform: p, Variant: model.VariantDefault,
		SlotVersion: model.FirstSlotVersion, CampaignName: dbtest.UniqueID(t, "campaign"),
		Status: model.CampaignStatusCreated, PlatformCampaignID: dbtest.UniqueID(t, "upstream"),
		FunnelStage: labels.FunnelStage, Tactic: labels.Tactic,
	}, nil)
	if err != nil {
		t.Fatalf("upsert into the empty slot: %v", err)
	}
	if row.FunnelStage == nil || *row.FunnelStage != model.FunnelStageMid || row.Tactic == nil || *row.Tactic != model.TacticCompetitor {
		t.Fatalf("recreated row labels = %v/%v, want mid/competitor", row.FunnelStage, row.Tactic)
	}
}

// TestLiveCampaignLabelChecksMatchTheModel: every model.FunnelStage and model.Tactic satisfies
// 000054's CHECKs, and a value outside them is refused by the database itself (23514), so no
// writer can store a label the model does not know.
func TestLiveCampaignLabelChecksMatchTheModel(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)
	slot := 0
	claim := func(l model.CampaignLabels) error {
		slot++
		_, _, err := repo.ClaimCampaignDispatch(ctx, project, briefID, model.ProviderMicrosoftAds, model.VariantDefault, slot, uuid.NewString(), nil, l)
		return err
	}
	for _, s := range model.FunnelStages() {
		if err := claim(model.CampaignLabels{FunnelStage: liveStage(s)}); err != nil {
			t.Errorf("stage %q refused by the database: %v", s, err)
		}
	}
	for _, tc := range model.Tactics() {
		if err := claim(model.CampaignLabels{Tactic: liveTactic(tc)}); err != nil {
			t.Errorf("tactic %q refused by the database: %v", tc, err)
		}
	}
	for _, bad := range []model.CampaignLabels{
		{FunnelStage: liveStage("awareness")},
		{Tactic: liveTactic("retarget video tofu")},
	} {
		err := claim(bad)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("labels %v/%v: err = %v, want a CHECK violation (23514)", bad.FunnelStage, bad.Tactic, err)
		}
	}
}

// liveCreateDispatcher creates a campaign with a unique upstream id and counts its calls.
type liveCreateDispatcher struct {
	mu    sync.Mutex
	calls int
}

func (d *liveCreateDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return &model.Campaign{Platform: p, PlatformCampaignID: uuid.NewString(), CampaignName: uuid.NewString(), Status: model.CampaignStatusCreated}, nil
}

// TestLiveRepeatedNewVersionRequestsCreateExactlyOne (AC 10, cs part) drives the real
// orchestrator over the real repositories: one new_version request built from slot 1 creates
// slot 2, and its repeats — a double click after the job finished, a reload, a second tab, a late
// replay — each return slot 2 with reused=true. Exactly one dispatch and two live campaigns.
func TestLiveRepeatedNewVersionRequestsCreateExactlyOne(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	pgPool := &postgres.Pool{Pool: pool}
	campaigns := postgres.NewCampaignRepo(pgPool)
	jobs := postgres.NewJobRepo(pgPool)
	briefID, project := insertApprovedBrief(ctx, t, pool)
	p := model.ProviderMicrosoftAds
	createSlotVersion(ctx, t, campaigns, project, briefID, p, model.FirstSlotVersion)

	brief, err := postgres.NewBriefRepo(pgPool).GetBrief(ctx, project, briefID)
	if err != nil {
		t.Fatalf("get brief: %v", err)
	}
	disp := &liveCreateDispatcher{}
	orch := service.NewOrchestrator(campaigns, jobs, map[model.Provider]service.PlatformDispatcher{p: disp})
	t.Cleanup(func() { _ = orch.Shutdown(context.Background(), 5*time.Second) })

	run := func(expected int) map[string]any {
		t.Helper()
		id, err := orch.StartWithOptions(ctx, brief, brief.Version, []model.Provider{p}, nil, service.StartOptions{
			NewVersion: true, ExpectedSlotVersions: map[model.Provider]int{p: expected},
		})
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

	first := run(1)
	if first["slot_version"] != float64(2) || first["reused"] == true {
		t.Fatalf("first request = %v, want slot 2 created", first)
	}
	for _, repeat := range []string{"double click", "reload", "second tab", "late replay"} {
		res := run(1)
		if res["slot_version"] != float64(2) || res["reused"] != true {
			t.Fatalf("%s = %v, want slot 2 returned with reused=true", repeat, res)
		}
	}
	if disp.calls != 1 {
		t.Fatalf("dispatcher called %d times, want exactly 1", disp.calls)
	}
	if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 2 {
		t.Fatalf("live campaigns = %d, want 2 (slot 1 and the one deliberate slot 2)", live)
	}
}
