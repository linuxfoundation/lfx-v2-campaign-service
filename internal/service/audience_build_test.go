// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	audiences "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBuilder records what the service asked the platform to do, so tests can assert the
// orchestration without a warehouse or a live HubSpot portal.
type fakeBuilder struct {
	mu sync.Mutex

	editions    []string
	editionsErr error

	created   []string          // list names, in creation order
	filters   map[string][]byte // name -> filter, so a test can assert the master's union
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
	if f.filters == nil {
		f.filters = map[string][]byte{}
	}
	f.filters[name] = append([]byte(nil), filter...)
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

// filterFor returns the filter a list was created with.
func (f *fakeBuilder) filterFor(name string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.filters[name]
}

// BeginBuild is a no-op in the fake: the scope only affects client caching, which the fake
// does not do.
func (f *fakeBuilder) BeginBuild(ctx context.Context) context.Context { return ctx }

func (f *fakeBuilder) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...)
}

// newBuildService wires an AudienceService with all three dependencies plus a brief.
func newBuildService(t *testing.T, b *fakeBuilder, details string) (*AudienceService, *fakeAudienceRepo, *fakeBriefRepo) {
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
	return s, arepo, brepo
}

// TestBuildAudience_HappyPath covers the whole point of the endpoint: an approved brief becomes
// a BUILT audience with a master list, which is what unblocks the HubSpot dispatcher (it
// refuses any brief whose audience is unbuilt or carries no master list).
func TestBuildAudience_HappyPath(t *testing.T) {
	b := &fakeBuilder{editions: []string{"KubeCon Korea 2025"}}
	s, _, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea","location":"Korea","year":"2026"}`)

	res, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, string(model.AudienceBuilt), res.Status)
	require.NotNil(t, res.PlatformMasterListID)
	assert.NotEmpty(t, *res.PlatformMasterListID, "a built audience MUST carry its master list id")

	// Three inclusion groups (education + past-edition registrants + region-wide) PLUS the
	// master. The master must be a real union: the dispatcher sends only to
	// platform_master_list_id, so recording one inclusion list as the master would create the
	// others and never email them.
	names := b.names()
	require.Len(t, names, 4, "three inclusion lists plus a master")
	assert.Contains(t, names[3], "Master", "the LAST list created must be the master union")
	assert.NotEqual(t, "list-"+names[0], *res.PlatformMasterListID,
		"the master must not be the first inclusion list")
	assert.Equal(t, "list-"+names[3], *res.PlatformMasterListID)

	// The master's filter must reference every inclusion list created before it.
	masterFilter := b.filterFor(names[3])
	for _, inc := range names[:3] {
		assert.Contains(t, string(masterFilter), "list-"+inc,
			"the master union must include every inclusion list")
	}

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
	s, _, _ := newBuildService(t, b, `{"eventName":"Brand New Summit 2026","country":"Japan"}`)

	res, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.NoError(t, err)
	assert.Equal(t, string(model.AudienceBuilt), res.Status)
	assert.Len(t, b.names(), 2, "the education-enrolled group plus its master")
	assert.Contains(t, *res.InclusionSummary, "No past editions resolved")
}

