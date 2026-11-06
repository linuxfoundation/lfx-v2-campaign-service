// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// campaignBriefFKs reports which of the two brief foreign keys campaigns carries: 000002's
// id-only key and 000056's composite tenant key.
func campaignBriefFKs(ctx context.Context, t *testing.T, conn *pgx.Conn) (idOnly, composite bool) {
	t.Helper()
	if err := conn.QueryRow(ctx, `
		SELECT
		  EXISTS (SELECT 1 FROM pg_constraint
		           WHERE conrelid = 'campaigns'::regclass AND contype = 'f'
		             AND conname = 'campaigns_brief_id_fkey'),
		  EXISTS (SELECT 1 FROM pg_constraint
		           WHERE conrelid = 'campaigns'::regclass AND contype = 'f'
		             AND conname = 'campaigns_brief_project_fkey')`).Scan(&idOnly, &composite); err != nil {
		t.Fatalf("read campaigns' foreign keys: %v", err)
	}
	return idOnly, composite
}

// seedBriefAndCampaign writes one brief owned by briefProject and one campaign under it naming
// campaignProject, straight through SQL so the migration under test sees exactly these rows.
func seedBriefAndCampaign(ctx context.Context, t *testing.T, conn *pgx.Conn, briefProject, campaignProject, slug string) (briefID, campaignID string) {
	t.Helper()
	if err := conn.QueryRow(ctx,
		`INSERT INTO campaign_briefs (project_id, program_type, event_slug)
		 VALUES ($1, 'events', $2) RETURNING id::text`, briefProject, slug).Scan(&briefID); err != nil {
		t.Fatalf("seed brief: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status)
		 VALUES ($1, $2, 'google-ads', 'n/a', 'created') RETURNING id::text`,
		campaignProject, briefID).Scan(&campaignID); err != nil {
		t.Fatalf("seed campaign: %v", err)
	}
	return briefID, campaignID
}

// TestLiveMigration000056RefusesAMismatchedCampaign pins the "fail loudly, never repair" half of
// 000056. A campaigns row whose project_id differs from its brief's is a tenant-boundary question
// a person has to answer, so the migration must abort — with its own legible message — and leave
// the row, and the id-only FK, exactly as they were.
func TestLiveMigration000056RefusesAMismatchedCampaign(t *testing.T) {
	_ = dbtest.Pool(t)
	ctx := context.Background()

	dsn := freshDatabase(ctx, t)
	m := newMigrator(t, dsn)
	if err := m.Migrate(55); err != nil {
		t.Fatalf("migrate to 55: %s", dbtest.SafeDSNErrFor(dsn, err))
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the scratch database: %s", dbtest.SafeDSNErrFor(dsn, err))
	}
	defer func() { _ = conn.Close(ctx) }()

	// One consistent row beside the bad one, so the check is shown to count mismatches rather
	// than rows.
	seedBriefAndCampaign(ctx, t, conn, "p-owner", "p-owner", "s-ok")
	_, badID := seedBriefAndCampaign(ctx, t, conn, "p-owner", "p-intruder", "s-bad")

	merr := m.Migrate(56)
	if merr == nil {
		t.Fatal("000056 applied over a campaign whose project_id differs from its brief's; it must " +
			"refuse rather than validate or rewrite the row")
	}
	if msg := dbtest.SafeDSNErrFor(dsn, merr); !strings.Contains(msg, "000056: 1 campaigns row(s)") {
		t.Errorf("000056 failed with %q, want its own pre-check naming the one mismatched row", msg)
	}
	// Release the failed migrator's advisory lock before reading the schema (see
	// TestLiveMigration000030RefusesToDropALiveEmailBrief for why a separate connection is used).
	if _, cerr := m.Close(); cerr != nil {
		t.Fatalf("close the migrator after the refused migration: %v", cerr)
	}

	idOnly, composite := campaignBriefFKs(ctx, t, conn)
	if !idOnly || composite {
		t.Errorf("after the refused migration: id-only FK = %v, composite FK = %v; want the "+
			"000055 schema untouched (true, false)", idOnly, composite)
	}
	var project string
	if err := conn.QueryRow(ctx, `SELECT project_id FROM campaigns WHERE id = $1`, badID).Scan(&project); err != nil {
		t.Fatalf("read the mismatched row back: %v", err)
	}
	if project != "p-intruder" {
		t.Errorf("the mismatched row's project_id is now %q; the migration must never rewrite data", project)
	}
}

// TestLiveMigration000056SwapsTheBriefFK pins the other half: over consistent rows 000056 applies,
// replaces the id-only key with the composite one, and from then on refuses a campaign that names
// a brief under another project.
func TestLiveMigration000056SwapsTheBriefFK(t *testing.T) {
	_ = dbtest.Pool(t)
	ctx := context.Background()

	dsn := freshDatabase(ctx, t)
	m := newMigrator(t, dsn)
	if err := m.Migrate(55); err != nil {
		t.Fatalf("migrate to 55: %s", dbtest.SafeDSNErrFor(dsn, err))
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the scratch database: %s", dbtest.SafeDSNErrFor(dsn, err))
	}
	defer func() { _ = conn.Close(ctx) }()

	briefID, _ := seedBriefAndCampaign(ctx, t, conn, "p-owner", "p-owner", "s-ok")

	if err := m.Migrate(56); err != nil {
		t.Fatalf("migrate to 56 over consistent rows: %s", dbtest.SafeDSNErrFor(dsn, err))
	}
	idOnly, composite := campaignBriefFKs(ctx, t, conn)
	if idOnly || !composite {
		t.Errorf("after 000056: id-only FK = %v, composite FK = %v; want (false, true)", idOnly, composite)
	}

	_, err = conn.Exec(ctx,
		`INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status)
		 VALUES ('p-intruder', $1, 'meta-ads', 'n/a', 'created')`, briefID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Errorf("a campaign naming p-owner's brief under p-intruder = %v, want a 23503 "+
			"foreign-key violation", err)
	}
}

// TestLiveReplaceCampaignBriefLockIsProjectScoped pins lockBriefRowForReplaceQuery's project_id
// term. Another project's ReplaceCampaign naming this brief must neither take nor WAIT FOR the
// brief's row lock: it is refused at once with ErrNotFound. With the lock keyed on id alone it
// would queue behind whoever holds the brief, which is what the held lock below makes visible —
// the replace would run into its deadline instead of returning ErrNotFound.
func TestLiveReplaceCampaignBriefLockIsProjectScoped(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	projectID := dbtest.UniqueID(t, "project")
	briefID := insertBrief(ctx, t, pool, projectID)
	var (
		id      string
		version int64
	)
	if err := pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status)
		VALUES ($1, $2, 'google-ads', $3, 'created') RETURNING id, version`,
		projectID, briefID, dbtest.UniqueID(t, "campaign")).Scan(&id, &version); err != nil {
		t.Fatalf("seed a campaign: %v", err)
	}
	loaded, err := repo.GetCampaign(ctx, projectID, briefID, id)
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lock holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM campaign_briefs WHERE id = $1 FOR UPDATE`, briefID); err != nil {
		t.Fatalf("hold the brief's row lock: %v", err)
	}

	foreign := *loaded
	foreign.ProjectID = dbtest.UniqueID(t, "other-project")
	foreign.CampaignName = "cross-project rename"
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, rerr := repo.ReplaceCampaign(rctx, &foreign, version, domain.CampaignLockToken{}, nil)
	if !errors.Is(rerr, domain.ErrNotFound) {
		t.Fatalf("ReplaceCampaign from another project = %v, want domain.ErrNotFound without "+
			"waiting on the brief's row lock: the lock is not project-scoped", rerr)
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the brief's row lock: %v", err)
	}
	after, err := repo.GetCampaign(ctx, projectID, briefID, id)
	if err != nil {
		t.Fatalf("GetCampaign after the refused replace: %v", err)
	}
	if after.Version != version || after.CampaignName != loaded.CampaignName {
		t.Errorf("the refused replace changed the row (version %d -> %d, name %q -> %q)",
			version, after.Version, loaded.CampaignName, after.CampaignName)
	}

	// The owner's own replace still takes the lock and writes.
	loaded.CampaignName = loaded.CampaignName + "-renamed"
	if _, err := repo.ReplaceCampaign(ctx, loaded, version, domain.CampaignLockToken{}, nil); err != nil {
		t.Fatalf("the owning project's ReplaceCampaign: %v", err)
	}
}
