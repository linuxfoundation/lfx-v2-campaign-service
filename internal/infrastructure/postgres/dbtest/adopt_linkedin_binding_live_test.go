// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// liCampaignID mints a LinkedIn-shaped (numeric) campaign id unique to this run, so sub-tests
// never collide on each other's leftover rows under a GLOBAL index.
func liCampaignID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 10)
}

// TestLiveLinkedInCampaignAdoptedIntoTwoBriefsConcurrently is the invariant migration 000042
// exists for: the SAME LinkedIn campaign adopted into two briefs — in two different projects,
// at the same moment — yields exactly one binding and one ErrPlatformCampaignAlreadyBound.
// Adoption's ON CONFLICT arbitrates only the destination slot, so before 000042 both inserts
// succeeded and two briefs controlled one paid campaign.
func TestLiveLinkedInCampaignAdoptedIntoTwoBriefsConcurrently(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	briefA, projectA := insertApprovedBrief(ctx, t, pool)
	briefB, projectB := insertApprovedBrief(ctx, t, pool)
	upstream := liCampaignID()

	adopt := func(project, brief string) error {
		_, err := repo.AdoptCampaign(ctx, &model.Campaign{
			ProjectID: project, BriefID: brief, Platform: model.ProviderLinkedInAds,
			Variant: model.VariantDefault, PlatformCampaignID: upstream,
			CampaignName: dbtest.UniqueID(t, "campaign"), Status: model.CampaignStatusCreated,
		}, 1, nil)
		return err
	}

	var (
		wg   sync.WaitGroup
		errs [2]error
		gate = make(chan struct{})
	)
	for i, pb := range [2][2]string{{projectA, briefA}, {projectB, briefB}} {
		wg.Add(1)
		go func(i int, project, brief string) {
			defer wg.Done()
			<-gate
			errs[i] = adopt(project, brief)
		}(i, pb[0], pb[1])
	}
	close(gate)
	wg.Wait()

	var ok, bound int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrPlatformCampaignAlreadyBound):
			bound++
		default:
			t.Errorf("unexpected adoption error: %v", err)
		}
	}
	if ok != 1 || bound != 1 {
		t.Fatalf("successes = %d, AlreadyBound = %d (errs %v); want exactly one of each — two briefs now control one LinkedIn campaign", ok, bound, errs)
	}

	var live int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM campaigns WHERE platform = 'linkedin-ads' AND platform_campaign_id = $1 AND status <> 'deleted'`, upstream).Scan(&live); err != nil {
		t.Fatalf("count live bindings: %v", err)
	}
	if live != 1 {
		t.Fatalf("live rows binding LinkedIn campaign %s = %d, want 1", upstream, live)
	}
}

// The dispatch path: a LinkedIn dispatch whose create resolved to a campaign another brief
// already binds is refused by the same index, and UpsertCampaign names it
// ErrPlatformCampaignAlreadyBound so the orchestrator reports a genuine duplicate rather than an
// anonymous persist failure. A soft-deleted binding frees the id.
func TestLiveLinkedInDispatchOntoABoundCampaignIsAlreadyBound(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})

	briefA, projectA := insertApprovedBrief(ctx, t, pool)
	briefB, projectB := insertApprovedBrief(ctx, t, pool)
	upstream := liCampaignID()

	row := func(project, brief string) *model.Campaign {
		return &model.Campaign{
			ProjectID: project, BriefID: brief, Platform: model.ProviderLinkedInAds,
			Variant: model.VariantDefault, SlotVersion: 1, PlatformCampaignID: upstream,
			CampaignName: dbtest.UniqueID(t, "campaign"), Status: model.CampaignStatusCreated,
		}
	}
	if _, err := repo.UpsertCampaign(ctx, row(projectA, briefA), nil); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	_, err := repo.UpsertCampaign(ctx, row(projectB, briefB), nil)
	if !errors.Is(err, domain.ErrPlatformCampaignAlreadyBound) {
		t.Fatalf("second dispatch onto the same LinkedIn campaign: err = %v, want ErrPlatformCampaignAlreadyBound", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE campaigns SET status = 'deleted' WHERE brief_id = $1`, briefA); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	if _, err := repo.UpsertCampaign(ctx, row(projectB, briefB), nil); err != nil {
		t.Fatalf("dispatch after the first binding was soft-deleted: %v — deletion must free the id", err)
	}
}

// The raw schema, so the guard holds whichever Go path writes the row: LinkedIn ids are
// globally unique, so the same id in two projects is refused (23505 on 000042's index), while
// an unrelated provider (Meta) with the same id string is still not constrained by it.
func TestLiveMigration000042_IsLinkedInScopedAndGlobal(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)

	t.Run("a different project may not bind the same LinkedIn id", func(t *testing.T) {
		id := liCampaignID()
		projectA, projectB := dbtest.UniqueID(t, "projectA"), dbtest.UniqueID(t, "projectB")
		briefA := insertBriefIn(ctx, t, pool, projectA, dbtest.UniqueID(t, "slug"))
		briefB := insertBriefIn(ctx, t, pool, projectB, dbtest.UniqueID(t, "slug"))
		if err := insertBindingOn(ctx, t, pool, model.ProviderLinkedInAds, projectA, briefA, &id, testCampaignStatus); err != nil {
			t.Fatalf("project A binding: %v", err)
		}
		err := insertBindingOn(ctx, t, pool, model.ProviderLinkedInAds, projectB, briefB, &id, testCampaignStatus)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != sqlstateUniqueViolation || pgErr.ConstraintName != "uq_campaigns_linkedin_campaign_live" {
			t.Fatalf("err = %v, want 23505 on uq_campaigns_linkedin_campaign_live", err)
		}
	})
	t.Run("the same id on another platform is not constrained by the LinkedIn index", func(t *testing.T) {
		id := liCampaignID()
		projectA, projectB := dbtest.UniqueID(t, "projectA"), dbtest.UniqueID(t, "projectB")
		briefA := insertBriefIn(ctx, t, pool, projectA, dbtest.UniqueID(t, "slug"))
		briefB := insertBriefIn(ctx, t, pool, projectB, dbtest.UniqueID(t, "slug"))
		if err := insertBindingOn(ctx, t, pool, model.ProviderLinkedInAds, projectA, briefA, &id, testCampaignStatus); err != nil {
			t.Fatalf("linkedin binding: %v", err)
		}
		if err := insertBindingOn(ctx, t, pool, model.ProviderMetaAds, projectB, briefB, &id, testCampaignStatus); err != nil {
			t.Fatalf("meta binding of the same id string: %v — 000042 must not reach beyond linkedin-ads", err)
		}
	})
}
