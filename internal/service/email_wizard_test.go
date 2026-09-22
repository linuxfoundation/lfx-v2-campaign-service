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
	"time"
	"unicode/utf8"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
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
	scrubE   error
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

func (r *fakeWizardSessionRepo) ScrubSessionsForBrief(_ context.Context, projectID, briefID string) (int64, error) {
	if r.scrubE != nil {
		return 0, r.scrubE
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, s := range r.items {
		// Scoped by BOTH ids for the same reason GetSession is: a fake that matched on the
		// brief alone would let a cross-project scrub pass here and wipe another tenant's
		// sessions only in production.
		if s.ProjectID != projectID || s.BriefID != briefID {
			continue
		}
		if len(s.ChatHistory) == 0 && len(s.PlanResult) == 0 && len(s.ReferenceVariant) == 0 &&
			len(s.StageVariant) == 0 && len(s.Sections) == 0 && s.CreatedBy == nil && s.UpdatedBy == nil {
			continue
		}
		// Every content column, matching the real statement. A fake that cleared fewer would
		// keep the service test green against a scrub that leaks four columns.
		s.ChatHistory = nil
		s.PlanResult = nil
		s.ReferenceVariant = nil
		s.StageVariant = nil
		s.Sections = nil
		s.CreatedBy = nil
		s.UpdatedBy = nil
		// The version bump is part of the statement, not bookkeeping: it is what makes an
		// in-flight turn's pre-scrub snapshot fail stale instead of repopulating the columns
		// this just cleared. A fake that skipped it would keep every service-level test green
		// against a scrub that can be silently undone.
		s.Version++
		n++
	}
	return n, nil
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

	clonedFrom        string
	clonedName        string
	wroteWidgets      map[string]string
	wroteSubject      string
	sendListID        string
	suppression       []string
	searchDeadline    time.Time
	searchHadDeadline bool
	// A COUNT, not a flag: a guard that skipped one read and made another would still
	// satisfy a boolean.
	getEmailCalls int
}

func newFakeWizardHubSpot() *fakeWizardHubSpot {
	return &fakeWizardHubSpot{
		portal: "portal-1",
		blocks: []hubspot.EmailHTMLBlock{{Key: "body", HTML: "<p>old</p>", Placed: true}},
	}
}

func (f *fakeWizardHubSpot) SearchEmails(ctx context.Context, _ string) ([]hubspot.Email, error) {
	// The DEADLINE the caller imposed, recorded so a test can assert findWizardSourceEmail still
	// bounds the whole paginated search rather than inheriting the request context.
	f.searchDeadline, f.searchHadDeadline = ctx.Deadline()
	return f.searchHits, f.searchErr
}

func (f *fakeWizardHubSpot) GetEmail(_ context.Context, id string) (*hubspot.Email, error) {
	f.getEmailCalls++
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
	// fromSystem simulates resolution falling back to the LF-wide connection, which the
	// clone-source search must refuse because SearchEmails is portal-wide.
	fromSystem bool
}

func (r fakeWizardResolver) ResolveHubSpotClient(context.Context, string) (HubSpotWizardClient, bool, error) {
	return r.client, r.fromSystem, r.err
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

// TestComposeWizardChatPrompt_StaysWithinTheComposedBudget pins that chat honours the same
// service-owned prompt budget the generate path enforces.
//
// Chat bypassed it entirely: maxWizardChatHistory is 12 and maxWizardChatMessage is 4000, so
// the retained history alone can reach 48,000 runes against a 24,000 budget — before facts,
// draft and the current message. generateWizardVariant REJECTS on overflow, which is right
// there because what overflows is a compiled-in template. Here the history is the caller's, so
// it degrades the way the history is already documented to: oldest turns dropped first.
func TestComposeWizardChatPrompt_StaysWithinTheComposedBudget(t *testing.T) {
	long := strings.Repeat("x", maxWizardChatMessage)
	history := make([]wizardTurnText, 0, maxWizardChatHistory)
	for i := 0; i < maxWizardChatHistory; i++ {
		history = append(history, wizardTurnText{Role: "user", Content: long})
	}

	system, user := composeWizardChatPrompt(wizardPromptFacts{eventName: "KubeCon EU"},
		wizardChatDraft{Subject: "Join us"}, history, "make it shorter")

	total := utf8.RuneCountInString(system) + utf8.RuneCountInString(user)
	if total > maxWizardComposedPromptSize {
		t.Errorf("composed chat prompt is %d runes, over the %d budget", total, maxWizardComposedPromptSize)
	}
	// The turn must still be answerable: the operator's own message survives the trim.
	if !strings.Contains(user, "make it shorter") {
		t.Error("the trim dropped the operator's current message, leaving nothing to answer")
	}
}

// TestGenerateWizardContent_DoesNotRegressAPhase pins that regeneration cannot walk a session
// backwards past a phase the model documents as irreversible.
//
// `cloned` is "the first phase with an effect OUTSIDE this service, so it is also the first one
// that cannot be undone". Setting the phase unconditionally meant a regenerate after a clone
// described a session whose HubSpot draft and recipients still exist as one that has no draft —
// and the freshly generated copy was not in that draft either, so the row contradicted both
// HubSpot and itself. Regeneration itself is still allowed; only the phase lie is refused.
func TestGenerateWizardContent_DoesNotRegressAPhase(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	h.hubspot.searchHits = []hubspot.Email{{ID: "src-1", Name: "KubeCon EU 2025 - Registration Open"}}
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

	// Regenerate AFTER the clone: allowed, but it must not claim the draft no longer exists.
	if _, err := h.svc.GenerateWizardContent(context.Background(), &briefs.GenerateWizardContentPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	}); err != nil {
		t.Fatalf("regenerating after a clone must still work: %v", err)
	}

	sess, err := h.svc.GetWizardSession(context.Background(), &briefs.GetWizardSessionPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	if err != nil {
		t.Fatalf("GetWizardSession: %v", err)
	}
	if sess.Phase == string(model.WizardPhaseContent) {
		t.Errorf("a cloned session regressed to %q; the draft still exists in HubSpot", sess.Phase)
	}
}

// TestChatWizardTurn_BoundsTheStoredReply pins that a model reply cannot grow the session row
// without limit.
//
// maxWizardStoredTurns bounds the COUNT of persisted turns at 60; nothing bounded their SIZE.
// The user's message was truncated to maxWizardChatMessage, the assistant's was stored verbatim,
// and the llm client accepts up to 8 MiB — so 60 turns is a row measured in hundreds of MB. The
// "under 200 words" line in the system prompt is an instruction to a model, not a guarantee.
//
// The reply RETURNED to the caller is deliberately not truncated: the operator reads the whole
// answer, only the stored copy is bounded.
func TestChatWizardTurn_BoundsTheStoredReply(t *testing.T) {
	huge := strings.Repeat("y", maxWizardChatMessage*3)
	h := newWizardHarness(t, func() string { return huge })
	started := h.start(t)

	out, err := h.svc.ChatWizardTurn(context.Background(), &briefs.ChatWizardTurnPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
		Message: "hello",
	})
	if err != nil {
		t.Fatalf("ChatWizardTurn: %v", err)
	}
	if len(out.Message) != len(huge) {
		t.Errorf("the caller must get the FULL reply; got %d runes of %d", len(out.Message), len(huge))
	}

	sess, serr := h.sessions.GetSession(context.Background(), wizardTestProject, wizardTestBrief, started.SessionID)
	if serr != nil {
		t.Fatalf("GetSession: %v", serr)
	}
	turns, terr := sess.Turns()
	if terr != nil {
		t.Fatalf("Turns: %v", terr)
	}
	for _, turn := range turns {
		if utf8.RuneCountInString(turn.Content) > maxWizardChatMessage {
			t.Errorf("a stored %s turn is %d runes, over the %d bound",
				turn.Role, utf8.RuneCountInString(turn.Content), maxWizardChatMessage)
		}
	}
}

// The progress token addresses an SSE stream that `WizardProgressHub.Publish` fans out to every
// subscriber of that token, and `progress_token` is deliberately NOT UNIQUE. While the caller
// could choose it, an attacker could pick one colliding with a victim's session in another
// project: `GetSessionByToken` resolves the NEWEST holder, so the handler's project check passed
// against the attacker's own row while the hub delivered the victim's frames.
//
// Minting it server-side removes the collision rather than trying to detect it. The payload no
// longer carries a `progress_token` field at all -- an earlier version of this test passed one
// and asserted it was ignored, which the design change makes unrepresentable. That is the
// stronger outcome: a caller cannot express the attack rather than having it declined.
//
// What remains testable, and is the property the whole fix rests on, is that two starts never
// collide.
func TestWizard_ProgressTokenIsServerMintedNotCallerSupplied(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)

	out, err := h.svc.StartEmailWizardPlan(context.Background(), &briefs.StartEmailWizardPlanPayload{
		ProjectID: wizardTestProject,
		BriefID:   wizardTestBrief,
	})
	if err != nil {
		t.Fatalf("StartEmailWizardPlan: %v", err)
	}
	if out.Token == "" {
		t.Fatal("a token must be returned, or the client has no stream to subscribe to")
	}

	second, err := h.svc.StartEmailWizardPlan(context.Background(), &briefs.StartEmailWizardPlanPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief,
	})
	if err != nil {
		t.Fatalf("second StartEmailWizardPlan: %v", err)
	}
	if second.Token == out.Token {
		t.Fatalf("two sessions were given the same progress token: %q", out.Token)
	}
}

