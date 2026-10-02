// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	explore "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attachExplorer implements only AttachExisting; see fakeExplorer for why the interface is
// embedded.
type attachExplorer struct {
	AudienceExplorer

	calls   int
	outcome *audience.ComposeOutcome
	err     error
}

func (f *attachExplorer) AttachExisting(_ context.Context, _, masterID string, suppressionIDs []string) (*audience.ComposeOutcome, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.outcome, nil
}

func attachPayload(briefID string) *explore.AttachExistingAudiencePayload {
	return &explore.AttachExistingAudiencePayload{ProjectID: "proj-1", Attach: &explore.AudienceAttachExistingInput{
		BriefID: briefID, MasterListID: "31027", SuppressionListIds: []string{"s1", "s2"},
	}}
}

// Reusing an earlier send's lists must produce the same dispatchable row a compose does:
// built, stamped with the portal, and carrying every suppression the send excluded.
func TestAttachExistingAudienceRecordsTheReusedListsWithTheirPortal(t *testing.T) {
	explorer := &attachExplorer{outcome: &audience.ComposeOutcome{
		Master:                 audience.ComposedList{ListRow: audience.ListRow{ListID: "31027", Name: "26Q3 - CNCF - AGNTCon - Master"}},
		SourceListIDs:          []string{"31027"},
		PortalID:               "8112310",
		AttachedSuppressionIDs: []string{"s1", "s2"},
		Attached:               true,
	}}
	repo := newCountingAudienceRepo()
	briefs := newFakeBriefRepo()
	seedBrief(t, briefs, "proj-1", "brief-1")
	svc := NewAudienceExploreService(explorer)
	svc.SetAudienceRepo(repo)
	svc.SetBriefRepo(briefs)

	res, err := svc.AttachExistingAudience(context.Background(), attachPayload("brief-1"))
	require.NoError(t, err)
	assert.Equal(t, "31027", res.Audience.PlatformMasterListID)
	assert.Equal(t, []string{"s1", "s2"}, res.SuppressionListIds)

	rows := repo.rows()
	require.Len(t, rows, 1)
	assert.Equal(t, "8112310", rows[0].BuiltInPortalID)
	assert.Equal(t, model.AudienceBuilt, rows[0].Status)
	assert.Equal(t, "Reused existing list 26Q3 - CNCF - AGNTCon - Master with 2 suppression list(s)", rows[0].InclusionSummary)
}

// An unknown brief is refused before any portal read.
func TestAttachExistingAudienceRefusesAnUnknownBriefBeforeReadingThePortal(t *testing.T) {
	explorer := &attachExplorer{}
	svc := NewAudienceExploreService(explorer)
	svc.SetAudienceRepo(newCountingAudienceRepo())
	svc.SetBriefRepo(newFakeBriefRepo())

	_, err := svc.AttachExistingAudience(context.Background(), attachPayload("missing"))
	var nf *explore.NotFoundError
	require.ErrorAs(t, err, &nf)
	assert.Zero(t, explorer.calls)
}

// A list the portal does not hold is a 404, and nothing is recorded.
func TestAttachExistingAudienceMapsAMissingListTo404(t *testing.T) {
	explorer := &attachExplorer{err: audience.ErrListNotFound}
	repo := newCountingAudienceRepo()
	briefs := newFakeBriefRepo()
	seedBrief(t, briefs, "proj-1", "brief-1")
	svc := NewAudienceExploreService(explorer)
	svc.SetAudienceRepo(repo)
	svc.SetBriefRepo(briefs)

	_, err := svc.AttachExistingAudience(context.Background(), attachPayload("brief-1"))
	var nf *explore.NotFoundError
	require.ErrorAs(t, err, &nf)
	assert.Zero(t, repo.calls)
}
