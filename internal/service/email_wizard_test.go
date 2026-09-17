// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// -----------------------------------------------------------------------------
// Fakes
// -----------------------------------------------------------------------------

// fakeWizardSessionRepo is an in-memory WizardSessionRepository.
//
// It models the two properties the real repository's contract turns on and the handlers
// depend on: reads are scoped to (project, brief), and an update at the wrong version is
// ErrStaleWizardSession rather than ErrNotFound. A fake that conflated those two would make
// the 409-vs-404 mapping untestable, which is the distinction the sentinel exists for.
type fakeWizardSessionRepo struct {
	mu       sync.Mutex
	items    map[string]*model.WizardSession
	seq      int
	createE  error
	getE     error
	updateE  error
	getCalls int
}

func newFakeWizardSessionRepo() *fakeWizardSessionRepo {
	return &fakeWizardSessionRepo{items: map[string]*model.WizardSession{}}
}

func (r *fakeWizardSessionRepo) CreateSession(_ context.Context, s *model.WizardSession) (*model.WizardSession, error) {
	if r.createE != nil {
		return nil, r.createE
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	cp := *s
	cp.ID = fmt.Sprintf("sess-%d", r.seq)
	cp.Version = 1
	stored := cp
	r.items[cp.ID] = &stored
	return &cp, nil
}

func (r *fakeWizardSessionRepo) GetSession(_ context.Context, projectID, briefID, id string) (*model.WizardSession, error) {
	if r.getE != nil {
		return nil, r.getE
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getCalls++
	s, ok := r.items[id]
	// Tenancy is proven in the lookup, exactly as the SQL proves it in the WHERE clause: a
	// fake that matched on the id alone would let a cross-project read pass here and fail
	// only in production.
	if !ok || s.ProjectID != projectID || s.BriefID != briefID {
		return nil, domain.ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (r *fakeWizardSessionRepo) GetSessionByToken(_ context.Context, token string) (*model.WizardSession, error) {
	if r.getE != nil {
		return nil, r.getE
	}
	if strings.TrimSpace(token) == "" {
		// The port requires this: an empty token must not match rows whose token is NULL.
		return nil, domain.ErrNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.items {
		if s.ProgressToken == token {
			cp := *s
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (r *fakeWizardSessionRepo) UpdateSession(_ context.Context, s *model.WizardSession, expectedVersion int64) (*model.WizardSession, error) {
	if r.updateE != nil {
		return nil, r.updateE
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.items[s.ID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if cur.Version != expectedVersion {
		return nil, domain.ErrStaleWizardSession
	}
	cp := *s
	cp.Version = cur.Version + 1
	// Identity and provenance are NOT mutable, per the port's contract.
	cp.ProjectID, cp.BriefID, cp.CreatedBy = cur.ProjectID, cur.BriefID, cur.CreatedBy
	stored := cp
	r.items[s.ID] = &stored
	return &cp, nil
}

// fakeWizardHubSpot is a HubSpotWizardClient recording what the wizard asked HubSpot to do.
type fakeWizardHubSpot struct {
	portal     string
	portalErr  error
	searchHits []hubspot.Email
	searchErr  error
	cloneErr   error
	sendErr    error
	getErr     error

	blocks []hubspot.EmailHTMLBlock

	clonedFrom   string
	clonedName   string
	wroteWidgets map[string]string
	wroteSubject string
	sendListID   string
	suppression  []string
}

func newFakeWizardHubSpot() *fakeWizardHubSpot {
	return &fakeWizardHubSpot{
		portal: "portal-1",
		blocks: []hubspot.EmailHTMLBlock{{Key: "body", HTML: "<p>old</p>", Placed: true}},
	}
}

func (f *fakeWizardHubSpot) SearchEmails(context.Context, string) ([]hubspot.Email, error) {
	return f.searchHits, f.searchErr
}

func (f *fakeWizardHubSpot) GetEmail(_ context.Context, id string) (*hubspot.Email, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &hubspot.Email{ID: id, Name: f.clonedName, Subject: f.wroteSubject, State: "DRAFT"}, nil
}

func (f *fakeWizardHubSpot) CloneEmail(_ context.Context, sourceID, cloneName string) (*hubspot.Email, error) {
	if f.cloneErr != nil {
		return nil, f.cloneErr
	}
	f.clonedFrom, f.clonedName = sourceID, cloneName
	return &hubspot.Email{ID: "email-99", Name: cloneName, State: "DRAFT", AppURL: "https://app.hubspot.com/email/1/edit/email-99"}, nil
}

func (f *fakeWizardHubSpot) PatchEmailSettings(_ context.Context, id string, settings hubspot.EmailSettings) (*hubspot.Email, error) {
	if settings.Subject != nil {
		f.wroteSubject = *settings.Subject
	}
	return &hubspot.Email{ID: id, Subject: f.wroteSubject}, nil
}

func (f *fakeWizardHubSpot) SetSendList(_ context.Context, id, ilsListID string, suppression []string) (*hubspot.Email, error) {
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.sendListID, f.suppression = ilsListID, suppression
	return &hubspot.Email{ID: id}, nil
}

func (f *fakeWizardHubSpot) GetEmailHTMLWidgets(context.Context, string) ([]hubspot.EmailHTMLBlock, error) {
	return f.blocks, nil
}

func (f *fakeWizardHubSpot) SetEmailHTMLWidgets(_ context.Context, id string, widgets map[string]string) (*hubspot.Email, error) {
	f.wroteWidgets = widgets
	return &hubspot.Email{ID: id}, nil
}

func (f *fakeWizardHubSpot) AuthenticatedPortalID(context.Context) (string, error) {
	return f.portal, f.portalErr
}

// fakeWizardResolver hands out one client, or refuses.
type fakeWizardResolver struct {
	client HubSpotWizardClient
	err    error
}

func (r fakeWizardResolver) ResolveHubSpotClient(context.Context, string) (HubSpotWizardClient, error) {
	return r.client, r.err
}

// orderedAudienceRepo returns audiences in a FIXED newest-first order.
//
// A map-ordered fake cannot test this: SetWizardSendList's contract is that the NEWEST
// HubSpot audience decides and an older one is never substituted, so the ordering has to be
// a property of the fake rather than of Go's map iteration.
type orderedAudienceRepo struct {
	newestFirst []*model.CampaignAudience
	listErr     error
}

func (r *orderedAudienceRepo) ListAudiences(context.Context, string, string) ([]*model.CampaignAudience, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.newestFirst, nil
}

func (r *orderedAudienceRepo) CreateAudience(context.Context, *model.CampaignAudience) (*model.CampaignAudience, error) {
	return nil, errors.New("not used")
}

func (r *orderedAudienceRepo) CreateAudienceForApprovedBrief(context.Context, *model.CampaignAudience) (*model.CampaignAudience, int64, error) {
	return nil, 0, errors.New("not used")
}

func (r *orderedAudienceRepo) GetAudience(context.Context, string, string, string) (*model.CampaignAudience, error) {
	return nil, domain.ErrNotFound
}

func (r *orderedAudienceRepo) UpdateAudience(context.Context, *model.CampaignAudience, int64) (*model.CampaignAudience, error) {
	return nil, errors.New("not used")
}

func (r *orderedAudienceRepo) ReleaseAudienceBuildLease(context.Context, string, string, string) error {
	return nil
}

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

const (
	wizardTestProject = "proj-w"
	wizardTestBrief   = "brief-w"
)

type wizardHarness struct {
	svc       *BriefService
	briefs    *fakeBriefRepo
	sessions  *fakeWizardSessionRepo
	hubspot   *fakeWizardHubSpot
	audiences *orderedAudienceRepo
}

// newWizardHarness wires a BriefService with every wizard collaborator faked.
func newWizardHarness(t *testing.T, llmBody func() string) *wizardHarness {
	t.Helper()
	repo := newFakeBriefRepo()
	repo.briefs[briefKey(wizardTestProject, wizardTestBrief)] = &model.CampaignBrief{
		ID:        wizardTestBrief,
		ProjectID: wizardTestProject,
		Stage:     "registration-open",
		URL:       "https://events.example.org/kubecon",
		EventDetails: json.RawMessage(
			`{"eventName":"KubeCon EU 2026","location":"Barcelona","startDate":"June 17","endDate":"June 20",` +
				`"speakers":["Ada Lovelace"],"sponsors":[{"name":"Acme","logoUrl":"https://cdn.example.org/acme.png"}]}`),
	}
	h := &wizardHarness{
		svc:       newTestBriefService(repo),
		briefs:    repo,
		sessions:  newFakeWizardSessionRepo(),
		hubspot:   newFakeWizardHubSpot(),
		audiences: &orderedAudienceRepo{},
	}
	h.svc.SetWizardBackend(h.sessions, fakeWizardResolver{client: h.hubspot}, h.audiences)
	if llmBody != nil {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			content, err := json.Marshal(llmBody())
			if err != nil {
				// t.Error, not Fatal: this runs on the server's goroutine.
				t.Error("marshal fake model content:", err)
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(content) + `},"finish_reason":"stop"}]}`))
		}))
		t.Cleanup(srv.Close)
		h.svc.SetLLMClient(newTestLLMClient(t, srv))
	}
	return h
}

// wizardModelJSON is a well-formed model response: a subject, preview text and two blocks.
func wizardModelJSON() string {
	return `{"subject":"Registration is open for KubeCon EU 2026",` +
		`"preview_text":"Barcelona, June 17-20",` +
		`"sections":[{"type":"rich_text","html":"<p>Join us in Barcelona.</p>"},` +
		`{"type":"button","text":"Register now","url":"https://events.example.org/kubecon"}]}`
}

func (h *wizardHarness) start(t *testing.T) *briefs.WizardPlanStart {
	t.Helper()
	out, err := h.svc.StartEmailWizardPlan(context.Background(), &briefs.StartEmailWizardPlanPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief,
		ExtraContext: strPtr("Write for sponsors, not attendees"),
	})
	if err != nil {
		t.Fatalf("StartEmailWizardPlan: %v", err)
	}
	return out
}

// -----------------------------------------------------------------------------
// End to end
// -----------------------------------------------------------------------------

// TestWizard_EndToEnd walks the whole wizard the way the UI does — plan-start, plan,
// generate, edit, clone, set-send-list, read back — against fakes, because the properties
// worth pinning are the ones that only appear ACROSS turns: that the session carries state
// from one request to the next, and that the draft ends up holding the operator's edits
// rather than the model's first draft.
func TestWizard_EndToEnd(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	h.hubspot.searchHits = []hubspot.Email{{ID: "src-1", Name: "KubeCon EU 2025 - Registration Open"}}
	h.audiences.newestFirst = []*model.CampaignAudience{{
		ID: "aud-1", ProjectID: wizardTestProject, BriefID: wizardTestBrief,
		Platform: model.ProviderHubSpot, Status: model.AudienceBuilt,
		PlatformMasterListID: "ils-77", BuiltInPortalID: "portal-1",
		SuppressionListIDs: json.RawMessage(`["sup-1"]`),
	}}

	started := h.start(t)
	if started.SessionID == "" || started.Token == "" {
		t.Fatalf("plan-start must return both a session id and a progress token, got %+v", started)
	}

	plan, err := h.svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	if err != nil {
		t.Fatalf("PlanEmailWizard: %v", err)
	}
	if plan.Mode != "reference" || plan.SourceEmail == nil || plan.SourceEmail.ID != "src-1" {
		t.Errorf("planning must pick the past email as the clone source, got mode=%q source=%+v", plan.Mode, plan.SourceEmail)
	}
	if plan.Phase != string(model.WizardPhasePlanning) {
		t.Errorf("phase after planning = %q, want planning", plan.Phase)
	}

	content, err := h.svc.GenerateWizardContent(context.Background(), &briefs.GenerateWizardContentPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	if err != nil {
		t.Fatalf("GenerateWizardContent: %v", err)
	}
	if content.Subject == "" || !strings.Contains(content.BodyHTML, "Join us in Barcelona") {
		t.Errorf("generated content is missing the model's copy: subject=%q body=%q", content.Subject, content.BodyHTML)
	}
	if content.VariantAMode != "ai-generated" {
		t.Errorf("both variants should generate when the model answers, got variant_a_mode=%q", content.VariantAMode)
	}
	if len(content.Sponsors) != 1 || content.Sponsors[0].Name != "Acme" {
		t.Errorf("the brief's scraped sponsors must be returned, got %+v", content.Sponsors)
	}

	// The operator edits the blocks. The renderer, not the model, produces this HTML.
	edited := []any{
		map[string]any{
			"type": "rich_text",
			"html": `<p>Edited by a human. <a href="https://events.example.org/register">Register</a></p>`,
		},
	}
	sections, err := h.svc.UpdateWizardSections(context.Background(), &briefs.UpdateWizardSectionsPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID, Sections: edited,
	})
	if err != nil {
		t.Fatalf("UpdateWizardSections: %v", err)
	}
	if !strings.Contains(sections.BodyHTML, "Edited by a human") {
		t.Errorf("update-sections must render the submitted blocks, got %q", sections.BodyHTML)
	}

	clone, err := h.svc.CloneWizardEmail(context.Background(), &briefs.CloneWizardEmailPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID, Approved: true,
	})
	if err != nil {
		t.Fatalf("CloneWizardEmail: %v", err)
	}
	if h.hubspot.clonedFrom != "src-1" {
		t.Errorf("the draft must be cloned from the planned source, got %q", h.hubspot.clonedFrom)
	}
	if clone.EmailID == nil || *clone.EmailID != "email-99" {
		t.Errorf("clone must report the new draft's id, got %+v", clone.EmailID)
	}
	// The EDIT, not the generated body: this is the assertion that update-sections is not
	// silently discarded by the clone.
	var wroteBody string
	for _, v := range h.hubspot.wroteWidgets {
		wroteBody += v
	}
	if !strings.Contains(wroteBody, "Edited by a human") {
		t.Errorf("the draft must carry the operator's edited body, got %q", wroteBody)
	}
	if !strings.Contains(wroteBody, "utm_source=") {
		t.Errorf("the draft's links must be UTM-tagged, got %q", wroteBody)
	}

	sendList, err := h.svc.SetWizardSendList(context.Background(), &briefs.SetWizardSendListPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	if err != nil {
		t.Fatalf("SetWizardSendList: %v", err)
	}
	if sendList.SendListID != "ils-77" {
		t.Errorf("with no explicit list the brief's built audience must decide, got %q", sendList.SendListID)
	}
	if got := derefStr(sendList.ListType); got != "audience" {
		t.Errorf("list_type = %q, want audience", got)
	}
	if len(h.hubspot.suppression) != 1 || h.hubspot.suppression[0] != "sup-1" {
		t.Errorf("the audience's suppression lists must be applied, got %v", h.hubspot.suppression)
	}

	session, err := h.svc.GetWizardSession(context.Background(), &briefs.GetWizardSessionPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	if err != nil {
		t.Fatalf("GetWizardSession: %v", err)
	}
	if session.Phase != string(model.WizardPhaseComplete) {
		t.Errorf("phase after the send list is set = %q, want complete", session.Phase)
	}
	if session.EmailID == nil || session.DraftURL == nil {
		t.Errorf("a completed session must report its draft, got email=%v url=%v", session.EmailID, session.DraftURL)
	}
}

// TestWizard_PlanCarriesOperatorGuidanceIntoGeneration pins the cross-turn path for the
// guidance fields, which exist only on plan-start and plan while the model call happens in a
// LATER request. Before the session carried them, "write for sponsors" was accepted by the
// API and silently dropped before it reached any prompt.
func TestWizard_PlanCarriesOperatorGuidanceIntoGeneration(t *testing.T) {
	var gotPrompt string
	h := newWizardHarness(t, wizardModelJSON)
	// Re-wire the model with a server that records what it was asked.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			gotPrompt += m.Content
		}
		content, _ := json.Marshal(wizardModelJSON())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(content) + `},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	h.svc.SetLLMClient(newTestLLMClient(t, srv))

	started := h.start(t)
	if _, err := h.svc.GenerateWizardContent(context.Background(), &briefs.GenerateWizardContentPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	}); err != nil {
		t.Fatalf("GenerateWizardContent: %v", err)
	}
	if !strings.Contains(gotPrompt, "Write for sponsors, not attendees") {
		t.Error("the guidance given at plan-start must reach the generation prompt; it was dropped")
	}
}

// TestWizard_StageVariantFailureStillReturnsThePrimary pins the asymmetry: the second
// variant is an ALTERNATIVE, so losing it must not throw away a primary that already cost a
// model call.
func TestWizard_StageVariantFailureStillReturnsThePrimary(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		content, _ := json.Marshal(wizardModelJSON())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(content) + `},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	h := newWizardHarness(t, nil)
	h.svc.SetLLMClient(newTestLLMClient(t, srv))
	started := h.start(t)

	content, err := h.svc.GenerateWizardContent(context.Background(), &briefs.GenerateWizardContentPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	if err != nil {
		t.Fatalf("a failed second variant must not fail the request: %v", err)
	}
	if content.Subject == "" {
		t.Error("the primary variant must still be returned")
	}
	if content.VariantAMode != "failed" {
		t.Errorf("variant_a_mode = %q, want failed so the UI can say which option is missing", content.VariantAMode)
	}
	if strings.Contains(content.VariantASubject, "500") {
		t.Error("the upstream error must not be echoed into the variant's fields")
	}
}

// TestWizard_PrimaryVariantFailureIs503 is the other half: the reference variant IS the
// endpoint's output, and an unusable model response is this service's dependency failing,
// not a bad request.
func TestWizard_PrimaryVariantFailureIs503(t *testing.T) {
	h := newWizardHarness(t, func() string { return "this is not json" })
	started := h.start(t)

	_, err := h.svc.GenerateWizardContent(context.Background(), &briefs.GenerateWizardContentPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	var unavail *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("unusable model output must be 503, got %T: %v", err, err)
	}
}

// TestWizard_CloneRequiresApproval pins the one gate in front of the first turn with an
// effect outside this service.
func TestWizard_CloneRequiresApproval(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	started := h.start(t)
	_, err := h.svc.CloneWizardEmail(context.Background(), &briefs.CloneWizardEmailPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID, Approved: false,
	})
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("clone without approval must be 400, got %T: %v", err, err)
	}
	if h.hubspot.clonedFrom != "" {
		t.Error("an unapproved clone must not reach HubSpot at all")
	}
}

// TestWizard_CloneWithoutContentIsConflict — the request is well-formed; it is the SESSION
// that is not ready, so the caller is told to generate first rather than to fix a field.
func TestWizard_CloneWithoutContentIsConflict(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	started := h.start(t)
	_, err := h.svc.CloneWizardEmail(context.Background(), &briefs.CloneWizardEmailPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID, Approved: true,
	})
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("cloning a session with no generated content must be 409, got %T: %v", err, err)
	}
}

// TestWizard_SendListRefusals covers every way the recipients cannot be resolved. Each arm
// must REFUSE rather than substitute: sending to the wrong list is unrecoverable once a human
// presses send in HubSpot.
func TestWizard_SendListRefusals(t *testing.T) {
	built := func(mut func(*model.CampaignAudience)) []*model.CampaignAudience {
		a := &model.CampaignAudience{
			ID: "aud-1", ProjectID: wizardTestProject, BriefID: wizardTestBrief,
			Platform: model.ProviderHubSpot, Status: model.AudienceBuilt,
			PlatformMasterListID: "ils-77", BuiltInPortalID: "portal-1",
		}
		if mut != nil {
			mut(a)
		}
		return []*model.CampaignAudience{a}
	}

	tests := []struct {
		name      string
		audiences []*model.CampaignAudience
		payload   func(*briefs.SetWizardSendListPayload)
		portal    string
		wantKind  string
	}{
		{
			name:      "two explicit lists",
			audiences: built(nil),
			payload: func(p *briefs.SetWizardSendListPayload) {
				p.SendListIds = []string{"ils-1", "ils-2"}
			},
			wantKind: "400",
		},
		{
			name:      "no audience at all",
			audiences: nil,
			wantKind:  "400",
		},
		{
			name:      "newest audience is not built",
			audiences: built(func(a *model.CampaignAudience) { a.Status = model.AudienceBuilding }),
			wantKind:  "409",
		},
		{
			name:      "built audience has no list id",
			audiences: built(func(a *model.CampaignAudience) { a.PlatformMasterListID = "" }),
			wantKind:  "409",
		},
		{
			name:      "audience does not record its portal",
			audiences: built(func(a *model.CampaignAudience) { a.BuiltInPortalID = "" }),
			wantKind:  "409",
		},
		{
			name:      "audience belongs to another portal",
			audiences: built(func(a *model.CampaignAudience) { a.BuiltInPortalID = "portal-other" }),
			wantKind:  "409",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newWizardHarness(t, wizardModelJSON)
			h.audiences.newestFirst = tc.audiences
			if tc.portal != "" {
				h.hubspot.portal = tc.portal
			}
			started := h.start(t)
			// A draft exists, so the refusal under test is about the recipients only.
			h.sessions.items[started.SessionID].EmailID = "email-99"

			p := &briefs.SetWizardSendListPayload{
				ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
			}
			if tc.payload != nil {
				tc.payload(p)
			}
			_, err := h.svc.SetWizardSendList(context.Background(), p)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			switch tc.wantKind {
			case "400":
				var bad *briefs.BadRequestError
				if !errors.As(err, &bad) {
					t.Fatalf("want 400, got %T: %v", err, err)
				}
			case "409":
				var conflict *briefs.ConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("want 409, got %T: %v", err, err)
				}
			}
			if h.hubspot.sendListID != "" {
				t.Errorf("a refused resolution must not touch the draft, but the send list was set to %q", h.hubspot.sendListID)
			}
		})
	}
}

