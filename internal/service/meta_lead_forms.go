// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// MetaLeadFormReader is an OPTIONAL dispatcher capability: list the Instant Forms of the Page a
// Meta create for the project uses (#550). Type-asserted like MetaCustomAudienceReader.
//
// A successful call MUST return a result with a NON-NIL Forms slice. called reports whether Meta
// was actually contacted.
type MetaLeadFormReader interface {
	ListMetaLeadForms(ctx context.Context, projectID string, platform model.Provider) (res *model.MetaLeadForms, called bool, err error)
}

const opListMetaLeadForms = "list_meta_lead_forms"

// metaLeadFormsCallTimeout bounds the lead form read: up to ten sequential Graph pages.
const metaLeadFormsCallTimeout = 30 * time.Second

// ReadMetaLeadForms lists the Instant Forms of the Page a Meta create for the project uses, through
// the platform's dispatcher. A pure read; nothing is persisted.
func (o *Orchestrator) ReadMetaLeadForms(ctx context.Context, projectID string, platform model.Provider) (*model.MetaLeadForms, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	r, ok := d.(MetaLeadFormReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, metaLeadFormsCallTimeout)
	defer cancel()
	start := time.Now()
	res, called, err := r.ListMetaLeadForms(callCtx, projectID, platform)
	// Recorded only when Meta was reached: a local refusal sent nothing.
	if called {
		o.recordUpstream(ctx, platform, opListMetaLeadForms, start, err)
	}
	if err != nil {
		return nil, err
	}
	if res == nil || res.Forms == nil {
		return nil, fmt.Errorf("%s lead form reader returned a nil result with no error", platform)
	}
	return res, nil
}

var metaAdsLeadFormLookup = accountDiscovery{
	provider:    model.ProviderMetaAds,
	displayName: "meta ads",
	notUsableRemedy: "check that it is active, that the stored credential is valid json " +
		"with access_token set, and that a page_id is set",
	operation: "lead form lookup",
}

// ListMetaLeadForms lists the Instant Forms of the connection's Page for the metaConfig.leadFormId
// picker (#550).
//
// Gated on global_marketing_ops at the gateway (the marketing-ops-api rule), like
// ListMetaCustomAudiences: the forms belong to the Page of the connection a create uses, which is
// the shared LF connection for every project without its own, so the answer can name other
// foundations' forms. Off unless META_LEAD_FORMS_ENABLED is "true" (400, before any credential is
// resolved), the flag that gates metaConfig.leadFormId itself.
func (s *ConnectionService) ListMetaLeadForms(ctx context.Context, p *conn.ListMetaLeadFormsPayload) (*conn.MetaLeadForms, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	if os.Getenv(constants.EnvMetaLeadFormsEnabled) != "true" {
		return nil, &conn.BadRequestError{Code: "400", Message: "the meta lead form read is disabled on this deployment (" +
			constants.EnvMetaLeadFormsEnabled + " is not \"true\"); ask an operator to enable it"}
	}
	_, _, orch, err := s.resolveBackendWithOrch(metaAdsLeadFormLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadMetaLeadForms(ctx, p.ProjectID, model.ProviderMetaAds)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, metaAdsLeadFormLookup, rerr)
	}
	forms := append([]model.MetaLeadForm(nil), res.Forms...)
	sort.SliceStable(forms, func(i, j int) bool {
		li, lj := strings.ToLower(forms[i].Name), strings.ToLower(forms[j].Name)
		if li != lj {
			return li < lj
		}
		return forms[i].ID < forms[j].ID
	})
	out := make([]*conn.MetaLeadForm, 0, len(forms))
	for _, f := range forms {
		out = append(out, &conn.MetaLeadForm{ID: f.ID, Name: f.Name, Status: f.Status})
	}
	return &conn.MetaLeadForms{PageID: res.PageID, Forms: out, FormCount: len(out)}, nil
}
