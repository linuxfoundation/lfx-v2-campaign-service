// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// slotRow is the stored state of one campaign row that a stale upsert must not touch.
type slotRow struct {
	platformCampaignID *string
	jobID              *string
	status             string
	version            int64
}

func readSlotRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) slotRow {
	t.Helper()
	var r slotRow
	if err := pool.QueryRow(ctx,
		`SELECT platform_campaign_id, job_id::text, status, version FROM campaigns WHERE id = $1`, id).
		Scan(&r.platformCampaignID, &r.jobID, &r.status, &r.version); err != nil {
		t.Fatalf("read campaign %s: %v", id, err)
	}
	return r
}

// staleDispatchUpsert is the upsert dispatchPlatform makes after its provider call returns,
// stamped with the job whose claim was cleared while that call was in flight.
func staleDispatchUpsert(ctx context.Context, t *testing.T, repo *postgres.CampaignRepo, project, briefID string, p model.Provider, jobID, upstream string) error {
	t.Helper()
	_, err := repo.UpsertCampaign(ctx, &model.Campaign{
		ProjectID: project, BriefID: briefID, JobID: &jobID,
		Platform: p, Variant: model.VariantDefault, SlotVersion: model.FirstSlotVersion,
		CampaignName: dbtest.UniqueID(t, "campaign"), Status: model.CampaignStatusCreated,
		PlatformCampaignID: upstream,
	}, nil)
	return err
}

// TestLiveStaleDispatchUpsertNeverOverwritesAnAdoption pins PR #260's review thread: claim →
// operator clears the pending claim while the provider call is in flight → an adopt binds the
// now-empty slot → the stale dispatch's upsert arrives. Before the conflict arm was restricted
// to the claim this job owns, that upsert DO UPDATEd the adopted row with its own upstream id,
// silently untracking the adopted (still spending) campaign — the repoint AdoptCampaign exists
// to refuse. It must now write nothing and report ErrDispatchClaimLost naming its upstream id.
func TestLiveStaleDispatchUpsertNeverOverwritesAnAdoption(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)
	p := model.ProviderMicrosoftAds
	staleJob := uuid.NewString()

	claimed, _, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p, model.VariantDefault, model.FirstSlotVersion, staleJob, nil)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v, want the claim won", claimed, err)
	}
	// The operator clears what looks like a stuck claim (StuckDispatchClaims → delete).
	if err := repo.ClearDispatchClaim(ctx, briefID, p, model.VariantDefault, model.FirstSlotVersion); err != nil {
		t.Fatalf("clear the pending claim: %v", err)
	}
	adopted, err := adoptInto(ctx, t, repo, project, briefID, p)
	if err != nil {
		t.Fatalf("adopt into the cleared slot: %v", err)
	}
	before := readSlotRow(ctx, t, pool, adopted.ID)

	staleUpstream := dbtest.UniqueID(t, "stale-upstream")
	err = staleDispatchUpsert(ctx, t, repo, project, briefID, p, staleJob, staleUpstream)
	if !errors.Is(err, domain.ErrDispatchClaimLost) {
		t.Fatalf("stale upsert over the adopted row: err = %v, want ErrDispatchClaimLost", err)
	}
	if !strings.Contains(err.Error(), staleUpstream) {
		t.Errorf("lost-claim error %q does not name the untracked upstream id %s", err, staleUpstream)
	}

	after := readSlotRow(ctx, t, pool, adopted.ID)
	if after.platformCampaignID == nil || *after.platformCampaignID != adopted.PlatformCampaignID {
		t.Fatalf("adopted row's platform_campaign_id = %v, want %s: the stale dispatch repointed the adoption",
			after.platformCampaignID, adopted.PlatformCampaignID)
	}
	if after.version != before.version || after.status != before.status || after.jobID != nil {
		t.Fatalf("adopted row changed: before=%+v after=%+v (job_id must stay NULL)", before, after)
	}
	if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 1 {
		t.Fatalf("live rows on the slot = %d, want 1 (the adoption only)", live)
	}
}

