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
	// entered/release, when non-nil, let a test hold a build inside the warehouse call: the
	// FIRST arrival closes `entered` and blocks until `release` is closed. This is the only
	// way to observe WHEN the build lease is taken relative to the slow pre-build work.
	// Only the first arrival is held, deliberately: a second one getting this far means the
	// lease was NOT taken first, and it has to be allowed to run on so the test fails on the
	// duplicate lists it creates rather than on the harness.
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once

	created   []string          // list names, in creation order
	filters   map[string][]byte // name -> filter, so a test can assert the master's union
	createErr error
	// duringResolve, when set, runs inside the warehouse call — the window between the claim
	// and the first HubSpot list. A test uses it to land a concurrent ReplaceBrief exactly
	// where one can actually do damage.
	duringResolve func()
	// failOnNth makes the Nth CreateList call fail (1-based), modelling a partial build.
	failOnNth int
	// emptyIDOnNth returns a 2xx-with-no-id on the Nth call (1-based) — HubSpot's
	// UNCONFIRMED case.
	emptyIDOnNth int
}

func (f *fakeBuilder) ResolvePastEditions(context.Context, string, string, string) ([]string, error) {
	if f.duringResolve != nil {
		f.duringResolve()
	}
	if f.entered != nil {
		held := false
		f.enterOnce.Do(func() { held = true })
		if held {
			close(f.entered)
			<-f.release
		}
	}
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
	// One brief store behind both fakes, because there is one campaign_briefs table behind
	// both the claim's row lock and the service's read.
	arepo.briefs = brepo

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
	// The error message is redacted to avoid exposing connection details in the response/storage;
	// the detailed error is only in the contextual slog.WarnContext call for trusted log sinks.
	assert.Contains(t, *res.InclusionSummary, "could NOT be resolved",
		"a warehouse outage must be reported as an outage")
	assert.Contains(t, *res.InclusionSummary, "warehouse failure",
		"a generic warehouse error message (redacted for security)")
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
	assert.Contains(t, ise.Message, "failed upstream",
		"unlike the success path, this failure DID start upstream — the label must stay accurate "+
			"in both directions, not merely be removed everywhere")

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

// TestBuildAudience_NoLocationRecordsTheBroadLookup pins that a brief with no location still
// builds, and says in the durable record that its editions were matched broadly.
//
// ResolvePastEventNames omits its location predicate when locationTerm is blank, so a multi-city
// family resolves other cities' editions. Refusing to build (or degrading to country-only) would
// throw away a correct returning-event audience every time a brief omits an OPTIONAL field —
// worse than the imprecision, because groups 5 and 7 AND the host country/region onto every
// edition branch, so a stray edition widens the audience to family alumni already in the target
// geography rather than reaching outside it.
func TestBuildAudience_NoLocationRecordsTheBroadLookup(t *testing.T) {
	b := &fakeBuilder{editions: []string{"Open Source Summit Milan 2025"}}
	// No "location" key at all — the optional field a brief may legitimately omit.
	s, _, _ := newBuildService(t, b, `{"eventName":"Open Source Summit 2026","country":"Japan","year":"2026"}`)

	res, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.NoError(t, err, "a missing optional location must not fail the build")
	assert.Equal(t, string(model.AudienceBuilt), res.Status)

	require.NotNil(t, res.InclusionSummary)
	assert.Contains(t, *res.InclusionSummary, "OTHER CITIES",
		"the summary must disclose that editions were matched on the family alone")
	assert.Contains(t, *res.InclusionSummary, "Open Source Summit Milan 2025",
		"and must list the editions actually matched, so the breadth is auditable")

	// A located brief must not carry the note, or it becomes noise on every audience.
	b2 := &fakeBuilder{editions: []string{"Open Source Summit Tokyo 2025"}}
	s2, _, _ := newBuildService(t, b2, `{"eventName":"Open Source Summit 2026","country":"Japan","location":"Tokyo","year":"2026"}`)
	res2, err2 := s2.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.NoError(t, err2)
	assert.NotContains(t, *res2.InclusionSummary, "OTHER CITIES")
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

	// The message must not blame HubSpot. Here HubSpot is the one system known to be FINE — it
	// created everything — and only the local write failed. "failed upstream" would send the
	// operator to investigate the platform when the remedy is to reconcile the ids listed below,
	// and would contradict the wrapped message telling them those lists EXIST.
	assert.NotContains(t, ise.Message, "failed upstream",
		"the upstream build succeeded; blaming it points reconciliation at the wrong system")
	assert.Contains(t, ise.Message, "created but recording them failed",
		"the message must name the step that actually failed")

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

			// A row IS recorded, and released. The claim is taken before the brief is read,
			// so by the time the details are found unusable the lease is already held — that
			// ordering is deliberate (see BuildAudience) and it is what makes the lease a
			// bound rather than a probability. What must not survive is the lease itself: a
			// row left `building` after a 400 would block every later build of this brief.
			rows := arepo.rows()
			require.Len(t, rows, 1, "the claim precedes the brief read, so its row exists")
			assert.Equal(t, model.AudienceFailed, rows[0].Status,
				"a claim abandoned before any HubSpot call must be released, not left holding "+
					"the lease")
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

		// A details year that is four digits but outside the range yearInName can extract.
		// The details field is hand-edited, so this is reachable; a first-digit-only check
		// would let "1000"/"2999" through here while the warehouse client rejected them,
		// which is the same predicate drift in the other direction. This is a separate copy
		// of isSupportedYear from the warehouse client's, so its own test has to reach it.
		{"Open Summit", "9999", "Open Summit", ""}, // above the range
		{"Open Summit", "0202", "Open Summit", ""}, // below the range
		{"Open Summit", "1899", "Open Summit", ""}, // just under 19xx
		{"Open Summit", "2100", "Open Summit", ""}, // just over 20xx
		{"Open Summit", "1900", "Open Summit", "1900"},
		{"Open Summit", "2099", "Open Summit", "2099"},
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

// TestBuildAudience_StaleApprovalIsRejected pins the TOCTOU guard where it actually has to
// hold: INSIDE the warehouse round-trip. The claim now runs before that round-trip, so the
// claim's own approval gate only dates the approval to the moment the lease was taken — a
// ReplaceBrief committing while past editions resolve is invisible to it, and the build would
// go on to create REAL HubSpot lists from a snapshot the operator has since withdrawn. The
// re-check immediately before the first list creation is the thing under test, so the brief is
// moved from inside ResolvePastEditions rather than before the call.
func TestBuildAudience_StaleApprovalIsRejected(t *testing.T) {
	b := &fakeBuilder{}
	s, arepo, brepo := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)

	brief := brepo.briefs[briefKey("cncf", "brief-1")]
	brief.Version = 3
	b.duringResolve = func() {
		// A concurrent ReplaceBrief: back to draft, version bumped. Either alone is
		// disqualifying, and a real edit does both.
		brief.Status = model.BriefDraft
		brief.Version = 4
	}

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err, "a brief that moved mid-build must not produce an audience")

	var conflict *audiences.ConflictError
	assert.ErrorAs(t, err, &conflict, "a moved brief is a 409, not a 404 or 500")

	assert.Empty(t, b.names(), "no HubSpot list may be created from a stale approved snapshot")

	// A row DOES exist now — the claim was taken before the brief moved, which is the whole
	// reason the re-check is needed. What matters is that the lease was handed back: left
	// `building`, it would block every later build of this brief behind a 409.
	rows := arepo.rows()
	require.Len(t, rows, 1, "the claim was taken before the brief moved, so its row exists")
	assert.Equal(t, model.AudienceFailed, rows[0].Status,
		"an abandoned claim must be released; nothing was created upstream, so there is "+
			"nothing to reconcile before failing it")
}

