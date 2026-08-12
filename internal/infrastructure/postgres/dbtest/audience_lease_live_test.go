// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
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
// Builds for the same (brief, platform) are released together. One must get a row; every
// other must get ErrAudienceBuildInFlight — NOT a second row, because a second row means a
// second complete set of HubSpot lists in the portal, indistinguishable from the first and
// billable. They cannot collide by list NAME either: the plan's BuildRef is the row id,
// chosen so a later build does not adopt an earlier one's lists, so nothing downstream
// would notice the duplication.
//
// Be precise about WHERE they contend, because it is not where the name suggests. The
// inserts are not concurrent: CreateAudienceForApprovedBrief opens with
// `SELECT ... FOR UPDATE` on the brief, so the eight transactions queue at the BRIEF row
// lock and each reaches its INSERT with the previous one already committed. What the index
// decides is the OUTCOME — the second through eighth inserts see a committed 'building' row
// and raise 23505 — not a race between simultaneous writes. That does not weaken the test:
// the index is still what produces the outcome, and releasing the goroutines together is
// what puts them in that queue at all.
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
			// The brief is untouched and approved, so all eight pass the approval guard
			// once the brief row lock lets them through, and the ONLY thing left that can
			// separate them is the lease.
			created, _, err := repo.CreateAudienceForApprovedBrief(ctx, row)
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

	first, _, err := repo.CreateAudienceForApprovedBrief(ctx, newRow())
	if err != nil {
		t.Fatalf("first build: %v", err)
	}

	// While it holds the lease, a second build is refused.
	if _, _, err := repo.CreateAudienceForApprovedBrief(ctx, newRow()); !errors.Is(err, domain.ErrAudienceBuildInFlight) {
		t.Fatalf("second build while the first is building: got %v, want ErrAudienceBuildInFlight", err)
	}

	// Finish it. 'built' leaves the partial index's predicate, so the slot frees.
	first.Status = model.AudienceBuilt
	first.PlatformMasterListID = "list-1"
	first.InclusionSummary = "geo"
	if _, err := repo.UpdateAudience(ctx, first, first.Version); err != nil {
		t.Fatalf("complete the first build: %v", err)
	}

	second, _, err := repo.CreateAudienceForApprovedBrief(ctx, newRow())
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

// TestAudienceBuildLeaseRefusesPlainCreate covers the OTHER way into the lease, and it is
// the one the tests above missed: `POST create-audience` reaches CreateAudience, not
// CreateAudienceForApprovedBrief. That path defaults status to 'building' too, so it takes
// the same lease and can lose it — and its 23505 mapping is a separate branch of code from
// the build path's. Untested, a regression there would answer a lost lease with a bare 500
// while every build-path test stayed green.
func TestAudienceBuildLeaseRefusesPlainCreate(t *testing.T) {
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

	// The BUILD path takes the lease first, so the plain create is genuinely second —
	// which is the case the branch exists for. Doing it the other way round would test
	// the same mapping from the wrong side.
	if _, _, err := repo.CreateAudienceForApprovedBrief(ctx, newRow()); err != nil {
		t.Fatalf("hold the lease with a build: %v", err)
	}

	if _, err := repo.CreateAudience(ctx, newRow()); !errors.Is(err, domain.ErrAudienceBuildInFlight) {
		t.Fatalf("CreateAudience while a build holds the lease: got %v, want "+
			"ErrAudienceBuildInFlight — an unmapped 23505 surfaces as a 500, which reads "+
			"as a broken service rather than an occupied slot", err)
	}
}