// TestBuildAudience_WarehouseOutageDegrades pins that a Snowflake failure narrows the audience
// rather than failing the build: group 4 needs no editions, so a usable audience is still
// produced and the gap is recorded.
func TestBuildAudience_WarehouseOutageDegrades(t *testing.T) {
	b := &fakeBuilder{editionsErr: errors.New("snowflake unreachable")}
	s, _, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)

	res, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.NoError(t, err, "a warehouse outage must not fail the whole build")
	assert.Equal(t, string(model.AudienceBuilt), res.Status)
	assert.Len(t, b.names(), 2, "the education-enrolled group plus its master")

	// The summary must say the history could not be READ. Reporting the first-time-event note
	// here would tell an operator the opposite of the truth: that a returning event legitimately
	// has no past editions, when in fact the audience is narrower than intended and should be
	// rebuilt. InclusionSummary is the durable record that outlives the log line.
	assert.Contains(t, *res.InclusionSummary, "could NOT be resolved",
		"a warehouse outage must be reported as an outage")
	assert.Contains(t, *res.InclusionSummary, "snowflake unreachable",
		"and must carry the underlying cause")
	assert.Contains(t, *res.InclusionSummary, "NARROWER THAN INTENDED")
	assert.NotContains(t, *res.InclusionSummary, "Expected for a first-time event",
		"an outage is NOT a first-time event; conflating them hides a rebuild that is needed")
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
	s, arepo, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)

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
	// The ids of lists that DO exist upstream must be recorded — they are the only handles an
	// operator has to reconcile them, and a retry without them would duplicate.
	assert.Contains(t, rows[0].InclusionSummary, "ALREADY CREATED",
		"a partial build must record what it left in the portal")
	assert.Contains(t, rows[0].InclusionSummary, "list-"+b.names()[0])

	// It must stop at the failure rather than creating more unrecordable portal state.
	assert.Len(t, b.names(), 2)
}

// TestBuildAudience_UnpersistedPartialReturnsTheListIDs pins the COMPOUND failure: the HubSpot
// build broke midway AND the write that records what it left behind also broke.
//
// TestBuildAudience_PartialBuildLeavesRowBuilding above covers the case where that write
// SUCCEEDS, so the ids live in inclusion_summary. When it fails, the row stays 'building' with an
// EMPTY summary while real HubSpot lists exist — the exact unreconcilable state the code comment
// claims is fixed. The API response is then the only channel left carrying the ids, so it must
// carry them: without this the operator learns a build broke, has no handle on the orphans, and a
// blind retry duplicates every list.
func TestBuildAudience_UnpersistedPartialReturnsTheListIDs(t *testing.T) {
	b := &fakeBuilder{
		editions:  []string{"KubeCon Korea 2025"},
		createErr: errors.New("hubspot 429"),
		failOnNth: 2, // the first list succeeds upstream, the second fails
	}
	s, arepo, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)
	arepo.updateE = errors.New("connection reset by peer")

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err)

	// Goa's generated Error() returns "" — assert on Message, or this passes vacuously.
	var ise *audiences.InternalServerError
	require.ErrorAs(t, err, &ise)

	created := b.names()
	require.NotEmpty(t, created, "the fixture must leave at least one list upstream")
	assert.Contains(t, ise.Message, "list-"+created[0],
		"the id of a list that EXISTS in HubSpot must reach the caller when the DB write cannot "+
			"record it; it is the only remaining handle for reconciliation")
	assert.Contains(t, ise.Message, "reconciled before retrying",
		"the caller must be told not to retry blindly into duplicate lists")
	assert.Contains(t, ise.Message, "hubspot 429",
		"the original build failure must not be swallowed by the persistence failure")

	// Precondition: the write really was rejected, so the DATABASE never received the summary.
	// (The fake stores the row by pointer, so rows()[0] aliases the in-memory struct the service
	// mutated — asserting on its InclusionSummary would read the value the real DB never got.
	// Version is the honest signal: UpdateAudience never ran to completion, so it is still 1.)
	rows := arepo.rows()
	require.Len(t, rows, 1, "the row must still exist so the partial build is visible at all")
	assert.Equal(t, int64(1), rows[0].Version,
		"precondition: the update was rejected, so the persisted row still carries no record of "+
			"the created lists — which is why the error must carry them instead")
}