// TestBuildAudience_ApprovalMovingBeforeTheClaimIsAPlain400 keeps the ordinary user error
// ordinary. The claim gates on approval itself, so a draft brief is refused by the repository
// with ErrStaleApproval — the sentinel whose message is about versions and whose status is 409.
// That is the right answer for a brief that moved mid-build and the wrong one for a brief that
// was simply never approved, which is a 400 telling the caller what to do about it.
func TestBuildAudience_ApprovalMovingBeforeTheClaimIsAPlain400(t *testing.T) {
	b := &fakeBuilder{}
	s, arepo, brepo := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)
	brepo.briefs[briefKey("cncf", "brief-1")].Status = model.BriefDraft

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err)

	var badReq *audiences.BadRequestError
	assert.ErrorAs(t, err, &badReq,
		"a brief that was never approved is a 400 about its status, not a 409 about versions")
	assert.Empty(t, arepo.rows(), "a refused claim inserts nothing")
}

// TestBuildAudience_MissingBriefIsStillA404 pins the third case the claim's gate cannot tell
// apart on its own, and the one moving the claim first was most likely to lose.
//
// The gate reads the brief under its lock and refuses anything not approved — including a brief
// that is not there, which comes back as the same domain.ErrStaleApproval a moved brief does.
// Left to that mapping, a build for a deleted or archived brief answers "the brief changed while
// its audience was being built; refresh and rebuild": a 409 sending the caller to refresh
// something that does not exist, about a race they were not in. Before the reorder this was a
// plain 404 — a brief read came first and answered the question directly — so the reorder must
// not be allowed to take that away.
func TestBuildAudience_MissingBriefIsStillA404(t *testing.T) {
	b := &fakeBuilder{}
	s, arepo, brepo := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)
	delete(brepo.briefs, briefKey("cncf", "brief-1"))

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err)

	var notFound *audiences.NotFoundError
	assert.ErrorAs(t, err, &notFound,
		"a brief that is not there is a 404, not a 409 telling the caller to refresh it")
	assert.Empty(t, arepo.rows(), "a refused claim inserts nothing")
}