// TestWizard_EveryTurnRefusesADeletedBrief pins the lifecycle guard in loadWizardSession.
//
// The check used to live in each handler, and four of the seven had it: plan, generate, clone
// and plan-start checked the brief, while send-list, update-sections and get-session did not.
// That is the failure mode a per-handler rule always reaches -- every new endpoint is another
// chance to forget, and the omission is invisible because the session still loads perfectly.
//
// It matters because ArchiveBrief is a SOFT delete: the session rows outlive it. Without the
// guard a caller holding a session id could go on driving the wizard for a brief the operator
// deleted, and SetWizardSendList would mutate the recipients of the HubSpot draft -- an effect
// outside this database entirely, on a brief that no longer exists.
//
// The seven public methods that call `loadWizardSession`: plan, generate-content, get-session,
// set-send-list, update-sections, clone and chat.
//
// The count is stated because it is CHECKABLE, and it has been wrong twice: first "every" while
// covering four, then "all six" while missing chat. Verify it rather than trust it --
//
//	awk '/^func \(s \*BriefService\)/{fn=$0} /loadWizardSession\(ctx/{print fn}' email_wizard.go
//
// -- which also lists `wizardHubSpotClient`, an unexported helper reached only through these.
// `t.Run` per method names the offender instead of stopping at the first.
func TestWizard_EveryTurnRefusesADeletedBrief(t *testing.T) {
	ctx := context.Background()

	turns := []struct {
		name string
		call func(h *wizardHarness, sessionID string) error
	}{
		{"plan", func(h *wizardHarness, id string) error {
			_, err := h.svc.PlanEmailWizard(ctx, &briefs.PlanEmailWizardPayload{
				ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: id})
			return err
		}},
		{"generate-content", func(h *wizardHarness, id string) error {
			_, err := h.svc.GenerateWizardContent(ctx, &briefs.GenerateWizardContentPayload{
				ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: id})
			return err
		}},
		{"get-session", func(h *wizardHarness, id string) error {
			_, err := h.svc.GetWizardSession(ctx, &briefs.GetWizardSessionPayload{
				ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: id})
			return err
		}},
		{"set-send-list", func(h *wizardHarness, id string) error {
			_, err := h.svc.SetWizardSendList(ctx, &briefs.SetWizardSendListPayload{
				ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: id})
			return err
		}},
		{"update-sections", func(h *wizardHarness, id string) error {
			_, err := h.svc.UpdateWizardSections(ctx, &briefs.UpdateWizardSectionsPayload{
				ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: id,
				Sections: []any{map[string]any{"type": "rich_text", "html": "<p>x</p>"}}})
			return err
		}},
		{"clone", func(h *wizardHarness, id string) error {
			_, err := h.svc.CloneWizardEmail(ctx, &briefs.CloneWizardEmailPayload{
				ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: id, Approved: true})
			return err
		}},
		{"chat", func(h *wizardHarness, id string) error {
			_, err := h.svc.ChatWizardTurn(ctx, &briefs.ChatWizardTurnPayload{
				ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: id, Message: "hi"})
			return err
		}},
	}

	for _, turn := range turns {
		t.Run(turn.name, func(t *testing.T) {
			h := newWizardHarness(t, wizardModelJSON)
			started, err := h.svc.StartEmailWizardPlan(ctx, &briefs.StartEmailWizardPlanPayload{
				ProjectID: wizardTestProject, BriefID: wizardTestBrief,
			})
			if err != nil {
				t.Fatalf("StartEmailWizardPlan: %v", err)
			}

			// The brief is deleted. The SESSION deliberately survives -- that is what makes
			// the guard necessary rather than redundant with the session lookup.
			delete(h.briefs.briefs, briefKey(wizardTestProject, wizardTestBrief))

			if cerr := turn.call(h, started.SessionID); cerr == nil {
				t.Fatal("the turn was allowed on a deleted brief")
			} else if nf := (*briefs.NotFoundError)(nil); !errors.As(cerr, &nf) {
				t.Errorf("err = %T (%v), want a 404 -- the brief is gone, so retrying cannot help", cerr, cerr)
			}
		})
	}
}

