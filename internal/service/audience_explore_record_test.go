// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	explore "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// The RECORDING half of ComposeAudienceMaster (LFXV2-2770 follow-up).
//
// Every assertion here is about one of two things: what reaches the portal, and what
// reaches the database. Compose CREATES real HubSpot contact lists and is not
// idempotent, so the tests that matter most are the negative ones -- the ones pinning
// that a compose which is going to be REFUSED is refused before the explorer is ever
// entered. A 404 or a 503 raised after two list creates cannot be rolled back and
// cannot be retried, so "zero compose calls" is the actual contract, not a detail of
// the current ordering.
// ---------------------------------------------------------------------------

// fakeExplorer implements only ComposeMaster. The interface is embedded so the other
// eight methods exist without being written out, and so a handler that starts calling
// one of them from this path panics loudly rather than silently passing.
type fakeExplorer struct {
	AudienceExplorer

	composeCalls int
	lastProject  string
	lastInput    audience.ComposeInput

	outcome *audience.ComposeOutcome
	err     error
}

func (f *fakeExplorer) ComposeMaster(_ context.Context, projectID string, in audience.ComposeInput) (*audience.ComposeOutcome, error) {
	f.composeCalls++
	f.lastProject = projectID
	f.lastInput = in
	if f.err != nil {
		return nil, f.err
	}
	return f.outcome, nil
}

// countingAudienceRepo is the package's ordinary audience fake plus a call counter.
// The count is the point of two of the tests below — "the insert was retried once" and
// "the exploratory path never wrote at all" are both statements about how many times
// CreateAudience was entered, which the store alone cannot answer once createE is set.
type countingAudienceRepo struct {
	*fakeAudienceRepo
	calls int
}

func newCountingAudienceRepo() *countingAudienceRepo {
	return &countingAudienceRepo{fakeAudienceRepo: newFakeAudienceRepo()}
}

func (r *countingAudienceRepo) CreateAudience(ctx context.Context, a *model.CampaignAudience) (*model.CampaignAudience, error) {
	r.calls++
	return r.fakeAudienceRepo.CreateAudience(ctx, a)
}

// rows returns the stored audiences. There is at most one in every test here, so the
// map's iteration order is not a source of flake.
func (r *countingAudienceRepo) rows() []*model.CampaignAudience {
	out := make([]*model.CampaignAudience, 0, len(r.items))
	for _, a := range r.items {
		out = append(out, a)
	}
	return out
}

// seedBrief puts a brief where a recording compose's pre-flight read will find it.
func seedBrief(t *testing.T, briefs *fakeBriefRepo, projectID, id string) {
	t.Helper()
	briefs.briefs[briefKey(projectID, id)] = &model.CampaignBrief{
		ID: id, ProjectID: projectID, Status: model.BriefApproved, Version: 1,
	}
}

func composedOutcome() *audience.ComposeOutcome {
	return &audience.ComposeOutcome{
		Master: audience.ComposedList{ListRow: audience.ListRow{
			ListID: "master-1", Name: "LF KubeCon Q3 Master",
			HubSpotURL: "https://app.hubspot.com/contacts/8112310/lists/master-1",
		}},
		Suppression: &audience.ComposedList{ListRow: audience.ListRow{
			ListID: "supp-1", Name: "LF KubeCon Q3 Combined Suppression",
			HubSpotURL: "https://app.hubspot.com/contacts/8112310/lists/supp-1",
		}},
		SourceListIDs: []string{"a", "b"},
		PortalID:      "8112310",
	}
}

func composePayload(briefID string) *explore.ComposeAudienceMasterPayload {
	in := &explore.AudienceComposeMasterInput{ListIds: []string{"a", "b"}, ExcludeListIds: []string{"x"}}
	if briefID != "" {
		in.BriefID = &briefID
	}
	return &explore.ComposeAudienceMasterPayload{ProjectID: "proj-1", Compose: in}
}