// TestBuildAudience_ConcurrentBuildIsRefusedWithItsOwnMessage is the service half of the build
// lease (migration 000018). The index does the arbitration, but what the loser is TOLD is a
// service decision, and getting it wrong is expensive here: the generic ErrConflict message,
// "the resource already exists", instructs the caller to stop asking for something that exists —
// when in fact nothing they asked for exists yet, and the right move is to wait.
//
// It also must not be confused with the stale-approval 409 above. Both are conflicts on the same
// call, and their remedies are opposites: a moved brief says REFRESH AND REBUILD, while a held
// lease says do NOT rebuild, because a rebuild is precisely what would duplicate the in-flight
// build's HubSpot lists.
func TestBuildAudience_ConcurrentBuildIsRefusedWithItsOwnMessage(t *testing.T) {
	b := &fakeBuilder{editions: []string{"KubeCon Korea 2025"}}
	s, arepo, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea"}`)
	arepo.leaseHeld = true

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, err)

	var conflict *audiences.ConflictError
	require.ErrorAs(t, err, &conflict, "a build that lost the lease is a 409")

	assert.Contains(t, conflict.Message, "already in progress",
		"the caller must be told a build is running, not that its own request duplicates something")
	assert.NotContains(t, conflict.Message, "the resource already exists",
		"the generic conflict message tells the caller to stop asking for something that exists; "+
			"nothing they asked for exists yet")
	assert.NotContains(t, conflict.Message, "refresh and rebuild",
		"that is the STALE-APPROVAL remedy, and it is the exact opposite of what to do here — "+
			"rebuilding is what duplicates the in-flight build's HubSpot lists")
	// The remedy has to survive the case it is most likely to be READ in. A build that is
	// genuinely stuck is the one least likely to have recorded its lists — the claim inserts
	// with an empty inclusion_summary and ids land only after createPlanLists returns — so a
	// message pointing only at the row lets an operator find nothing, conclude there is
	// nothing to reconcile, fail the row, and let the next build duplicate lists that are
	// sitting in the portal. The row-id prefix finds them either way.
	assert.Contains(t, conflict.Message, "first 8 characters of the audience row id",
		"the remedy must name the handle that works when the row recorded nothing")
	assert.Contains(t, conflict.Message, "EMPTY inclusion_summary",
		"and must say WHY, or the prefix reads as a redundant second option")

	assert.Empty(t, arepo.rows(), "the loser must not record a row; the index rejected its insert")
}