// TestWizard_PortalIdentityFailureFailsClosed — an unreadable portal identity is an unknown,
// not a match, so the lists must not be applied on the guess.
func TestWizard_PortalIdentityFailureFailsClosed(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	h.hubspot.portalErr = errors.New("portal lookup failed")
	h.audiences.newestFirst = []*model.CampaignAudience{{
		ID: "aud-1", Platform: model.ProviderHubSpot, Status: model.AudienceBuilt,
		PlatformMasterListID: "ils-77", BuiltInPortalID: "portal-1",
	}}
	started := h.start(t)
	h.sessions.items[started.SessionID].EmailID = "email-99"

	_, err := h.svc.SetWizardSendList(context.Background(), &briefs.SetWizardSendListPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	var unavail *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("an unprovable portal must be 503, got %T: %v", err, err)
	}
	if h.hubspot.sendListID != "" {
		t.Error("the send list must not be applied when the portal cannot be proven")
	}
}

// TestWizard_ExplicitSendListSkipsTheAudience — an operator naming a list is making a
// choice, and it must not be second-guessed against the brief's audience.
func TestWizard_ExplicitSendListSkipsTheAudience(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	h.audiences.listErr = errors.New("the audience repository must not be consulted")
	started := h.start(t)
	h.sessions.items[started.SessionID].EmailID = "email-99"

	out, err := h.svc.SetWizardSendList(context.Background(), &briefs.SetWizardSendListPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
		SendListID: strPtr("ils-explicit"),
	})
	if err != nil {
		t.Fatalf("SetWizardSendList: %v", err)
	}
	if out.SendListID != "ils-explicit" || derefStr(out.ListType) != "explicit" {
		t.Errorf("an explicit list must be passed through, got id=%q type=%q", out.SendListID, derefStr(out.ListType))
	}
}