// unconfirmedHubSpotErr returns a real, ambiguous HubSpot error: a mutating request answered
// 429, which means the server MAY have applied the change. Built through the real client rather
// than hand-rolled, because IsUnconfirmed reads the concrete type -- a stand-in error would
// classify as definite and the test would prove nothing.
func unconfirmedHubSpotErr(t *testing.T) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	hc := hubspot.NewClient(
		hubspot.Credentials{PrivateAppToken: "t"}, hubspot.AccountConfig{PortalID: "8112310"},
		hubspot.WithBaseURL(srv.URL),
	)
	_, err := hc.SetSendList(context.Background(), "email-1", "list-1", nil)
	if err == nil {
		t.Fatal("fixture precondition: a mutating 429 must be an error")
	}
	if !hubspot.IsUnconfirmed(err) {
		t.Fatal("fixture precondition: a mutating 429 must classify as unconfirmed, else this test proves nothing")
	}
	return err
}

// TestWizard_AnUnconfirmedSendListIsNotReportedAsUnchanged pins the wording of both send-list
// failure paths against an AMBIGUOUS outcome.
//
// A 429 or 5xx on a mutating request means HubSpot may already have applied the list. Telling
// the operator to "set it again" -- or worse, that "the draft is unchanged" -- is a definite
// claim the service cannot support, and they act on it: a blind retry targets a draft that is
// not in the state they were told it was in. The service already draws this distinction
// everywhere else through hubspot.IsUnconfirmed; these two paths did not.
func TestWizard_AnUnconfirmedSendListIsNotReportedAsUnchanged(t *testing.T) {
	ctx := context.Background()
	ambiguous := unconfirmedHubSpotErr(t)

	t.Run("clone reports it as an issue, not as a retry instruction", func(t *testing.T) {
		h := newWizardHarness(t, wizardModelJSON)
		h.hubspot.searchHits = []hubspot.Email{{ID: "src-1", Name: "KubeCon EU 2025 - Registration Open"}}
		h.hubspot.sendErr = ambiguous
		started, err := h.svc.StartEmailWizardPlan(ctx, &briefs.StartEmailWizardPlanPayload{
			ProjectID: wizardTestProject, BriefID: wizardTestBrief})
		if err != nil {
			t.Fatalf("StartEmailWizardPlan: %v", err)
		}
		if _, perr := h.svc.PlanEmailWizard(ctx, &briefs.PlanEmailWizardPayload{
			ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID}); perr != nil {
			t.Fatalf("PlanEmailWizard: %v", perr)
		}
		if _, gerr := h.svc.GenerateWizardContent(ctx, &briefs.GenerateWizardContentPayload{
			ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID}); gerr != nil {
			t.Fatalf("GenerateWizardContent: %v", gerr)
		}
		listID := "list-1"
		out, cerr := h.svc.CloneWizardEmail(ctx, &briefs.CloneWizardEmailPayload{
			ProjectID: wizardTestProject, BriefID: wizardTestBrief,
			SessionID: started.SessionID, SendListID: &listID, Approved: true,
		})
		if cerr != nil {
			t.Fatalf("CloneWizardEmail: %v", cerr)
		}
		joined := strings.Join(out.ValidationIssues, " | ")
		if !strings.Contains(joined, "unknown") {
			t.Errorf("issues = %q, want the outcome described as unknown", joined)
		}
		if strings.Contains(joined, "could not be applied; set it again") {
			t.Errorf("issues = %q, want no blind-retry instruction for an unconfirmed outcome", joined)
		}
	})

	t.Run("set-send-list does not claim the draft is unchanged", func(t *testing.T) {
		h := newWizardHarness(t, wizardModelJSON)
		h.hubspot.searchHits = []hubspot.Email{{ID: "src-1", Name: "KubeCon EU 2025 - Registration Open"}}
		started, err := h.svc.StartEmailWizardPlan(ctx, &briefs.StartEmailWizardPlanPayload{
			ProjectID: wizardTestProject, BriefID: wizardTestBrief})
		if err != nil {
			t.Fatalf("StartEmailWizardPlan: %v", err)
		}
		if _, perr := h.svc.PlanEmailWizard(ctx, &briefs.PlanEmailWizardPayload{
			ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID}); perr != nil {
			t.Fatalf("PlanEmailWizard: %v", perr)
		}
		if _, gerr := h.svc.GenerateWizardContent(ctx, &briefs.GenerateWizardContentPayload{
			ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID}); gerr != nil {
			t.Fatalf("GenerateWizardContent: %v", gerr)
		}
		if _, cerr := h.svc.CloneWizardEmail(ctx, &briefs.CloneWizardEmailPayload{
			ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID, Approved: true,
		}); cerr != nil {
			t.Fatalf("CloneWizardEmail: %v", cerr)
		}

		h.hubspot.sendErr = ambiguous
		listID := "list-1"
		_, serr := h.svc.SetWizardSendList(ctx, &briefs.SetWizardSendListPayload{
			ProjectID: wizardTestProject, BriefID: wizardTestBrief,
			SessionID: started.SessionID, SendListID: &listID,
		})
		if serr == nil {
			t.Fatal("an unconfirmed send-list must still be reported as a failure")
		}
		// Goa's generated error types render an empty Error(); the operator-facing text is the
		// Message field, which is what actually reaches them.
		var unavailable *briefs.ConnServiceUnavailableError
		if !errors.As(serr, &unavailable) {
			t.Fatalf("err = %T (%v), want *briefs.ConnServiceUnavailableError", serr, serr)
		}
		msg := unavailable.Message
		if strings.Contains(msg, "the draft is unchanged") {
			t.Errorf("err = %q -- claims the draft is unchanged when the write may have landed", msg)
		}
		if !strings.Contains(msg, "unknown") {
			t.Errorf("err = %q, want the outcome described as unknown", msg)
		}
	})
}

