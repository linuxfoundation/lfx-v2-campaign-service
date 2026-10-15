// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"strings"

	explore "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// ---------------------------------------------------------------------------
// Custom-audience handlers
//
// Interpret, preview and create. Mapping only: the criteria are translated to
// and from the generated types, and failures to the contract's statuses.
// ---------------------------------------------------------------------------

func (s *AudienceExploreService) InterpretCustomAudience(ctx context.Context, p *explore.InterpretCustomAudiencePayload) (*explore.AudienceCustomInterpretResult, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	answers := make([]audience.CustomAnswer, 0, len(p.Answers))
	for _, a := range p.Answers {
		if a == nil {
			continue
		}
		answers = append(answers, audience.CustomAnswer{
			QuestionID: derefStr(a.QuestionID), Question: derefStr(a.Question), Answer: a.Answer,
		})
	}
	out, ierr := explorer.InterpretCustom(ctx, p.ProjectID, p.Request, derefStr(p.EventURL), answers)
	if ierr != nil {
		return nil, customErr(ctx, "interpret custom audience", p.ProjectID, ierr)
	}
	res := &explore.AudienceCustomInterpretResult{
		Criteria:     customCriteriaResult(out.Draft.Criteria),
		Gate:         listSearchResult(out.Gate),
		SpeakersList: listSearchResult(out.Speakers),
		Questions:    make([]*explore.AudienceCustomQuestion, 0, len(out.Draft.Questions)),
		Notes:        nonNilStrings(out.Draft.Notes),
		Similar:      make([]*explore.AudienceCustomSimilarList, 0, len(out.Similar)),
	}
	for _, sim := range out.Similar {
		row := sim.ListRow
		item := &explore.AudienceCustomSimilarList{List: listSearchResult(&row)}
		if rec := string(sim.Recommendation); rec != "" {
			item.Recommendation = &rec
		}
		if reason := strings.TrimSpace(sim.Reason); reason != "" {
			item.Reason = &reason
		}
		res.Similar = append(res.Similar, item)
	}
	if brand := strings.TrimSpace(out.Draft.BrandShort); brand != "" {
		res.BrandShort = &brand
	}
	if out.Event != nil {
		res.Event = eventIdentityResult(*out.Event)
	}
	for _, q := range out.Draft.Questions {
		row := &explore.AudienceCustomQuestion{ID: q.ID, Question: q.Question, Options: nonNilStrings(q.Options)}
		if why := strings.TrimSpace(q.Why); why != "" {
			row.Why = &why
		}
		res.Questions = append(res.Questions, row)
	}
	return res, nil
}

func (s *AudienceExploreService) PreviewCustomAudience(ctx context.Context, p *explore.PreviewCustomAudiencePayload) (*explore.AudienceCustomPreviewResult, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	preview, perr := explorer.PreviewCustom(ctx, p.ProjectID, customCriteria(p.Custom.Criteria), derefStr(p.Custom.Name))
	if perr != nil {
		return nil, customErr(ctx, "preview custom audience", p.ProjectID, perr)
	}
	res := &explore.AudienceCustomPreviewResult{
		Name:                     preview.Name,
		Criteria:                 customCriteriaResult(preview.Criteria),
		BranchCount:              preview.BranchCount,
		EducationTopicCandidates: nonNilStrings(preview.EducationTopicCandidates),
		Gate:                     listSearchResult(preview.Gate),
		UpperBound:               preview.UpperBound,
		Warnings:                 nonNilStrings(preview.Warnings),
	}
	return res, nil
}

func (s *AudienceExploreService) CreateCustomAudience(ctx context.Context, p *explore.CreateCustomAudiencePayload) (*explore.AudienceComposedList, error) {
	explorer, err := s.ready()
	if err != nil {
		return nil, err
	}
	created, cerr := explorer.CreateCustom(ctx, p.ProjectID, customCriteria(p.Custom.Criteria), derefStr(p.Custom.Name))
	if cerr != nil {
		var partial *audience.ComposePartialError
		if errors.As(cerr, &partial) {
			return nil, composeErr(ctx, p.ProjectID, cerr)
		}
		return nil, customErr(ctx, "create custom audience", p.ProjectID, cerr)
	}
	return composedListResult(created), nil
}

