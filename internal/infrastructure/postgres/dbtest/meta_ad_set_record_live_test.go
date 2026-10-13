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
// then answers APPLIED — the "send" is counted, nothing leaves the process.
type pauseRecordDispatcher struct{ sent int }

func (d *pauseRecordDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *pauseRecordDispatcher) ToggleMetaAdSetStatus(ctx context.Context, _ string, _ model.Provider, _ *model.Campaign, adSetID, _ string, beforeWrite service.MetaAdSetBeforeWrite) (*model.MetaAdSetStatusResult, error) {
	if beforeWrite != nil {
		if err := beforeWrite(ctx); err != nil {
			return nil, fmt.Errorf("nothing was sent: %w", err)
		}
	}
	d.sent++
	return &model.MetaAdSetStatusResult{AdSetID: adSetID, Outcome: model.MetaAdSetApplied, PreviousStatus: "ACTIVE"}, nil
}

// failMarkerCommits installs a DEFERRED constraint trigger that fails the COMMIT of any update
// writing the deliberate-pause record onto campaign id — the first `failures` of them (a
// sequence counts, since a sequence is not rolled back with the failed transaction), or every one
// when failures is 0. The failed commit rolls back and leaves the session usable: exactly the
// ambiguous-but-rolled-back case.
func failMarkerCommits(ctx context.Context, t *testing.T, pool *pgxpool.Pool, campaignID string, failures int) {
	t.Helper()
	name := sanitizeIdent("fmc_" + dbtest.UniqueID(t, "trg"))
	cond := "TRUE"
	if failures > 0 {
		cond = fmt.Sprintf("nextval('%s') <= %d", name, failures)
	}
	stmts := []string{
		`CREATE SEQUENCE ` + name,
		`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.id::text = '` + campaignID + `' AND NEW.result::text LIKE '%UserPausedAdSetID%' AND ` + cond + ` THEN
					RAISE EXCEPTION 'deferred failure at commit';
				END IF;
				RETURN NULL;
			END $$`,
		`CREATE CONSTRAINT TRIGGER ` + name + ` AFTER UPDATE ON campaigns
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ` + name + `()`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("install commit failure: %v", err)
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

// The deadline a starved reconciliation would hit is persistResultTimeout (5s) per step; both
// cases below must finish well inside it.
const reconcileBudget = 4 * time.Second

// TestLiveMetaPauseRecordReconcilesOnTheClaimedConnection: the record's first COMMIT fails and
// rolls back with the session still usable, on a ONE-connection pool. The reconciliation must
// redo the write on the claimed connection (not a second pooled one), complete promptly, record
// the pause and send it.
func TestLiveMetaPauseRecordReconcilesOnTheClaimedConnection(t *testing.T) {
	shared := dbtest.Pool(t)
	ctx := context.Background()
	projectID, briefID, id, version := seedRecordedMetaCampaign(ctx, t, shared)
	failMarkerCommits(ctx, t, shared, id, 1)
	d := &pauseRecordDispatcher{}
	s, campaigns := onePoolService(ctx, t, d)

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	res, err := s.ToggleMetaAdSetStatus(callCtx, pausePayload(projectID, briefID, id, version))
	if elapsed := time.Since(start); elapsed > reconcileBudget {
		t.Fatalf("the reconciliation took %s — it waited on a pooled connection the claim pins", elapsed)
	}
	if err != nil {
		t.Fatalf("ToggleMetaAdSetStatus: %T %v", err, err)
	}
	if want := fmt.Sprintf("%q", fmt.Sprint(version+1)); res.Etag == nil || *res.Etag != want || d.sent != 1 {
		t.Fatalf("etag = %v (want %s), sent = %d", res.Etag, want, d.sent)
	}
	row, gerr := campaigns.GetCampaign(ctx, projectID, briefID, id)
	if gerr != nil || !model.MetaAdSetHeldPaused(row.Result, "888") || row.Version != version+1 {
		t.Fatalf("row after the call = %+v, %v; want the pause on record at version %d", row, gerr, version+1)
	}
}

// TestLiveMetaPauseRecordUnsettleableStillSendsThePauseAndReleasesTheClaim: every commit of the
// record fails, on a ONE-connection pool. The fail-safe holds — the pause is still sent and the
// answer is the 503 "could not be confirmed" — within bounded time, and the claim is released
// afterwards (a fresh claim at the row's version succeeds on the same one-connection pool).
func TestLiveMetaPauseRecordUnsettleableStillSendsThePauseAndReleasesTheClaim(t *testing.T) {
	shared := dbtest.Pool(t)
	ctx := context.Background()
	projectID, briefID, id, version := seedRecordedMetaCampaign(ctx, t, shared)
	failMarkerCommits(ctx, t, shared, id, 0)
	d := &pauseRecordDispatcher{}
	s, campaigns := onePoolService(ctx, t, d)

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := s.ToggleMetaAdSetStatus(callCtx, pausePayload(projectID, briefID, id, version))
	if elapsed := time.Since(start); elapsed > reconcileBudget {
		t.Fatalf("the reconciliation took %s — it waited on a pooled connection the claim pins", elapsed)
	}
	var unavailable *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(unavailable.Message, "could not be confirmed") || !strings.Contains(unavailable.Message, "WAS paused on Meta") {
		t.Fatalf("err = %T %v; want the 503 saying the pause was sent but its record could not be confirmed", err, err)
	}
	if d.sent != 1 {
		t.Fatalf("sent = %d; a pause whose record may have landed must be sent", d.sent)
	}
	claimCtx, ccancel := context.WithTimeout(ctx, 2*time.Second)
	defer ccancel()
	if _, tok, cerr := campaigns.ClaimCampaignVersion(claimCtx, projectID, briefID, id, version); cerr != nil {
		t.Fatalf("claim after the call: %v (the claim or its connection leaked)", cerr)
	} else {
		_ = campaigns.ReleaseCampaignLock(ctx, tok)
	}
}
