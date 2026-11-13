// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// blockWait is how long a test waits to decide that a blocked statement is really blocked.
const blockWait = 300 * time.Millisecond

// reindexOrderCase is one resource kind for the FOR SHARE ordering tests (#590): how to seed a
// live row, the terminal write a write path makes (update the row and enqueue its "deleted"
// message in one transaction), and how to run one reindex batch over the project.
type reindexOrderCase struct {
	name       string
	objectType string
	seed       func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, project string) string
	terminal   string // UPDATE ... WHERE id = $1 that makes the row terminal
	reindex    func(ctx context.Context, repo *postgres.ReindexRepo, project string, build func(action, id string) ([]byte, error)) error
}

func reindexOrderCases() []reindexOrderCase {
	seedBrief := func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, project string) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO campaign_briefs (project_id, program_type, event_slug, status)
			VALUES ($1, 'events', $2, 'draft') RETURNING id::text`, project, dbtest.UniqueID(t, "slug")).Scan(&id); err != nil {
			t.Fatalf("seed brief: %v", err)
		}
		return id
	}
	return []reindexOrderCase{
		{
			name:       "brief",
			objectType: "campaign_brief",
			seed:       seedBrief,
			terminal:   `UPDATE campaign_briefs SET status = 'archived' WHERE id = $1::uuid`,
			reindex: func(ctx context.Context, repo *postgres.ReindexRepo, project string, build func(string, string) ([]byte, error)) error {
				_, err := repo.ReindexBriefs(ctx, project, "", 100, true, func(action string, b *model.CampaignBrief) ([]byte, error) {
					return build(action, b.ID)
				})
				return err
			},
		},
		{
			name:       "campaign",
			objectType: "campaign",
			seed: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, project string) string {
				brief := seedBrief(t, ctx, pool, project)
				var id string
				if err := pool.QueryRow(ctx, `INSERT INTO campaigns (project_id, brief_id, platform, variant, campaign_name, status, platform_campaign_id)
					VALUES ($1, $2, 'google-ads', $3, 'n', 'created', $4) RETURNING id::text`,
					project, brief, dbtest.UniqueID(t, "variant"), dbtest.UniqueID(t, "upstream")).Scan(&id); err != nil {
					t.Fatalf("seed campaign: %v", err)
				}
				return id
			},
			terminal: `UPDATE campaigns SET status = 'deleted' WHERE id = $1::uuid`,
			reindex: func(ctx context.Context, repo *postgres.ReindexRepo, project string, build func(string, string) ([]byte, error)) error {
				_, err := repo.ReindexCampaigns(ctx, project, "", 100, true, func(action string, c *model.Campaign) ([]byte, error) {
					return build(action, c.ID)
				})
				return err
			},
		},
	}
}

func reindexOrderSetup(t *testing.T, tc reindexOrderCase) (context.Context, *pgxpool.Pool, *postgres.ReindexRepo, string, string) {
	t.Helper()
	pool := dbtest.Pool(t)
	ctx := context.Background()
	project := dbtest.UniqueID(t, "project")
	id := tc.seed(t, ctx, pool, project)
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
	return ctx, pool, postgres.NewReindexRepo(&postgres.Pool{Pool: pool}), project, id
}

// outboxActions returns the object's outbox actions in id order.
func outboxActions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, objectType, id string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT payload->>'action' FROM index_outbox
		WHERE object_type = $1 AND object_id = $2 ORDER BY id`, objectType, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		got = append(got, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func payloadOf(action, id string) ([]byte, error) {
	return json.Marshal(map[string]string{"action": action, "id": id})
}

// A write in flight holds the row lock: the reindex batch must wait for it to commit and then
// re-send the COMMITTED (terminal) state, after the write's own message. Without FOR SHARE the
// batch reads the old live row and appends a stale "updated" after the write's "deleted".
func TestLiveReindexWaitsForWriteInFlight(t *testing.T) {
	for _, tc := range reindexOrderCases() {
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool, repo, project, id := reindexOrderSetup(t, tc)

			writer, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Rollback(ctx) }()
			if _, err := writer.Exec(ctx, tc.terminal, id); err != nil {
				t.Fatalf("terminal write: %v", err)
			}
			if _, err := writer.Exec(ctx, `INSERT INTO index_outbox (object_type, object_id, payload) VALUES ($1, $2, $3)`,
				tc.objectType, id, `{"action":"deleted"}`); err != nil {
				t.Fatalf("write enqueue: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- tc.reindex(ctx, repo, project, payloadOf) }()
			select {
			case err := <-done:
				t.Fatalf("reindex batch finished (%v) while a write held the row lock; FOR SHARE is missing", err)
			case <-time.After(blockWait):
			}
			if err := writer.Commit(ctx); err != nil {
				t.Fatalf("commit write: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("reindex: %v", err)
			}

			got := outboxActions(t, ctx, pool, tc.objectType, id)
			if len(got) != 2 || got[0] != "deleted" || got[1] != "deleted" {
				t.Fatalf("outbox actions = %v, want [deleted deleted]: the batch must re-send the committed state after the write", got)
			}
		})
	}
}

// A batch in flight holds FOR SHARE: a write that starts during it must wait for the batch to
// commit, so the write's "deleted" is enqueued after the batch's "updated" and is the last
// message for the resource.
func TestLiveReindexWriteWaitsForBatch(t *testing.T) {
	for _, tc := range reindexOrderCases() {
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool, repo, project, id := reindexOrderSetup(t, tc)

			building := make(chan struct{})
			var once sync.Once
			release := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- tc.reindex(ctx, repo, project, func(action, oid string) ([]byte, error) {
					once.Do(func() { close(building) }) // the batch has read (and share-locked) the row
					<-release
					return payloadOf(action, oid)
				})
			}()
			select {
			case <-building:
			case err := <-done:
				t.Fatalf("reindex finished before building a payload: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("reindex never reached the payload builder")
			}

			writeDone := make(chan error, 1)
			go func() {
				tx, err := pool.Begin(ctx)
				if err != nil {
					writeDone <- err
					return
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if _, err := tx.Exec(ctx, tc.terminal, id); err != nil {
					writeDone <- err
					return
				}
				if _, err := tx.Exec(ctx, `INSERT INTO index_outbox (object_type, object_id, payload) VALUES ($1, $2, $3)`,
					tc.objectType, id, `{"action":"deleted"}`); err != nil {
					writeDone <- err
					return
				}
				writeDone <- tx.Commit(ctx)
			}()
			select {
			case err := <-writeDone:
				close(release)
				<-done
				t.Fatalf("write finished (%v) while the batch held FOR SHARE; FOR SHARE is missing", err)
			case <-time.After(blockWait):
			}

			close(release)
			if err := <-done; err != nil {
				t.Fatalf("reindex: %v", err)
			}
			if err := <-writeDone; err != nil {
				t.Fatalf("write: %v", err)
			}

			got := outboxActions(t, ctx, pool, tc.objectType, id)
			if len(got) != 2 || got[0] != "updated" || got[1] != "deleted" {
				t.Fatalf("outbox actions = %v, want [updated deleted]: the write's message must come last", got)
			}
		})
	}
}
