// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// pauseRecordDispatcher is a Meta ad-set toggler that runs the service's beforeWrite hook and
// then answers APPLIED — the "send" is counted, nothing leaves the process. hookDeadline, when
// set, is the deadline the hook runs under: a COMMIT still awaited when it passes loses its
// acknowledgement, the ambiguous case.
type pauseRecordDispatcher struct {
	sent         int
	hookDeadline time.Duration
}

func (d *pauseRecordDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *pauseRecordDispatcher) ToggleMetaAdSetStatus(ctx context.Context, _ string, _ model.Provider, _ *model.Campaign, adSetID, _ string, beforeWrite service.MetaAdSetBeforeWrite) (*model.MetaAdSetStatusResult, error) {
	if beforeWrite != nil {
		hctx := ctx
		if d.hookDeadline > 0 {
			var cancel context.CancelFunc
			hctx, cancel = context.WithTimeout(ctx, d.hookDeadline)
			defer cancel()
		}
		if err := beforeWrite(hctx); err != nil {
			return nil, fmt.Errorf("nothing was sent: %w", err)
		}
	}
	d.sent++
	return &model.MetaAdSetStatusResult{AdSetID: adSetID, Outcome: model.MetaAdSetApplied, PreviousStatus: "ACTIVE"}, nil
}

// markerCommitTrigger installs a DEFERRED constraint trigger that runs at the COMMIT of any
// update writing the deliberate-pause record onto campaign id: the FIRST such commit runs first
// (a sequence counts — it is not rolled back with a failed transaction), every later one runs
// later. Each is a PL/pgSQL statement list, or "" for nothing. Only this row is affected.
func markerCommitTrigger(ctx context.Context, t *testing.T, pool *pgxpool.Pool, campaignID, first, later string) {
	t.Helper()
	name := sanitizeIdent("fmc_" + dbtest.UniqueID(t, "trg"))
	stmts := []string{
		`CREATE SEQUENCE ` + name,
		`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.id::text = '` + campaignID + `' AND NEW.result::text LIKE '%UserPausedAdSetID%' THEN
					IF nextval('` + name + `') = 1 THEN ` + first + ` ELSE ` + later + ` END IF;
				END IF;
				RETURN NULL;
			END $$`,
		`CREATE CONSTRAINT TRIGGER ` + name + ` AFTER UPDATE ON campaigns
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ` + name + `()`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("install commit trigger: %v", err)
		}
	}
	t.Cleanup(func() {
		cctx, cancel := dbtest.CleanupContext()
		defer cancel()
		_, _ = pool.Exec(cctx, `DROP TRIGGER IF EXISTS `+name+` ON campaigns`)
		_, _ = pool.Exec(cctx, `DROP FUNCTION IF EXISTS `+name+`()`)
		_, _ = pool.Exec(cctx, `DROP SEQUENCE IF EXISTS `+name)
	})
}

const (
	commitRaise = `RAISE EXCEPTION 'deferred failure at commit';`
	commitSlow  = `PERFORM pg_sleep(1.5);`
	commitOK    = `NULL;`
)

// onePoolService wires a BriefService over a pool of ONE connection — the claim pins it, so any
// second pooled connection taken while the claim is held would starve until its deadline.
func onePoolService(ctx context.Context, t *testing.T, d *pauseRecordDispatcher) (*service.BriefService, *postgres.CampaignRepo) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dbtest.DSN())
	if err != nil {
		t.Fatalf("parse DSN: %s", dbtest.SafeDSNErr(err))
	}
	cfg.MaxConns = 1
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open a one-connection pool: %s", dbtest.SafeDSNErr(err))
	}
	t.Cleanup(small.Close)
	pool := &postgres.Pool{Pool: small}
	campaigns := postgres.NewCampaignRepo(pool)
	jobs := postgres.NewJobRepo(pool)
	orch := service.NewOrchestrator(campaigns, jobs, map[model.Provider]service.PlatformDispatcher{model.ProviderMetaAds: d})
	return service.NewBriefService(postgres.NewBriefRepo(pool), campaigns, jobs, orch), campaigns
}

