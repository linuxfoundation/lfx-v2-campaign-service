// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	audiences "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBuilder records what the service asked the platform to do, so tests can assert the
// orchestration without a warehouse or a live HubSpot portal.
type fakeBuilder struct {
	mu sync.Mutex

	editions    []string
	editionsErr error

	created   []string // list names, in creation order
	createErr error
	// failOnNth makes the Nth CreateList call fail (1-based), modelling a partial build.
	failOnNth int
	// emptyIDOnNth returns a 2xx-with-no-id on the Nth call (1-based) — HubSpot's
	// UNCONFIRMED case.
	emptyIDOnNth int
}

func (f *fakeBuilder) ResolvePastEditions(context.Context, string, string, string) ([]string, error) {
	if f.editionsErr != nil {
		return nil, f.editionsErr
	}
	return f.editions, nil
}

func (f *fakeBuilder) CreateList(_ context.Context, projectID, name string, filter json.RawMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, name)
	n := len(f.created)
	if f.createErr != nil && (f.failOnNth == 0 || f.failOnNth == n) {
		return "", f.createErr
	}
	if f.emptyIDOnNth == n {
		return "", nil
	}
	if len(filter) == 0 {
		return "", errors.New("filter was empty")
	}
	if strings.TrimSpace(projectID) == "" {
		// Credentials are per project; an empty one would build in the wrong portal.
		return "", errors.New("project id was empty")
	}
	return "list-" + name, nil
}

func (f *fakeBuilder) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...)
}

// newBuildService wires an AudienceService with all three dependencies plus a brief.
func newBuildService(t *testing.T, b *fakeBuilder, details string) (*AudienceService, *fakeAudienceRepo) {
	t.Helper()
	arepo := newFakeAudienceRepo()
	brepo := newFakeBriefRepo()

	brief := &model.CampaignBrief{
		ID:           "brief-1",
		ProjectID:    "cncf",
		EventSlug:    "kubecon-korea-2026",
		EventDetails: json.RawMessage(details),
		Status:       model.BriefApproved,
	}
	brepo.briefs[briefKey(brief.ProjectID, brief.ID)] = brief

	s := NewAudienceService(arepo)
	s.SetBriefRepo(brepo)
	s.SetBuilder(b)
	return s, arepo
}

// TestBuildAudience_HappyPath covers the whole point of the endpoint: an approved brief becomes
// a BUILT audience with a master list, which is what unblocks the HubSpot dispatcher (it
// refuses any brief whose audience is unbuilt or carries no master list).
func TestBuildAudience_HappyPath(t *testing.T) {
	b := &fakeBuilder{editions: []string{"KubeCon Korea 2025"}}
	s, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea","location":"Korea","year":"2026"}`)

	res, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, string(model.AudienceBuilt), res.Status)
	require.NotNil(t, res.PlatformMasterListID)
	assert.NotEmpty(t, *res.PlatformMasterListID, "a built audience MUST carry its master list id")

	// All three deterministic groups: education + past-edition registrants + region-wide.
	assert.Len(t, b.names(), 3)

	// The provenance must record what was and was not built.
	require.NotNil(t, res.InclusionSummary)
	assert.Contains(t, *res.InclusionSummary, "KubeCon Korea 2025")
	assert.Contains(t, *res.InclusionSummary, "APAC")
	assert.Contains(t, *res.InclusionSummary, "Not included:")
}

// TestBuildAudience_FirstTimeEventStillBuilds pins that an event with no past editions still
// produces a usable audience. Refusing would leave the email channel permanently unable to
// send for any new event.
func TestBuildAudience_FirstTimeEventStillBuilds(t *testing.T) {
	b := &fakeBuilder{} // no editions
	s, _ := newBuildService(t, b, `{"eventName":"Brand New Summit 2026","country":"Japan"}`)

	res, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.AudienceBuilt), res.Status)
	assert.Len(t, b.names(), 1, "only the education-enrolled group is derivable without past editions")
	assert.Contains(t, *res.InclusionSummary, "No past editions resolved")
}