// A recording compose must produce a row the DISPATCH guard will accept. That is the
// whole reason the portal is threaded out of the orchestration at all:
// assertAudiencePortal refuses an audience with no recorded portal, and
// refuseProvenanceBreakingPatch makes the value unrepairable afterwards -- so a blank
// built_in_portal_id here is a permanently undispatchable audience, not a cosmetic gap.
func TestComposeAudienceMasterRecordsTheComposedRowWithItsPortal(t *testing.T) {
	explorer := &fakeExplorer{outcome: composedOutcome()}
	repo := newCountingAudienceRepo()
	briefs := newFakeBriefRepo()
	seedBrief(t, briefs, "proj-1", "brief-1")

	svc := NewAudienceExploreService(explorer)
	svc.SetAudienceRepo(repo)
	svc.SetBriefRepo(briefs)

	res, err := svc.ComposeAudienceMaster(context.Background(), composePayload("brief-1"))
	require.NoError(t, err)

	assert.Equal(t, "brief-1", explorer.lastInput.RecordUnderBriefID,
		"the orchestration must know it is recording, since that is what makes it resolve the portal")

	rows := repo.rows()
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, "8112310", row.BuiltInPortalID, "provenance must be the portal the lists were actually created in")
	assert.Equal(t, model.AudienceBuilt, row.Status)
	assert.Equal(t, model.ProviderHubSpot, row.Platform)
	assert.Equal(t, "master-1", row.PlatformMasterListID)
	assert.Equal(t, "brief-1", row.BriefID)
	assert.Equal(t, "proj-1", row.ProjectID)

	var suppression []string
	require.NoError(t, json.Unmarshal(row.SuppressionListIDs, &suppression))
	assert.Equal(t, []string{"supp-1"}, suppression)
	assert.Contains(t, row.InclusionSummary, "2", "a derived summary must name how many lists the master unions")

	// `recorded` is explicit so no caller has to infer attachment from a present object.
	assert.True(t, res.Recorded)
	require.NotNil(t, res.Audience)
	assert.Equal(t, row.ID, res.Audience.ID)
	assert.Equal(t, "built", res.Audience.Status)
	assert.Equal(t, int64(1), res.Audience.Version)
	assert.Equal(t, "master-1", res.Audience.PlatformMasterListID)
}

// An explicit inclusion_summary is operator-written provenance and must not be
// overwritten by the derived one.
func TestComposeAudienceMasterKeepsAnExplicitInclusionSummary(t *testing.T) {
	explorer := &fakeExplorer{outcome: composedOutcome()}
	repo := newCountingAudienceRepo()
	briefs := newFakeBriefRepo()
	seedBrief(t, briefs, "proj-1", "brief-1")

	svc := NewAudienceExploreService(explorer)
	svc.SetAudienceRepo(repo)
	svc.SetBriefRepo(briefs)

	p := composePayload("brief-1")
	summary := "Attendees + speakers, minus unsubscribes"
	p.Compose.InclusionSummary = &summary

	_, err := svc.ComposeAudienceMaster(context.Background(), p)
	require.NoError(t, err)
	rows := repo.rows()
	require.Len(t, rows, 1)
	assert.Equal(t, summary, rows[0].InclusionSummary)
}

// The exploratory compose -- no brief to attach to -- must be exactly what it was
// before recording existed: no brief read, no repository write, and an input that does
// not ask the orchestration for the portal (which is a live HubSpot round trip).
func TestComposeAudienceMasterWithoutABriefRecordsNothing(t *testing.T) {
	explorer := &fakeExplorer{outcome: composedOutcome()}
	repo := newCountingAudienceRepo()
	// getErr rather than a call counter: an exploratory compose that read the brief
	// would fail outright here, which is a louder signal than a count assertion.
	briefs := newFakeBriefRepo()
	briefs.getErr = errors.New("GetBrief must not be called for an exploratory compose")

	svc := NewAudienceExploreService(explorer)
	svc.SetAudienceRepo(repo)
	svc.SetBriefRepo(briefs)

	res, err := svc.ComposeAudienceMaster(context.Background(), composePayload(""))
	require.NoError(t, err)

	assert.Empty(t, explorer.lastInput.RecordUnderBriefID)
	assert.Zero(t, repo.calls, "an exploratory compose must not write an audience row")
	assert.False(t, res.Recorded)
	assert.Nil(t, res.Audience)
	require.NotNil(t, res.Master)
	assert.Equal(t, "master-1", res.Master.ListID)
}

