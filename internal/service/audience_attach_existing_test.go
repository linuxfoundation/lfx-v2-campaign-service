// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"strconv"
	"strings"
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

	calls      int
	includeIDs []string
	outcome    *audience.ComposeOutcome
	err        error
}

func (f *attachExplorer) AttachExisting(_ context.Context, _ string, includeIDs, _ []string) (*audience.ComposeOutcome, error) {
	f.calls++
	f.includeIDs = includeIDs
	if f.err != nil {
		return nil, f.err
	}
	return f.outcome, nil
}

func attachPayload(briefID string) *explore.AttachExistingAudiencePayload {
	return &explore.AttachExistingAudiencePayload{ProjectID: "proj-1", Attach: &explore.AudienceAttachExistingInput{
		BriefID: briefID, MasterListID: strptr("31027"), SuppressionListIds: []string{"s1", "s2"},
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

func multiAttachPayload(includes, suppression []string) *explore.AttachExistingAudiencePayload {
	return &explore.AttachExistingAudiencePayload{ProjectID: "proj-1", Attach: &explore.AudienceAttachExistingInput{
		BriefID: "brief-1", IncludeListIds: includes, SuppressionListIds: suppression,
	}}
}

func attachService(t *testing.T, explorer *attachExplorer) (*AudienceExploreService, *countingAudienceRepo) {
	t.Helper()
	repo := newCountingAudienceRepo()
	briefs := newFakeBriefRepo()
	seedBrief(t, briefs, "proj-1", "brief-1")
	svc := NewAudienceExploreService(explorer)
	svc.SetAudienceRepo(repo)
	svc.SetBriefRepo(briefs)
	return svc, repo
}

// Several existing lists attached directly: every one reaches the explorer (trimmed, de-duplicated,
// in order), the row records them all in include_list_ids with the first as its master, and the
// response carries them on both the result and its audience.
func TestAttachExistingAudienceRecordsSeveralIncludeLists(t *testing.T) {
	explorer := &attachExplorer{outcome: &audience.ComposeOutcome{
		Master:                 audience.ComposedList{ListRow: audience.ListRow{ListID: "31027", Name: "Past attendees"}},
		SourceListIDs:          []string{"31027", "31028"},
		PortalID:               "8112310",
		AttachedSuppressionIDs: []string{"s1"},
		Attached:               true,
	}}
	svc, repo := attachService(t, explorer)

	res, err := svc.AttachExistingAudience(context.Background(),
		multiAttachPayload([]string{" 31027 ", "31028", "31027"}, []string{"s1"}))
	require.NoError(t, err)
	assert.Equal(t, []string{"31027", "31028"}, explorer.includeIDs, "trimmed and de-duplicated, order kept")
	assert.Equal(t, []string{"31027", "31028"}, res.IncludeListIds)
	assert.Equal(t, []string{"31027", "31028"}, res.Audience.IncludeListIds)
	assert.Equal(t, "31027", res.Audience.PlatformMasterListID)

	rows := repo.rows()
	require.Len(t, rows, 1)
	assert.Equal(t, "31027", rows[0].PlatformMasterListID, "the master holds the first include list")
	send, serr := rows[0].SendListIDs()
	require.NoError(t, serr)
	assert.Equal(t, []string{"31027", "31028"}, send)
	assert.Equal(t, "8112310", rows[0].BuiltInPortalID)
	assert.Equal(t, "Reused 2 existing lists (first: Past attendees) with 1 suppression list(s)", rows[0].InclusionSummary)
}

// The single master_list_id form is unchanged: one list to the explorer, no include_list_ids on
// the row or the response.
func TestAttachExistingAudienceSingleMasterRecordsNoIncludeList(t *testing.T) {
	explorer := &attachExplorer{outcome: &audience.ComposeOutcome{
		Master:        audience.ComposedList{ListRow: audience.ListRow{ListID: "31027", Name: "m"}},
		SourceListIDs: []string{"31027"}, PortalID: "8112310", Attached: true,
	}}
	svc, repo := attachService(t, explorer)

	res, err := svc.AttachExistingAudience(context.Background(), attachPayload("brief-1"))
	require.NoError(t, err)
	assert.Equal(t, []string{"31027"}, explorer.includeIDs)
	assert.Nil(t, res.IncludeListIds)
	assert.Nil(t, res.Audience.IncludeListIds)
	require.Len(t, repo.rows(), 1)
	assert.Nil(t, repo.rows()[0].IncludeListIDs)
}

// The request must name its lists exactly one way, and an include list that is also suppressed is
// refused: HubSpot applies exclusions after inclusions, so it would silently drop that whole list.
// Every refusal is a 400 before the portal is read or anything is recorded.
func TestAttachExistingAudienceRefusesAmbiguousOrContradictoryLists(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *explore.AudienceAttachExistingInput
		want string
	}{
		{"neither", &explore.AudienceAttachExistingInput{BriefID: "brief-1"}, "required"},
		{"blank master and no includes", &explore.AudienceAttachExistingInput{BriefID: "brief-1", MasterListID: strptr("  ")}, "required"},
		{"both", &explore.AudienceAttachExistingInput{BriefID: "brief-1", MasterListID: strptr("31027"), IncludeListIds: []string{"31028"}}, "not both"},
		{"blank include entry", &explore.AudienceAttachExistingInput{BriefID: "brief-1", IncludeListIds: []string{"31027", " "}}, "blank"},
		{"include also suppressed", &explore.AudienceAttachExistingInput{BriefID: "brief-1",
			IncludeListIds: []string{"31027", "31028"}, SuppressionListIds: []string{"s1", " 31028 "}}, "31028"},
		{"non-numeric include id", &explore.AudienceAttachExistingInput{BriefID: "brief-1", IncludeListIds: []string{"31027", "abc"}}, "not a HubSpot list id"},
		{"oversized include id", &explore.AudienceAttachExistingInput{BriefID: "brief-1", IncludeListIds: []string{strings.Repeat("1", 33)}}, "not a HubSpot list id"},
		{"includes and suppressions over the combined cap", &explore.AudienceAttachExistingInput{BriefID: "brief-1",
			IncludeListIds: numericIDs(1, 150), SuppressionListIds: numericIDs(1000, 51)}, "together cannot name more than 200"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			explorer := &attachExplorer{}
			svc, repo := attachService(t, explorer)
			_, err := svc.AttachExistingAudience(context.Background(),
				&explore.AttachExistingAudiencePayload{ProjectID: "proj-1", Attach: tc.in})
			var bad *explore.BadRequestError
			require.ErrorAs(t, err, &bad)
			assert.Contains(t, bad.Message, tc.want)
			assert.Zero(t, explorer.calls, "refused before the portal is read")
			assert.Zero(t, repo.calls, "nothing recorded")
		})
	}
}

// A missing include list keeps the existing not-found mapping: 404, nothing recorded.
func TestAttachExistingAudienceMapsAMissingIncludeListTo404(t *testing.T) {
	explorer := &attachExplorer{err: audience.ErrListNotFound}
	svc, repo := attachService(t, explorer)

	_, err := svc.AttachExistingAudience(context.Background(), multiAttachPayload([]string{"31027", "404"}, nil))
	var nf *explore.NotFoundError
	require.ErrorAs(t, err, &nf)
	assert.Zero(t, repo.calls)
}

// numericIDs returns n distinct numeric list ids starting at from.
func numericIDs(from, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, strconv.Itoa(from+i))
	}
	return out
}