// The same guard against the OTHER row that can take a cleared slot: a later dispatch's claim.
// The stale upsert must not finalize the new job's pending row with its own upstream id (the
// new job's own upsert would then overwrite it again, orphaning the stale one with nothing
// reporting it), and the new job must still finalize its own claim afterwards.
func TestLiveStaleDispatchUpsertNeverFinalizesAnotherJobsClaim(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)
	p := model.ProviderMicrosoftAds
	staleJob, newJob := uuid.NewString(), uuid.NewString()

	if claimed, _, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p, model.VariantDefault, model.FirstSlotVersion, staleJob, nil); err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	if err := repo.ClearDispatchClaim(ctx, briefID, p, model.VariantDefault, model.FirstSlotVersion); err != nil {
		t.Fatalf("clear the pending claim: %v", err)
	}
	claimed, newClaim, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p, model.VariantDefault, model.FirstSlotVersion, newJob, nil)
	if err != nil || !claimed {
		t.Fatalf("second claim: claimed=%v err=%v", claimed, err)
	}
	before := readSlotRow(ctx, t, pool, newClaim.ID)

	if err := staleDispatchUpsert(ctx, t, repo, project, briefID, p, staleJob, dbtest.UniqueID(t, "stale-upstream")); !errors.Is(err, domain.ErrDispatchClaimLost) {
		t.Fatalf("stale upsert over another job's claim: err = %v, want ErrDispatchClaimLost", err)
	}
	if after := readSlotRow(ctx, t, pool, newClaim.ID); after.version != before.version || after.platformCampaignID != nil || after.status != "pending" {
		t.Fatalf("the new job's claim changed under a stale upsert: before=%+v after=%+v", before, after)
	}

	ownUpstream := dbtest.UniqueID(t, "own-upstream")
	if err := staleDispatchUpsert(ctx, t, repo, project, briefID, p, newJob, ownUpstream); err != nil {
		t.Fatalf("the claim's owner finalizing its own row: %v", err)
	}
	if after := readSlotRow(ctx, t, pool, newClaim.ID); after.platformCampaignID == nil || *after.platformCampaignID != ownUpstream {
		t.Fatalf("owner's upsert did not finalize its claim in place: %+v", after)
	}
}

// TestLiveStaleClaimReleaseNeverDeletesASuccessorsClaim pins PR #260's stale-cleanup thread: job
// A's claim is cleared by an operator, job B claims the slot, and A's delayed release (its
// pre-create cleanup, or the claim's read-back rollback) then runs. Released by slot
// coordinates alone, it deleted B's pending row while B was creating upstream, so a third job
// could claim and pay for a duplicate. Keyed on A's job id, it must delete nothing; B's own
// release must still work, and a release naming no job is refused.
func TestLiveStaleClaimReleaseNeverDeletesASuccessorsClaim(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)
	p := model.ProviderMicrosoftAds
	jobA, jobB := uuid.NewString(), uuid.NewString()

	if claimed, _, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p, model.VariantDefault, model.FirstSlotVersion, jobA, nil); err != nil || !claimed {
		t.Fatalf("A's claim: claimed=%v err=%v", claimed, err)
	}
	if err := repo.ClearDispatchClaim(ctx, briefID, p, model.VariantDefault, model.FirstSlotVersion); err != nil {
		t.Fatalf("operator clear of A's claim: %v", err)
	}
	claimed, bClaim, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p, model.VariantDefault, model.FirstSlotVersion, jobB, nil)
	if err != nil || !claimed {
		t.Fatalf("B's claim: claimed=%v err=%v", claimed, err)
	}
	before := readSlotRow(ctx, t, pool, bClaim.ID)

	if err := repo.DeleteDispatchClaim(ctx, briefID, p, model.VariantDefault, model.FirstSlotVersion, jobA); err != nil {
		t.Fatalf("A's stale release: %v", err)
	}
	if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 1 {
		t.Fatalf("live rows after A's stale release = %d, want 1: A deleted B's in-flight claim", live)
	}
	if after := readSlotRow(ctx, t, pool, bClaim.ID); after.version != before.version || after.jobID == nil || *after.jobID != jobB {
		t.Fatalf("B's claim changed under A's stale release: before=%+v after=%+v", before, after)
	}

	if err := repo.DeleteDispatchClaim(ctx, briefID, p, model.VariantDefault, model.FirstSlotVersion, ""); err == nil {
		t.Fatal("a release naming no job was accepted; it identifies no claim and must be refused")
	}
	if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 1 {
		t.Fatalf("live rows after the refused job-less release = %d, want 1", live)
	}

	if err := repo.DeleteDispatchClaim(ctx, briefID, p, model.VariantDefault, model.FirstSlotVersion, jobB); err != nil {
		t.Fatalf("B's own release: %v", err)
	}
	if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 0 {
		t.Fatalf("live rows after B's own release = %d, want 0", live)
	}
}