// A blank-but-present brief_id is the exploratory case, not a recording one. Trimming
// it to empty is what keeps a client that sends a blank string from tripping the 503.
func TestComposeAudienceMasterTreatsABlankBriefIDAsExploratory(t *testing.T) {
	explorer := &fakeExplorer{outcome: composedOutcome()}
	svc := NewAudienceExploreService(explorer)
	// Deliberately no repositories: a blank brief id must not reach the 503 arm.

	p := composePayload("")
	blank := "   "
	p.Compose.BriefID = &blank

	res, err := svc.ComposeAudienceMaster(context.Background(), p)
	require.NoError(t, err)
	assert.False(t, res.Recorded)
	assert.Equal(t, 1, explorer.composeCalls)
}

// The three refusals that must happen while refusing is still free.
func TestComposeAudienceMasterRefusesBeforeCreatingAnyList(t *testing.T) {
	tests := []struct {
		name     string
		repo     domain.AudienceRepository
		briefs   domain.BriefRepository
		assertOn func(t *testing.T, err error)
		why      string
	}{
		{
			name:   "no audience repository",
			repo:   nil,
			briefs: newFakeBriefRepo(),
			assertOn: func(t *testing.T, err error) {
				var unavailable *explore.ConnServiceUnavailableError
				require.ErrorAs(t, err, &unavailable)
				assert.Equal(t, "503", unavailable.Code)
				assert.Contains(t, unavailable.Message, "cannot record an audience")
			},
			why: "composing without recording would hand back a master the operator believes is wired to the send",
		},
		{
			name:   "no brief repository",
			repo:   newFakeAudienceRepo(),
			briefs: nil,
			assertOn: func(t *testing.T, err error) {
				var unavailable *explore.ConnServiceUnavailableError
				require.ErrorAs(t, err, &unavailable)
				assert.Equal(t, "503", unavailable.Code)
			},
			why: "without a brief repository the brief cannot be verified, so the attachment cannot be trusted",
		},
		{
			name:   "unknown brief",
			repo:   newFakeAudienceRepo(),
			briefs: newFakeBriefRepo(), // empty: GetBrief answers ErrNotFound
			assertOn: func(t *testing.T, err error) {
				var notFound *explore.NotFoundError
				require.ErrorAs(t, err, &notFound)
				assert.Equal(t, "404", notFound.Code)
				// audienceExploreErr reads domain.ErrNotFound as "no usable HubSpot
				// connection", so routing this read through it would send the operator
				// to reconnect HubSpot over a mistyped brief id.
				assert.Contains(t, notFound.Message, "no such brief")
			},
			why: "a 404 raised after two real HubSpot creates is unrollbackable and unretryable",
		},
		{
			name: "brief unreadable",
			repo: newFakeAudienceRepo(),
			briefs: func() *fakeBriefRepo {
				b := newFakeBriefRepo()
				b.getErr = errors.New("pool exhausted")
				return b
			}(),
			assertOn: func(t *testing.T, err error) {
				var unavailable *explore.ConnServiceUnavailableError
				require.ErrorAs(t, err, &unavailable)
				assert.Equal(t, "503", unavailable.Code)
				assert.Contains(t, unavailable.Message, "the brief could not be read")
			},
			why: "not knowing whether the brief exists is not evidence that it does not",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			explorer := &fakeExplorer{outcome: composedOutcome()}
			svc := NewAudienceExploreService(explorer)
			if tc.repo != nil {
				svc.SetAudienceRepo(tc.repo)
			}
			if tc.briefs != nil {
				svc.SetBriefRepo(tc.briefs)
			}

			_, err := svc.ComposeAudienceMaster(context.Background(), composePayload("brief-1"))
			require.Error(t, err)
			tc.assertOn(t, err)
			assert.Zero(t, explorer.composeCalls, tc.why)
		})
	}
}