// TestWizard_StaleSessionIsConflict pins the sentinel's mapping. A caller told "not found"
// for what is really a stale write would start a second session for the same run — and on
// the clone turn, that means a second HubSpot draft.
func TestWizard_StaleSessionIsConflict(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	started := h.start(t)
	h.sessions.updateE = domain.ErrStaleWizardSession

	_, err := h.svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("a stale session must be 409, got %T: %v", err, err)
	}
}

// TestWizard_SessionIsTenantScoped — a session carries unannounced marketing copy, and the
// id being a UUID is not an authorization model.
func TestWizard_SessionIsTenantScoped(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	started := h.start(t)

	_, err := h.svc.GetWizardSession(context.Background(), &briefs.GetWizardSessionPayload{
		ProjectID: "some-other-project", BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	var notFound *briefs.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("a cross-project read must be 404, got %T: %v", err, err)
	}
}

// TestWizard_PlanStartRequiresAnExistingBrief — the brief is read first so a bad pair is a
// 404 rather than a foreign-key error surfacing as a 500.
func TestWizard_PlanStartRequiresAnExistingBrief(t *testing.T) {
	h := newWizardHarness(t, nil)
	_, err := h.svc.StartEmailWizardPlan(context.Background(), &briefs.StartEmailWizardPlanPayload{
		ProjectID: wizardTestProject, BriefID: "no-such-brief",
	})
	var notFound *briefs.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("want 404 for a missing brief, got %T: %v", err, err)
	}
}