// TestBuildAudience_SecondRequestIsRejectedWhileTheFirstResolvesEditions pins the ORDERING the
// build lease depends on. The partial unique index only serializes builds whose rows overlap, so
// the claim has to be inserted before the slow pre-build work, not after it: with the warehouse
// round-trip in front, a double-click's second request can be delayed past the first request's
// entire build, insert cleanly against its now-`built` row, and create a second complete set of
// HubSpot lists. Concurrent repository inserts cannot catch that — the two requests never reach
// the repository at the same time. This test holds request A inside ResolvePastEditions and runs
// B to completion against it.
func TestBuildAudience_SecondRequestIsRejectedWhileTheFirstResolvesEditions(t *testing.T) {
	b := &fakeBuilder{
		editions: []string{"KubeCon Korea 2025"},
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	s, _, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea","location":"Korea","year":"2026"}`)

	firstErr := make(chan error, 1)
	go func() {
		_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
			ProjectID: "cncf", BriefID: "brief-1",
		})
		firstErr <- err
	}()
	<-b.entered // A is in the warehouse call, before any HubSpot create.

	_, err := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	var conflict *audiences.ConflictError
	require.ErrorAs(t, err, &conflict,
		"the second request must be refused while the first holds the lease, got %v", err)
	assert.Contains(t, conflict.Message, "already in progress")

	close(b.release)
	require.NoError(t, <-firstErr, "the holding request must still complete normally")

	// Four lists: three inclusion groups plus the master. A second set would double this.
	assert.Len(t, b.names(), 4,
		"the refused request must not have created any HubSpot list; created %v", b.names())
}

// TestBuildAudience_SecondRequestIsRejectedWhileTheFirstReadsTheBrief is the companion to the
// warehouse-window test above, and it exists because that one cannot fail for this reason. The
// lease covers only the interval its row is `building`, so EVERY blocking call ahead of the
// claim is a window — and once the warehouse read moved behind the claim, the brief read
// became the first one. It is a local database round-trip rather than a Snowflake one, which
// makes the window smaller and not narrower: a request delayed there past the first request's
// entire build still claims cleanly against a now-`built` row and creates a second full set of
// HubSpot lists.
//
// So the claim goes before the brief read too, and this pins it. Request A is held INSIDE its
// first GetBrief (one-shot, so B runs freely); if A has not already claimed by then, B sails
// through and the duplicate lists show up in the assertion below.
func TestBuildAudience_SecondRequestIsRejectedWhileTheFirstReadsTheBrief(t *testing.T) {
	b := &fakeBuilder{editions: []string{"KubeCon Korea 2025"}}
	s, _, brepo := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea","location":"Seoul"}`)

	entered := make(chan struct{})
	release := make(chan struct{})
	brepo.onGet = func() {
		close(entered)
		<-release
	}

	var (
		errA  error
		doneA = make(chan struct{})
	)
	go func() {
		defer close(doneA)
		_, errA = s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
			ProjectID: "cncf", BriefID: "brief-1",
		})
	}()
	<-entered

	_, errB := s.BuildAudience(context.Background(), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	require.Error(t, errB, "the second request must be refused while the first holds the lease")
	var conflict *audiences.ConflictError
	require.ErrorAs(t, errB, &conflict, "a held lease is a 409")
	assert.Contains(t, conflict.Message, "already in progress",
		"and it must say WAIT, not that the resource already exists")

	close(release)
	<-doneA
	require.NoError(t, errA, "the request that holds the lease must still finish")

	assert.Len(t, b.names(), 4,
		"exactly one build's worth of HubSpot lists — a second set is portal garbage nobody "+
			"knows to delete")
}