// TestComposeAudienceMasterDoesNotRetryADefiniteRefusal is the counterpart to the retry test
// below: a transient blip earns one bounded retry, a SETTLED answer earns none.
//
// `ErrNotFound` means the brief was archived between the compose and the record.
// `ErrAudienceBuildInFlight` means another build for the same brief and platform holds the slot.
// Neither changes in 150ms, so retrying spends the caller's deadline for nothing -- and the
// partial it produces tells the operator to attach by hand rather than retry, which is wrong for
// an in-flight build, the one case where a later retry WOULD succeed.
func TestComposeAudienceMasterDoesNotRetryADefiniteRefusal(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"the brief was archived mid-request", domain.ErrNotFound},
		{"another build holds the slot", domain.ErrAudienceBuildInFlight},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			explorer := &fakeExplorer{outcome: composedOutcome()}
			repo := newCountingAudienceRepo()
			repo.createE = tc.err
			briefs := newFakeBriefRepo()
			seedBrief(t, briefs, "proj-1", "brief-1")

			svc := NewAudienceExploreService(explorer)
			svc.SetAudienceRepo(repo)
			svc.SetBriefRepo(briefs)

			_, err := svc.ComposeAudienceMaster(context.Background(), composePayload("brief-1"))
			require.Error(t, err)

			assert.Equal(t, 1, repo.calls,
				"a settled refusal must not be retried; it costs a round trip and 150ms of the caller's deadline")
			assert.Equal(t, 1, explorer.composeCalls, "and the list creates are never repeated")
		})
	}
}

// When the lists exist and only the attachment failed, the caller gets the fifth
// partial shape -- the one carrying a CONFIRMED master. It is a partial rather than a
// 500 because a 500 invites the retry that mints a second master list; the UI
// suppresses its retry affordance on the partial type alone.
func TestComposeAudienceMasterReportsARecordFailureAsAPartialCarryingTheMaster(t *testing.T) {
	explorer := &fakeExplorer{outcome: composedOutcome()}
	repo := newCountingAudienceRepo()
	repo.createE = errors.New("insert audience: connection reset")
	briefs := newFakeBriefRepo()
	seedBrief(t, briefs, "proj-1", "brief-1")

	svc := NewAudienceExploreService(explorer)
	svc.SetAudienceRepo(repo)
	svc.SetBriefRepo(briefs)

	_, err := svc.ComposeAudienceMaster(context.Background(), composePayload("brief-1"))
	require.Error(t, err)

	var partial *explore.AudienceComposePartialError
	require.ErrorAs(t, err, &partial)
	assert.Equal(t, "500", partial.Code)
	require.NotNil(t, partial.Master, "the operator can only attach the master by hand if they are told its id")
	assert.Equal(t, "master-1", partial.Master.ListID)
	require.NotNil(t, partial.Suppression)
	assert.Equal(t, "supp-1", partial.Suppression.ListID)
	assert.Nil(t, partial.MasterName, "MasterName is for an UNCONFIRMED master; this one is confirmed")
	assert.Contains(t, partial.Message, "do not compose again")

	assert.Equal(t, composeRecordAttempts, repo.calls,
		"the irreversible half already succeeded, so a transient blip gets one bounded retry")
	assert.Equal(t, 1, explorer.composeCalls, "the retry is of the INSERT, never of the list creates")
}