// TestWizard_UpdateSectionsNeedsNoModel is the property that keeps the editing half of the
// wizard alive when the AI proxy is unconfigured — and the reason re-rendering is
// deterministic rather than a second generation pass.
func TestWizard_UpdateSectionsNeedsNoModel(t *testing.T) {
	h := newWizardHarness(t, nil) // no LLM client at all
	started := h.start(t)

	out, err := h.svc.UpdateWizardSections(context.Background(), &briefs.UpdateWizardSectionsPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
		Sections: []any{map[string]any{"type": "rich_text", "html": "<p>Hand written.</p>"}},
	})
	if err != nil {
		t.Fatalf("update-sections must work with no model configured: %v", err)
	}
	if !strings.Contains(out.BodyHTML, "Hand written") {
		t.Errorf("body = %q, want the submitted block", out.BodyHTML)
	}
}

// TestWizard_UpdateSectionsRefusesEmptyAndOversized — an empty list renders an empty email
// and is far more likely a client bug than an intended blank send.
func TestWizard_UpdateSectionsRefusesEmptyAndOversized(t *testing.T) {
	h := newWizardHarness(t, nil)
	started := h.start(t)

	for _, tc := range []struct {
		name     string
		sections []any
	}{
		{name: "empty", sections: nil},
		{name: "over the block limit", sections: make([]any, maxWizardSections+1)},
		{name: "nothing usable", sections: []any{map[string]any{"type": "not-a-real-block"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.svc.UpdateWizardSections(context.Background(), &briefs.UpdateWizardSectionsPayload{
				ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
				Sections: tc.sections,
			})
			var bad *briefs.BadRequestError
			if !errors.As(err, &bad) {
				t.Fatalf("want 400, got %T: %v", err, err)
			}
		})
	}
}

// TestWizard_ChatAnswersAndPersists — the turn must survive a pod change between two
// messages, which is the whole reason the transcript is a column rather than memory.
func TestWizard_ChatAnswersAndPersists(t *testing.T) {
	h := newWizardHarness(t, func() string { return "Use the edit step to change the subject line." })
	started := h.start(t)

	out, err := h.svc.ChatWizardTurn(context.Background(), &briefs.ChatWizardTurnPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
		Message: "Can you make the subject shorter?",
	})
	if err != nil {
		t.Fatalf("ChatWizardTurn: %v", err)
	}
	if out.Message == "" {
		t.Error("the chat turn must return the model's reply")
	}
	turns, terr := h.sessions.items[started.SessionID].Turns()
	if terr != nil {
		t.Fatalf("stored history must decode: %v", terr)
	}
	if len(turns) != 2 || turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Errorf("both sides of the turn must be persisted, got %+v", turns)
	}
}

