// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// insertCreativeAsset stores one asset through the repository and returns it. It goes through
// CreateAsset rather than a direct INSERT so the dimension conversion under test is the one the
// service actually uses.
func insertCreativeAsset(ctx context.Context, t *testing.T, repo *postgres.CreativeAssetRepo,
	projectID, briefID string, width, height int, checksum string) *model.CreativeAsset {
	t.Helper()

	bytes := []byte("not-a-real-png-but-the-column-only-measures-it")
	asset, _, err := repo.CreateAsset(ctx, &model.CreativeAsset{
		ProjectID: projectID,
		BriefID:   briefID,
		MimeType:  model.MimeTypePNG,
		ByteSize:  int64(len(bytes)),
		Checksum:  checksum,
		Width:     width,
		Height:    height,
		Bytes:     bytes,
	})
	if err != nil {
		t.Fatalf("create creative asset: %v", err)
	}
	return asset
}

// assetExists reports whether the row is still stored, which is the assertion that separates a
// refused delete from one that answered ErrCreativeAssetInUse after already removing the bytes.
func assetExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, assetID string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM creative_assets WHERE id = $1`, assetID).Scan(&n); err != nil {
		t.Fatalf("count the asset row: %v", err)
	}
	return n > 0
}

// TestLiveDeleteAssetWaitsForAnInFlightDispatchClaim is the race DeleteAsset's brief-row lock
// exists for, and it can only be observed against a real database: the arbitration is PostgreSQL's
// row-lock conflict matrix, and no fake repository has one.
//
// The window is narrow and entirely invisible to the delete's own statement. Under READ COMMITTED
// a statement's snapshot is taken when the STATEMENT begins, so deleteCreativeAssetQuery's
// `referenced` check and its delete share one snapshot and cannot disagree with each other — but
// that snapshot still predates any claim that commits a moment later. Such a claim is invisible to
// the check, the delete proceeds, and the dispatcher then resolves a campaign whose asset is gone:
// a failed dispatch instead of the clean 409 the caller should have had.
//
// What closes it is lockBriefForAssetWriteQuery, and specifically its STRENGTH. A claim's INSERT
// into campaigns takes FOR KEY SHARE on the parent brief row as its foreign-key check and holds it
// until COMMIT. FOR UPDATE conflicts with FOR KEY SHARE, so a claim that has already inserted
// forces the delete to wait — and the delete then runs as the NEXT statement on that transaction,
// with a fresh snapshot that sees the committed claim.
//
// The test plays the claim's INSERT in a transaction it does not commit, which is the state a
// real claim is in for the duration of its dispatch work. Both halves of the fix are pinned:
//
//   - WITHOUT the lock, DeleteAsset would not wait at all; it would take its own snapshot,
//     see no live campaign, and delete. waitForBackendsBlockedBy reports that as the specific
//     defect rather than as a timeout.
//   - WITHOUT the second statement — if the delete ran AS the locking statement — it would hold
//     the pre-lock snapshot and still miss the claim, so the final ErrCreativeAssetInUse is what
//     proves the fresh snapshot, not merely that something blocked.
func TestLiveDeleteAssetWaitsForAnInFlightDispatchClaim(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCreativeAssetRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)

	asset := insertCreativeAsset(ctx, t, repo, project, briefID, 1200, 628, "aa01")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the claim transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The claim as claimCampaignDispatchQuery writes it: status 'pending' with a NULL
	// config_snapshot, which is the first half of deleteCreativeAssetQuery's `referenced` arm —
	// a claim that has not yet resolved its config could still name any asset on the brief.
	//
	// Written directly rather than through schema_live_test.go's insertCampaignSQL: that helper
	// bakes in an ON CONFLICT arbiter clause, which is the thing ITS tests vary and which this
	// row must not carry, and it runs on the pool, while this insert has to run on an
	// uncommitted transaction so the claim's row lock is still held.
	if _, err := tx.Exec(ctx, `INSERT INTO campaigns
		(project_id, brief_id, platform, variant, slot_version, campaign_name, status)
		VALUES ($1, $2, 'reddit-ads', $3, 1, '', 'pending')`,
		project, briefID, model.VariantDefault); err != nil {
		t.Fatalf("insert the in-flight dispatch claim: %v", err)
	}
	var holderPID int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("read the claim's backend pid: %v", err)
	}

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- repo.DeleteAsset(ctx, project, briefID, asset.ID) }()

	// Positive evidence, not a timeout: PostgreSQL must show the delete's backend waiting on the
	// claim's. A timeout alone would also pass when the goroutine is merely slow to reach the
	// database — and once the claim commits, the assertion below passes with no lock at all.
	waitForBackendsBlockedBy(ctx, t, pool, holderPID, 1, func() string {
		select {
		case derr := <-deleteDone:
			return fmt.Sprintf("DeleteAsset finished (%v) while an uncommitted dispatch claim held "+
				"the brief's FOR KEY SHARE lock; it must wait for that claim to commit", derr)
		default:
			return ""
		}
	})

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the dispatch claim: %v", err)
	}

	select {
	case derr := <-deleteDone:
		if !errors.Is(derr, domain.ErrCreativeAssetInUse) {
			t.Fatalf("DeleteAsset after the claim committed: err=%v, want ErrCreativeAssetInUse", derr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DeleteAsset still blocked after the dispatch claim committed")
	}

	if !assetExists(ctx, t, pool, asset.ID) {
		t.Fatal("the refused delete removed the asset anyway: a 409 must write nothing")
	}
}

// TestLiveDeleteAssetSucceedsOnceTheClaimIsDeleted is the contrast that keeps the test above
// honest. A lock that made every delete wait forever, or a `referenced` arm that matched any
// campaign at all, would satisfy the race test and break the feature — so the same brief, with
// the same lock taken, must still allow the delete when no LIVE campaign remains.
func TestLiveDeleteAssetSucceedsOnceTheClaimIsDeleted(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCreativeAssetRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)

	asset := insertCreativeAsset(ctx, t, repo, project, briefID, 1200, 628, "bb02")

	var campaignID string
	if err := pool.QueryRow(ctx, `INSERT INTO campaigns
		(project_id, brief_id, platform, variant, slot_version, campaign_name, status)
		VALUES ($1, $2, 'reddit-ads', $3, 1, '', 'pending') RETURNING id`,
		project, briefID, model.VariantDefault).Scan(&campaignID); err != nil {
		t.Fatalf("insert the claim: %v", err)
	}
	if err := repo.DeleteAsset(ctx, project, briefID, asset.ID); !errors.Is(err, domain.ErrCreativeAssetInUse) {
		t.Fatalf("delete while the claim is live: err=%v, want ErrCreativeAssetInUse", err)
	}

	// 'deleted' is the status deleteCreativeAssetQuery's `referenced` arm excludes, and it is the
	// real end state of an abandoned claim rather than a row that vanishes.
	if _, err := pool.Exec(ctx, `UPDATE campaigns SET status='deleted' WHERE id=$1`, campaignID); err != nil {
		t.Fatalf("soft-delete the claim: %v", err)
	}
	if err := repo.DeleteAsset(ctx, project, briefID, asset.ID); err != nil {
		t.Fatalf("delete once no live campaign references the asset: %v", err)
	}
	if assetExists(ctx, t, pool, asset.ID) {
		t.Fatal("DeleteAsset reported success but the row is still stored")
	}
}

// TestLiveDimensionsCheckRefusesAHalfKnownPair makes true the claim the unit test in
// creative_asset_migration_test.go defers to it — that the CHECK's behaviour, not only its text,
// is covered.
//
// The unit test can only assert the constraint's SOURCE TEXT, because it has no database. That is
// worth pinning but it is not the property: what matters is that PostgreSQL REJECTS (1200, NULL).
// It is the one case the obvious spelling gets wrong — a CHECK admits anything that is not false,
// and `width > 0 AND height > 0` alone evaluates to NULL for a half-known pair, so Postgres would
// ACCEPT exactly the row the constraint exists to refuse. Only a live insert can tell the two
// spellings apart.
func TestLiveDimensionsCheckRefusesAHalfKnownPair(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	briefID, project := insertApprovedBrief(ctx, t, pool)

	insert := func(t *testing.T, checksum string, width, height any) error {
		t.Helper()
		bytes := []byte("bytes")
		_, err := pool.Exec(ctx, `INSERT INTO creative_assets
			(project_id, brief_id, mime_type, byte_size, checksum, width, height, bytes)
			VALUES ($1, $2, 'image/png', $3, $4, $5, $6, $7)`,
			project, briefID, int64(len(bytes)), checksum, width, height, bytes)
		return err
	}

	// A constraint violation, named by SQLSTATE and by constraint, rather than "any error" — a
	// typo'd column or a tripped byte_size CHECK would otherwise read as this test passing.
	refused := func(t *testing.T, label string, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("%s: err=%v, want a PostgreSQL check violation", label, err)
		}
		if pgErr.Code != "23514" || pgErr.ConstraintName != "creative_assets_dimensions_positive" {
			t.Fatalf("%s: SQLSTATE %s on constraint %q, want 23514 on creative_assets_dimensions_positive",
				label, pgErr.Code, pgErr.ConstraintName)
		}
	}

	refused(t, "a width with no height", insert(t, "cc01", 1200, nil))
	refused(t, "a height with no width", insert(t, "cc02", nil, 628))
	refused(t, "a zero dimension", insert(t, "cc03", 1200, 0))
	refused(t, "a negative dimension", insert(t, "cc04", 1200, -628))

	// Both arms of the constraint must still admit what they are meant to: the NULL pair an
	// asset stored before 000047 carries, and an ordinary known shape.
	if err := insert(t, "cc05", nil, nil); err != nil {
		t.Fatalf("the unknown pair (NULL, NULL) must be accepted — that is what keeps the columns "+
			"nullable for pre-000047 rows: %v", err)
	}
	if err := insert(t, "cc06", 1200, 628); err != nil {
		t.Fatalf("a known pair must be accepted: %v", err)
	}
}

// TestLiveCreateAssetConvertsAHalfKnownPairToUnknown pins the WRITER's half of the same rule.
//
// The constraint refuses (1200, 0); the question this answers is what CreateAsset does when a
// caller hands it one. Two independent NULLIFs — the obvious spelling — would store (1200, NULL)
// and be refused outright, turning a dimensionless write into an insert failure. The CASE pair
// decides both columns with one predicate over both parameters, so the row stores (NULL, NULL):
// honestly unknown, and read back as the zero pair the classifier reports as unclassified rather
// than as the wrong shape.
func TestLiveCreateAssetConvertsAHalfKnownPairToUnknown(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCreativeAssetRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)

	// A width with no height. The insert must SUCCEED and must not persist the half-known pair.
	stored := insertCreativeAsset(ctx, t, repo, project, briefID, 1200, 0, "dd01")
	if stored.Width != 0 || stored.Height != 0 {
		t.Fatalf("a half-known pair read back as (%d, %d), want the zero pair meaning unknown",
			stored.Width, stored.Height)
	}

	// Read the COLUMNS, not the model: creativeAssetCols wraps both in COALESCE(..., 0), so a
	// literal 0 in the column and a NULL are indistinguishable through the repository.
	var width, height *int
	if err := pool.QueryRow(ctx,
		`SELECT width, height FROM creative_assets WHERE id = $1`, stored.ID).Scan(&width, &height); err != nil {
		t.Fatalf("read the stored dimension columns: %v", err)
	}
	if width != nil || height != nil {
		t.Fatalf("stored (width, height) = (%v, %v), want both NULL: a zero dimension means "+
			"unknown and the column's spelling of unknown is NULL, not 0", width, height)
	}

	// A NEGATIVE dimension takes the same path. The predicate is `> 0`, not `<> 0`, so a caller
	// that produces one gets an honestly unknown pair rather than the SQLSTATE 23514 the CHECK
	// would raise — which the direct-insert test above proves the CHECK really does raise, so
	// this is the writer absorbing the value and not the constraint having gone soft.
	negative := insertCreativeAsset(ctx, t, repo, project, briefID, 1200, -628, "dd03")
	if negative.Width != 0 || negative.Height != 0 {
		t.Fatalf("a negative dimension read back as (%d, %d), want the zero pair meaning unknown",
			negative.Width, negative.Height)
	}

	// A real pair still round-trips, so the conversion is not simply discarding dimensions.
	known := insertCreativeAsset(ctx, t, repo, project, briefID, 1200, 628, "dd02")
	if known.Width != 1200 || known.Height != 628 {
		t.Fatalf("a known pair read back as (%d, %d), want (1200, 628)", known.Width, known.Height)
	}
}

// TestLiveDeleteAssetRefusesWhileACampaignIsPending pins the `referenced` CTE's status arm —
// the REMOVAL direction of the config_snapshot hazard, and the one the lock above cannot cover.
//
// The claim test above works because the claim's row is still uncommitted and carries a NULL
// snapshot. This is the case AFTER that: a 'pending' campaign whose snapshot has been written
// and committed, and whose contents do not name this asset. Nothing is in flight that a lock
// could order against, and both of the arm's other halves answer "not referenced" — the snapshot
// is not NULL, and neither strpos finds the id. Before `c.status = 'pending'` was its own arm,
// the delete therefore succeeded, and the dispatch that had not yet resolved went on to use an
// asset that was gone.
//
// Why a committed non-NULL snapshot on a pending row is reachable at all: update-campaign used to
// accept a config edit in exactly that window. internal/service/brief.go now refuses one with
// `campaign_dispatch_in_flight`, which is the writer's half. This is the reader's half, and it
// holds whatever any writer does — a row can also reach this shape from an older release, or from
// a path that does not go through update-campaign.
//
// The second half of the test is what keeps the arm from being "refuse everything": the same row
// at status 'created', with the same snapshot, must let the delete through. Without it an arm
// that ignored status entirely would pass.
func TestLiveDeleteAssetRefusesWhileACampaignIsPending(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCreativeAssetRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)

	asset := insertCreativeAsset(ctx, t, repo, project, briefID, 1200, 628, "ee01")

	// A COMMITTED snapshot that does not mention the asset: both strpos halves miss it and the
	// NULL half does not apply, so status is the only thing left that can refuse the delete.
	var campaignID string
	if err := pool.QueryRow(ctx, `INSERT INTO campaigns
		(project_id, brief_id, platform, variant, slot_version, campaign_name, status, config_snapshot)
		VALUES ($1, $2, 'reddit-ads', $3, 1, '', 'pending', '{"headline":"nothing to do with the asset"}')
		RETURNING id`,
		project, briefID, model.VariantDefault).Scan(&campaignID); err != nil {
		t.Fatalf("insert the pending campaign: %v", err)
	}

	if err := repo.DeleteAsset(ctx, project, briefID, asset.ID); !errors.Is(err, domain.ErrCreativeAssetInUse) {
		t.Fatalf("delete while a campaign is pending: err=%v, want ErrCreativeAssetInUse", err)
	}
	if !assetExists(ctx, t, pool, asset.ID) {
		t.Fatal("the refused delete removed the asset anyway: a 409 must write nothing")
	}

	// Same row, same snapshot, dispatch resolved. The arm must now stand aside.
	if _, err := pool.Exec(ctx, `UPDATE campaigns SET status='created' WHERE id=$1`, campaignID); err != nil {
		t.Fatalf("resolve the campaign: %v", err)
	}
	if err := repo.DeleteAsset(ctx, project, briefID, asset.ID); err != nil {
		t.Fatalf("delete once the campaign is no longer pending and its snapshot does not name the "+
			"asset: %v", err)
	}
	if assetExists(ctx, t, pool, asset.ID) {
		t.Fatal("DeleteAsset reported success but the row is still stored")
	}
}

// TestLiveReplaceCampaignTakesTheBriefLockBeforeWritingTheSnapshot pins the ADDITION direction,
// which neither the status arm nor the pending guard covers: a campaign that is already 'created'
// writes a config_snapshot that names an asset the concurrent delete's snapshot did not see.
//
// Nothing ordered those two writes before this. ReplaceCampaign's UPDATE touches `campaigns` and
// nothing else, DeleteAsset's statement reads `campaigns` but locks `campaign_briefs`, and the
// per-campaign advisory lock a caller may hold is keyed on the CAMPAIGN — so the delete could take
// its snapshot, see an unreferenced asset, and delete it while a replace adding exactly that
// reference was in flight. ReplaceCampaign now takes lockBriefRowByIDQuery as the opening
// statement of its transaction, which is the same row lock DeleteAsset takes, so one waits for the
// other and the second one's statement runs with a snapshot that sees the first.
//
// It is the method, not any one caller, that owns this: ReplaceCampaign writes EVERY mutable
// column including config_snapshot, so all five call sites rewrite the snapshot whether or not
// they meant to change it. Hence the unconditional lock, and hence a test that asserts the lock is
// taken rather than one that only exercises a config-bearing caller.
//
// The holder here is a plain FOR UPDATE on the brief — exactly what DeleteAsset holds for the
// duration of its statement — rather than a real concurrent DeleteAsset, because a delete's own
// lock is released when its single statement ends and that window is too short to observe
// deterministically. What must be observed is that the replace WAITS; waitForBackendsBlockedBy
// reports a replace that sailed past as that specific defect rather than as a timeout.
func TestLiveReplaceCampaignTakesTheBriefLockBeforeWritingTheSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	assets := postgres.NewCreativeAssetRepo(&postgres.Pool{Pool: pool})
	campaigns := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID, project := insertApprovedBrief(ctx, t, pool)

	asset := insertCreativeAsset(ctx, t, assets, project, briefID, 1200, 628, "ff01")

	var campaignID string
	if err := pool.QueryRow(ctx, `INSERT INTO campaigns
		(project_id, brief_id, platform, variant, slot_version, campaign_name, status, config_snapshot)
		VALUES ($1, $2, 'reddit-ads', $3, 1, 'before', 'created', '{"headline":"no asset yet"}')
		RETURNING id`,
		project, briefID, model.VariantDefault).Scan(&campaignID); err != nil {
		t.Fatalf("insert the created campaign: %v", err)
	}

	// Load OUTSIDE the goroutine: GetCampaign takes no lock, so doing it here keeps the only thing
	// the goroutine can block on being the brief lock itself.
	loaded, err := campaigns.GetCampaign(ctx, project, briefID, campaignID)
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	version := loaded.Version
	loaded.ConfigSnapshot = []byte(fmt.Sprintf(`{"image_asset_id":%q}`, asset.ID))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`SELECT status FROM campaign_briefs WHERE id = $1 AND project_id = $2 FOR UPDATE`,
		briefID, project); err != nil {
		t.Fatalf("take the brief's write lock: %v", err)
	}
	var holderPID int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("read the holder's backend pid: %v", err)
	}

	replaceDone := make(chan error, 1)
	go func() {
		_, rerr := campaigns.ReplaceCampaign(ctx, loaded, version, domain.CampaignLockToken{}, nil)
		replaceDone <- rerr
	}()

	waitForBackendsBlockedBy(ctx, t, pool, holderPID, 1, func() string {
		select {
		case rerr := <-replaceDone:
			return fmt.Sprintf("ReplaceCampaign finished (%v) while another transaction held the "+
				"brief's FOR UPDATE lock; it must take that lock and wait, or its snapshot write is "+
				"not ordered against a concurrent DeleteAsset", rerr)
		default:
			return ""
		}
	})

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("release the brief lock: %v", err)
	}

	select {
	case rerr := <-replaceDone:
		if rerr != nil {
			t.Fatalf("ReplaceCampaign after the brief lock was released: %v", rerr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ReplaceCampaign still blocked after the brief lock was released")
	}

	// The reference it added is now visible to the delete's `referenced` check — which is the
	// outcome the ordering exists to produce, not merely that something blocked.
	if err := assets.DeleteAsset(ctx, project, briefID, asset.ID); !errors.Is(err, domain.ErrCreativeAssetInUse) {
		t.Fatalf("delete after the replace named the asset: err=%v, want ErrCreativeAssetInUse", err)
	}
	if !assetExists(ctx, t, pool, asset.ID) {
		t.Fatal("the refused delete removed the asset anyway: a 409 must write nothing")
	}
}