// TestWizard_ASaveCannotRepopulateAScrubbedSession pins the READ ORDER inside
// loadWizardSession, which is load-bearing and invisible.
//
// If the brief were checked BEFORE the session is read, a delete landing between the two reads
// would pass the brief check (it ran before the archive) and then hand back the POST-scrub
// session. Its version matches what the scrub left, so a later save succeeds and repopulates
// exactly the content the deletion just cleared -- and the operator is never told.
//
// Reading the session FIRST inverts that: a scrub landing afterwards bumps `version` past the
// value the caller holds, and saveWizardSession -- which writes back at the version it read --
// fails stale.
//
// The interleaving is forced through the fake's onGet hook, which fires after the first repo
// read, so the scrub lands in exactly the window under test rather than by luck.
func TestWizard_ASaveCannotRepopulateAScrubbedSession(t *testing.T) {
	ctx := context.Background()
	h := newWizardHarness(t, wizardModelJSON)

	started, err := h.svc.StartEmailWizardPlan(ctx, &briefs.StartEmailWizardPlanPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief,
	})
	if err != nil {
		t.Fatalf("StartEmailWizardPlan: %v", err)
	}

	// Give the session something to scrub. A freshly started session has no content yet, so
	// the scrub's already-scrubbed guard would skip it and the test would pass for the wrong
	// reason -- proving only that a no-op scrub changes nothing.
	seeded, gerr := h.sessions.GetSession(ctx, wizardTestProject, wizardTestBrief, started.SessionID)
	if gerr != nil {
		t.Fatalf("GetSession (seed): %v", gerr)
	}
	seeded.ChatHistory = json.RawMessage(`[{"role":"user","content":"my unpublished keynote"}]`)
	if _, uerr := h.sessions.UpdateSession(ctx, seeded, seeded.Version); uerr != nil {
		t.Fatalf("UpdateSession (seed): %v", uerr)
	}

	// The delete lands in the WINDOW BETWEEN the two reads: onGet fires immediately after
	// GetBrief returns. This is what makes the ordering observable -- with the brief read
	// first, the scrub happens before the session read and the caller gets a post-scrub
	// snapshot whose version a later save still matches. With the session read first, the
	// scrub lands after it and the version has moved on.
	var scrubbed int64
	h.briefs.onGet = func() {
		var serr error
		if scrubbed, serr = h.sessions.ScrubSessionsForBrief(ctx, wizardTestProject, wizardTestBrief); serr != nil {
			t.Errorf("ScrubSessionsForBrief: %v", serr)
		}
	}

	sess, _, lerr := loadWizardSession(ctx, h.briefs, h.sessions, wizardTestProject, wizardTestBrief, started.SessionID)
	if lerr != nil {
		t.Fatalf("loadWizardSession: %v", lerr)
	}
	if scrubbed != 1 {
		t.Fatalf("fixture precondition: the scrub matched %d rows, want 1 -- it must actually clear this session", scrubbed)
	}

	// The in-flight turn tries to persist what it loaded.
	sess.ChatHistory = json.RawMessage(`[{"role":"user","content":"repopulated"}]`)
	if _, saveErr := saveWizardSession(ctx, h.sessions, sess, &model.Actor{Name: "Ada Lovelace"}); saveErr == nil {
		t.Fatal("a save at the pre-scrub version succeeded -- it repopulates the content the delete cleared")
	}

	after, gerr := h.sessions.GetSession(ctx, wizardTestProject, wizardTestBrief, started.SessionID)
	if gerr != nil {
		t.Fatalf("GetSession: %v", gerr)
	}
	if len(after.ChatHistory) != 0 {
		t.Errorf("chat_history = %s -- the scrubbed content came back", after.ChatHistory)
	}
}