// TestWizard_ChatRefusesAnEmptyMessage keeps a model call off the wire for a request that
// cannot produce an answer.
func TestWizard_ChatRefusesAnEmptyMessage(t *testing.T) {
	h := newWizardHarness(t, func() string {
		t.Error("the model must not be called for an empty message")
		return ""
	})
	started := h.start(t)
	_, err := h.svc.ChatWizardTurn(context.Background(), &briefs.ChatWizardTurnPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID, Message: "   ",
	})
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("want 400, got %T: %v", err, err)
	}
}

// -----------------------------------------------------------------------------
// Degraded wiring
// -----------------------------------------------------------------------------

// TestWizard_UnwiredDegradesNotPanics is the test that makes the wizard genuinely optional.
//
// A BriefService with no wizard backend is the no-database mode and the cold-start window,
// and both are REACHABLE in production. Every one of the eight routes must answer the typed
// 503 there; a nil-dereference would take down the process instead.
func TestWizard_UnwiredDegradesNotPanics(t *testing.T) {
	svc := newTestBriefService(newFakeBriefRepo())

	calls := map[string]func() error{
		"plan-start": func() error {
			_, err := svc.StartEmailWizardPlan(context.Background(), &briefs.StartEmailWizardPlanPayload{ProjectID: "p", BriefID: "b"})
			return err
		},
		"plan": func() error {
			_, err := svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{ProjectID: "p", BriefID: "b", SessionID: "s"})
			return err
		},
		"generate-content": func() error {
			_, err := svc.GenerateWizardContent(context.Background(), &briefs.GenerateWizardContentPayload{ProjectID: "p", BriefID: "b", SessionID: "s"})
			return err
		},
		"update-sections": func() error {
			_, err := svc.UpdateWizardSections(context.Background(), &briefs.UpdateWizardSectionsPayload{ProjectID: "p", BriefID: "b", SessionID: "s"})
			return err
		},
		"clone": func() error {
			_, err := svc.CloneWizardEmail(context.Background(), &briefs.CloneWizardEmailPayload{ProjectID: "p", BriefID: "b", SessionID: "s", Approved: true})
			return err
		},
		"set-send-list": func() error {
			_, err := svc.SetWizardSendList(context.Background(), &briefs.SetWizardSendListPayload{ProjectID: "p", BriefID: "b", SessionID: "s"})
			return err
		},
		"chat": func() error {
			_, err := svc.ChatWizardTurn(context.Background(), &briefs.ChatWizardTurnPayload{ProjectID: "p", BriefID: "b", SessionID: "s", Message: "hi"})
			return err
		},
		"session": func() error {
			_, err := svc.GetWizardSession(context.Background(), &briefs.GetWizardSessionPayload{ProjectID: "p", BriefID: "b", SessionID: "s"})
			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			var unavail *briefs.ConnServiceUnavailableError
			if !errors.As(err, &unavail) {
				t.Fatalf("an unwired wizard must answer the typed 503, got %T: %v", err, err)
			}
		})
	}
}

