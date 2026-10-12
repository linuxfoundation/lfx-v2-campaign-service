// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"testing"

	explore "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// customExplorer implements only the custom-audience methods.
type customExplorer struct {
	AudienceExplorer

	criteria  audience.CustomCriteria
	name      string
	interpret *audience.CustomInterpretOutcome
	preview   *audience.CustomPreview
	created   *audience.ComposedList
	err       error
}

func (f *customExplorer) InterpretCustom(context.Context, string, string, string, []audience.CustomAnswer) (*audience.CustomInterpretOutcome, error) {
	return f.interpret, f.err
}

func (f *customExplorer) PreviewCustom(_ context.Context, _ string, c audience.CustomCriteria, name string) (*audience.CustomPreview, error) {
	f.criteria, f.name = c, name
	return f.preview, f.err
}

func (f *customExplorer) CreateCustom(_ context.Context, _ string, c audience.CustomCriteria, name string) (*audience.ComposedList, error) {
	f.criteria, f.name = c, name
	return f.created, f.err
}

func customPayload() *explore.AudienceCustomInput {
	yes := true
	gate := "26716"
	return &explore.AudienceCustomInput{Criteria: &explore.AudienceCustomCriteria{
		Cities: []string{"Seoul"}, ContactLocation: &yes, GateListID: &gate,
		PropertyFilters: []*explore.AudienceCustomPropertyFilter{{Property: "industry", Operator: "is_any_of", Values: []string{"X"}}},
		Speakers:        &explore.AudienceCustomSpeakers{Scope: "current", EventNames: []string{"OSS Korea 2026"}},
	}}
}

func TestPreviewCustomAudience_MapsCriteriaBothWays(t *testing.T) {
	bound := 2500000
	f := &customExplorer{preview: &audience.CustomPreview{
		Name: "26Q4 - Seoul - Mailable Contacts", BranchCount: 6, UpperBound: &bound,
		Criteria: audience.CustomCriteria{Cities: []string{"Seoul"}, ContactLocation: true, GateListID: "26716",
			PropertyFilters: []audience.CustomPropertyFilter{{Property: "industry", Operator: audience.CustomOpAnyOf, Values: []string{"X"}, Enumeration: true}}},
	}}
	res, err := NewAudienceExploreService(f).PreviewCustomAudience(context.Background(),
		&explore.PreviewCustomAudiencePayload{ProjectID: "proj-1", Custom: customPayload()})
	require.NoError(t, err)

	assert.True(t, f.criteria.ContactLocation)
	assert.Equal(t, "26716", f.criteria.GateListID)
	require.NotNil(t, f.criteria.Speakers)
	assert.Equal(t, audience.SpeakerScopeCurrent, f.criteria.Speakers.Scope)
	assert.False(t, f.criteria.PropertyFilters[0].Enumeration, "property types are never taken from the caller")

	assert.Equal(t, 6, res.BranchCount)
	assert.Equal(t, &bound, res.UpperBound)
	assert.Equal(t, []string{}, res.Warnings, "an empty array, not null")
	assert.Equal(t, "is_any_of", res.Criteria.PropertyFilters[0].Operator)
}

func TestCustomAudience_ErrorMapping(t *testing.T) {
	cases := map[string]struct {
		err  error
		want any
		msg  string
	}{
		"unknown properties name themselves": {
			err: &audience.UnknownPropertiesError{Names: []string{"job_level"}}, want: &explore.BadRequestError{}, msg: "the portal has no contact property job_level",
		},
		"criteria errors carry their own reason": {
			err: fmt.Errorf("%w: a location is required for location or event-history branches", audience.ErrInvalidRequest), want: &explore.BadRequestError{},
			msg: "a location is required for location or event-history branches",
		},
		"no model is a 503 that points at the form": {
			err: audience.ErrCustomInterpretUnavailable, want: &explore.ConnServiceUnavailableError{}, msg: "fill in the criteria form instead",
		},
		"a missing gate is a 404": {
			err: fmt.Errorf("list 1: %w", audience.ErrListNotFound), want: &explore.NotFoundError{},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewAudienceExploreService(&customExplorer{err: tc.err}).PreviewCustomAudience(context.Background(),
				&explore.PreviewCustomAudiencePayload{ProjectID: "proj-1", Custom: customPayload()})
			require.Error(t, err)
			assert.IsType(t, tc.want, err)
			assert.Contains(t, exploreErrMessage(err), tc.msg)
		})
	}
}

func TestCreateCustomAudience_UnconfirmedIsAComposePartial(t *testing.T) {
	f := &customExplorer{err: &audience.ComposePartialError{MasterName: "Seoul test", Err: fmt.Errorf("unconfirmed")}}
	_, err := NewAudienceExploreService(f).CreateCustomAudience(context.Background(),
		&explore.CreateCustomAudiencePayload{ProjectID: "proj-1", Custom: customPayload()})
	var partial *explore.AudienceComposePartialError
	require.ErrorAs(t, err, &partial, "an unconfirmed create must never reach the client as a retryable 500")
	require.NotNil(t, partial.MasterName)
	assert.Equal(t, "Seoul test", *partial.MasterName)
}

func TestInterpretCustomAudience_ReturnsArraysNotNulls(t *testing.T) {
	f := &customExplorer{interpret: &audience.CustomInterpretOutcome{
		Draft: audience.CustomDraft{
			BrandShort: "LF",
			Questions:  []audience.CustomQuestion{{ID: "mailability", Question: "Gate?"}},
		},
		Gate: &audience.ListRow{ListID: "26716", Name: "26Q2 - The Linux Foundation - Master-list"},
		Similar: []audience.CustomSimilarList{
			{ListRow: audience.ListRow{ListID: "411", Name: "25Q3 - Seoul - Mailable Contacts"}, Recommendation: audience.CustomReuseUpdate, Reason: "older quarter"},
			{ListRow: audience.ListRow{ListID: "412", Name: "Seoul Meetup"}},
		},
	}}
	res, err := NewAudienceExploreService(f).InterpretCustomAudience(context.Background(),
		&explore.InterpretCustomAudiencePayload{ProjectID: "proj-1", Request: "Seoul"})
	require.NoError(t, err)
	assert.Equal(t, []string{}, res.Notes)
	assert.Equal(t, []string{}, res.Questions[0].Options)
	assert.Nil(t, res.Questions[0].Why)
	assert.Equal(t, "26716", res.Gate.ListID)
	assert.Nil(t, res.SpeakersList)
	assert.Equal(t, "LF", *res.BrandShort)
	require.Len(t, res.Similar, 2)
	assert.Equal(t, "update", *res.Similar[0].Recommendation)
	assert.Nil(t, res.Similar[1].Recommendation, "an unassessed list carries no recommendation rather than an empty one")
	assert.Equal(t, []string{}, res.Criteria.PageViewUrls)
}

// exploreErrMessage reads a generated error's Message; Goa's Error() on these types is empty.
func exploreErrMessage(err error) string {
	switch e := err.(type) {
	case *explore.BadRequestError:
		return e.Message
	case *explore.ConnServiceUnavailableError:
		return e.Message
	case *explore.NotFoundError:
		return e.Message
	}
	return err.Error()
}