// findWizardSourceEmail must bound the WHOLE paginated search, not inherit the request context.
//
// SearchEmails walks up to 200 pages and only the individual requests carry a deadline, so a slow
// portal could keep this planning handler running long past the server's write deadline while
// every single request looked healthy. Without this test the `context.WithTimeout` wrapper could
// be dropped in a refactor and nothing would fail until it hung in production.
func TestWizardPlanBoundsTheEmailSearch(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	h.hubspot.searchHits = []hubspot.Email{{ID: "src-1", Name: "KubeCon EU 2025 - Registration Open"}}
	started := h.start(t)

	// A context with NO deadline of its own: any deadline the fake observes can then only have
	// come from the wrapper under test.
	beforeCall := time.Now()
	if _, err := h.svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	}); err != nil {
		t.Fatalf("PlanEmailWizard: %v", err)
	}
	afterCall := time.Now()

	if !h.hubspot.searchHadDeadline {
		t.Fatal("SearchEmails ran with NO deadline; the accountsCallTimeout wrapper is gone")
	}
	expectedMin := beforeCall.Add(accountsCallTimeout)
	expectedMax := afterCall.Add(accountsCallTimeout)
	if h.hubspot.searchDeadline.Before(expectedMin) || h.hubspot.searchDeadline.After(expectedMax) {
		t.Errorf("SearchEmails deadline %v outside [%v, %v] — expected now+accountsCallTimeout (%v)",
			h.hubspot.searchDeadline, expectedMin, expectedMax, accountsCallTimeout)
	}
}

