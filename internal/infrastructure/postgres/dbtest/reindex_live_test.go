// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// The reindex repo (#590) against the real tables: a live brief gets "updated", an archived
// one "deleted"; a live campaign "updated", a soft-deleted one "deleted", and a bare dispatch
// claim is skipped. A dry run writes nothing; -apply writes exactly one outbox row per
// re-indexed resource, built by the supplied builder; and the project filter scopes it.
func TestLiveReindexEnqueuesEveryResourceOnce(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	repo := postgres.NewReindexRepo(&postgres.Pool{Pool: pool})
	project := dbtest.UniqueID(t, "project")

	seedBrief := func(status string) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO campaign_briefs (project_id, program_type, event_slug, status)
			VALUES ($1, 'events', $2, $3) RETURNING id::text`, project, dbtest.UniqueID(t, "slug"), status).Scan(&id); err != nil {
			t.Fatalf("seed brief: %v", err)
		}
		return id
	}
	live, archived := seedBrief("draft"), seedBrief("archived")
	seedCampaign := func(status, platformID string, result []byte) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO campaigns (project_id, brief_id, platform, variant, campaign_name, status, platform_campaign_id, result)
			VALUES ($1, $2, 'google-ads', $3, 'n', $4, $5, $6) RETURNING id::text`,
			project, live, dbtest.UniqueID(t, "variant"), status, platformID, result).Scan(&id); err != nil {
			t.Fatalf("seed campaign: %v", err)
		}
		return id
	}
	liveCampaign := seedCampaign("created", "123", nil)
	deletedCampaign := seedCampaign("deleted", "456", nil)
	bareClaim := seedCampaign("pending", "", nil) // a bare claim
	t.Cleanup(func() {
		cctx, cancel := dbtest.CleanupContext()
		defer cancel()
		for _, q := range []string{
			`DELETE FROM index_outbox WHERE object_id IN (SELECT id::text FROM campaigns WHERE project_id=$1) OR object_id IN (SELECT id::text FROM campaign_briefs WHERE project_id=$1)`,
			`DELETE FROM campaigns WHERE project_id=$1`,
			`DELETE FROM campaign_briefs WHERE project_id=$1`,
		} {
			if _, err := pool.Exec(cctx, q, project); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})

	outboxRows := func() map[string]string {
		rows, err := pool.Query(ctx, `SELECT object_id, payload->>'action' FROM index_outbox
			WHERE object_id IN ($1, $2, $3, $4, $5)`, live, archived, liveCampaign, deletedCampaign, bareClaim)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var id, action string
			if err := rows.Scan(&id, &action); err != nil {
				t.Fatal(err)
			}
			if _, dup := got[id]; dup {
				t.Errorf("object %s enqueued twice", id)
			}
			got[id] = action
		}
		return got
	}
	buildBrief := func(action string, b *model.CampaignBrief) ([]byte, error) {
		return json.Marshal(map[string]string{"action": action, "id": b.ID})
	}
	buildCampaign := func(action string, c *model.Campaign) ([]byte, error) {
		return json.Marshal(map[string]string{"action": action, "id": c.ID})
	}

	// Dry run: counted, nothing written.
	b, err := repo.ReindexBriefs(ctx, project, "", 100, false, buildBrief)
	if err != nil || b.Read != 2 || b.Live != 1 || b.Deleted != 1 {
		t.Fatalf("dry-run briefs = %+v, %v", b, err)
	}
	c, err := repo.ReindexCampaigns(ctx, project, "", 100, false, buildCampaign)
	if err != nil || c.Read != 3 || c.Live != 1 || c.Deleted != 1 || c.Skipped != 1 {
		t.Fatalf("dry-run campaigns = %+v, %v", c, err)
	}
	if got := outboxRows(); len(got) != 0 {
		t.Fatalf("a dry run wrote %v", got)
	}

	// Another project's filter finds nothing of ours.
	if other, err := repo.ReindexBriefs(ctx, dbtest.UniqueID(t, "other"), "", 100, false, buildBrief); err != nil || other.Read != 0 {
		t.Fatalf("other project = %+v, %v", other, err)
	}

	// Apply, one row per batch, so paging is exercised.
	after := ""
	for {
		b, err := repo.ReindexBriefs(ctx, project, after, 1, true, buildBrief)
		if err != nil {
			t.Fatal(err)
		}
		if b.Read == 0 {
			break
		}
		after = b.LastID
	}
	applied, err := repo.ReindexCampaigns(ctx, project, "", 100, true, buildCampaign)
	if err != nil || applied.Read != 3 || applied.Live != 1 || applied.Deleted != 1 || applied.Skipped != 1 {
		t.Fatalf("applied campaigns = %+v, %v", applied, err)
	}
	want := map[string]string{live: "updated", archived: "deleted", liveCampaign: "updated", deletedCampaign: "deleted"}
	got := outboxRows()
	for id, action := range want {
		if got[id] != action {
			t.Errorf("object %s: outbox action %q, want %q", id, got[id], action)
		}
	}
	// The bare dispatch claim was never indexed by the service, so it gets no row here either.
	if action, ok := got[bareClaim]; ok {
		t.Errorf("bare claim %s was enqueued (%q); it must be skipped", bareClaim, action)
	}
}