// TestWizard_NoHubSpotConnectionDegradesTheRightWay — HubSpot is needed to CLONE, not to
// plan. Planning without it must still produce a plan (from the stage guide), while cloning
// must refuse.
func TestWizard_NoHubSpotConnectionDegradesTheRightWay(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	h.svc.SetWizardBackend(h.sessions, fakeWizardResolver{err: errors.New("no connection")}, h.audiences)
	started := h.start(t)

	plan, err := h.svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	if err != nil {
		t.Fatalf("planning must survive an unresolvable HubSpot connection: %v", err)
	}
	if plan.Mode != "stage" || plan.SourceEmail != nil {
		t.Errorf("without HubSpot the plan must fall back to the stage guide, got mode=%q source=%+v", plan.Mode, plan.SourceEmail)
	}

	if _, err := h.svc.CloneWizardEmail(context.Background(), &briefs.CloneWizardEmailPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID, Approved: true,
	}); err == nil {
		t.Error("cloning without a HubSpot connection must refuse")
	}
}

// TestWizard_ContentGenerationWithoutAModelIs503 — the endpoint cannot do its job, and the
// answer must be the typed 503 rather than a panic on a nil client.
func TestWizard_ContentGenerationWithoutAModelIs503(t *testing.T) {
	h := newWizardHarness(t, nil)
	started := h.start(t)
	_, err := h.svc.GenerateWizardContent(context.Background(), &briefs.GenerateWizardContentPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	var unavail *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("want 503 with no model configured, got %T: %v", err, err)
	}
}