// A portal-wide clone-source search must be REFUSED on the shared LF connection.
//
// `SearchEmails` is portal-wide and `projectID` only chooses the connection — it never filters
// the results. So on the system fallback a hit can be another project's past sent email, and
// cloning it would seed this project's draft with that one's subject, body, sponsor names and
// pricing. `EmailReferenceSource.Get` already refuses for exactly this reason; the wizard's
// search did not, which made the same cross-tenant read reachable by a different door.
//
// This is the COMMON case, not an edge one: every project without its own HubSpot connection
// resolves to the shared row.
func TestWizardPlanRefusesPortalWideSearchOnSharedConnection(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	// A hit the search WOULD return, so a pass here cannot come from an empty portal.
	h.hubspot.searchHits = []hubspot.Email{{ID: "other-project-email", Name: "KubeCon EU 2025 - Registration Open"}}
	h.svc.SetWizardBackend(h.sessions, fakeWizardResolver{client: h.hubspot, fromSystem: true}, h.audiences)
	started := h.start(t)

	plan, err := h.svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	if err != nil {
		t.Fatalf("PlanEmailWizard must still succeed by falling back to the stage guide: %v", err)
	}
	if plan.Mode == "reference" || plan.SourceEmail != nil {
		t.Errorf("shared-connection planning must NOT pick a clone source, got mode=%q source=%+v", plan.Mode, plan.SourceEmail)
	}
	if h.hubspot.searchHadDeadline {
		t.Error("SearchEmails was called at all; the refusal must happen BEFORE the portal-wide query")
	}
}

// The other direction: a project with its OWN connection must still get a clone source.
//
// Without this, "always refuse" passes the test above and silently removes the feature for every
// project that legitimately has HubSpot history.
func TestWizardPlanStillSearchesOnProjectOwnedConnection(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	h.hubspot.searchHits = []hubspot.Email{{ID: "src-1", Name: "KubeCon EU 2025 - Registration Open"}}
	h.svc.SetWizardBackend(h.sessions, fakeWizardResolver{client: h.hubspot, fromSystem: false}, h.audiences)
	started := h.start(t)

	plan, err := h.svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	if err != nil {
		t.Fatalf("PlanEmailWizard: %v", err)
	}
	if plan.Mode != "reference" || plan.SourceEmail == nil || plan.SourceEmail.ID != "src-1" {
		t.Errorf("a project-owned connection must still pick the clone source, got mode=%q source=%+v", plan.Mode, plan.SourceEmail)
	}
}

// A url supplied at PLAN-START must survive to the plan turn.
//
// It was the one optional plan-start field accepted and then never read: `ExtraContext`,
// `EmailType` and `IsTransactional` were all persisted, `URL` was not. A caller who supplied it
// as the contract documents ("Event page to plan from") got a 200 and planning from the brief's
// own scraped details instead, with nothing in the response saying which page was used.
func TestWizardPlanStartPersistsTheRequestedURL(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	const want = "https://events.linuxfoundation.org/a-different-page/"

	started, err := h.svc.StartEmailWizardPlan(context.Background(), &briefs.StartEmailWizardPlanPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief,
		URL: strPtr(want),
	})
	if err != nil {
		t.Fatalf("StartEmailWizardPlan: %v", err)
	}

	sess, _, lerr := loadWizardSession(context.Background(), h.briefs, h.sessions,
		wizardTestProject, wizardTestBrief, started.SessionID)
	if lerr != nil {
		t.Fatalf("loadWizardSession: %v", lerr)
	}
	if got := wizardStoredPlan(sess).URL; got != want {
		t.Errorf("plan-start url not persisted:\n got %q\nwant %q", got, want)
	}
}

