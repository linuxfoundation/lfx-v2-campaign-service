// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// LinkedIn Lead Gen Form picker (#664).

type mockLinkedInLeadFormDispatcher struct {
	res   *model.LinkedInLeadForms
	calls int
	acct  string
}

func (m *mockLinkedInLeadFormDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockLinkedInLeadFormDispatcher) ListLinkedInLeadForms(ctx context.Context, _ string, _ model.Provider) (*model.LinkedInLeadForms, bool, error) {
	m.calls++
	m.acct = model.PickerAccount(ctx)
	return m.res, true, nil
}

// TestListLinkedinAdsLeadForms pins the gate (off answers 400 before the dispatcher) and, on,
// every field mapped and the picker's account_id carried to the dispatcher.
func TestListLinkedinAdsLeadForms(t *testing.T) {
	d := &mockLinkedInLeadFormDispatcher{res: &model.LinkedInLeadForms{AccountID: "512", Forms: []model.LinkedInLeadForm{
		{ID: "42", URN: "urn:li:adForm:42", Name: "CFP", State: "PUBLISHED"},
	}}}
	acct := "512"
	p := &conn.ListLinkedinAdsLeadFormsPayload{ProjectID: "cncf", AccountID: &acct}

	t.Setenv(constants.EnvLinkedInAdsLeadGenEnabled, "TRUE")
	_, err := linkedInLookupService(d).ListLinkedinAdsLeadForms(context.Background(), p)
	var bad *conn.BadRequestError
	if !errors.As(err, &bad) || !strings.Contains(bad.Message, "LINKEDIN_ADS_LEAD_GEN_ENABLED is not \"true\"") || d.calls != 0 {
		t.Fatalf("flag off: err %v calls %d, want a 400 naming the flag", err, d.calls)
	}

	t.Setenv(constants.EnvLinkedInAdsLeadGenEnabled, "true")
	res, err := linkedInLookupService(d).ListLinkedinAdsLeadForms(context.Background(), p)
	if err != nil {
		t.Fatalf("ListLinkedinAdsLeadForms: %v", err)
	}
	b, _ := json.Marshal(res)
	const want = `{"AccountID":"512","Forms":[{"ID":"42","Urn":"urn:li:adForm:42","Name":"CFP","State":"PUBLISHED"}],"FormCount":1}`
	if string(b) != want || d.acct != "512" {
		t.Errorf("res = %s (account %q)\nwant  %s (account 512)", b, d.acct, want)
	}
}