// TestBuildAudience_UnpersistedSuccessReturnsTheListIDs pins the WORST version of the
// unrecorded-lists problem: the build fully SUCCEEDED upstream and only the write failed.
//
// This path is strictly worse than the partial one. Every inclusion list exists AND so does the
// MASTER, so a blind retry duplicates the entire set rather than part of it. It also had no
// safety net: mapAudienceErr has no case for a database error, so a pgx failure fell through to
// `default:` and returned a bare "an internal server error occurred" carrying nothing at all.
// The ids existed only in a slog line the caller deciding whether to retry cannot see.
func TestBuildAudience_UnpersistedSuccessReturnsTheListIDs(t *testing.T) {
	b := &fakeBuilder{editions: []string{"KubeCon Korea 2025"}} // no createErr: the build succeeds
	s, arepo, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)
	arepo.updateE = errors.New("pq: connection reset by peer")

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err)

	var ise *audiences.InternalServerError
	require.ErrorAs(t, err, &ise)

	created := b.names()
	require.Len(t, created, 4, "fixture precondition: three inclusion lists plus the master")
	masterID := "list-" + created[3]
	inclusionID := "list-" + created[0]

	assert.Contains(t, ise.Message, masterID,
		"the MASTER exists upstream and is the list the dispatcher sends to; without its id in "+
			"the response the operator cannot reconcile it and a retry duplicates it")
	assert.Contains(t, ise.Message, inclusionID,
		"every inclusion list that exists must be reconcilable too")
	assert.Contains(t, ise.Message, "reconciled before retrying")
	assert.NotEqual(t, "an internal server error occurred", ise.Message,
		"a DB error must not fall through to the generic 500 that carries no ids")

	// The master must be named exactly once: createPlanLists already returns it as the last
	// element of ids, so a naive append would list it twice and read like two orphans.
	assert.Equal(t, 1, strings.Count(ise.Message, masterID),
		"the master id must appear once, not be duplicated by re-appending it to ids")

	// Precondition: the write really was rejected, so the row never got the summary. (The fake
	// stores rows by pointer, so assert on Version, which only advances on a completed update.)
	rows := arepo.rows()
	require.Len(t, rows, 1)
	assert.Equal(t, int64(1), rows[0].Version,
		"precondition: the update was rejected, so the persisted row records none of these lists")
}

// TestBuildAudience_AmbiguousUpstreamSaysUnconfirmed pins that EVERY ambiguous outcome says so
// in the RESPONSE, not just in the row state.
//
// The row logic classifies ambiguity through hubspot.IsUnconfirmed, which covers four sources: a
// 2xx-with-no-id, a mutating 429, a mutating 5xx, and a mutating transport failure. Only the
// first spells out "verify before retrying" in its own message — the other three used to surface
// as a plain 500 reading like an ordinary transient error, inviting the blind retry that
// duplicates a list HubSpot may already have created.
//
// The error here comes from a REAL hubspot.Client against a 429, not a hand-rolled stand-in: the
// ambiguous error types are unexported, so fabricating one would test the fake rather than the
// classification that actually runs in production.
func TestBuildAudience_AmbiguousUpstreamSaysUnconfirmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A mutating 429: HubSpot may or may not have applied the create.
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	hc := hubspot.NewClient(
		hubspot.Credentials{PrivateAppToken: "t"}, hubspot.AccountConfig{PortalID: "8112310"},
		hubspot.WithBaseURL(srv.URL),
	)
	_, upstreamErr := hc.CreateList(context.Background(), "probe", json.RawMessage(`{"filterBranches":[]}`))
	require.Error(t, upstreamErr)
	require.True(t, hubspot.IsUnconfirmed(upstreamErr),
		"fixture precondition: a mutating 429 must classify as ambiguous, else this test proves nothing")
	require.NotContains(t, upstreamErr.Error(), "UNCONFIRMED",
		"fixture precondition: the raw error must NOT already carry the warning — that is the gap")

	b := &fakeBuilder{createErr: upstreamErr, failOnNth: 1}
	s, arepo, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err)

	var ise *audiences.InternalServerError
	require.ErrorAs(t, err, &ise)
	assert.Contains(t, ise.Message, "UNCONFIRMED",
		"an ambiguous upstream outcome must be labelled in the response, not only in the row")
	assert.Contains(t, ise.Message, "verify before",
		"the caller must be told to verify rather than retry blindly into a duplicate list")

	// And the row agrees: ambiguous means BUILDING, never FAILED, even with no ids created.
	rows := arepo.rows()
	require.Len(t, rows, 1)
	assert.Equal(t, model.AudienceBuilding, rows[0].Status,
		"an ambiguous outcome must not be marked failed: a list may exist upstream")
}

