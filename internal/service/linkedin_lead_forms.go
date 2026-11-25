// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"os"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// LinkedInLeadFormReader is an OPTIONAL dispatcher capability: list the Lead Gen Forms on the ad
// account a LinkedIn create for the project uses (#664). Type-asserted like MetaLeadFormReader.
//
// A successful call MUST return a result with a NON-NIL Forms slice. called reports whether
// LinkedIn was actually contacted.
type LinkedInLeadFormReader interface {
	ListLinkedInLeadForms(ctx context.Context, projectID string, platform model.Provider) (res *model.LinkedInLeadForms, called bool, err error)
}

const opListLinkedInLeadForms = "list_linkedin_lead_forms"

// linkedInLeadFormsCallTimeout bounds the lead form read: up to ten sequential finder pages.
const linkedInLeadFormsCallTimeout = 30 * time.Second

// ReadLinkedInLeadForms lists the Lead Gen Forms on the ad account a LinkedIn create for the
// project uses, through the platform's dispatcher. A pure read; nothing is persisted.
func (o *Orchestrator) ReadLinkedInLeadForms(ctx context.Context, projectID string, platform model.Provider) (*model.LinkedInLeadForms, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	r, ok := d.(LinkedInLeadFormReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, linkedInLeadFormsCallTimeout)
	defer cancel()
	start := time.Now()
	res, called, err := r.ListLinkedInLeadForms(callCtx, projectID, platform)
	// Recorded only when LinkedIn was reached: a local refusal sent nothing.
	if called {
		o.recordUpstream(ctx, platform, opListLinkedInLeadForms, start, err)
	}
	if err != nil {
		return nil, err
	}
	if res == nil || res.Forms == nil {
		return nil, fmt.Errorf("%s lead form reader returned a nil result with no error", platform)
	}
	return res, nil
}

var linkedInAdsLeadFormLookup = accountDiscovery{
	provider:    model.ProviderLinkedInAds,
	displayName: "linkedin ads",
	notUsableRemedy: "check that it is active, that the stored credential is valid json " +
		"with access_token set, and that an account_id is selected",
	operation: "lead gen form lookup",
}

// ListLinkedinAdsLeadForms lists the Lead Gen Forms on the LinkedIn ad account a create for this
// project uses, for the linkedInConfig.leadGenForm picker (#664).
//
// Gated on global_marketing_ops at the gateway (the marketing-ops-api rule), like the other
// LinkedIn pickers: the LF system account is the fallback of every project without its own
// LinkedIn connection, so the answer can name other foundations' forms. Off unless
// LINKEDIN_ADS_LEAD_GEN_ENABLED is "true" (400, before any credential is resolved), the flag that
// gates linkedInConfig.leadGenForm itself. The reserved LF scope stays unaddressable.
func (s *ConnectionService) ListLinkedinAdsLeadForms(ctx context.Context, p *conn.ListLinkedinAdsLeadFormsPayload) (*conn.LinkedinAdsLeadForms, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	if os.Getenv(constants.EnvLinkedInAdsLeadGenEnabled) != "true" {
		return nil, &conn.BadRequestError{Code: "400", Message: "the linkedin lead gen form read is disabled on this deployment (" +
			constants.EnvLinkedInAdsLeadGenEnabled + " is not \"true\"); ask an operator to enable it"}
	}
	_, _, orch, err := s.resolveBackendWithOrch(linkedInAdsLeadFormLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadLinkedInLeadForms(linkedInPickerContext(ctx, p.AccountID), p.ProjectID, model.ProviderLinkedInAds)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, linkedInAdsLeadFormLookup, rerr)
	}
	out := make([]*conn.LinkedinAdsLeadForm, 0, len(res.Forms))
	for _, f := range res.Forms {
		out = append(out, &conn.LinkedinAdsLeadForm{ID: f.ID, Urn: f.URN, Name: f.Name, State: f.State})
	}
	return &conn.LinkedinAdsLeadForms{AccountID: res.AccountID, Forms: out, FormCount: len(out)}, nil
}