// TestBuildAudience_WarehouseOutageDegrades pins that a Snowflake failure narrows the audience
// rather than failing the build: group 4 needs no editions, so a usable audience is still
// produced and the gap is recorded.
func TestBuildAudience_WarehouseOutageDegrades(t *testing.T) {
	b := &fakeBuilder{editionsErr: errors.New("snowflake unreachable")}
	s, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)

	res, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.NoError(t, err, "a warehouse outage must not fail the whole build")
	assert.Equal(t, string(model.AudienceBuilt), res.Status)
	assert.Len(t, b.names(), 1)
	assert.Contains(t, *res.InclusionSummary, "No past editions resolved")
}

// TestBuildAudience_PartialBuildLeavesRowBuilding pins the reconciliation contract. When a
// HubSpot create fails midway, some lists already exist upstream — so the row must NOT be
// marked built (it has no master list) and must NOT be silently dropped either. Leaving it
// BUILDING with the failure recorded is what makes the partial state reconcilable.
func TestBuildAudience_PartialBuildLeavesRowBuilding(t *testing.T) {
	b := &fakeBuilder{
		editions:  []string{"KubeCon Korea 2025"},
		createErr: errors.New("hubspot 429"),
		failOnNth: 2, // the first list succeeds, the second fails
	}
	s, arepo := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err, "a failed upstream create must surface to the caller")

	rows := arepo.rows()
	require.Len(t, rows, 1, "the audience row must still exist so the partial build is visible")
	assert.Equal(t, model.AudienceBuilding, rows[0].Status,
		"a partial build must stay BUILDING: it has no master list, and lists exist upstream to reconcile")
	assert.Empty(t, rows[0].PlatformMasterListID)
	assert.Contains(t, rows[0].InclusionSummary, "Build incomplete")

	// It must stop at the failure rather than creating more unrecordable portal state.
	assert.Len(t, b.names(), 2)
}

// TestBuildAudience_UnconfirmedCreateFailsClosed pins the UNCONFIRMED case: a 2xx with no list
// id means HubSpot MAY have created the list. Treating that as success would point the
// audience at nothing; treating it as a clean failure would invite a blind retry that
// duplicates the list. It must error and say so.
func TestBuildAudience_UnconfirmedCreateFailsClosed(t *testing.T) {
	b := &fakeBuilder{emptyIDOnNth: 1}
	s, arepo := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err)
	// Goa's generated Error() returns "" — the operator-facing text travels in Message, so
	// assert there. Checking err.Error() would pass vacuously against any error at all.
	var ise *audiences.InternalServerError
	require.ErrorAs(t, err, &ise)
	assert.Contains(t, strings.ToUpper(ise.Message), "UNCONFIRMED",
		"the caller must be told to verify before retrying, not to retry blindly")

	rows := arepo.rows()
	require.Len(t, rows, 1)
	assert.Equal(t, model.AudienceBuilding, rows[0].Status)
}

// TestBuildAudience_RequiresEventDetails pins the two brief fields with no safe default. A
// missing country cannot be inferred, and a missing event name would produce colliding
// portal-global list names.
func TestBuildAudience_RequiresEventDetails(t *testing.T) {
	cases := map[string]string{
		"no country":    `{"eventName":"KubeCon Korea 2026"}`,
		"no event name": `{"country":"South Korea"}`,
		"empty details": `{}`,
	}
	for name, details := range cases {
		t.Run(name, func(t *testing.T) {
			s, arepo := newBuildService(t, &fakeBuilder{}, details)
			_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
				ProjectID: "cncf", BriefID: "brief-1",
			})
			require.Error(t, err)
			var badReq *audiences.BadRequestError
			assert.ErrorAs(t, err, &badReq, "a malformed brief is a 400, not a 500")
			assert.Empty(t, arepo.rows(), "nothing may be recorded when the brief cannot be planned")
		})
	}
}

// TestBuildAudience_UnavailableWithoutDeps pins the typed 503. A deployment without the
// HubSpot/Snowflake clients must return the contract's 503 rather than panicking on a nil
// client — the CRUD routes stay usable.
func TestBuildAudience_UnavailableWithoutDeps(t *testing.T) {
	s := NewAudienceService(newFakeAudienceRepo()) // no brief repo, no builder
	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err)
	var unavail *audiences.ConnServiceUnavailableError
	assert.ErrorAs(t, err, &unavail)
}

// rows returns the stored audiences, so a test can inspect what survived a partial build.
func (r *fakeAudienceRepo) rows() []*model.CampaignAudience {
	out := make([]*model.CampaignAudience, 0, len(r.items))
	for _, a := range r.items {
		out = append(out, a)
	}
	return out
}