// TestLiveStaleSlot2UpsertAfterAnAdoptionIsALostClaim pins PR #260's slot-2 thread. An adopt
// always writes slot_version 1, so after a cleared slot-2 claim it does not take the stale
// upsert's four-column arbiter, and with 000022's three-column index dropped (000050) no index
// stops the upsert's INSERT arm either. UpsertCampaign's ownership check must report the same
// lost claim the conflict arm reports — the upstream id named, nothing written, the adoption
// untouched — not an ordinary persist failure that says the (cleared) claim was retained.
func TestLiveStaleSlot2UpsertAfterAnAdoptionIsALostClaim(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)
	p := model.ProviderMicrosoftAds

	// Slot 1 completed and then soft-deleted, so the slot is EMPTY once the slot-2 claim is
	// cleared and the adopt (which binds only an empty slot) is admitted.
	first := createSlotVersion(ctx, t, repo, project, briefID, p, model.FirstSlotVersion)
	if _, err := pool.Exec(ctx, `UPDATE campaigns SET status = 'deleted' WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("soft-delete slot 1: %v", err)
	}
	staleJob := uuid.NewString()
	if claimed, _, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p, model.VariantDefault, 2, staleJob, nil); err != nil || !claimed {
		t.Fatalf("slot-2 claim: claimed=%v err=%v", claimed, err)
	}
	if err := repo.ClearDispatchClaim(ctx, briefID, p, model.VariantDefault, 2); err != nil {
		t.Fatalf("operator clear of the slot-2 claim: %v", err)
	}
	adopted, err := adoptInto(ctx, t, repo, project, briefID, p)
	if err != nil {
		t.Fatalf("adopt into the emptied slot: %v", err)
	}
	if adopted.SlotVersion != model.FirstSlotVersion {
		t.Fatalf("adopted slot_version = %d, want 1", adopted.SlotVersion)
	}
	before := readSlotRow(ctx, t, pool, adopted.ID)

	staleUpstream := dbtest.UniqueID(t, "stale-upstream")
	_, err = repo.UpsertCampaign(ctx, &model.Campaign{
		ProjectID: project, BriefID: briefID, JobID: &staleJob,
		Platform: p, Variant: model.VariantDefault, SlotVersion: 2,
		CampaignName: dbtest.UniqueID(t, "campaign"), Status: model.CampaignStatusCreated,
		PlatformCampaignID: staleUpstream,
	}, nil)
	if !errors.Is(err, domain.ErrDispatchClaimLost) {
		t.Fatalf("stale slot-2 upsert beside the adoption: err = %v, want ErrDispatchClaimLost", err)
	}
	if !strings.Contains(err.Error(), staleUpstream) {
		t.Errorf("lost-claim error %q does not name the untracked upstream id %s", err, staleUpstream)
	}
	if after := readSlotRow(ctx, t, pool, adopted.ID); after.version != before.version ||
		after.platformCampaignID == nil || *after.platformCampaignID != adopted.PlatformCampaignID {
		t.Fatalf("adopted row changed: before=%+v after=%+v", before, after)
	}
	if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 1 {
		t.Fatalf("live rows on the slot = %d, want 1 (the adoption only)", live)
	}
}

// TestLiveStaleSlot2UpsertIsALostClaimWithoutTheLegacyIndex pins PR #260's contract-phase
// thread against the contracted schema 000050 leaves. With 000022's index gone, nothing in the
// schema stops a stale slot-2 INSERT from landing beside an adopted slot-1 row, and the
// advisory lock only orders the two writes. UpsertCampaign's ownership check must refuse it: this job's claim is
// gone and the slot is occupied, so it is ErrDispatchClaimLost and only the adoption is live.
// The same test also pins the legitimate flow the check must let through on that schema: a
// new_version claim of slot 2, made while slot 1 is live and complete, finalizes normally.
func TestLiveStaleSlot2UpsertIsALostClaimWithoutTheLegacyIndex(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	p := model.ProviderMicrosoftAds

	t.Run("claim slot 2, clear, adopt slot 1, stale slot-2 upsert", func(t *testing.T) {
		briefID, project := insertApprovedBrief(ctx, t, pool)
		staleJob := uuid.NewString()
		if claimed, _, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p, model.VariantDefault, 2, staleJob, nil); err != nil || !claimed {
			t.Fatalf("slot-2 claim: claimed=%v err=%v", claimed, err)
		}
		if err := repo.ClearDispatchClaim(ctx, briefID, p, model.VariantDefault, 2); err != nil {
			t.Fatalf("operator clear: %v", err)
		}
		adopted, err := adoptInto(ctx, t, repo, project, briefID, p)
		if err != nil {
			t.Fatalf("adopt slot 1: %v", err)
		}
		before := readSlotRow(ctx, t, pool, adopted.ID)

		staleUpstream := dbtest.UniqueID(t, "stale-upstream")
		_, err = repo.UpsertCampaign(ctx, &model.Campaign{
			ProjectID: project, BriefID: briefID, JobID: &staleJob,
			Platform: p, Variant: model.VariantDefault, SlotVersion: 2,
			CampaignName: dbtest.UniqueID(t, "campaign"), Status: model.CampaignStatusCreated,
			PlatformCampaignID: staleUpstream,
		}, nil)
		if !errors.Is(err, domain.ErrDispatchClaimLost) {
			t.Fatalf("stale slot-2 upsert beside the adoption: err = %v, want ErrDispatchClaimLost", err)
		}
		if !strings.Contains(err.Error(), staleUpstream) {
			t.Errorf("lost-claim error %q does not name the upstream id %s", err, staleUpstream)
		}
		if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 1 {
			t.Fatalf("live rows on the slot = %d, want 1 (the adoption only)", live)
		}
		if after := readSlotRow(ctx, t, pool, adopted.ID); after.version != before.version {
			t.Fatalf("adopted row changed: before=%+v after=%+v", before, after)
		}
	})

	t.Run("new_version claim beside a live slot 1 finalizes", func(t *testing.T) {
		briefID, project := insertApprovedBrief(ctx, t, pool)
		createSlotVersion(ctx, t, repo, project, briefID, p, model.FirstSlotVersion)
		second := createSlotVersion(ctx, t, repo, project, briefID, p, 2)
		if second.SlotVersion != 2 || second.PlatformCampaignID == "" {
			t.Fatalf("slot 2 = %+v, want a finalized second campaign", second)
		}
		if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 2 {
			t.Fatalf("live rows on the slot = %d, want 2 (both versions)", live)
		}
	})
}