// TestBuildAudience_UnconfirmedCreateFailsClosed pins the UNCONFIRMED case: a 2xx with no list
// id means HubSpot MAY have created the list. Treating that as success would point the
// audience at nothing; treating it as a clean failure would invite a blind retry that
// duplicates the list. It must error and say so.
func TestBuildAudience_UnconfirmedCreateFailsClosed(t *testing.T) {
	b := &fakeBuilder{emptyIDOnNth: 1}
	s, arepo, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)

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
			s, arepo, _ := newBuildService(t, &fakeBuilder{}, details)
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

// TestBuildAudience_RequiresAnApprovedBrief pins the lifecycle guard. Building creates REAL
// HubSpot lists and makes the brief sendable, so a draft must not reach it — its event details
// are still being edited, and the campaign-creation path applies the same guard.
func TestBuildAudience_RequiresAnApprovedBrief(t *testing.T) {
	for _, status := range []model.BriefStatus{model.BriefDraft, model.BriefArchived} {
		t.Run(string(status), func(t *testing.T) {
			b := &fakeBuilder{}
			s, arepo, brepo := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)
			// Move the seeded brief out of the approved state.
			brepo.briefs[briefKey("cncf", "brief-1")].Status = status

			_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
				ProjectID: "cncf", BriefID: "brief-1",
			})
			require.Error(t, err, "a %s brief must not be buildable", status)
			var badReq *audiences.BadRequestError
			assert.ErrorAs(t, err, &badReq)

			assert.Empty(t, b.names(), "no HubSpot list may be created for an unapproved brief")
			assert.Empty(t, arepo.rows(), "no audience row may be recorded either")
		})
	}
}

// TestBuildAudience_DefiniteFailureMarksFailed pins the failed-vs-building distinction. When
// NOTHING was created and the failure is definite (bad credentials, a plain 4xx), the row must
// be FAILED: leaving it building tells an operator to hunt for portal orphans that do not exist.
func TestBuildAudience_DefiniteFailureMarksFailed(t *testing.T) {
	b := &fakeBuilder{createErr: errors.New("hubspot 401 unauthorized"), failOnNth: 1}
	s, arepo, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err)

	rows := arepo.rows()
	require.Len(t, rows, 1)
	assert.Equal(t, model.AudienceFailed, rows[0].Status,
		"nothing was created and the failure is definite: FAILED, not building")
	assert.Contains(t, rows[0].InclusionSummary, "No HubSpot lists were created.")
}

// TestBuildAudience_UnconfirmedFirstCreateStaysBuilding pins the opposite case. A 2xx with no
// list id on the FIRST create means a list may exist upstream even though `ids` is empty — so
// the row must stay BUILDING. hubspot.IsUnconfirmed cannot classify this (the client returned
// no typed error), which is why the sentinel exists.
func TestBuildAudience_UnconfirmedFirstCreateStaysBuilding(t *testing.T) {
	b := &fakeBuilder{emptyIDOnNth: 1}
	s, arepo, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err)

	rows := arepo.rows()
	require.Len(t, rows, 1)
	assert.Equal(t, model.AudienceBuilding, rows[0].Status,
		"an unconfirmed create may have made a list: BUILDING, so the operator verifies before retrying")
}

