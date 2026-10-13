// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// installCommitTrigger installs a DEFERRED constraint trigger on campaigns that runs body (a
// PL/pgSQL statement list) at COMMIT for updates of rows whose campaign_name is name — only this
// test's own row, so no other test's writes are affected. Removed on cleanup.
func installCommitTrigger(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name, body string) {
	t.Helper()
	fn := sanitizeIdent("ct_" + dbtest.UniqueID(t, "fn"))
	if _, err := pool.Exec(ctx, `CREATE FUNCTION `+fn+`() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.campaign_name = '`+name+`' THEN `+body+` END IF;
			RETURN NULL;
		END $$`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE CONSTRAINT TRIGGER `+fn+` AFTER UPDATE ON campaigns
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION `+fn+`()`); err != nil {
		t.Fatalf("create deferred trigger: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := dbtest.CleanupContext()
		defer cancel()
		_, _ = pool.Exec(cctx, `DROP TRIGGER IF EXISTS `+fn+` ON campaigns`)
		_, _ = pool.Exec(cctx, `DROP FUNCTION IF EXISTS `+fn+`()`)
	})
}

func seedCommitCampaign(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (repo *postgres.CampaignRepo, loaded *model.Campaign, version int64) {
	t.Helper()
	repo = postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID := insertBrief(ctx, t, pool)
	projectID := dbtest.UniqueID(t, "project")
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status)
		VALUES ($1, $2, $3, $4, 'created')
		RETURNING id, version`,
		projectID, briefID, string(model.ProviderMetaAds), dbtest.UniqueID(t, "campaign")).Scan(&id, &version); err != nil {
		t.Fatalf("seed a campaign: %v", err)
	}
	loaded, err := repo.GetCampaign(ctx, projectID, briefID, id)
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	return repo, loaded, version
}

// TestLiveReplaceCampaignCommitErrorClasses pins how ReplaceCampaign classifies a failed COMMIT
// (PR #309): the SERVER rejecting the commit confirms the rollback and is a definite error, while
// a lost acknowledgement — here the context ending while the commit is awaited — may have
// committed and carries domain.ErrWriteOutcomeUnknown. A guarded-update miss before the commit
// carries neither.
func TestLiveReplaceCampaignCommitErrorClasses(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	t.Run("pre-commit miss is a plain precondition failure", func(t *testing.T) {
		repo, loaded, version := seedCommitCampaign(ctx, t, pool)
		if _, err := repo.ReplaceCampaign(ctx, loaded, version+7, domain.CampaignLockToken{}, nil); !errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrWriteOutcomeUnknown) {
			t.Fatalf("err = %v; want ErrPreconditionFailed without ErrWriteOutcomeUnknown", err)
		}
	})

	t.Run("a server-side commit rejection is a confirmed rollback", func(t *testing.T) {
		repo, loaded, version := seedCommitCampaign(ctx, t, pool)
		poison := loaded.CampaignName + "-raise"
		installCommitTrigger(ctx, t, pool, poison, `RAISE EXCEPTION 'deferred failure at commit';`)
		loaded.CampaignName = poison
		_, err := repo.ReplaceCampaign(ctx, loaded, version, domain.CampaignLockToken{}, nil)
		if err == nil || errors.Is(err, domain.ErrWriteOutcomeUnknown) {
			t.Fatalf("err = %v; want a definite (rolled back) error, not ErrWriteOutcomeUnknown", err)
		}
		row, gerr := repo.GetCampaign(ctx, loaded.ProjectID, loaded.BriefID, loaded.ID)
		if gerr != nil || row.Version != version {
			t.Fatalf("row after the rejected commit = %+v, %v; want it unchanged at version %d", row, gerr, version)
		}
	})

	t.Run("a lost commit acknowledgement is ambiguous", func(t *testing.T) {
		repo, loaded, version := seedCommitCampaign(ctx, t, pool)
		slow := loaded.CampaignName + "-slow"
		installCommitTrigger(ctx, t, pool, slow, `PERFORM pg_sleep(1.5);`)
		loaded.CampaignName = slow
		cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		_, err := repo.ReplaceCampaign(cctx, loaded, version, domain.CampaignLockToken{}, nil)
		if !errors.Is(err, domain.ErrWriteOutcomeUnknown) {
			t.Fatalf("err = %v (%T); want domain.ErrWriteOutcomeUnknown", err, errors.Unwrap(err))
		}
	})
}

// sanitizeIdent keeps only [a-z0-9_] so a unique id is usable as an unquoted identifier.
func sanitizeIdent(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+'a'-'A')
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) > 60 {
		out = out[len(out)-60:]
		out[0] = 'f'
	}
	return string(out)
}