// customErr maps the custom path's own failures before deferring to the shared
// mapping. Its 400s carry the specific reason, because the generic message
// ("check the event URL or list reference") names neither thing a custom request has.
func customErr(ctx context.Context, op, projectID string, err error) error {
	var unknown *audience.UnknownPropertiesError
	switch {
	case errors.As(err, &unknown):
		return &explore.BadRequestError{Code: "400", Message: strings.TrimPrefix(unknown.Error(), audience.ErrInvalidRequest.Error()+": ")}
	case errors.Is(err, audience.ErrCustomInterpretUnavailable):
		return &explore.ConnServiceUnavailableError{Code: "503", Message: "this deployment has no model configured to interpret requests; fill in the criteria form instead"}
	case hubspot.ValidationMessage(err) != "":
		// HubSpot refused a request on this path and said why. The reason names a field or
		// operator, never credentials, and without it the operator cannot fix the criteria.
		// The wording is neutral: preview's reads (list search, property lookups) can be
		// refused as well as create's list definition.
		return &explore.BadRequestError{Code: "400", Message: "HubSpot rejected the request: " + hubspot.ValidationMessage(err)}
	case errors.Is(err, audience.ErrInvalidRequest):
		// Every ErrInvalidRequest on this path is raised by ValidateCustomCriteria,
		// CustomInterpretPrompt or freeCustomName, whose messages are written for the
		// operator and carry no upstream detail.
		msg := err.Error()
		if i := strings.Index(msg, audience.ErrInvalidRequest.Error()+": "); i >= 0 {
			msg = msg[i+len(audience.ErrInvalidRequest.Error())+2:]
		}
		return &explore.BadRequestError{Code: "400", Message: msg}
	}
	return audienceExploreErr(ctx, op, projectID, err)
}

func customCriteria(in *explore.AudienceCustomCriteria) audience.CustomCriteria {
	if in == nil {
		return audience.CustomCriteria{}
	}
	out := audience.CustomCriteria{
		Cities:            in.Cities,
		Countries:         in.Countries,
		ContactLocation:   derefBool(in.ContactLocation),
		EventRegistered:   derefBool(in.EventRegistered),
		EducationEnrolled: derefBool(in.EducationEnrolled),
		RegisteredFor:     in.RegisteredFor,
		GateEventHistory:  derefBool(in.GateEventHistory),
		GateListID:        derefStr(in.GateListID),
		Ambassadors:       derefBool(in.Ambassadors),
		EducationTopics:   in.EducationTopics,
		PageViewURLs:      in.PageViewUrls,
		Label:             derefStr(in.Label),
		// Honoured downstream only when it is one of the portal's own candidates.
		EducationTopicProperty: derefStr(in.EducationTopicProperty),
	}
	for _, f := range in.PropertyFilters {
		if f == nil {
			continue
		}
		// Enumeration is deliberately not read from the caller: the orchestration
		// sets it from the portal's property definition.
		out.PropertyFilters = append(out.PropertyFilters, audience.CustomPropertyFilter{
			Property: f.Property, Operator: audience.CustomPropertyOperator(f.Operator), Values: f.Values,
		})
	}
	if sp := in.Speakers; sp != nil {
		out.Speakers = &audience.CustomSpeakers{
			Scope: audience.SpeakerScope(sp.Scope), ListID: derefStr(sp.ListID), EventNames: sp.EventNames,
		}
	}
	return out
}

func customCriteriaResult(c audience.CustomCriteria) *explore.AudienceCustomCriteria {
	b := func(v bool) *bool { return &v }
	out := &explore.AudienceCustomCriteria{
		Cities:            nonNilStrings(c.Cities),
		Countries:         nonNilStrings(c.Countries),
		ContactLocation:   b(c.ContactLocation),
		EventRegistered:   b(c.EventRegistered),
		EducationEnrolled: b(c.EducationEnrolled),
		RegisteredFor:     nonNilStrings(c.RegisteredFor),
		GateEventHistory:  b(c.GateEventHistory),
		Ambassadors:       b(c.Ambassadors),
		EducationTopics:   nonNilStrings(c.EducationTopics),
		PageViewUrls:      nonNilStrings(c.PageViewURLs),
		PropertyFilters:   make([]*explore.AudienceCustomPropertyFilter, 0, len(c.PropertyFilters)),
	}
	if prop := strings.TrimSpace(c.EducationTopicProperty); prop != "" {
		out.EducationTopicProperty = &prop
	}
	if id := strings.TrimSpace(c.GateListID); id != "" {
		out.GateListID = &id
	}
	if label := strings.TrimSpace(c.Label); label != "" {
		out.Label = &label
	}
	for _, f := range c.PropertyFilters {
		out.PropertyFilters = append(out.PropertyFilters, &explore.AudienceCustomPropertyFilter{
			Property: f.Property, Operator: string(f.Operator), Values: nonNilStrings(f.Values),
		})
	}
	if sp := c.Speakers; sp != nil {
		row := &explore.AudienceCustomSpeakers{Scope: string(sp.Scope), EventNames: nonNilStrings(sp.EventNames)}
		if id := strings.TrimSpace(sp.ListID); id != "" {
			row.ListID = &id
		}
		out.Speakers = row
	}
	return out
}

func listSearchResult(r *audience.ListRow) *explore.AudienceListSearchResult {
	if r == nil {
		return nil
	}
	return &explore.AudienceListSearchResult{ListID: r.ListID, Name: r.Name, Size: r.Size, HubspotURL: r.HubSpotURL}
}

// nonNilStrings keeps an empty array on the wire as [] rather than null.
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
