// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// TestLiveReplaceCampaignCommitErrorIsMarkedAmbiguous pins that a ReplaceCampaign whose COMMIT
// fails carries domain.ErrWriteOutcomeUnknown — what the Meta ad-set lever reconciles on (PR
// #309) — while a guarded-update miss before the commit does not. The commit is made to fail with
// a DEFERRED constraint trigger that fires only for this test's own row, so no other test's
// writes are affected.
func TestLiveReplaceCampaignCommitErrorIsMarkedAmbiguous(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	briefID := insertBrief(ctx, t, pool)
	projectID := dbtest.UniqueID(t, "project")
	name := dbtest.UniqueID(t, "campaign")
	poison := name + "-fail-at-commit"

	var (
		id      string
		version int64
	)
	if err := pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status)
		VALUES ($1, $2, $3, $4, 'created')
		RETURNING id, version`,
		projectID, briefID, string(model.ProviderMetaAds), name).Scan(&id, &version); err != nil {
		t.Fatalf("seed a campaign: %v", err)
	}

	fn := "fail_commit_" + dbtest.UniqueID(t, "fn")
	fn = sanitizeIdent(fn)
	if _, err := pool.Exec(ctx, `CREATE FUNCTION `+fn+`() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.campaign_name = '`+poison+`' THEN RAISE EXCEPTION 'deferred failure at commit'; END IF;
			RETURN NULL;
		END $$`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE CONSTRAINT TRIGGER `+fn+` AFTER UPDATE ON campaigns
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION `+fn+`()`); err != nil {
		t.Fatalf("create deferred trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+fn+` ON campaigns`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`()`)
	})

	loaded, err := repo.GetCampaign(ctx, projectID, briefID, id)
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	stale := *loaded
	if _, err := repo.ReplaceCampaign(ctx, &stale, version+7, domain.CampaignLockToken{}, nil); !errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrWriteOutcomeUnknown) {
		t.Fatalf("a pre-commit miss = %v; want ErrPreconditionFailed without ErrWriteOutcomeUnknown", err)
	}
	loaded.CampaignName = poison
	if _, err := repo.ReplaceCampaign(ctx, loaded, version, domain.CampaignLockToken{}, nil); !errors.Is(err, domain.ErrWriteOutcomeUnknown) {
		t.Fatalf("a failed commit = %v; want it marked domain.ErrWriteOutcomeUnknown", err)
	}
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
