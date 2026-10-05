// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// liveSlotLockSQL is the repo's lockCampaignSlotQuery with its namespace argument inlined,
// copied rather than imported (the constant is unexported, and this package tests the
// migrated schema from outside). A drift between the two fails
// TestLiveClaimAndAdoptWaitForTheSlotLock — the repo would then take a DIFFERENT lock from
// the one this test holds, and the "must block" assertions would see it run straight through.
const liveSlotLockSQL = `SELECT pg_advisory_xact_lock(1936486260::int4,
	hashtext($1::uuid::text || '|' || $2::text || '|' || $3::text))`

// countLive returns the number of live campaign rows on one (brief, platform, variant) slot.
func countLive(ctx context.Context, t *testing.T, pool *pgxpool.Pool, briefID string, p model.Provider, variant string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM campaigns WHERE brief_id=$1 AND platform=$2 AND variant=$3 AND status <> 'deleted'`,
		briefID, string(p), variant).Scan(&n); err != nil {
		t.Fatalf("count live rows: %v", err)
	}
	return n
}

// createSlotVersion claims and completes one slot version through the real repo methods, the
// way dispatchPlatform does, and returns the completed row.
func createSlotVersion(ctx context.Context, t *testing.T, repo *postgres.CampaignRepo, project, briefID string, p model.Provider, slotVersion int) *model.Campaign {
	t.Helper()
	jobID := uuid.NewString()
	claimed, _, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p, model.VariantDefault, slotVersion, jobID, nil)
	if err != nil {
		t.Fatalf("claim slot %d: %v", slotVersion, err)
	}
	if !claimed {
		t.Fatalf("claim slot %d: not claimed", slotVersion)
	}
	created, err := repo.UpsertCampaign(ctx, &model.Campaign{
		ProjectID: project, BriefID: briefID, JobID: &jobID,
		Platform: p, Variant: model.VariantDefault, SlotVersion: slotVersion,
		CampaignName: dbtest.UniqueID(t, "campaign"), Status: model.CampaignStatusCreated,
		PlatformCampaignID: dbtest.UniqueID(t, "upstream"),
	}, nil)
	if err != nil {
		t.Fatalf("upsert slot %d: %v", slotVersion, err)
	}
	return created
}

func adoptInto(ctx context.Context, t *testing.T, repo *postgres.CampaignRepo, project, briefID string, p model.Provider) (*model.Campaign, error) {
	t.Helper()
	return repo.AdoptCampaign(ctx, &model.Campaign{
		ProjectID: project, BriefID: briefID, Platform: p, Variant: model.VariantDefault,
		PlatformCampaignID: dbtest.UniqueID(t, "adopted"),
		CampaignName:       dbtest.UniqueID(t, "campaign"),
		Status:             model.CampaignStatusCreated,
	}, 1, nil)
}

// TestLiveNewVersionCreatesSlotVersion2 drives the real claim/upsert/read methods against the
// contracted schema: 000037's four-column index, and no 000022 three-column one (000038).
//
// It pins what the contract step promises:
//   - a retry of slot 1 still conflicts on the four-column arbiter and is swallowed, exactly
//     as before — byte-identical slot-1 behaviour;
//   - a claim for slot 2 on the same (brief, platform, variant) now SUCCEEDS, and the slot
//     then holds two live campaigns, the latest of which is the one GetCampaignByPlatform
//     returns;
//   - slot_version is not the optimistic-concurrency `version`: the upsert bumps version and
//     leaves slot_version alone. The two share a word, and an earlier draft of 000036 added
//     "version" with IF NOT EXISTS — a silent no-op on the existing counter.
func TestLiveNewVersionCreatesSlotVersion2(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)
	p := model.ProviderMicrosoftAds

	first := createSlotVersion(ctx, t, repo, project, briefID, p, model.FirstSlotVersion)
	if first.SlotVersion != model.FirstSlotVersion || first.Version < 2 {
		t.Fatalf("after upsert: slot_version=%d version=%d, want slot_version=1 and version bumped past 1 "+
			"(the upsert's conflict arm bumps the concurrency counter, never the slot version)",
			first.SlotVersion, first.Version)
	}

	retried, row, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p,
		model.VariantDefault, model.FirstSlotVersion, uuid.NewString(), nil)
	if err != nil {
		t.Fatalf("retry claim of slot 1: %v", err)
	}
	if retried || row.ID != first.ID {
		t.Fatalf("retry claim of slot 1: claimed=%v row=%s, want the existing campaign %s handed back", retried, row.ID, first.ID)
	}

	second := createSlotVersion(ctx, t, repo, project, briefID, p, 2)
	if second.SlotVersion != 2 || second.ID == first.ID {
		t.Fatalf("second campaign: id=%s slot_version=%d, want a NEW row at slot_version 2", second.ID, second.SlotVersion)
	}

	if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 2 {
		t.Fatalf("live campaigns on the slot = %d, want 2 — the deliberate second campaign beside the first", live)
	}
	latest, err := repo.GetCampaignByPlatform(ctx, project, briefID, p, model.VariantDefault)
	if err != nil {
		t.Fatalf("GetCampaignByPlatform: %v", err)
	}
	if latest.ID != second.ID {
		t.Fatalf("GetCampaignByPlatform = %s, want slot 2's campaign %s (the latest)", latest.ID, second.ID)
	}
}

// TestLiveConcurrentSlot1ClaimsHaveOneWinnerAndNoError pins the first-create race. A
// double-submitted first create is a skip or a reuse, never an error: exactly one claim wins
// and every other one gets the winner's row. With the claims serialized by the slot lock, the
// losers see the winner's committed row and conflict on the arbiter (DO NOTHING); before
// 000038 a loser could instead hit 23505 on 000022's index, which had to be special-cased.
func TestLiveConcurrentSlot1ClaimsHaveOneWinnerAndNoError(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)

	winners, errs := raceClaims(ctx, repo, project, briefID, model.ProviderMicrosoftAds, model.FirstSlotVersion, 16)
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}
	if len(errs) > 0 {
		t.Errorf("%d concurrent slot-1 claims errored, want 0; first: %v", len(errs), errs[0])
	}
}

// raceClaims fires n concurrent claims of one slot version and returns how many won, plus
// every error — a lost claim that came back without that slot version's row counts as one.
func raceClaims(ctx context.Context, repo *postgres.CampaignRepo, project, briefID string, p model.Provider, slotVersion, n int) (int, []error) {
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
			claimed, row, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p,
				model.VariantDefault, slotVersion, uuid.NewString(), nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				errs = append(errs, err)
			case claimed:
				winners++
			case row == nil || row.SlotVersion != slotVersion:
				errs = append(errs, fmt.Errorf("lost claim returned no slot-%d row", slotVersion))
			}
		}()
	}
	close(start)
	wg.Wait()
	return winners, errs
}

// TestLiveConcurrentNewVersionClaims covers the two shapes concurrent new-version claims
// take, now that nothing but the four-column index keys the slot:
//   - several claims that computed the SAME next version (a double-submitted form): one
//     wins, the others get the winner's row, and nothing errors — the orchestrator reports
//     them as skipped or reused, so the form makes one campaign, not two;
//   - claims for DIFFERENT versions: every one succeeds, with no error and no 23505, and the
//     slot ends up holding each version exactly once.
func TestLiveConcurrentNewVersionClaims(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)
	p := model.ProviderMicrosoftAds

	createSlotVersion(ctx, t, repo, project, briefID, p, model.FirstSlotVersion)

	winners, errs := raceClaims(ctx, repo, project, briefID, p, 2, 16)
	if winners != 1 {
		t.Errorf("same-version race: winners = %d, want exactly 1", winners)
	}
	if len(errs) > 0 {
		t.Errorf("same-version race: %d claims errored, want 0; first: %v", len(errs), errs[0])
	}

	const top = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		fails []error
		start = make(chan struct{})
	)
	for v := 3; v <= top; v++ {
		wg.Add(1)
		go func(v int) {
			defer wg.Done()
			<-start
			claimed, row, err := repo.ClaimCampaignDispatch(ctx, project, briefID, p,
				model.VariantDefault, v, uuid.NewString(), nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				fails = append(fails, fmt.Errorf("slot %d: %w", v, err))
			case !claimed || row == nil || row.SlotVersion != v:
				fails = append(fails, fmt.Errorf("slot %d: claimed=%v, want its own new row", v, claimed))
			}
		}(v)
	}
	close(start)
	wg.Wait()
	for _, f := range fails {
		t.Errorf("distinct-version race: %v", f)
	}

	rows, err := pool.Query(ctx,
		`SELECT slot_version FROM campaigns WHERE brief_id=$1 AND platform=$2 AND status <> 'deleted'`, briefID, string(p))
	if err != nil {
		t.Fatalf("read slot versions: %v", err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var v int
		if serr := rows.Scan(&v); serr != nil {
			t.Fatalf("scan slot version: %v", serr)
		}
		got = append(got, v)
	}
	sort.Ints(got)
	if want := []int{1, 2, 3, 4, 5, 6, 7, 8}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("live slot versions = %v, want %v — each version exactly once", got, want)
	}
}

// TestLiveConcurrentClaimAndAdoptLeaveOneLiveRow races a first-create claim against an adopt
// of the same empty slot, many times over. Whichever lands first, the slot must end with
// exactly ONE live row: either the claim won and the adopt is a 409 (ErrConflict), or the adopt
// won and the claim lost, handing back the adopted row. Never both, never an error other
// than that 409.
func TestLiveConcurrentClaimAndAdoptLeaveOneLiveRow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	p := model.ProviderMicrosoftAds

	for i := 0; i < 20; i++ {
		briefID, project := insertApprovedBrief(ctx, t, pool)
		var (
			wg       sync.WaitGroup
			claimed  bool
			claimRow *model.Campaign
			claimErr error
			adopted  *model.Campaign
			adoptErr error
			start    = make(chan struct{})
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			claimed, claimRow, claimErr = repo.ClaimCampaignDispatch(ctx, project, briefID, p,
				model.VariantDefault, model.FirstSlotVersion, uuid.NewString(), nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			adopted, adoptErr = adoptInto(ctx, t, repo, project, briefID, p)
		}()
		close(start)
		wg.Wait()

		if claimErr != nil {
			t.Fatalf("round %d: claim errored: %v", i, claimErr)
		}
		switch {
		case claimed:
			if !errors.Is(adoptErr, domain.ErrConflict) {
				t.Fatalf("round %d: claim won but adopt returned (%v, %v), want ErrConflict", i, adopted, adoptErr)
			}
		default:
			if adoptErr != nil {
				t.Fatalf("round %d: claim lost but adopt failed too: %v", i, adoptErr)
			}
			if claimRow == nil || claimRow.ID != adopted.ID {
				t.Fatalf("round %d: lost claim returned %v, want the adopted row %s", i, claimRow, adopted.ID)
			}
		}
		if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 1 {
			t.Fatalf("round %d: live rows on the slot = %d, want exactly 1", i, live)
		}
	}
}

// TestLiveAdoptRefusesASlotWhoseOnlyLiveCampaignIsALaterVersion is the hole 000038 would
// open without the adopt's occupancy check. Adopt always writes slot_version 1, so its
// ON CONFLICT arm only sees a live slot-1 row; once 000022's index is gone, a slot whose
// slot-1 campaign was deleted but whose slot-2 campaign is live would accept the adopt BESIDE
// it. Adopt binds only an empty slot, so it must be a 409 and write nothing.
func TestLiveAdoptRefusesASlotWhoseOnlyLiveCampaignIsALaterVersion(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)
	p := model.ProviderMicrosoftAds

	first := createSlotVersion(ctx, t, repo, project, briefID, p, model.FirstSlotVersion)
	createSlotVersion(ctx, t, repo, project, briefID, p, 2)
	if _, err := pool.Exec(ctx, `UPDATE campaigns SET status='deleted' WHERE id=$1`, first.ID); err != nil {
		t.Fatalf("soft-delete slot 1: %v", err)
	}

	if _, err := adoptInto(ctx, t, repo, project, briefID, p); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("adopt onto a slot holding a live slot-2 campaign: err=%v, want ErrConflict", err)
	}
	if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 1 {
		t.Fatalf("live rows on the slot = %d, want 1: the refused adopt must write nothing", live)
	}
}

// TestLiveClaimAndAdoptWaitForTheSlotLock pins the serialization itself, deterministically,
// rather than hoping a race lands in the window.
//
// The test plays a claim of slot 2 that holds the slot lock, as insertDispatchClaim does, but
// has NOT inserted anything yet. That ordering is the point: an INSERT would take FOR KEY SHARE
// on the brief through the foreign key, which blocks the adopt's FOR UPDATE on its own and
// would make the adopt wait even with no slot lock at all. With only the advisory lock held,
// nothing but the slot lock can stop the adopt — without it the adopt finds the slot empty and
// inserts slot 1 straight away, failing the "still blocked" assertion. Then the "claim"
// inserts its slot-2 row and commits, and the adopt must see that row and answer 409. A real
// claim of the same slot must wait for the lock too.
func TestLiveClaimAndAdoptWaitForTheSlotLock(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)
	p := model.ProviderMicrosoftAds

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, liveSlotLockSQL, briefID, string(p), model.VariantDefault); err != nil {
		t.Fatalf("take the slot lock: %v", err)
	}

	adoptDone := make(chan error, 1)
	go func() {
		_, aerr := adoptInto(ctx, t, repo, project, briefID, p)
		adoptDone <- aerr
	}()
	type claimResult struct {
		claimed bool
		err     error
	}
	claimDone := make(chan claimResult, 1)
	go func() {
		c, _, cerr := repo.ClaimCampaignDispatch(ctx, project, briefID, p, model.VariantDefault, 3, uuid.NewString(), nil)
		claimDone <- claimResult{c, cerr}
	}()

	select {
	case aerr := <-adoptDone:
		t.Fatalf("adopt finished (%v) while the slot lock was held; it must wait for it", aerr)
	case r := <-claimDone:
		t.Fatalf("claim finished (%+v) while the slot lock was held; it must wait for it", r)
	case <-time.After(500 * time.Millisecond):
	}

	// Only now does the "claim" write its row — the adopt and the real claim are parked on the
	// slot lock, so this INSERT's FK lock on the brief contends with nobody.
	if _, err := tx.Exec(ctx, `INSERT INTO campaigns
		(project_id, brief_id, platform, variant, slot_version, campaign_name, status)
		VALUES ($1, $2, $3, $4, 2, '', 'pending')`, project, briefID, string(p), model.VariantDefault); err != nil {
		t.Fatalf("insert the slot-2 claim: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the slot-2 claim: %v", err)
	}

	select {
	case aerr := <-adoptDone:
		if !errors.Is(aerr, domain.ErrConflict) {
			t.Fatalf("adopt after the slot-2 claim committed: err=%v, want ErrConflict", aerr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("adopt still blocked after the lock was released")
	}
	select {
	case r := <-claimDone:
		if r.err != nil || !r.claimed {
			t.Fatalf("slot-3 claim after the lock was released: claimed=%v err=%v, want claimed", r.claimed, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("claim still blocked after the lock was released")
	}

	// Slot 2 (the committed "claim") and slot 3 (the real one); no adopted slot 1.
	if live := countLive(ctx, t, pool, briefID, p, model.VariantDefault); live != 2 {
		t.Fatalf("live rows on the slot = %d, want 2 (slots 2 and 3, and no adopted slot 1)", live)
	}
}