// TestAudienceBuildLeaseRefusesUpdateBackToBuilding covers the third and rarest way in,
// and the reason it is worth a branch at all: this is the retry an operator reaches for.
// A build died holding the lease, they reconcile the portal, PATCH the stuck row to
// 'failed', and PATCH an earlier failed row back to 'building' to try again. If someone
// else has taken the slot in between, that PATCH hits the lease — on the UPDATE statement,
// a different 23505 site from either create.
//
// The completion test cannot reach it: it moves 'building' to 'built', which LEAVES the
// index predicate rather than entering it.
func TestAudienceBuildLeaseRefusesUpdateBackToBuilding(t *testing.T) {
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

	// An earlier attempt that failed. It is outside the index predicate, so it does not
	// hold the lease and a fresh build can start alongside it.
	failed, _, err := repo.CreateAudienceForApprovedBrief(ctx, newRow())
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	failed.Status = model.AudienceFailed
	failed, err = repo.UpdateAudience(ctx, failed, failed.Version)
	if err != nil {
		t.Fatalf("mark the first build failed: %v", err)
	}

	// Somebody else now holds the lease.
	if _, _, err := repo.CreateAudienceForApprovedBrief(ctx, newRow()); err != nil {
		t.Fatalf("second build after the first failed: %v", err)
	}

	// The operator retries the failed row. The slot is taken.
	failed.Status = model.AudienceBuilding
	if _, err := repo.UpdateAudience(ctx, failed, failed.Version); !errors.Is(err, domain.ErrAudienceBuildInFlight) {
		t.Fatalf("PATCH a failed row back to building while the lease is held: got %v, want "+
			"ErrAudienceBuildInFlight", err)
	}
}

// TestAudienceLeaseMappingIgnoresOtherUniqueIndexes is the test the constraint-name
// narrowing exists for, and the only one in this file that the SQLSTATE-only predicate
// fails. Every other lease test fires the REAL lease index, so a mapping that matches
// any 23505 on campaign_audiences answers them all correctly — which is precisely why
// none of them binds the change.
//
// What the narrowing buys is the case below: a DIFFERENT unique index on the table
// raises 23505, and the caller must NOT be told a build is already running. Today no
// second unique index exists, so the case is unreachable in production and cannot be
// reached by arranging rows — it has to be constructed. The probe index is that
// construction, and it is the whole test: without one, the property has no witness and
// the next migration to add a unique index inherits ErrAudienceBuildInFlight silently.
//
// The two indexes are separated by their PREDICATE, not by their key. The lease covers
// status = 'building'; the probe covers status = 'failed', so two failed rows under one
// brief violate the probe and never enter the lease's predicate at all. Separating them
// by platform instead would not work: migration 000006 CHECKs platform IN ('hubspot'),
// so every audience row this table can hold shares the lease's second key column.
//
// The probe's predicate is ALSO pinned to this test's own brief, and its name carries that
// brief's id, because an index is a schema-wide object while dbtest.go:66-72 requires the
// opposite of every identifier a test writes: "Tests therefore share a schema and MUST NOT
// share rows — use UniqueID for every identifier a test writes, so two tests (or two runs
// against the same database) cannot collide." An unscoped `WHERE status = 'failed'` spans
// every failed audience the shared database holds, including rows other tests and earlier
// runs left behind, so it is exactly such a shared identifier.
//
// Be honest about the reachability, because it is not what the hazard sounds like: no test
// in this suite leaves two failed audiences under ONE brief, so the unscoped form did not
// actually fail — measured, not assumed. What was measured is the mechanism. Two failed
// rows under one brief are legal (the lease covers 'building' only, and nothing else
// constrains 'failed'), and with such a pair present CREATE UNIQUE INDEX refuses:
// `could not create unique index ... Key (brief_id)=(...) is duplicated`. So this test
// would have started failing on the first unrelated test that persisted that pair, for a
// reason having nothing to do with the mapping it exists to pin. Scoping costs nothing —
// the probe is still a second unique index on the table, and still the only thing that can
// refuse the second insert — so there is no reason to leave that trap armed.
//
// Asserting only "not the sentinel" would pass if the insert failed for some unrelated
// reason — that CHECK included — so the test also asserts the error really is a 23505
// naming the probe.
func TestAudienceLeaseMappingIgnoresOtherUniqueIndexes(t *testing.T) {
	// All THREE migrated call sites, not just the plain create. Each maps a 23505 to
	// ErrAudienceBuildInFlight, each was narrowed to the lease index by name, and each is a
	// separate `isUniqueViolationOn` call that a later edit can widen back on its own — so a
	// test driving one of them proves nothing about the other two.
	//
	// They differ in how they reach the insert (a plain create, a create gated on the brief
	// being approved, a build claim), which is why this drives the repository methods rather
	// than the helper: the helper is already unit-tested, and what is under test here is that
	// each SITE passes the index name.
	for _, tc := range []struct {
		name   string
		insert func(context.Context, *postgres.AudienceRepo, *model.CampaignAudience) error
	}{
		{"CreateAudience", func(ctx context.Context, r *postgres.AudienceRepo, a *model.CampaignAudience) error {
			_, err := r.CreateAudience(ctx, a)
			return err
		}},
		{"CreateAudienceForApprovedBrief", func(ctx context.Context, r *postgres.AudienceRepo, a *model.CampaignAudience) error {
			_, _, err := r.CreateAudienceForApprovedBrief(ctx, a)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audienceLeaseNarrowingProbe(t, tc.insert)
		})
	}
}