// -----------------------------------------------------------------------------
// Event facts
// -----------------------------------------------------------------------------

// TestDecodeWizardEventDetails covers the decode the wizard uses instead of the copy
// endpoint's, including the bound on the two list fields — event_details is declared Any in
// the design, so a caller can write it directly.
func TestDecodeWizardEventDetails(t *testing.T) {
	t.Run("absent blob is not an error", func(t *testing.T) {
		if got := decodeWizardEventDetails(nil); got.EventName != "" {
			t.Errorf("want the zero value, got %+v", got)
		}
	})
	t.Run("unparseable blob yields the zero value", func(t *testing.T) {
		if got := decodeWizardEventDetails(json.RawMessage(`{`)); got.EventName != "" {
			t.Errorf("want the zero value, got %+v", got)
		}
	})
	t.Run("speakers and sponsors are bounded", func(t *testing.T) {
		var sb strings.Builder
		sb.WriteString(`{"speakers":[`)
		for i := 0; i < maxWizardFactListEntries+20; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, `"speaker %d"`, i)
		}
		sb.WriteString(`]}`)
		got := decodeWizardEventDetails(json.RawMessage(sb.String()))
		if len(got.Speakers) != maxWizardFactListEntries {
			t.Errorf("speakers = %d, want the bound %d", len(got.Speakers), maxWizardFactListEntries)
		}
	})
}

