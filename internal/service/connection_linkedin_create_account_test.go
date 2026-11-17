// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

type createAccountDispatcher struct {
	acct  *model.LinkedInCreateAccount
	err   error
	calls int
}

func (createAccountDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("Dispatch must never be called by the create-account read")
}

func (d *createAccountDispatcher) CreateAccount(_ context.Context, _ string, _ model.Provider) (*model.LinkedInCreateAccount, error) {
	d.calls++
	return d.acct, d.err
}

type noCreateAccountDispatcher struct{}

func (noCreateAccountDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func createAccountService(d PlatformDispatcher) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(NewOrchestrator(&fakeCampaignRepo{byID: map[string]*model.Campaign{}}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: d}))
	return svc
}

// TestGetLinkedinAdsCreateAccount pins the create-account read's mapping (#581): source, the
// default flag on the first selectable account only, and the 404/400/reserved-scope answers.
func TestGetLinkedinAdsCreateAccount(t *testing.T) {
	ctx := context.Background()
	payload := &conn.GetLinkedinAdsCreateAccountPayload{ProjectID: "tlf"}
	disp := &createAccountDispatcher{acct: &model.LinkedInCreateAccount{
		AccountID: "538170226", OrgID: "208777", Label: "Linux Foundation system account", FromSystem: true,
		Status: model.StatusActive, SelectionEnabled: true,
		Selectable: []model.LinkedInAccountGrant{
			{AccountID: "538170226", OrgID: "208777", Label: "Linux Foundation system account"},
			{AccountID: "509430019", OrgID: "1111", Label: "LF Events"},
		},
	}}
	got, err := createAccountService(disp).GetLinkedinAdsCreateAccount(ctx, payload)
	if err != nil {
		t.Fatalf("GetLinkedinAdsCreateAccount: %v", err)
	}
	if got.AccountID != "538170226" || got.OrgID != "208777" || got.Source != "system" || got.Status != "active" || !got.AccountSelectionEnabled {
		t.Fatalf("result = %+v", got)
	}
	if len(got.SelectableAccounts) != 2 || !got.SelectableAccounts[0].Default || got.SelectableAccounts[1].Default ||
		got.SelectableAccounts[1].AccountID != "509430019" || got.SelectableAccounts[1].OrgID != "1111" || got.SelectableAccounts[1].Label != "LF Events" {
		t.Fatalf("selectable = %+v %+v", got.SelectableAccounts[0], got.SelectableAccounts[1])
	}

	disp.acct = &model.LinkedInCreateAccount{AccountID: "111", OrgID: "222", Status: model.StatusInactive,
		Selectable: []model.LinkedInAccountGrant{{AccountID: "111", OrgID: "222"}}}
	got, err = createAccountService(disp).GetLinkedinAdsCreateAccount(ctx, payload)
	if err != nil || got.Source != "project" || got.Status != "inactive" || got.AccountSelectionEnabled || len(got.SelectableAccounts) != 1 {
		t.Fatalf("own connection = %+v, %v", got, err)
	}

	disp.err = fmt.Errorf("no linkedin-ads connection: %w", domain.ErrNotFound)
	var nf *conn.NotFoundError
	if _, err := createAccountService(disp).GetLinkedinAdsCreateAccount(ctx, payload); !errors.As(err, &nf) {
		t.Fatalf("no connection = %T %v, want 404", err, err)
	}

	var bad *conn.BadRequestError
	if _, err := createAccountService(noCreateAccountDispatcher{}).GetLinkedinAdsCreateAccount(ctx, payload); !errors.As(err, &bad) {
		t.Fatalf("a dispatcher without the capability = %T %v, want 400", err, err)
	}

	disp.calls = 0
	if _, err := createAccountService(disp).GetLinkedinAdsCreateAccount(ctx, &conn.GetLinkedinAdsCreateAccountPayload{ProjectID: model.SystemProjectID}); err == nil || disp.calls != 0 {
		t.Fatalf("the reserved LF scope = %v (dispatcher called %d time(s)), want a refusal before it", err, disp.calls)
	}
}