// And a url supplied ONLY at plan-start must not be overridden by the absence of one on the plan
// turn — the fallback has to be one-directional.
func TestWizardPlanTurnDoesNotClearTheStoredURL(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	const want = "https://events.linuxfoundation.org/a-different-page/"

	started, err := h.svc.StartEmailWizardPlan(context.Background(), &briefs.StartEmailWizardPlanPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, URL: strPtr(want),
	})
	if err != nil {
		t.Fatalf("StartEmailWizardPlan: %v", err)
	}
	// The plan turn sends NO url of its own, which is the case the fallback exists for.
	if _, perr := h.svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	}); perr != nil {
		t.Fatalf("PlanEmailWizard: %v", perr)
	}

	sess, _, lerr := loadWizardSession(context.Background(), h.briefs, h.sessions,
		wizardTestProject, wizardTestBrief, started.SessionID)
	if lerr != nil {
		t.Fatalf("loadWizardSession: %v", lerr)
	}
	if got := wizardStoredPlan(sess).URL; got != want {
		t.Errorf("the plan turn dropped the stored url:\n got %q\nwant %q", got, want)
	}
}

// recordingEventFetcher captures the URL it was asked to fetch.
type recordingEventFetcher struct {
	gotURL string
	body   []byte
}

func (f *recordingEventFetcher) Fetch(_ context.Context, eventURL string) ([]byte, error) {
	f.gotURL = eventURL
	return f.body, nil
}

type fixedEventParser struct{ details eventurl.EventDetails }

func (p fixedEventParser) Parse([]byte) eventurl.EventDetails { return p.details }

// The plan-start url must actually DRIVE the page fetch, not merely round-trip through the blob.
//
// The two persistence tests above assert storage only, and structurally cannot assert usage:
// `resolveWizardDetails` returns early whenever the brief already carries a non-empty EventName,
// which the shared harness always seeds. So they would still pass if a future change re-broke the
// read path while continuing to store the value — which is the exact bug this was.
func TestWizardPlanFetchesThePlanStartURL(t *testing.T) {
	const planStartURL = "https://events.linuxfoundation.org/a-different-page/"

	h := newWizardHarness(t, wizardModelJSON)
	// Blanked so resolveWizardDetails does NOT short-circuit and actually reaches the fetch.
	h.briefs.briefs[briefKey(wizardTestProject, wizardTestBrief)].EventDetails = json.RawMessage(`{}`)
	fetcher := &recordingEventFetcher{body: []byte("<html></html>")}
	h.svc.SetEventURL(fetcher, fixedEventParser{details: eventurl.EventDetails{Name: "Parsed Event"}})

	started, err := h.svc.StartEmailWizardPlan(context.Background(), &briefs.StartEmailWizardPlanPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, URL: strPtr(planStartURL),
	})
	if err != nil {
		t.Fatalf("StartEmailWizardPlan: %v", err)
	}
	// The plan turn sends NO url of its own: the stored one is all there is.
	if _, perr := h.svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	}); perr != nil {
		t.Fatalf("PlanEmailWizard: %v", perr)
	}

	if fetcher.gotURL != planStartURL {
		t.Errorf("plan-start url never reached the fetcher:\n got %q\nwant %q", fetcher.gotURL, planStartURL)
	}
}

// And the plan turn's OWN url must win when both are present — the fallback is one-directional.
func TestWizardPlanTurnURLWinsOverPlanStart(t *testing.T) {
	const planStartURL = "https://events.linuxfoundation.org/from-plan-start/"
	const planTurnURL = "https://events.linuxfoundation.org/from-the-plan-turn/"

	h := newWizardHarness(t, wizardModelJSON)
	h.briefs.briefs[briefKey(wizardTestProject, wizardTestBrief)].EventDetails = json.RawMessage(`{}`)
	fetcher := &recordingEventFetcher{body: []byte("<html></html>")}
	h.svc.SetEventURL(fetcher, fixedEventParser{details: eventurl.EventDetails{Name: "Parsed Event"}})

	started, err := h.svc.StartEmailWizardPlan(context.Background(), &briefs.StartEmailWizardPlanPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, URL: strPtr(planStartURL),
	})
	if err != nil {
		t.Fatalf("StartEmailWizardPlan: %v", err)
	}
	if _, perr := h.svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
		URL: strPtr(planTurnURL),
	}); perr != nil {
		t.Fatalf("PlanEmailWizard: %v", perr)
	}

	if fetcher.gotURL != planTurnURL {
		t.Errorf("this turn's url must win:\n got %q\nwant %q", fetcher.gotURL, planTurnURL)
	}
}