// audienceLeaseNarrowingProbe is the body shared by every call site above.
//
// It creates a SECOND unique index scoped to this brief and outside the lease's predicate, then
// drives an insert that only that index can refuse. A 23505 from it must NOT map to
// ErrAudienceBuildInFlight — that sentinel claims a build holds the lease, and none does.
func audienceLeaseNarrowingProbe(t *testing.T, insert func(context.Context, *postgres.AudienceRepo, *model.CampaignAudience) error) {
	t.Helper()
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewAudienceRepo(&postgres.Pool{Pool: pool})

	briefID, projectID := insertApprovedBrief(ctx, t, pool)
	newRow := func() *model.CampaignAudience {
		return &model.CampaignAudience{
			ProjectID: projectID,
			BriefID:   briefID,
			Platform:  model.ProviderHubSpot,
			Status:    model.AudienceFailed,
		}
	}

	if err := insert(ctx, repo, newRow()); err != nil {
		t.Fatalf("first audience: %v", err)
	}

	// Created without IF NOT EXISTS, and named for this brief: a leftover index from an
	// earlier run cannot be silently adopted as this one, and cannot collide with it either.
	// The cleanup below still matters — a leak accumulates in the shared schema even though
	// it can no longer break a later run.
	//
	// briefID is interpolated rather than bound: CREATE INDEX takes no parameters. It is a
	// UUID Postgres itself minted and this test read straight back through RETURNING, never
	// input from anywhere else, and it is re-parsed below so a change to that helper cannot
	// quietly turn this into a place a string reaches DDL.
	if _, err := uuid.Parse(briefID); err != nil {
		t.Fatalf("brief id %q is not a UUID, so it cannot be interpolated into DDL: %v", briefID, err)
	}
	probeIndex := "uq_probe_second_unique_index_" + strings.ReplaceAll(briefID, "-", "")
	if _, err := pool.Exec(ctx, `CREATE UNIQUE INDEX `+probeIndex+
		` ON campaign_audiences (brief_id) WHERE status = 'failed' AND brief_id = '`+
		briefID+`'::uuid`); err != nil {
		t.Fatalf("create the probe index: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DROP INDEX `+probeIndex); err != nil {
			t.Errorf("drop the probe index: %v — it is now visible to every later test "+
				"in this package", err)
		}
	})

	// 'failed' is outside the lease's predicate, so the lease cannot refuse this row and
	// no build is in flight for it. Only the probe can refuse it.
	err := insert(ctx, repo, newRow())
	if err == nil {
		t.Fatal("second audience succeeded, so the probe index did not fire and the test " +
			"proves nothing — check the probe's predicate against the row being inserted")
	}
	// Checked before the shape assertion below: the sentinel is the defect under test, and
	// mapping to it discards the *pgconn.PgError, so the shape check would otherwise fail
	// first and report the wrong reason.
	if errors.Is(err, domain.ErrAudienceBuildInFlight) {
		t.Fatalf("a 23505 from %s was mapped to ErrAudienceBuildInFlight, which claims a "+
			"build holds the lease for brief %s. None does — every row here is 'failed', "+
			"outside the lease's predicate. A caller told this retries or reports an "+
			"occupied slot that does not exist", probeIndex, briefID)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != probeIndex {
		t.Fatalf("second audience: got %v, want a 23505 naming %s — the test must fail on "+
			"the probe, not on something else", err, probeIndex)
	}
}

