// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// adoptE2ECampaignRepo answers every slot lookup "free" and the brief as holding no campaign;
// every other method is the embedded nil interface, so a refused adoption that reached a write
// would panic.
type adoptE2ECampaignRepo struct{ domain.CampaignRepository }

func (adoptE2ECampaignRepo) GetCampaignByPlatform(context.Context, string, string, model.Provider, string) (*model.Campaign, error) {
	return nil, domain.ErrNotFound
}

func (adoptE2ECampaignRepo) ListCampaignsForBrief(context.Context, string, string) ([]*model.Campaign, error) {
	return nil, nil
}

type adoptE2EVerifier struct{}

func (adoptE2EVerifier) VerifyActor(context.Context, string) (*model.Actor, error) {
	return &model.Actor{Name: "Ada Lovelace", Email: "ada@lf.dev", Username: "ada"}, nil
}

type adoptE2EMarketingOps struct{}

func (adoptE2EMarketingOps) IsGlobalMarketingOps(context.Context, string, string) (bool, error) {
	return true, nil
}

// TestLinkedIn_AdoptLookupSystemCredentialRejected drives the REAL LinkedIn dispatcher through
// BriefService.AdoptCampaign with LinkedIn answering the campaign read 401 (an expired or revoked
// token). With account_id the credential is the LF system row's, so the fault is the operator's:
// ErrSystemConnectionNotUsable and a 500 — never the 409 that tells the caller to repair the
// project's connection. Without account_id the project's own row was used and the 409 stands.
func TestLinkedIn_AdoptLookupSystemCredentialRejected(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"status":401,"serviceErrorCode":65602,"message":"The token used in the request has expired"}`)
	}))
	t.Cleanup(srv.Close)

	own := activeLinkedInConn(goodLinkedInCreds)
	own.ID, own.Version, own.ProjectID = "own-li", 1, "cncf"
	repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow(), "cncf": own}}

	adopt := func(t *testing.T, account *string) (error, error) {
		t.Helper()
		d := NewLinkedInDispatcher(repo, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
		ctx := context.Background()
		if account != nil {
			ctx = model.WithAdoptionAccount(ctx, *account)
		}
		_, lerr := d.LookupCampaign(ctx, "cncf", model.ProviderLinkedInAds, "312")

		brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf", EventSlug: "kubecon", Status: model.BriefApproved,
			DeliveryType: model.DeliveryPaidMarketing, Version: 1}
		jobs := &e2eJobRepo{}
		camps := adoptE2ECampaignRepo{}
		orch := service.NewOrchestrator(camps, jobs, map[model.Provider]service.PlatformDispatcher{model.ProviderLinkedInAds: d})
		s := service.NewBriefService(e2eBriefRepo{brief: brief}, camps, jobs, orch)
		s.SetTokenVerifier(adoptE2EVerifier{})
		s.SetMarketingOpsChecker(adoptE2EMarketingOps{})
		actx, aerr := s.JWTAuth(context.Background(), "token", nil)
		if aerr != nil {
			t.Fatalf("JWTAuth: %v", aerr)
		}
		_, serr := s.AdoptCampaign(actx, &briefs.AdoptCampaignPayload{ProjectID: "cncf", BriefID: "b1",
			Platform: string(model.ProviderLinkedInAds), PlatformCampaignID: "312", AccountID: account})
		return lerr, serr
	}

	t.Run("account_id: the LF system credential is the operator's fault", func(t *testing.T) {
		account := "500928401"
		lerr, serr := adopt(t, &account)
		if !errors.Is(lerr, domain.ErrSystemConnectionNotUsable) || !errors.Is(lerr, linkedin.ErrCredentialsExpired) {
			t.Fatalf("LookupCampaign err = %v, want ErrSystemConnectionNotUsable wrapping ErrCredentialsExpired", lerr)
		}
		var ise *briefs.InternalServerError
		if !errors.As(serr, &ise) || ise.Code != "500" {
			t.Fatalf("AdoptCampaign = %T %v, want the operator-owned 500", serr, serr)
		}
	})
	t.Run("no account_id: the project's own credential stays the caller's 409", func(t *testing.T) {
		lerr, serr := adopt(t, nil)
		if errors.Is(lerr, domain.ErrSystemConnectionNotUsable) || !errors.Is(lerr, domain.ErrConnectionNotUsable) {
			t.Fatalf("LookupCampaign err = %v, want a project-scoped ErrConnectionNotUsable", lerr)
		}
		var conflict *briefs.ConflictError
		if !errors.As(serr, &conflict) || conflict.Code != "409" {
			t.Fatalf("AdoptCampaign = %T %v, want 409", serr, serr)
		}
	})
}