// Setting the send list must be REFUSED when the connection changed since the clone turn.
//
// `sess.EmailID` is recorded by the CLONE turn, so it is a cross-turn id exactly like the plan's
// `src.ID` — an earlier revision of this file's comment claimed otherwise, and both review bots
// caught it independently. After a revoke or rotation the same numeric id names a different
// portal's email, and this call MUTATES it: it would change the recipients of another tenant's
// draft, which is worse than reading one.
func TestWizardSendListRefusesAfterTheConnectionChanged(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	h.audiences.newestFirst = []*model.CampaignAudience{{
		ID: "aud-1", Platform: model.ProviderHubSpot, Status: model.AudienceBuilt,
		PlatformMasterListID: "ils-77", BuiltInPortalID: "portal-1",
	}}
	started := h.start(t)
	// A draft recorded by an earlier turn, which is the whole point.
	h.sessions.items[started.SessionID].EmailID = "email-99"

	// The connection now resolves to the shared LF row.
	h.svc.SetWizardBackend(h.sessions, fakeWizardResolver{client: h.hubspot, fromSystem: true}, h.audiences)

	_, err := h.svc.SetWizardSendList(context.Background(), &briefs.SetWizardSendListPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("a changed connection must be a 409, got %T: %v", err, err)
	}
	// The MUTATION is the thing that must not have happened. A refusal that still applied the
	// send list would satisfy the error assertion above and leak anyway.
	if h.hubspot.sendListID != "" {
		t.Errorf("the send list was applied to %q despite the refusal", h.hubspot.sendListID)
	}
}

// The other direction: a project-owned connection must still be able to set the send list.
func TestWizardSendListStillWorksOnProjectOwnedConnection(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	h.audiences.newestFirst = []*model.CampaignAudience{{
		ID: "aud-1", Platform: model.ProviderHubSpot, Status: model.AudienceBuilt,
		PlatformMasterListID: "ils-77", BuiltInPortalID: "portal-1",
	}}
	started := h.start(t)
	h.sessions.items[started.SessionID].EmailID = "email-99"
	h.svc.SetWizardBackend(h.sessions, fakeWizardResolver{client: h.hubspot, fromSystem: false}, h.audiences)

	if _, err := h.svc.SetWizardSendList(context.Background(), &briefs.SetWizardSendListPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	}); err != nil {
		t.Fatalf("SetWizardSendList on the project's own connection: %v", err)
	}
	if h.hubspot.sendListID == "" {
		t.Error("the send list must still be applied when the connection is the project's own")
	}
}

// The clone turn must refuse when the connection changed since planning.
//
// `src.ID` was captured by the plan turn against whatever portal resolved then, and `CloneEmail`
// reads that id's content from whatever portal resolves NOW — so after a revoke or rotation this
// would clone another tenant's sent email into a draft attributed to this project, persisted on
// the session and handed back as `DraftURL`.
func TestWizardCloneRefusesAfterTheConnectionChanged(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	h.hubspot.searchHits = []hubspot.Email{{ID: "src-1", Name: "KubeCon EU 2025 - Registration Open"}}
	started := h.start(t)

	// Plan AND generate on the project's OWN connection, so both of CloneWizardEmail's earlier
	// 409s (no generated content, no planned source) are already satisfied. Without the generate
	// step this test passed with the guard REMOVED -- it was asserting the 409 TYPE while a
	// different 409 answered first.
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

	// The connection now resolves to the shared LF row.
	h.svc.SetWizardBackend(h.sessions, fakeWizardResolver{client: h.hubspot, fromSystem: true}, h.audiences)

	_, err := h.svc.CloneWizardEmail(context.Background(), &briefs.CloneWizardEmailPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID, Approved: true,
	})
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("a changed connection must be a 409, got %T: %v", err, err)
	}
	// WHICH 409, not just a 409: three of them live in this handler, and asserting only the type
	// let this test pass with the guard removed.
	if !strings.Contains(conflict.Message, "no longer reachable through this project's HubSpot connection") {
		t.Errorf("wrong 409 -- this must be the provenance refusal, got %q", conflict.Message)
	}
	// The CLONE is what must not have happened — a 409 that still cloned would leak anyway.
	if h.hubspot.clonedFrom != "" {
		t.Errorf("an email was cloned from %q despite the refusal", h.hubspot.clonedFrom)
	}
}

// The voice-reference read must skip on a changed connection, rather than read by a stale id.
//
// `GetEmail`/`GetEmailHTMLWidgets` are read-only, so unlike the clone this degrades silently —
// generating without a reference is what a project with no clone source already gets.
func TestWizardReferenceReadSkipsAfterTheConnectionChanged(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	h.hubspot.searchHits = []hubspot.Email{{ID: "src-1", Name: "KubeCon EU 2025 - Registration Open"}}
	started := h.start(t)
	if _, err := h.svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	}); err != nil {
		t.Fatalf("PlanEmailWizard: %v", err)
	}

	h.svc.SetWizardBackend(h.sessions, fakeWizardResolver{client: h.hubspot, fromSystem: true}, h.audiences)
	h.hubspot.getEmailCalls = 0

	// Generation still SUCCEEDS: the reference is an enrichment, not a precondition.
	if _, err := h.svc.GenerateWizardContent(context.Background(), &briefs.GenerateWizardContentPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	}); err != nil {
		t.Fatalf("GenerateWizardContent must still succeed without a voice reference: %v", err)
	}
	if h.hubspot.getEmailCalls != 0 {
		t.Errorf("the clone source was read %d time(s) through a connection it was not found under", h.hubspot.getEmailCalls)
	}
}