// TestConfirmBriefApprovedWaitsForAnInFlightWithdrawal is the property that made
// ConfirmBriefApproved a repository operation instead of a GetBrief and a comparison, and
// like the lease above it can only be observed against a real database: what is under test
// is PostgreSQL's row locking, and no fake has any.
//
// The build's last act before creating real HubSpot lists is to confirm the brief is still
// approved. Under READ COMMITTED a plain SELECT reads the last COMMITTED row, so an operator
// withdrawing approval — a ReplaceBrief that has updated the row and not yet committed — is
// INVISIBLE to it. The build would read "approved", the withdrawal would commit, and the
// lists would be created from an approval that no longer existed. Nothing about that is
// detectable afterwards: the lists look exactly like a legitimate build's.
//
// `FOR UPDATE` makes the confirmation queue behind the writer instead of reading around it.
// The test asserts both halves, and the first is the one a non-locking implementation passes
// by accident: the call must still be BLOCKED while the writer is open, and must report
// ErrStaleApproval once it commits.
func TestConfirmBriefApprovedWaitsForAnInFlightWithdrawal(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewBriefRepo(&postgres.Pool{Pool: pool})

	briefID, projectID := insertApprovedBrief(ctx, t, pool)
	var version int64
	if err := pool.QueryRow(ctx, `SELECT version FROM campaign_briefs WHERE id = $1`, briefID).Scan(&version); err != nil {
		t.Fatalf("read the brief's version: %v", err)
	}

	// The withdrawal: updated, deliberately NOT committed.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the withdrawing transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE campaign_briefs SET status = 'draft', version = version + 1 WHERE id = $1`, briefID); err != nil {
		t.Fatalf("withdraw approval: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- repo.ConfirmBriefApproved(ctx, projectID, briefID, version) }()

	// Wait for PostgreSQL to SHOW the confirmation blocked on the writer's row lock, rather
	// than inferring it from "the call has not returned yet". The inference is not sound: if
	// the goroutine is simply not scheduled into ConfirmBriefApproved before the deadline
	// expires, the test commits, a plain SELECT then reads the committed draft, and
	// ErrStaleApproval comes back — so a non-locking implementation passes both assertions on
	// a slow machine. `pg_blocking_pids` turns the first assertion into positive evidence: a
	// backend in this database is waiting on another backend, which only `FOR UPDATE` against
	// the open transaction produces here.
	waitForBlockedBackend(ctx, t, pool, done)

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the withdrawal: %v", err)
	}

	select {
	case cerr := <-done:
		if !errors.Is(cerr, domain.ErrStaleApproval) {
			t.Fatalf("ConfirmBriefApproved = %v, want ErrStaleApproval — the lock was granted "+
				"after the withdrawal committed, so the row it read is the withdrawn one", cerr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ConfirmBriefApproved never returned after the withdrawal committed")
	}
}

// waitForBlockedBackend blocks until PostgreSQL reports a backend in this database waiting on
// another backend's lock, failing the test if that never happens or if the call under test
// returns first. `done` is polled alongside so a non-locking implementation is reported as the
// specific defect it is rather than as a generic timeout.
func waitForBlockedBackend(ctx context.Context, t *testing.T, pool *pgxpool.Pool, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case cerr := <-done:
			t.Fatalf("ConfirmBriefApproved returned %v while a withdrawal was in flight; it read "+
				"around the uncommitted writer instead of locking behind it, which is how a build "+
				"creates HubSpot lists from an approval that is already being withdrawn", cerr)
		default:
		}
		var blocked int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity a
			WHERE a.datname = current_database()
			  AND a.pid <> pg_backend_pid()
			  AND cardinality(pg_blocking_pids(a.pid)) > 0`).Scan(&blocked)
		if err != nil {
			t.Fatalf("inspect pg_stat_activity for a blocked backend: %v", err)
		}
		if blocked > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no backend ever blocked on the withdrawing transaction's row lock; " +
				"ConfirmBriefApproved is not taking FOR UPDATE against the brief row")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