// TestBuildAudience_SendsAYearFreeSearchTerm pins the fix at the SERVICE boundary. The warehouse
// query matches the term AND excludes the year, so passing the full event name ("KubeCon Korea
// 2026") asks for rows containing 2026 that do not contain 2026 — unsatisfiable. Every returning
// event then degraded to a country-only audience while reporting success.
//
// The concrete builder also strips the year, but the INTERFACE is what matters: relying on an
// implementation to undo a bad argument means the next implementation reinherits the bug.
func TestBuildAudience_SendsAYearFreeSearchTerm(t *testing.T) {
	rb := &recordingBuilder{editions: []string{"KubeCon Korea 2025"}}
	s, _, _ := newBuildService(t, &fakeBuilder{}, `{"eventName":"KubeCon Korea 2026","country":"South Korea","year":"2026"}`)
	s.SetBuilder(rb)

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.NoError(t, err)

	assert.Equal(t, "KubeCon Korea", rb.term,
		"the search term must be the year-free family, or the warehouse query matches nothing")
	assert.NotContains(t, rb.term, "2026")
	assert.Equal(t, "2026", rb.year)
}

// TestEventFamily covers the year sources and the degrade.
func TestEventFamily(t *testing.T) {
	cases := []struct{ name, detailYear, wantFamily, wantYear string }{
		{"KubeCon Korea 2026", "2026", "KubeCon Korea", "2026"},
		{"KubeCon Korea 2026", "", "KubeCon Korea", "2026"},     // derived from the name
		{"KubeCon Korea 2026", "bad", "KubeCon Korea", "2026"},  // malformed detail year ignored
		{"KubeCon Korea 2026", "2025", "KubeCon Korea", "2026"}, // STALE detail year loses to the name
		{"Open Summit", "2027", "Open Summit", "2027"},          // year not in the name
		{"Open Summit", "", "Open Summit", ""},                  // no year anywhere: degrade
		{"2026", "2026", "2026", "2026"},                        // stripping would empty it
	}
	for _, c := range cases {
		f, y := eventFamily(c.name, c.detailYear)
		assert.Equal(t, c.wantFamily, f, "family for %q/%q", c.name, c.detailYear)
		assert.Equal(t, c.wantYear, y, "year for %q/%q", c.name, c.detailYear)
	}
}

// recordingBuilder captures what the service passed across the AudienceBuilder interface.
type recordingBuilder struct {
	term, location, year string
	editions             []string
}

func (r *recordingBuilder) ResolvePastEditions(_ context.Context, eventTerm, locationTerm, currentYear string) ([]string, error) {
	r.term, r.location, r.year = eventTerm, locationTerm, currentYear
	return r.editions, nil
}

func (r *recordingBuilder) CreateList(_ context.Context, _, name string, _ json.RawMessage) (string, error) {
	return "list-" + name, nil
}

func (r *recordingBuilder) BeginBuild(ctx context.Context) context.Context { return ctx }

// TestBuildAudience_StaleApprovalIsRejected pins the TOCTOU guard. The approval check happens
// BEFORE past-edition resolution (a warehouse round-trip), so a concurrent ReplaceBrief can
// reset the brief to draft and bump its version in that window. The plain create only checks
// `status <> 'archived'`, so without this gate the build would go on to create REAL HubSpot
// lists from a stale approved snapshot.
func TestBuildAudience_StaleApprovalIsRejected(t *testing.T) {
	b := &fakeBuilder{}
	s, arepo, brepo := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)

	// Give the brief a real version, then mark that version stale: the insert is gated on the
	// version observed at the approval check, so this models a ReplaceBrief committing in the
	// window between that check and the insert.
	brief := brepo.briefs[briefKey("cncf", "brief-1")]
	brief.Version = 3
	arepo.staleAt = brief.Version

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err, "a brief that moved mid-build must not produce an audience")

	var conflict *audiences.ConflictError
	assert.ErrorAs(t, err, &conflict, "a moved brief is a 409, not a 404 or 500")

	assert.Empty(t, b.names(), "no HubSpot list may be created from a stale approved snapshot")
	assert.Empty(t, arepo.rows(), "and no audience row may be recorded")
}
