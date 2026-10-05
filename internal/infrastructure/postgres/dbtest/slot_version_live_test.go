// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// TestLiveSlotVersionDuringExpandPhase drives the real claim/upsert/read methods against the
// schema this release ships: 000037's four-column index AND 000022's three-column one.
//
// It pins the three things the expand phase promises:
//   - a retry of slot 1 still conflicts on the four-column arbiter and is swallowed, exactly
//     as before;
//   - a claim for slot 2 is REFUSED as ErrSlotVersionUnavailable (the three-column index raises
//     23505 because it is not the arbiter) and writes nothing;
//   - slot_version is not the optimistic-concurrency `version`: the upsert bumps version and
//     leaves slot_version alone. The two share a word, and an earlier draft of 000036 added
//     "version" with IF NOT EXISTS — a silent no-op on the existing counter.
func TestLiveSlotVersionDuringExpandPhase(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	briefID, project := insertApprovedBrief(ctx, t, pool)
	jobID := uuid.NewString()

	claimed, row, err := repo.ClaimCampaignDispatch(ctx, project, briefID, model.ProviderGoogleAds,
		model.VariantDefault, model.FirstSlotVersion, jobID, nil)
	if err != nil {
		t.Fatalf("claim slot 1: %v", err)
	}
	if !claimed || row.SlotVersion != model.FirstSlotVersion {
		t.Fatalf("claim slot 1: claimed=%v slot_version=%d, want claimed=true slot_version=1", claimed, row.SlotVersion)
	}

	created, err := repo.UpsertCampaign(ctx, &model.Campaign{
		ProjectID: project, BriefID: briefID, JobID: &jobID,
		Platform: model.ProviderGoogleAds, Variant: model.VariantDefault, SlotVersion: model.FirstSlotVersion,
		CampaignName: dbtest.UniqueID(t, "campaign"), Status: "created",
		PlatformCampaignID: dbtest.UniqueID(t, "upstream"),
	}, nil)
	if err != nil {
		t.Fatalf("upsert slot 1: %v", err)
	}
	if created.SlotVersion != model.FirstSlotVersion || created.Version < 2 {
		t.Fatalf("after upsert: slot_version=%d version=%d, want slot_version=1 and version bumped past 1 "+
			"(the upsert's conflict arm bumps the concurrency counter, never the slot version)",
			created.SlotVersion, created.Version)
	}

	retried, row, err := repo.ClaimCampaignDispatch(ctx, project, briefID, model.ProviderGoogleAds,
		model.VariantDefault, model.FirstSlotVersion, uuid.NewString(), nil)
	if err != nil {
		t.Fatalf("retry claim of slot 1: %v", err)
	}
	if retried || row.ID != created.ID {
		t.Fatalf("retry claim of slot 1: claimed=%v row=%s, want the existing campaign %s handed back", retried, row.ID, created.ID)
	}

	claimed, _, err = repo.ClaimCampaignDispatch(ctx, project, briefID, model.ProviderGoogleAds,
		model.VariantDefault, 2, uuid.NewString(), nil)
	if !errors.Is(err, domain.ErrSlotVersionUnavailable) {
		t.Fatalf("claim slot 2 while 000022's index exists: err=%v, want ErrSlotVersionUnavailable", err)
	}
	if claimed {
		t.Fatal("claim slot 2 reported claimed alongside an error")
	}

	var live int
	if qerr := pool.QueryRow(ctx,
		`SELECT count(*) FROM campaigns WHERE brief_id=$1 AND status <> 'deleted'`, briefID).Scan(&live); qerr != nil {
		t.Fatalf("count live rows: %v", qerr)
	}
	if live != 1 {
		t.Fatalf("live campaigns for the brief = %d, want 1: the refused slot-2 claim must write nothing", live)
	}

	latest, err := repo.GetCampaignByPlatform(ctx, project, briefID, model.ProviderGoogleAds, model.VariantDefault)
	if err != nil {
		t.Fatalf("GetCampaignByPlatform: %v", err)
	}
	if latest.ID != created.ID {
		t.Fatalf("GetCampaignByPlatform = %s, want slot 1's campaign %s", latest.ID, created.ID)
	}
}

// TestLiveConcurrentSlot1ClaimsHaveOneWinnerAndNoError pins the race the expand phase opened.
// The claim names the four-column index as its arbiter, and Postgres pre-checks only the arbiter,
// so concurrent slot-1 claims can all pass it; every loser then hits 23505 on 000022's legacy
// index once the winner commits. That must read as a lost claim (the winner's row), not as
// ErrSlotVersionUnavailable: a double-submitted first create is a skip or a reuse, never a
// "not available yet" failure.
func TestLiveConcurrentSlot1ClaimsHaveOneWinnerAndNoError(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)

	const n = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		errs    []error
		start   = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claimed, row, err := repo.ClaimCampaignDispatch(ctx, project, briefID, model.ProviderMicrosoftAds,
				model.VariantDefault, model.FirstSlotVersion, uuid.NewString(), nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				errs = append(errs, err)
			case claimed:
				winners++
			case row == nil || row.SlotVersion != model.FirstSlotVersion:
				errs = append(errs, errors.New("lost claim returned no slot-1 row"))
			}
		}()
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}
	if len(errs) > 0 {
		t.Errorf("%d of %d concurrent slot-1 claims errored, want 0; first: %v", len(errs), n, errs[0])
	}
}