func seedRecordedMetaCampaign(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (projectID, briefID, id string, version int64) {
	t.Helper()
	briefID = insertBrief(ctx, t, pool)
	projectID = dbtest.UniqueID(t, "project")
	if err := pool.QueryRow(ctx, `
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, platform_campaign_id, result)
		VALUES ($1, $2, $3, $4, 'created', '555', '{"AccountID":"act_777","CampaignID":"555","AdSetID":"888"}')
		RETURNING id, version`,
		projectID, briefID, string(model.ProviderMetaAds), dbtest.UniqueID(t, "campaign")).Scan(&id, &version); err != nil {
		t.Fatalf("seed a meta campaign: %v", err)
	}
	return projectID, briefID, id, version
}

func pausePayload(projectID, briefID, id string, version int64) *briefs.ToggleMetaAdSetStatusPayload {
	etag := fmt.Sprint(version)
	return &briefs.ToggleMetaAdSetStatusPayload{ProjectID: projectID, BriefID: briefID, CampaignID: id, AdSetID: "888", Status: "PAUSED", IfMatch: &etag}
}

// The deadline a starved reconciliation would hit is persistResultTimeout (5s) per step; every
// case below must finish well inside it.
const reconcileBudget = 4 * time.Second

// claimReleased proves the call left no claim (or connection) behind: a fresh claim at the row's
// current version succeeds on the same one-connection pool.
func claimReleased(ctx context.Context, t *testing.T, campaigns *postgres.CampaignRepo, projectID, briefID, id string) {
	t.Helper()
	row, err := campaigns.GetCampaign(ctx, projectID, briefID, id)
	if err != nil {
		t.Fatalf("read after the call: %v", err)
	}
	claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, tok, cerr := campaigns.ClaimCampaignVersion(claimCtx, projectID, briefID, id, row.Version)
	if cerr != nil {
		t.Fatalf("claim after the call: %v (the claim or its connection leaked)", cerr)
	}
	_ = campaigns.ReleaseCampaignLock(ctx, tok)
}

func timedPause(ctx context.Context, t *testing.T, s *service.BriefService, projectID, briefID, id string, version int64) (*briefs.MetaAdSetStatusChange, error) {
	t.Helper()
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	res, err := s.ToggleMetaAdSetStatus(callCtx, pausePayload(projectID, briefID, id, version))
	if elapsed := time.Since(start); elapsed > reconcileBudget {
		t.Fatalf("the call took %s — the reconciliation waited on a pooled connection the claim pins", elapsed)
	}
	return res, err
}

// The server REJECTS the record's commit: a confirmed rollback, so nothing is on record and the
// pause is NOT sent.
func TestLiveMetaPauseRecordConfirmedRollbackSendsNothing(t *testing.T) {
	shared := dbtest.Pool(t)
	ctx := context.Background()
	projectID, briefID, id, version := seedRecordedMetaCampaign(ctx, t, shared)
	markerCommitTrigger(ctx, t, shared, id, commitRaise, commitRaise)
	d := &pauseRecordDispatcher{}
	s, campaigns := onePoolService(ctx, t, d)

	_, err := timedPause(ctx, t, s, projectID, briefID, id, version)
	var unavailable *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(unavailable.Message, "nothing was changed on Meta") || d.sent != 0 {
		t.Fatalf("err = %T %v, sent = %d; want the not-recorded 503 and nothing sent", err, err, d.sent)
	}
	row, gerr := campaigns.GetCampaign(ctx, projectID, briefID, id)
	if gerr != nil || model.MetaAdSetHeldPaused(row.Result, "888") || row.Version != version {
		t.Fatalf("row = %+v, %v; want it unchanged", row, gerr)
	}
	claimReleased(ctx, t, campaigns, projectID, briefID, id)
}

// The record's commit loses its acknowledgement (the hook's deadline passes while it is
// awaited) on a ONE-connection pool. The reconciliation must not wait on a second pooled
// connection: it completes promptly, the pause is SENT, and the record ends up on the row.
func TestLiveMetaPauseRecordReconcilesWithoutASecondPooledConnection(t *testing.T) {
	shared := dbtest.Pool(t)
	ctx := context.Background()
	projectID, briefID, id, version := seedRecordedMetaCampaign(ctx, t, shared)
	markerCommitTrigger(ctx, t, shared, id, commitSlow, commitOK)
	d := &pauseRecordDispatcher{hookDeadline: 300 * time.Millisecond}
	s, campaigns := onePoolService(ctx, t, d)

	_, err := timedPause(ctx, t, s, projectID, briefID, id, version)
	if d.sent != 1 {
		t.Fatalf("sent = %d (err %v); a pause whose record may have landed must be sent", d.sent, err)
	}
	if err != nil {
		t.Logf("answer: %T %v", err, err)
	}
	row, gerr := campaigns.GetCampaign(ctx, projectID, briefID, id)
	if gerr != nil || !model.MetaAdSetHeldPaused(row.Result, "888") {
		t.Fatalf("row = %+v, %v; want the pause on record", row, gerr)
	}
	claimReleased(ctx, t, campaigns, projectID, briefID, id)
}

// The record's first commit is ambiguous (pgx then closes the claimed connection, so the claim
// is released and re-taken), and the server REJECTS the write on the re-taken claim. Re-claimed
// at the original version, the first attempt is proven not to have landed, so that rejection is
// definite: the pause is NOT sent, nothing is on record, promptly, and the claim is released.
func TestLiveMetaPauseRecordRejectedOnTheReTakenClaimSendsNothing(t *testing.T) {
	shared := dbtest.Pool(t)
	ctx := context.Background()
	projectID, briefID, id, version := seedRecordedMetaCampaign(ctx, t, shared)
	markerCommitTrigger(ctx, t, shared, id, commitSlow, commitRaise)
	d := &pauseRecordDispatcher{hookDeadline: 300 * time.Millisecond}
	s, campaigns := onePoolService(ctx, t, d)

	_, err := timedPause(ctx, t, s, projectID, briefID, id, version)
	var unavailable *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(unavailable.Message, "nothing was changed on Meta") || d.sent != 0 {
		t.Fatalf("err = %T %v, sent = %d; want the not-recorded 503 and nothing sent", err, err, d.sent)
	}
	row, gerr := campaigns.GetCampaign(ctx, projectID, briefID, id)
	if gerr != nil || model.MetaAdSetHeldPaused(row.Result, "888") {
		t.Fatalf("row = %+v, %v; want no record", row, gerr)
	}
	claimReleased(ctx, t, campaigns, projectID, briefID, id)
}
