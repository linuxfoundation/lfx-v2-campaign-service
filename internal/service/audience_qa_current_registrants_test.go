// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	explore "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Check 4 (current-edition registrants) on the WIRE.
//
// These tests exist because the contradiction dealako found on PR #240 went unnoticed
// precisely for want of them: `CheckCurrentRegistrants` returned a NEEDS VERIFY verdict
// for every caller that supplied no event name, while the godoc, the design attribute
// and the PR text all promised the field would be OMITTED when the check could not run.
// Nothing asserted what a client actually receives, so code and contract drifted apart
// without any test failing.
//
// What is pinned here is the CLIENT-OBSERVABLE half of that contract, which is the half
// the dispatch-layer tests cannot see: whether `current_registrants` is present in the
// payload, and whether `overall` carries the roll-up the check contributed. A test at
// the check's own level can only prove the verdict it returns, not that the verdict
// survives mapping — and the mapping is where `nil` versus a zero-valued struct decides
// what the operator reads.
// ---------------------------------------------------------------------------

// qaExplorer implements only RunQA. The interface is embedded so the other methods exist
// without being written out, and so a handler that starts calling one of them from this
// path panics loudly rather than silently passing.
type qaExplorer struct {
	AudienceExplorer

	gotEventName string
	outcome      *audience.QaOutcome
}

func (q *qaExplorer) RunQA(_ context.Context, _, _, eventName string, _, _ bool) (*audience.QaOutcome, error) {
	q.gotEventName = eventName
	return q.outcome, nil
}

func qaPayload(eventName *string) *explore.RunAudienceQaPayload {
	return &explore.RunAudienceQaPayload{
		ProjectID: "11111111-1111-1111-1111-111111111111",
		ListRef:   "123",
		EventName: eventName,
	}
}

func TestRunAudienceQaOmitsCheckFourWhenNoEventWasNamed(t *testing.T) {
	// The zero Check is what CheckCurrentRegistrants returns for an empty event name:
	// no verdict, no findings. Overall is PASS, as it would have been before check 4
	// existed — that equality IS the "no existing caller's verdict changes" promise.
	svc := NewAudienceExploreService(&qaExplorer{outcome: &audience.QaOutcome{
		ListID:  "123",
		Name:    "26Q2 - CNCF - KubeCon NA - Prospects",
		Overall: audience.VerdictPass,
		Checks: audience.QaChecks{
			CurrentRegistrants: audience.Check{}, // did not run
		},
	}})

	res, err := svc.RunAudienceQa(context.Background(), qaPayload(nil))
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Nil(t, res.Checks.CurrentRegistrants,
		"check 4 must be ABSENT from the payload when no event name reached the audit — "+
			"a zero-valued check on the wire is a verdict the client has to interpret, and "+
			"the one interpretation that must never be reached is \"passed\"")
	require.NotNil(t, res.Overall)
	assert.Equal(t, string(audience.VerdictPass), *res.Overall,
		"omitting the event name must leave the roll-up untouched; a check that could not "+
			"run must not make an unrelated audit look worse")
}

func TestRunAudienceQaCarriesCheckFourWhenTheEventIsNamed(t *testing.T) {
	name := "KubeCon North America"
	svc := NewAudienceExploreService(&qaExplorer{outcome: &audience.QaOutcome{
		ListID:  "123",
		Name:    "26Q2 - CNCF - KubeCon NA - Prospects",
		Overall: audience.VerdictFail,
		Checks: audience.QaChecks{
			CurrentRegistrants: audience.Check{
				Verdict: audience.VerdictFail,
			},
		},
	}})

	res, err := svc.RunAudienceQa(context.Background(), qaPayload(&name))
	require.NoError(t, err)
	require.NotNil(t, res)

	require.NotNil(t, res.Checks.CurrentRegistrants,
		"check 4 must be PRESENT once an event name is supplied — this is the half of the "+
			"contract the omitted case cannot prove on its own")
	assert.Equal(t, string(audience.VerdictFail), res.Checks.CurrentRegistrants.Verdict)
	require.NotNil(t, res.Overall)
	assert.Equal(t, string(audience.VerdictFail), *res.Overall)
}

func TestRunAudienceQaForwardsTheEventNameItWasGiven(t *testing.T) {
	// The omitted-vs-present distinction above is only meaningful if the name actually
	// reaches the explorer: a service that dropped it would make the first test pass for
	// the wrong reason, since a dropped name and an absent one are indistinguishable in
	// the response.
	for _, tc := range []struct {
		name  string
		given *string
		want  string
	}{
		{name: "absent reaches the explorer as empty", given: nil, want: ""},
		{name: "supplied reaches the explorer verbatim", given: strPtrQA("KubeCon North America"), want: "KubeCon North America"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &qaExplorer{outcome: &audience.QaOutcome{
				ListID: "123", Name: "L", Overall: audience.VerdictPass,
			}}
			_, err := NewAudienceExploreService(ex).RunAudienceQa(context.Background(), qaPayload(tc.given))
			require.NoError(t, err)
			assert.Equal(t, tc.want, ex.gotEventName)
		})
	}
}

func strPtrQA(s string) *string { return &s }
