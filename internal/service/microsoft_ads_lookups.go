// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// MicrosoftRemarketingListReader is an OPTIONAL dispatcher capability: list the remarketing lists
// on the Microsoft Advertising account a create for the project uses (#563 gap 4 follow-up).
// Type-asserted like TwitterCustomAudienceReader; a dispatcher without it yields
// ErrTargetingLookupUnsupported (400).
//
// A successful call MUST return a non-nil result with a non-nil Lists slice. called reports
// whether the platform was actually contacted: false for the deployment gate and a local refusal
// before the request (no connection, an unusable one), so only real upstream calls are recorded.
type MicrosoftRemarketingListReader interface {
	ListMicrosoftRemarketingLists(ctx context.Context, projectID string, platform model.Provider) (res *model.MicrosoftRemarketingLists, called bool, err error)
}

const opListMicrosoftRemarketingLists = "list_microsoft_remarketing_lists"

// microsoftAdsRemarketingListLookup reuses classifyDiscoveryError's connection-state arms under
// its own operation noun, as the X lookup does.
var microsoftAdsRemarketingListLookup = accountDiscovery{
	provider:        model.ProviderMicrosoftAds,
	displayName:     microsoftAdsAccountDiscovery.displayName,
	notUsableRemedy: microsoftAdsAccountDiscovery.notUsableRemedy,
	operation:       "remarketing list lookup",
}

// ReadMicrosoftRemarketingLists lists the remarketing lists on the Microsoft Advertising account
// a create for the project uses, through the platform's dispatcher. A pure read; nothing is
// persisted.
//
// Like ReadTwitterCustomAudiences, it is NOT refused on a shared-tenancy platform: the answer
// holds every foundation's lists on the shared LF account, so the gateway admits only
// global_marketing_ops of the project to this route (the marketing-ops-api rule).
func (o *Orchestrator) ReadMicrosoftRemarketingLists(ctx context.Context, projectID string, platform model.Provider) (*model.MicrosoftRemarketingLists, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	r, ok := d.(MicrosoftRemarketingListReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	res, called, err := r.ListMicrosoftRemarketingLists(callCtx, projectID, platform)
	if called {
		o.recordUpstream(ctx, platform, opListMicrosoftRemarketingLists, start, err)
	}
	if err != nil {
		return nil, err
	}
	if res == nil || res.Lists == nil {
		return nil, fmt.Errorf("%s remarketing list reader returned a nil result with no error", platform)
	}
	return res, nil
}

// ListMicrosoftAdsRemarketingLists lists the remarketing lists on the Microsoft Advertising
// account a create for this project uses, for the audienceIds / excludedAudienceIds picker (#563
// gap 4 follow-up).
func (s *ConnectionService) ListMicrosoftAdsRemarketingLists(ctx context.Context, p *conn.ListMicrosoftAdsRemarketingListsPayload) (*conn.MicrosoftAdsRemarketingLists, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(microsoftAdsRemarketingListLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadMicrosoftRemarketingLists(ctx, p.ProjectID, model.ProviderMicrosoftAds)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, microsoftAdsRemarketingListLookup, rerr)
	}
	rows := append([]model.MicrosoftRemarketingList(nil), res.Lists...)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := strings.ToLower(rows[i].Name), strings.ToLower(rows[j].Name)
		if a != b {
			return a < b
		}
		return rows[i].ID < rows[j].ID
	})
	out := make([]*conn.MicrosoftAdsRemarketingList, 0, len(rows))
	for _, l := range rows {
		out = append(out, &conn.MicrosoftAdsRemarketingList{
			ID: l.ID, Name: l.Name, Description: l.Description, Scope: l.Scope,
			MembershipDuration: l.MembershipDuration, SearchSize: l.SearchSize,
		})
	}
	return &conn.MicrosoftAdsRemarketingLists{
		AccountID: res.AccountID,
		Lists:     out,
		ListCount: len(out),
	}, nil
}

// MicrosoftConversionGoalReader is an OPTIONAL dispatcher capability: list the conversion goals on
// the Microsoft Advertising account a create for the project uses (#563 gap 7). Type-asserted like
// MicrosoftRemarketingListReader; a dispatcher without it yields ErrTargetingLookupUnsupported
// (400). A successful call MUST return a non-nil result with a non-nil Goals slice; called is as
// for MicrosoftRemarketingListReader.
type MicrosoftConversionGoalReader interface {
	ListMicrosoftConversionGoals(ctx context.Context, projectID string, platform model.Provider) (res *model.MicrosoftConversionGoals, called bool, err error)
}

const opListMicrosoftConversionGoals = "list_microsoft_conversion_goals"

var microsoftAdsConversionGoalLookup = accountDiscovery{
	provider:        model.ProviderMicrosoftAds,
	displayName:     microsoftAdsAccountDiscovery.displayName,
	notUsableRemedy: microsoftAdsAccountDiscovery.notUsableRemedy,
	operation:       "conversion goal lookup",
}

// ReadMicrosoftConversionGoals lists the conversion goals on the Microsoft Advertising account a
// create for the project uses, through the platform's dispatcher. A pure read; nothing is
// persisted. Like ReadMicrosoftRemarketingLists it is not refused on a shared-tenancy platform:
// the gateway admits only global_marketing_ops of the project to this route.
func (o *Orchestrator) ReadMicrosoftConversionGoals(ctx context.Context, projectID string, platform model.Provider) (*model.MicrosoftConversionGoals, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	r, ok := d.(MicrosoftConversionGoalReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	res, called, err := r.ListMicrosoftConversionGoals(callCtx, projectID, platform)
	if called {
		o.recordUpstream(ctx, platform, opListMicrosoftConversionGoals, start, err)
	}
	if err != nil {
		return nil, err
	}
	if res == nil || res.Goals == nil {
		return nil, fmt.Errorf("%s conversion goal reader returned a nil result with no error", platform)
	}
	return res, nil
}

// ListMicrosoftAdsConversionGoals lists the conversion goals on the Microsoft Advertising account a
// create for this project uses, for the conversionGoalIds picker (#563 gap 7).
func (s *ConnectionService) ListMicrosoftAdsConversionGoals(ctx context.Context, p *conn.ListMicrosoftAdsConversionGoalsPayload) (*conn.MicrosoftAdsConversionGoals, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(microsoftAdsConversionGoalLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadMicrosoftConversionGoals(ctx, p.ProjectID, model.ProviderMicrosoftAds)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, microsoftAdsConversionGoalLookup, rerr)
	}
	rows := append([]model.MicrosoftConversionGoal(nil), res.Goals...)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := strings.ToLower(rows[i].Name), strings.ToLower(rows[j].Name)
		if a != b {
			return a < b
		}
		return rows[i].ID < rows[j].ID
	})
	out := make([]*conn.MicrosoftAdsConversionGoal, 0, len(rows))
	for _, g := range rows {
		out = append(out, &conn.MicrosoftAdsConversionGoal{ID: g.ID, Name: g.Name, Status: g.Status, Type: g.Type, Scope: g.Scope})
	}
	return &conn.MicrosoftAdsConversionGoals{AccountID: res.AccountID, Goals: out, GoalCount: len(out)}, nil
}
