// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// TestAudienceBuildLeaseAdmitsExactlyOneConcurrentBuild is the property migration 000018
// exists for, and it can only be observed against a real database: the arbitration is the
// index, and no fake repository has one.
//
// Two builds for the same (brief, platform) start together. One must get a row; the other
// must get ErrAudienceBuildInFlight — NOT a second row, because a second row means a
// second complete set of HubSpot lists in the portal, indistinguishable from the first and
// billable. They cannot collide by list NAME either: the plan's BuildRef is the row id,
// chosen so a later build does not adopt an earlier one's lists, so nothing downstream
// would notice the duplication.
func TestAudienceBuildLeaseAdmitsExactlyOneConcurrentBuild(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewAudienceRepo(&postgres.Pool{Pool: pool})

	briefID, projectID := insertApprovedBrief(ctx, t, pool)

	const builders = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		rowIDs  []string
		inFlt   int
		otherEr []error
	)
	start := make(chan struct{})
	for range builders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			row := &model.CampaignAudience{
				ProjectID: projectID,
				BriefID:   briefID,
				Platform:  model.ProviderHubSpot,
				Status:    model.AudienceBuilding,
			}
			// Version 1 for every caller: the brief is untouched, so all eight pass the
			// approval guard and the ONLY thing that can separate them is the lease.
			created, err := repo.CreateAudienceForApprovedBrief(ctx, row, 1)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				rowIDs = append(rowIDs, created.ID)
			case errors.Is(err, domain.ErrAudienceBuildInFlight):
				inFlt++
			default:
				otherEr = append(otherEr, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range otherEr {
		t.Errorf("unexpected error from a concurrent build: %v", err)
	}
	if len(rowIDs) != 1 {
		t.Fatalf("%d of %d concurrent builds got a row, want exactly 1 — every extra row is a "+
			"full duplicate set of HubSpot lists nobody knows to delete", len(rowIDs), builders)
	}
	if want := builders - 1; inFlt != want {
		t.Errorf("%d builds were told a build is already in flight, want %d", inFlt, want)
	}
}

// TestAudienceBuildLeaseFreesOnCompletion pins the other half, and it is the half a
// stricter constraint would have broken. The lease covers 'building' ONLY, so a brief can
// be rebuilt once its previous build has finished — 000005 records that a brief may have
// many audiences over time, and BuildRef exists precisely because a later build for the
// same brief is expected. A constraint over 'status <> failed' (the shape campaigns uses,
// where a brief has exactly one live campaign per platform) would make the first
// successful build permanent and every rebuild a 409.
func TestAudienceBuildLeaseFreesOnCompletion(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewAudienceRepo(&postgres.Pool{Pool: pool})

	briefID, projectID := insertApprovedBrief(ctx, t, pool)
	newRow := func() *model.CampaignAudience {
		return &model.CampaignAudience{
			ProjectID: projectID,
			BriefID:   briefID,
			Platform:  model.ProviderHubSpot,
			Status:    model.AudienceBuilding,
		}
	}

	first, err := repo.CreateAudienceForApprovedBrief(ctx, newRow(), 1)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}

	// While it holds the lease, a second build is refused.
	if _, err := repo.CreateAudienceForApprovedBrief(ctx, newRow(), 1); !errors.Is(err, domain.ErrAudienceBuildInFlight) {
		t.Fatalf("second build while the first is building: got %v, want ErrAudienceBuildInFlight", err)
	}

	// Finish it. 'built' leaves the partial index's predicate, so the slot frees.
	first.Status = model.AudienceBuilt
	first.PlatformMasterListID = "list-1"
	first.InclusionSummary = "geo"
	if _, err := repo.UpdateAudience(ctx, first, first.Version); err != nil {
		t.Fatalf("complete the first build: %v", err)
	}

	second, err := repo.CreateAudienceForApprovedBrief(ctx, newRow(), 1)
	if err != nil {
		t.Fatalf("rebuild after the first build completed: %v — the lease must cover "+
			"concurrency only, not a brief's whole history", err)
	}
	if second.ID == first.ID {
		t.Errorf("the rebuild reused row %s; it must be its own row, since BuildRef (the row id) "+
			"is what keeps its HubSpot list names distinct from the first build's", first.ID)
	}
}

// TestAudienceBuildLeaseIndexIsValid is the assertion that makes IF NOT EXISTS in 000018
// safe. A failed CONCURRENTLY build does NOT roll back — it leaves the index present but
// marked INVALID, and a re-run of the migration then sees the name, skips the rebuild and
// reports success. The table would carry an index that enforces nothing while every test
// naming it by name still passes. indisvalid is the only thing that tells them apart.
func TestAudienceBuildLeaseIndexIsValid(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	var valid, ready bool
	err := pool.QueryRow(ctx, `
		SELECT indisvalid, indisready FROM pg_index
		WHERE indexrelid = 'uq_campaign_audiences_brief_platform_building'::regclass`).Scan(&valid, &ready)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.SQLState() == sqlstateUndefinedTable {
			t.Fatalf("the audience build lease index does not exist; migration 000018 did not apply")
		}
		t.Fatalf("read pg_index: %v", err)
	}
	if !valid || !ready {
		t.Errorf("uq_campaign_audiences_brief_platform_building is indisvalid=%v indisready=%v, "+
			"want both true — a failed CONCURRENTLY build leaves it in this state, and the "+
			"migration's IF NOT EXISTS would then skip the rebuild while reporting success",
			valid, ready)
	}
}

// sqlstateUndefinedTable is what ::regclass raises for a name no relation carries. The
// condition name is undefined_table even when the missing relation is an INDEX.
const sqlstateUndefinedTable = "42P01"

// insertApprovedBrief creates the APPROVED parent every audience build requires and
// returns (briefID, projectID). insertBrief in schema_live_test.go creates a DRAFT, which
// CreateAudienceForApprovedBrief rejects with ErrStaleApproval before the lease is ever
// consulted — so a test built on it would pass for the wrong reason.
func insertApprovedBrief(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (briefID, projectID string) {
	t.Helper()

	// Unique per test: the package migrates ONE schema and shares it, and campaign_briefs
	// carries a partial-unique (project_id, event_slug).
	projectID = dbtest.UniqueID(t, "brief")
	err := pool.QueryRow(ctx, `
		INSERT INTO campaign_briefs (project_id, program_type, event_slug, status)
		VALUES ($1, 'events', $1, 'approved')
		RETURNING id`, projectID).Scan(&briefID)
	if err != nil {
		t.Fatalf("insert approved parent brief: %v", err)
	}
	return briefID, projectID
}
