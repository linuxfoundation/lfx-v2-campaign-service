// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// bindingProvider is one provider's binding index (000043-000046) as a test sees it: how its
// result JSON records the ad account, and whether the key is global or per account.
type bindingProvider struct {
	platform model.Provider
	// result builds the provenance blob the provider's adapter writes for account acct.
	result        func(acct string) json.RawMessage
	accountScoped bool
}

var bindingProviders = []bindingProvider{
	{model.ProviderMetaAds, func(a string) json.RawMessage { return mustJSON(map[string]string{"AccountID": "act_" + a}) }, false},
	{model.ProviderMicrosoftAds, func(a string) json.RawMessage { return mustJSON(map[string]string{"accountId": a}) }, true},
	{model.ProviderRedditAds, func(a string) json.RawMessage { return mustJSON(map[string]string{"accountId": "t2_" + a}) }, true},
	{model.ProviderTwitterAds, func(a string) json.RawMessage { return mustJSON(map[string]string{"AccountID": "acc" + a}) }, true},
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

var bindingSeq atomic.Int64

// uniqueUpstreamID mints a run-unique numeric id: the binding indexes span the whole shared schema.
func uniqueUpstreamID() string {
	return strconv.FormatInt(time.Now().UnixNano()+bindingSeq.Add(1), 10)
}

func adoptRow(t *testing.T, p bindingProvider, project, brief, upstream string, result json.RawMessage) *model.Campaign {
	return &model.Campaign{
		ProjectID: project, BriefID: brief, Platform: p.platform, Variant: model.VariantDefault,
		PlatformCampaignID: upstream, CampaignName: dbtest.UniqueID(t, "campaign"),
		Status: model.CampaignStatusCreated, Result: result,
	}
}

// adoptConcurrently adopts the same upstream id into two briefs (two projects) at once.
func adoptConcurrently(ctx context.Context, t *testing.T, repo *postgres.CampaignRepo, p bindingProvider, upstream string, results [2]json.RawMessage) [2]error {
	t.Helper()
	pool := dbtest.Pool(t)
	var briefs [2][2]string
	for i := range briefs {
		b, pr := insertApprovedBrief(ctx, t, pool)
		briefs[i] = [2]string{pr, b}
	}
	var (
		wg   sync.WaitGroup
		errs [2]error
		gate = make(chan struct{})
	)
	for i := range briefs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			_, errs[i] = repo.AdoptCampaign(ctx, adoptRow(t, p, briefs[i][0], briefs[i][1], upstream, results[i]), 1, nil)
		}(i)
	}
	close(gate)
	wg.Wait()
	return errs
}

// TestLiveAdoptionBindsOneUpstreamCampaignToOneBrief: per provider, the same upstream campaign
// adopted into two briefs concurrently (SAME account) yields exactly one success and one
// ErrPlatformCampaignAlreadyBound. For the account-keyed providers the same id under two
// DIFFERENT accounts is two campaigns, and both bind.
func TestLiveAdoptionBindsOneUpstreamCampaignToOneBrief(t *testing.T) {
	ctx := context.Background()
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: dbtest.Pool(t)})
	for _, p := range bindingProviders {
		t.Run(string(p.platform)+"/same account", func(t *testing.T) {
			acct := uniqueUpstreamID()
			errs := adoptConcurrently(ctx, t, repo, p, uniqueUpstreamID(), [2]json.RawMessage{p.result(acct), p.result(acct)})
			var ok, bound int
			for _, err := range errs {
				switch {
				case err == nil:
					ok++
				case errors.Is(err, domain.ErrPlatformCampaignAlreadyBound):
					bound++
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}
			if ok != 1 || bound != 1 {
				t.Fatalf("successes = %d, AlreadyBound = %d (%v); want exactly one of each", ok, bound, errs)
			}
		})
		if !p.accountScoped {
			continue
		}
		t.Run(string(p.platform)+"/different accounts", func(t *testing.T) {
			errs := adoptConcurrently(ctx, t, repo, p, uniqueUpstreamID(), [2]json.RawMessage{p.result(uniqueUpstreamID()), p.result(uniqueUpstreamID())})
			for i, err := range errs {
				if err != nil {
					t.Errorf("adoption %d under its own account: %v — account-scoped ids in two accounts are two campaigns", i, err)
				}
			}
		})
	}
}

// The dispatch path: a second row for the same upstream campaign (same account) is refused by the
// provider's index and named ErrPlatformCampaignAlreadyBound by UpsertCampaign.
func TestLiveDispatchOntoABoundCampaignIsAlreadyBoundPerProvider(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	for _, p := range bindingProviders {
		t.Run(string(p.platform), func(t *testing.T) {
			upstream, acct := uniqueUpstreamID(), uniqueUpstreamID()
			briefA, projectA := insertApprovedBrief(ctx, t, pool)
			briefB, projectB := insertApprovedBrief(ctx, t, pool)
			first := adoptRow(t, p, projectA, briefA, upstream, p.result(acct))
			first.SlotVersion = 1
			if _, err := repo.UpsertCampaign(ctx, first, nil); err != nil {
				t.Fatalf("first dispatch: %v", err)
			}
			second := adoptRow(t, p, projectB, briefB, upstream, p.result(acct))
			second.SlotVersion = 1
			if _, err := repo.UpsertCampaign(ctx, second, nil); !errors.Is(err, domain.ErrPlatformCampaignAlreadyBound) {
				t.Fatalf("second dispatch: err = %v, want ErrPlatformCampaignAlreadyBound", err)
			}
		})
	}
}

// LEGACY rows on the account-keyed indexes: a live row recording NO account is NULL in the key,
// so it never blocks a legitimate binding (a dispatch under a known account inserts), but
// adoption refuses to bind an id such a row already holds rather than slip a second binding past
// the index.
func TestLiveLegacyAccountlessRowsNeitherBlockNorSlipThrough(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewCampaignRepo(&postgres.Pool{Pool: pool})
	for _, p := range bindingProviders {
		if !p.accountScoped {
			continue
		}
		t.Run(string(p.platform), func(t *testing.T) {
			upstream := uniqueUpstreamID()
			legacyProject := dbtest.UniqueID(t, "legacy")
			legacyBrief := insertBriefIn(ctx, t, pool, legacyProject, dbtest.UniqueID(t, "slug"))
			if _, err := pool.Exec(ctx, `INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status, platform_campaign_id, result)
				VALUES ($1, $2, $3, $4, 'created', $5, '{}'::jsonb)`,
				legacyProject, legacyBrief, string(p.platform), dbtest.UniqueID(t, "campaign"), upstream); err != nil {
				t.Fatalf("seed an account-less legacy row: %v", err)
			}

			brief, project := insertApprovedBrief(ctx, t, pool)
			if _, err := repo.AdoptCampaign(ctx, adoptRow(t, p, project, brief, upstream, p.result(uniqueUpstreamID())), 1, nil); !errors.Is(err, domain.ErrPlatformCampaignAlreadyBound) {
				t.Fatalf("adopting an id an account-less live row holds: err = %v, want ErrPlatformCampaignAlreadyBound", err)
			}

			dispBrief, dispProject := insertApprovedBrief(ctx, t, pool)
			row := adoptRow(t, p, dispProject, dispBrief, upstream, p.result(uniqueUpstreamID()))
			row.SlotVersion = 1
			if _, err := repo.UpsertCampaign(ctx, row, nil); err != nil {
				t.Fatalf("a dispatch under a known account was blocked by an account-less legacy row: %v", err)
			}
		})
	}
}