// TestWizardDates prefers the scraper's combined string and never invents a placeholder — a
// literal "Date TBD" in the prompt is a fact a model will print into a marketing email.
func TestWizardDates(t *testing.T) {
	tests := []struct {
		name string
		in   wizardEventDetails
		want string
	}{
		{name: "combined wins", in: wizardEventDetails{Dates: "June 17-20", StartDate: "June 17"}, want: "June 17-20"},
		{name: "range", in: wizardEventDetails{StartDate: "June 17", EndDate: "June 20"}, want: "June 17 - June 20"},
		{name: "single day", in: wizardEventDetails{StartDate: "June 17", EndDate: "June 17"}, want: "June 17"},
		{name: "unknown stays empty", in: wizardEventDetails{}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := wizardDates(tc.in); got != tc.want {
				t.Errorf("wizardDates = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWizardSponsorsOut drops a sponsor the design cannot represent rather than emitting one
// with an empty required field.
func TestWizardSponsorsOut(t *testing.T) {
	got := wizardSponsorsOut(wizardEventDetails{Sponsors: []wizardEventSponsor{
		{Name: "Acme", Logo: "https://cdn.example.org/a.png", URL: "https://acme.example.org", Tier: "gold"},
		{Name: "No Logo"},
		{Logo: "https://cdn.example.org/b.png"},
		{Name: "Bad Logo", Logo: "javascript:alert(1)"},
	}})
	if len(got) != 1 || got[0].Name != "Acme" {
		t.Fatalf("only the complete sponsor may survive, got %+v", got)
	}
	if got[0].Tier == nil || *got[0].Tier != "gold" {
		t.Errorf("tier must be carried through, got %v", got[0].Tier)
	}
}

// TestComposeWizardChatPrompt_CarriesTheCurrentDraft pins that a chat turn can see the email
// being built, not only the event behind it.
//
// The prompt previously carried event facts and the transcript and nothing else, so "make the
// subject shorter" drew "I don't see the current subject line in the event facts you've
// provided" — a refusal to do the one thing this step exists for, from a model that had the
// answer withheld from it. Verified against the live proxy before and after.
func TestComposeWizardChatPrompt_CarriesTheCurrentDraft(t *testing.T) {
	facts := wizardPromptFacts{eventName: "KubeCon EU"}
	draft := wizardChatDraft{Subject: "Join us at KubeCon + CloudNativeCon EU in London!", PreviewText: "Register before prices rise"}

	_, user := composeWizardChatPrompt(facts, draft, nil, "make the subject shorter")

	if !strings.Contains(user, draft.Subject) {
		t.Errorf("the chat prompt omits the current subject, so the model cannot act on it:\n%s", user)
	}
	if !strings.Contains(user, draft.PreviewText) {
		t.Errorf("the chat prompt omits the current preview text:\n%s", user)
	}
}

// An empty draft must not add the section at all: a chat turn before generation has no email to
// describe, and an empty "Current draft:" heading invites the model to invent one.
func TestComposeWizardChatPrompt_OmitsAnEmptyDraft(t *testing.T) {
	_, user := composeWizardChatPrompt(wizardPromptFacts{eventName: "KubeCon EU"}, wizardChatDraft{}, nil, "hello")

	if strings.Contains(user, "Current draft:") {
		t.Errorf("an empty draft still rendered its heading:\n%s", user)
	}
}

// TestSetWizardSendList_RefusesAnotherDraft pins the tenancy boundary on the one endpoint that
// takes a HubSpot id from the caller.
//
// `firstNonEmpty(p.EmailID, sess.EmailID)` let a supplied id WIN over the session's recorded
// draft with no ownership check. HubSpot credentials resolve through the shared LF portal, so a
// campaign manager authorised for THIS project could retarget the recipients of any draft in
// the portal — another foundation's included. Authorization here is scoped to the brief, so the
// draft that brief created is the only one this endpoint may touch.
func TestSetWizardSendList_RefusesAnotherDraft(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	h.hubspot.searchHits = []hubspot.Email{{ID: "src-1", Name: "KubeCon EU 2025 - Registration Open"}}
	h.audiences.newestFirst = []*model.CampaignAudience{{
		ID: "aud-1", ProjectID: wizardTestProject, BriefID: wizardTestBrief,
		Platform: model.ProviderHubSpot, Status: model.AudienceBuilt,
		PlatformMasterListID: "ils-77", BuiltInPortalID: "portal-1",
	}}
	started := h.start(t)
	if _, err := h.svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	}); err != nil {
		t.Fatalf("PlanEmailWizard: %v", err)
	}
	if _, err := h.svc.GenerateWizardContent(context.Background(), &briefs.GenerateWizardContentPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	}); err != nil {
		t.Fatalf("GenerateWizardContent: %v", err)
	}
	if _, err := h.svc.CloneWizardEmail(context.Background(), &briefs.CloneWizardEmailPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID, Approved: true,
	}); err != nil {
		t.Fatalf("CloneWizardEmail: %v", err)
	}

	h.hubspot.sendListID = ""
	foreign := "999999-not-this-session"
	_, err := h.svc.SetWizardSendList(context.Background(), &briefs.SetWizardSendListPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
		EmailID: &foreign,
	})
	if err == nil {
		t.Fatal("a caller-supplied email_id retargeted a draft this session does not own")
	}
	// The Goa error types render an empty Error(); assert on the typed value instead.
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("want a 400 naming the mismatch, got %T: %v", err, err)
	}
	if !strings.Contains(bad.Message, "does not match this session") {
		t.Errorf("the refusal must name the cause; got %q", bad.Message)
	}
	if h.hubspot.sendListID != "" {
		t.Fatalf("SetSendList ran despite the refusal, list=%q", h.hubspot.sendListID)
	}
}
