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
	"testing"
	"time"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// fakeFlowHubSpot records the one call this path makes to HubSpot, so a test can assert
// WHICH emails were mapped and in WHAT order -- the property a workflow gets wrong silently.
type fakeFlowHubSpot struct {
	HubSpotWizardClient // every other method panics if the path ever calls one

	calls    int
	flowID   string
	opts     hubspot.CloneFlowOptions
	cloneErr error

	// portal is what AuthenticatedPortalID answers; lists is what SearchLists and GetList read.
	// A list GetList does not know reads as a static (MANUAL) list, which has no filter.
	portal    string
	lists     map[string]*hubspot.List
	listReads int
	// sourceSuppressionID is the source workflow's own suppression list ("900" when unset).
	sourceSuppressionID string
}

func (f *fakeFlowHubSpot) AuthenticatedPortalID(context.Context) (string, error) {
	return f.portal, nil
}

func (f *fakeFlowHubSpot) SearchLists(_ context.Context, query string) ([]hubspot.List, error) {
	var out []hubspot.List
	for _, l := range f.lists {
		if l.Name == query {
			out = append(out, *l)
		}
	}
	return out, nil
}

func (f *fakeFlowHubSpot) GetList(_ context.Context, id string) (*hubspot.List, error) {
	f.listReads++
	if l, ok := f.lists[id]; ok {
		return l, nil
	}
	return &hubspot.List{ListID: id, Name: "list " + id, ProcessingType: "MANUAL"}, nil
}

func (f *fakeFlowHubSpot) CloneFlowWithEmails(_ context.Context, flowID string, opts hubspot.CloneFlowOptions) (*hubspot.FlowRef, error) {
	f.flowID = flowID
	f.opts = opts
	if f.cloneErr != nil {
		return nil, f.cloneErr
	}
	// The source workflow's own suppression list, as CloneFlowWithEmails would merge it.
	suppression := append([]string{f.sourceSuppression()}, opts.SuppressionListIDs...)
	if opts.BeforeCreate != nil {
		if err := opts.BeforeCreate(suppression); err != nil {
			return nil, err
		}
	}
	f.calls++
	return &hubspot.FlowRef{
		FlowID: "new-flow-1",
		Name:   opts.Name,
		// What a client with no configured portal id builds: a link the service must not echo.
		EditURL:            "https://app.hubspot.com/workflows//platform/flow/new-flow-1/edit",
		SourceFlowID:       flowID,
		SourceFlowName:     "26Q2 - LF Research - Survey Promo",
		IsEnabled:          false,
		SendEmailSteps:     len(opts.EmailIDs),
		DatedSteps:         len(opts.SendDates),
		EnrollmentListIDs:  opts.EnrollmentListIDs,
		SuppressionListIDs: suppression,
	}, nil
}

func (f *fakeFlowHubSpot) sourceSuppression() string {
	if f.sourceSuppressionID != "" {
		return f.sourceSuppressionID
	}
	return "900"
}

type fakeFlowResolver struct {
	client     *fakeFlowHubSpot
	err        error
	fromSystem bool
}

func (r fakeFlowResolver) ResolveHubSpotClient(context.Context, string) (HubSpotWizardClient, bool, error) {
	if r.err != nil {
		return nil, false, r.err
	}
	return r.client, r.fromSystem, nil
}

// sendWorkflowHarness wires a service whose briefs are staged and ready to sequence.
func sendWorkflowHarness(t *testing.T) (*BriefService, *fakeCampaignRepo, *fakeFlowHubSpot) {
	t.Helper()
	repo := newFakeBriefRepo()
	camps := &fakeCampaignRepo{}
	jobs := newFakeJobRepo()
	s := NewBriefService(repo, camps, jobs, NewOrchestrator(camps, jobs, nil))
	// Before every date the tests send, so the dates are in the future.
	s.SetClock(func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) })

	hs := &fakeFlowHubSpot{portal: "8112310"}
	// Every send built against the same audience: the ordinary, enrollable series.
	audiences := &fakeSeriesAudienceRepo{byBrief: map[string]*model.CampaignAudience{}}
	for _, id := range []string{"b-promo", "b-reminder", "b-closing"} {
		audiences.byBrief[id] = seriesAudienceRow("4477", nil, `["9002"]`)
	}
	s.SetWizardBackend(newFakeWizardSessionRepo(), fakeFlowResolver{client: hs}, audiences)

	for _, b := range []struct {
		id      string
		stage   string
		emailID string
	}{
		{"b-promo", "Survey Promo", "email-1"},
		{"b-reminder", "Survey Reminder", "email-2"},
		{"b-closing", "Survey Closing", "email-3"},
	} {
		repo.briefs[briefKey("tlf", b.id)] = &model.CampaignBrief{
			ID: b.id, ProjectID: "tlf", DeliveryType: model.DeliverySurvey, Stage: b.stage,
		}
		camps.storeRow(&model.Campaign{
			BriefID: b.id, Platform: model.ProviderHubSpot, Variant: model.VariantDefault,
			PlatformCampaignID: b.emailID, Status: model.CampaignStatusCreated,
			Result: json.RawMessage(`{"id":"` + b.emailID + `","portalId":"8112310","contentStaged":true}`),
		})
	}
	return s, camps, hs
}

// sendWorkflowPayload builds a request. dates nil means one valid date per brief, since the fake
// source has one dated step per send; pass []string{} to send none.
func sendWorkflowPayload(briefIDs []string, dates []string) *briefs.CreateSendWorkflowPayload {
	if dates == nil {
		for i := range briefIDs {
			dates = append(dates, fmt.Sprintf("2026-10-%02d", 9+i*5))
		}
	}
	sourceURL := "https://app.hubspot.com/workflows/8112310/platform/flow/1841232183/edit"
	return &briefs.CreateSendWorkflowPayload{
		ProjectID: "tlf",
		Input: &briefs.SendWorkflowInput{
			SourceWorkflowURL: &sourceURL,
			Name:              "2026 Cloud Native Survey - Workflow",
			BriefIds:          briefIDs,
			SendDates:         dates,
		},
	}
}

func TestCreateSendWorkflow_MapsStagedDraftsInSendOrder(t *testing.T) {
	s, _, hs := sendWorkflowHarness(t)

	res, err := s.CreateSendWorkflow(context.Background(),
		sendWorkflowPayload([]string{"b-promo", "b-reminder", "b-closing"}, []string{"2026-10-09", "2026-10-19", "2026-10-23"}))
	if err != nil {
		t.Fatalf("CreateSendWorkflow: %v", err)
	}

	// The emails travel in the order the BRIEFS were given, which is the order the workflow
	// sends them. Any other order still produces a complete-looking workflow that sends the
	// closing email first.
	if got := strings.Join(hs.opts.EmailIDs, ","); got != "email-1,email-2,email-3" {
		t.Errorf("email ids = %q, want the staged drafts in brief order", got)
	}
	if hs.flowID != "1841232183" {
		t.Errorf("flow id = %q, want it parsed out of the pasted URL", hs.flowID)
	}
	want := []time.Time{
		time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC),
	}
	for i, w := range want {
		if !hs.opts.SendDates[i].Equal(w) {
			t.Errorf("send date %d = %v, want %v", i, hs.opts.SendDates[i], w)
		}
	}
	if res.IsEnabled {
		t.Error("the created workflow must be reported disabled")
	}
}

// fakeSeriesAudienceRepo answers ListAudiences PER BRIEF.
//
// A separate fake from audience_test.go's, which returns every stored row whatever brief is
// asked for -- that is harmless where it is used, but here it would make two sends appear to
// share an audience no matter what, turning the rule under test into a tautology.
type fakeSeriesAudienceRepo struct {
	domain.AudienceRepository // every other method panics if this path ever calls one

	byBrief map[string]*model.CampaignAudience
}

func (r *fakeSeriesAudienceRepo) ListAudiences(_ context.Context, _, briefID string) ([]*model.CampaignAudience, error) {
	row, ok := r.byBrief[briefID]
	if !ok {
		return nil, nil
	}
	return []*model.CampaignAudience{row}, nil
}

// seriesAudienceRow is a BUILT HubSpot audience in the harness's portal. include, when set, is
// the attach-existing list set (the master holds its first id); suppression is a JSON array.
func seriesAudienceRow(master string, include []string, suppression string) *model.CampaignAudience {
	row := &model.CampaignAudience{
		Platform:             model.ProviderHubSpot,
		Status:               model.AudienceBuilt,
		PlatformMasterListID: master,
		BuiltInPortalID:      "8112310",
		CreatedAt:            time.Now(),
	}
	if include != nil {
		raw, _ := json.Marshal(include)
		row.IncludeListIDs = raw
	}
	if suppression != "" {
		row.SuppressionListIDs = json.RawMessage(suppression)
	}
	return row
}

// seriesAudiences replaces the harness's audiences.
func seriesAudiences(s *BriefService, hs *fakeFlowHubSpot, byBrief map[string]*model.CampaignAudience) {
	s.SetWizardBackend(newFakeWizardSessionRepo(), fakeFlowResolver{client: hs}, &fakeSeriesAudienceRepo{byBrief: byBrief})
}

func TestCreateSendWorkflow_ChoosesTheSourceWorkflow(t *testing.T) {
	// The ordinary case names no source: making an operator paste a workflow URL to do the usual
	// thing would be a step with no decision in it. The deployment's setting wins over the in-code
	// default, and a URL on the request wins over both.
	named := "https://app.hubspot.com/workflows/8112310/platform/flow/777/edit"
	cases := []struct {
		name, env string
		url       *string
		want      string
	}{
		{name: "nothing configured", env: "", want: defaultSurveySequenceWorkflowID},
		{name: "blank setting", env: "  ", want: defaultSurveySequenceWorkflowID},
		{name: "deployment setting", env: "555", want: "555"},
		{name: "request URL", env: "555", url: &named, want: "777"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvSurveySequenceWorkflowID, tc.env)
			s, _, hs := sendWorkflowHarness(t)
			p := sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil)
			p.Input.SourceWorkflowURL = tc.url

			if _, err := s.CreateSendWorkflow(context.Background(), p); err != nil {
				t.Fatalf("CreateSendWorkflow: %v", err)
			}
			if hs.flowID != tc.want {
				t.Errorf("flow id = %q, want %q", hs.flowID, tc.want)
			}
		})
	}
}

func TestCreateSendWorkflow_RefusesDatesThatWouldSendOutOfTurn(t *testing.T) {
	// A wait-until-date step whose date has passed moves a contact straight on, so a past date
	// sends two emails back to back, and a descending pair sends the closing push first.
	cases := map[string][]string{
		"today":         {"2026-10-01", "2026-10-09"},
		"in the past":   {"2026-09-30", "2026-10-09"},
		"out of order":  {"2026-10-19", "2026-10-09"},
		"the same date": {"2026-10-09", "2026-10-09"},
	}
	for name, dates := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, hs := sendWorkflowHarness(t)
			_, err := s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, dates))
			var bad *briefs.BadRequestError
			if !errors.As(err, &bad) || !strings.Contains(bad.Message, "is not after") {
				t.Fatalf("error = %T (%v), want a 400 naming the date", err, err)
			}
			if hs.calls != 0 {
				t.Error("a workflow was created with dates that send out of turn")
			}
		})
	}
}

func TestCreateSendWorkflow_RefusesADraftThatIsNotFullyStaged(t *testing.T) {
	// A pending row can carry the draft's id while its content step never ran, so the draft still
	// holds the TEMPLATE's copy; an unconfirmed one is the orchestrator's to reconcile.
	for _, status := range []string{model.CampaignStatusPending, model.CampaignStatusUnconfirmed} {
		t.Run(status, func(t *testing.T) {
			s, camps, hs := sendWorkflowHarness(t)
			row, err := camps.GetCampaignByPlatform(context.Background(), "tlf", "b-reminder", model.ProviderHubSpot, model.VariantDefault)
			if err != nil {
				t.Fatal(err)
			}
			row.Status = status

			_, err = s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil))
			var conflict *briefs.ConflictError
			if !errors.As(err, &conflict) || !strings.Contains(conflict.Message, "send 2") || !strings.Contains(conflict.Message, status) {
				t.Fatalf("error = %T (%v), want a 409 naming send 2 and its status", err, err)
			}
			if hs.calls != 0 {
				t.Error("a workflow was created around a draft that is not fully staged")
			}
		})
	}
}

func TestCreateSendWorkflow_RefusesADraftStagedInAnotherPortal(t *testing.T) {
	// The audience was rebuilt in the project's own portal after this draft was staged in the LF
	// one: the audience's portal check passes, but the send step would name an email the new
	// portal cannot see. A row that never recorded its portal is unproven, and refused the same.
	for name, result := range map[string]string{
		"another portal":     `{"portalId":"1","contentStaged":true}`,
		"no recorded portal": `{"id":"email-2","contentStaged":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, camps, hs := sendWorkflowHarness(t)
			row, err := camps.GetCampaignByPlatform(context.Background(), "tlf", "b-reminder", model.ProviderHubSpot, model.VariantDefault)
			if err != nil {
				t.Fatal(err)
			}
			row.Result = json.RawMessage(result)

			_, err = s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil))
			var conflict *briefs.ConflictError
			if !errors.As(err, &conflict) || !strings.Contains(conflict.Message, "send 2") {
				t.Fatalf("error = %T (%v), want a 409 naming send 2", err, err)
			}
			if hs.calls != 0 {
				t.Error("a workflow was created around a draft from another portal")
			}
		})
	}
}

func TestCreateSendWorkflow_RefusesASendListTheSourceWorkflowSuppresses(t *testing.T) {
	// The check runs against the suppressions the workflow will ACTUALLY apply, which include the
	// source workflow's own -- known only once the source is read.
	s, _, hs := sendWorkflowHarness(t)
	hs.sourceSuppressionID = "4477"

	_, err := s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil))
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Message, "source workflow suppresses") {
		t.Fatalf("error = %T (%v), want a 409 naming the source's suppression", err, err)
	}
	if hs.calls != 0 {
		t.Error("a workflow was created that would reach nobody on its own audience")
	}
}

func TestCreateSendWorkflow_MissingDefaultWorkflowNamesTheSetting(t *testing.T) {
	// The request named no source, so a 404 is the deployment's to fix, not the caller's.
	t.Setenv(constants.EnvSurveySequenceWorkflowID, "")
	s, _, hs := sendWorkflowHarness(t)
	// A real client's 404, so the mapping is tested against the error HubSpot actually yields.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	real := hubspot.NewClient(hubspot.Credentials{PrivateAppToken: "t"}, hubspot.AccountConfig{PortalID: "8112310"}, hubspot.WithBaseURL(srv.URL))
	_, notFound := real.GetFlow(context.Background(), defaultSurveySequenceWorkflowID)
	if !hubspot.IsNotFound(notFound) {
		t.Fatalf("precondition: want a HubSpot 404, got %v", notFound)
	}
	hs.cloneErr = notFound
	p := sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil)
	p.Input.SourceWorkflowURL = nil

	_, err := s.CreateSendWorkflow(context.Background(), p)
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Message, constants.EnvSurveySequenceWorkflowID) {
		t.Fatalf("error = %T (%v), want a 409 naming the deployment setting", err, err)
	}
}

func TestCreateSendWorkflow_RefusesSendsBuiltAgainstDifferentAudiences(t *testing.T) {
	// A workflow enrolls contacts ONCE and sends every step to them. If the sends were built
	// against different audiences, any single choice sends some of the emails to people the
	// operator never selected for them -- and the enrollment step looks correct either way.
	cases := map[string]*model.CampaignAudience{
		"a different master list": seriesAudienceRow("4478", nil, `["9002"]`),
		"different suppressions":  seriesAudienceRow("4477", nil, `["9003"]`),
		"a different include set": seriesAudienceRow("4477", []string{"4477", "4479"}, `["9002"]`),
		"a different portal": func() *model.CampaignAudience {
			r := seriesAudienceRow("4477", nil, `["9002"]`)
			r.BuiltInPortalID = "1"
			return r
		}(),
		"no suppressions at all": seriesAudienceRow("4477", nil, ""),
	}
	for name, second := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, hs := sendWorkflowHarness(t)
			seriesAudiences(s, hs, map[string]*model.CampaignAudience{
				"b-promo":    seriesAudienceRow("4477", nil, `["9002"]`),
				"b-reminder": second,
			})

			_, err := s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil))
			var conflict *briefs.ConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("error = %T (%v), want a 409", err, err)
			}
			if !strings.Contains(conflict.Message, "send 2") {
				t.Errorf("message = %q, want it to name which send disagrees", conflict.Message)
			}
			if hs.calls != 0 {
				t.Error("a workflow was created despite the sends disagreeing about their audience")
			}
		})
	}
}

func TestCreateSendWorkflow_EnrollsTheAudienceTheSendsShare(t *testing.T) {
	// Every list the audience sends to is enrolled -- an attach-existing audience sends to all of
	// its include lists, so enrolling only the master would drop the rest -- and the audience's
	// suppressions travel with it, because a workflow's send step ignores the draft's own.
	s, _, hs := sendWorkflowHarness(t)
	shared := func() *model.CampaignAudience {
		return seriesAudienceRow("4477", []string{"4477", "4478"}, `["9002","9003"]`)
	}
	seriesAudiences(s, hs, map[string]*model.CampaignAudience{"b-promo": shared(), "b-reminder": shared()})

	res, err := s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil))
	if err != nil {
		t.Fatalf("CreateSendWorkflow: %v", err)
	}
	if got := strings.Join(hs.opts.EnrollmentListIDs, ","); got != "4477,4478" {
		t.Errorf("enrollment lists = %q, want every list the audience sends to", got)
	}
	if got := strings.Join(hs.opts.SuppressionListIDs, ","); got != "9002,9003" {
		t.Errorf("suppression lists = %q, want the audience's own", got)
	}
	if got := strings.Join(res.EnrollmentListIds, ","); got != "4477,4478" {
		t.Errorf("result enrollment_list_ids = %q, want the lists that were enrolled", got)
	}
	if got := strings.Join(res.SuppressionListIds, ","); got != "900,9002,9003" {
		t.Errorf("result suppression_list_ids = %q, want the workflow's whole suppression set", got)
	}
}

func TestCreateSendWorkflow_RefusesASendWithNoBuiltAudience(t *testing.T) {
	// Without an audience the only thing left to enroll is the SOURCE workflow's own -- another
	// campaign's audience, and one the staging-time suppression checks never saw.
	building := seriesAudienceRow("4477", nil, `["9002"]`)
	building.Status = model.AudienceBuilding
	for name, row := range map[string]*model.CampaignAudience{"none": nil, "still building": building} {
		t.Run(name, func(t *testing.T) {
			s, _, hs := sendWorkflowHarness(t)
			byBrief := map[string]*model.CampaignAudience{"b-promo": seriesAudienceRow("4477", nil, `["9002"]`)}
			if row != nil {
				byBrief["b-reminder"] = row
			}
			seriesAudiences(s, hs, byBrief)

			_, err := s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil))
			var conflict *briefs.ConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("error = %T (%v), want a 409", err, err)
			}
			if !strings.Contains(conflict.Message, "send 2 (brief b-reminder)") || strings.Contains(conflict.Message, "send_list_id") {
				t.Errorf("message = %q, want it to name the send and no wizard-only remedy", conflict.Message)
			}
			if hs.calls != 0 {
				t.Error("a workflow was created for a send with no built audience")
			}
		})
	}
}

func TestCreateSendWorkflow_RefusesAnAudienceFromAnotherPortal(t *testing.T) {
	// The audience's list ids mean nothing outside the portal they were built in, and the
	// credential resolved now can reach a different one.
	s, _, hs := sendWorkflowHarness(t)
	hs.portal = "999"

	_, err := s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil))
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Message, "portal") {
		t.Fatalf("error = %T (%v), want a 409 naming the portal", err, err)
	}
	if hs.calls != 0 {
		t.Error("a workflow was created against lists the credential's portal cannot see")
	}
}

func TestCreateSendWorkflow_RefusesASendListTheStandingListNowCovers(t *testing.T) {
	// Enrolling the list IS the send on this path, so the standing-suppression send-time check
	// (#396) applies here exactly as it does before staging's and the wizard's SetSendList: the
	// project's standing list grew to cover the list the series sends to, and the suppression it
	// carries would exclude that whole list.
	s, _, hs := sendWorkflowHarness(t)
	covering, err := audience.MasterListFilter([]string{"7001", "4477"})
	if err != nil {
		t.Fatal(err)
	}
	hs.lists = map[string]*hubspot.List{
		"77": {ListID: "77", Name: audience.StandardSuppressionName("tlf"), FilterBranch: covering},
	}
	shared := func() *model.CampaignAudience { return seriesAudienceRow("4477", nil, `["77","9002"]`) }
	seriesAudiences(s, hs, map[string]*model.CampaignAudience{"b-promo": shared(), "b-reminder": shared()})

	_, err = s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil))
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %T (%v), want a 409", err, err)
	}
	if !strings.Contains(conflict.Message, "4477") || !strings.Contains(conflict.Message, "standing standard suppression list 77") {
		t.Errorf("the refusal must name both lists; got %q", conflict.Message)
	}
	if hs.calls != 0 {
		t.Error("a workflow was created that would exclude its own audience")
	}

	// Covering only other lists is no obstacle.
	fine, _ := audience.MasterListFilter([]string{"7001"})
	hs.lists["77"].FilterBranch = fine
	if _, err := s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil)); err != nil {
		t.Fatalf("CreateSendWorkflow: %v", err)
	}
}

func TestCreateSendWorkflow_RefusesAListBothSentToAndSuppressed(t *testing.T) {
	s, _, hs := sendWorkflowHarness(t)
	shared := func() *model.CampaignAudience { return seriesAudienceRow("4477", nil, `["4477"]`) }
	seriesAudiences(s, hs, map[string]*model.CampaignAudience{"b-promo": shared(), "b-reminder": shared()})

	_, err := s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil))
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Message, "both a recipient and a suppression list") {
		t.Fatalf("error = %T (%v), want a 409 naming the overlap", err, err)
	}
	if hs.calls != 0 {
		t.Error("a workflow was created that would exclude its own audience")
	}
}

func TestCreateSendWorkflow_NoAudienceStoreIsUnavailable(t *testing.T) {
	s, _, hs := sendWorkflowHarness(t)
	s.SetWizardBackend(newFakeWizardSessionRepo(), fakeFlowResolver{client: hs}, nil)

	_, err := s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil))
	var unavailable *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %T (%v), want a 503", err, err)
	}
	if hs.calls != 0 {
		t.Error("a workflow was created without knowing who it enrolls")
	}
}

func TestCreateSendWorkflow_RefusesBeforeTouchingHubSpot(t *testing.T) {
	cases := []struct {
		name     string
		briefIDs []string
		dates    []string
		url      string
		want     string
	}{
		{
			// A single email is staged, scheduled and sent on its own. Wrapping it in a
			// workflow adds an enrollment step between the draft and its send for no gain.
			name: "one brief is not a sequence", briefIDs: []string{"b-promo"},
			want: "at least two briefs",
		},
		{
			// Two sends pointed at ONE draft would send identical copy on both dates, and
			// read as correct in the editor.
			name: "the same brief twice", briefIDs: []string{"b-promo", "b-promo"},
			want: "appears twice",
		},
		{
			name: "a brief with no staged draft", briefIDs: []string{"b-promo", "b-unstaged"},
			want: "",
		},
		{
			// Go would read this as 2 March and move the operator's send by two days.
			name: "an impossible date", briefIDs: []string{"b-promo", "b-reminder"},
			dates: []string{"2026-02-30", "2026-03-01"},
			want:  "not a calendar date",
		},
		{
			name: "a URL that names no workflow", briefIDs: []string{"b-promo", "b-reminder"},
			url:  "https://app.hubspot.com/email/8112310/edit/224196159396/content",
			want: "names no workflow",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, hs := sendWorkflowHarness(t)
			p := sendWorkflowPayload(tc.briefIDs, tc.dates)
			if tc.url != "" {
				p.Input.SourceWorkflowURL = &tc.url
			}

			_, err := s.CreateSendWorkflow(context.Background(), p)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if tc.want != "" && !strings.Contains(errMessage(err), tc.want) {
				t.Errorf("error = %q, want it to name the problem (%q)", errMessage(err), tc.want)
			}
			// Nothing reached HubSpot. A workflow created from a request this service was
			// going to refuse is one a human then has to find and delete.
			if hs.calls != 0 {
				t.Errorf("HubSpot was called %d time(s) for a request that should be refused first", hs.calls)
			}
		})
	}
}

func TestCreateSendWorkflow_UnstagedBriefSaysWhichSend(t *testing.T) {
	// The operator has to know WHICH email to go and stage. "A brief is not staged" sends
	// them to check three briefs by hand.
	s, _, _ := sendWorkflowHarness(t)
	repo := s.briefs.(*fakeBriefRepo)
	repo.briefs[briefKey("tlf", "b-draftless")] = &model.CampaignBrief{
		ID: "b-draftless", ProjectID: "tlf", DeliveryType: model.DeliverySurvey, Stage: "Survey Closing",
	}

	_, err := s.CreateSendWorkflow(context.Background(),
		sendWorkflowPayload([]string{"b-promo", "b-reminder", "b-draftless"}, nil))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"b-draftless", "send 3", "no staged HubSpot draft"} {
		if !strings.Contains(errMessage(err), want) {
			t.Errorf("error = %q, want it to contain %q", errMessage(err), want)
		}
	}
}

func TestCreateSendWorkflow_RefusesAPaidBrief(t *testing.T) {
	// A paid brief carries ad copy, not a subject and a body, and has no draft to send.
	// Answered before the campaign lookup so the message names the real problem rather than
	// reporting a missing draft that was never going to exist.
	s, _, hs := sendWorkflowHarness(t)
	repo := s.briefs.(*fakeBriefRepo)
	repo.briefs[briefKey("tlf", "b-paid")] = &model.CampaignBrief{
		ID: "b-paid", ProjectID: "tlf", DeliveryType: model.DeliveryPaidMarketing,
	}

	_, err := s.CreateSendWorkflow(context.Background(),
		sendWorkflowPayload([]string{"b-promo", "b-paid"}, nil))
	if err == nil || !strings.Contains(errMessage(err), "paid-marketing") {
		t.Fatalf("error = %q, want it to name the paid surface", errMessage(err))
	}
	if hs.calls != 0 {
		t.Error("HubSpot was called for a request carrying a paid brief")
	}
}

func TestCreateSendWorkflow_ShapeMismatchIsAConflict(t *testing.T) {
	// The request is well formed and the briefs are staged; what conflicts is the SHAPE of
	// the workflow the operator chose. A 400 would tell them to fix their request, when the
	// fix is to pick a workflow with one send step per email.
	s, _, hs := sendWorkflowHarness(t)
	hs.cloneErr = fmt.Errorf("%w: workflow 1841232183 has 2 send-email step(s) but 3 email(s) were built; point at a workflow with one send step per email",
		hubspot.ErrFlowStepCountMismatch)

	_, err := s.CreateSendWorkflow(context.Background(),
		sendWorkflowPayload([]string{"b-promo", "b-reminder", "b-closing"}, nil))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %T (%v), want a 409", err, err)
	}
	// The counts are what make the message actionable, so they survive the mapping.
	if !strings.Contains(conflict.Message, "2 send-email step(s) but 3 email(s)") {
		t.Errorf("message = %q, want it to keep both counts", conflict.Message)
	}
}

func TestCreateSendWorkflow_SharedConnectionCopiesOnlyTheDeploymentsWorkflow(t *testing.T) {
	// On the shared LF connection a workflow id is portal-wide: a caller-named one can be another
	// project's workflow, whose definition would be read and whose other steps would run against
	// this series' audience. Only the deployment's own survey workflow is accepted there.
	onShared := func(t *testing.T) (*BriefService, *fakeFlowHubSpot) {
		t.Helper()
		s, _, hs := sendWorkflowHarness(t)
		byBrief := map[string]*model.CampaignAudience{}
		for _, id := range []string{"b-promo", "b-reminder"} {
			byBrief[id] = seriesAudienceRow("4477", nil, `["9002"]`)
		}
		s.SetWizardBackend(newFakeWizardSessionRepo(), fakeFlowResolver{client: hs, fromSystem: true},
			&fakeSeriesAudienceRepo{byBrief: byBrief})
		return s, hs
	}

	t.Run("a caller-named workflow is refused", func(t *testing.T) {
		t.Setenv(constants.EnvSurveySequenceWorkflowID, "555")
		s, hs := onShared(t)
		other := "https://app.hubspot.com/workflows/8112310/platform/flow/777/edit"
		p := sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil)
		p.Input.SourceWorkflowURL = &other

		_, err := s.CreateSendWorkflow(context.Background(), p)
		var conflict *briefs.ConflictError
		if !errors.As(err, &conflict) || !strings.Contains(conflict.Message, "connect the project's own HubSpot portal") {
			t.Fatalf("error = %T (%v), want a 409 telling the caller to connect its own portal", err, err)
		}
		if hs.flowID != "" || hs.calls != 0 {
			t.Errorf("the named workflow was read (flow %q, %d creates); it must not be touched", hs.flowID, hs.calls)
		}
	})

	for name, url := range map[string]*string{
		"no source named":            nil,
		"the configured id by name":  strPtr("https://app.hubspot.com/workflows/8112310/platform/flow/555/edit"),
		"the configured id, bare id": strPtr("555"),
	} {
		t.Run(name+" is accepted", func(t *testing.T) {
			t.Setenv(constants.EnvSurveySequenceWorkflowID, "555")
			s, hs := onShared(t)
			p := sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil)
			p.Input.SourceWorkflowURL = url

			if _, err := s.CreateSendWorkflow(context.Background(), p); err != nil {
				t.Fatalf("CreateSendWorkflow: %v", err)
			}
			if hs.flowID != "555" {
				t.Errorf("flow id = %q, want the configured 555", hs.flowID)
			}
		})
	}

	t.Run("the in-code default is accepted when nothing is configured", func(t *testing.T) {
		t.Setenv(constants.EnvSurveySequenceWorkflowID, "")
		s, hs := onShared(t)
		if _, err := s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil)); err != nil {
			t.Fatalf("CreateSendWorkflow: %v", err)
		}
		if hs.flowID != defaultSurveySequenceWorkflowID {
			t.Errorf("flow id = %q, want the default", hs.flowID)
		}
	})
}

func TestCreateSendWorkflow_RefusesADraftWithoutItsOwnCopy(t *testing.T) {
	// Dispatch's copy write is best-effort, so a `created` row can still hold the TEMPLATE's
	// subject and body. A workflow sends without anyone opening each draft, so only a draft
	// recorded as carrying its own copy may go in. A row that predates the record is unproven.
	for name, result := range map[string]string{
		"copy write failed":        `{"portalId":"8112310","contentStaged":false}`,
		"staged before the record": `{"portalId":"8112310"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, camps, hs := sendWorkflowHarness(t)
			row, err := camps.GetCampaignByPlatform(context.Background(), "tlf", "b-reminder", model.ProviderHubSpot, model.VariantDefault)
			if err != nil {
				t.Fatal(err)
			}
			row.Result = json.RawMessage(result)

			_, err = s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil))
			var conflict *briefs.ConflictError
			if !errors.As(err, &conflict) || !strings.Contains(conflict.Message, "send 2") || !strings.Contains(conflict.Message, "its own subject and body") {
				t.Fatalf("error = %T (%v), want a 409 naming send 2's missing copy", err, err)
			}
			if hs.calls != 0 {
				t.Error("a workflow was created around a draft that may hold the template's copy")
			}
		})
	}
}

func TestCreateSendWorkflow_EditLinkUsesTheVerifiedPortal(t *testing.T) {
	// A connection with no configured portal id leaves the client's link as /workflows//...; the
	// series' portal was verified before the create, so the link is built from that.
	s, _, _ := sendWorkflowHarness(t)
	res, err := s.CreateSendWorkflow(context.Background(), sendWorkflowPayload([]string{"b-promo", "b-reminder"}, nil))
	if err != nil {
		t.Fatalf("CreateSendWorkflow: %v", err)
	}
	if want := "https://app.hubspot.com/workflows/8112310/platform/flow/new-flow-1/edit"; res.EditURL != want {
		t.Errorf("edit_url = %q, want %q", res.EditURL, want)
	}
}

// A workflow create whose outcome is unknown (here a mutating 429 from a real client) must say
// that a workflow may already exist, so the operator checks HubSpot instead of retrying into a
// duplicate. Only the unreadable-success path used to say so.
func TestMapFlowCloneErr_UnconfirmedCreateSaysCheckHubSpot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	hc := hubspot.NewClient(
		hubspot.Credentials{PrivateAppToken: "t"}, hubspot.AccountConfig{PortalID: "8112310"},
		hubspot.WithBaseURL(srv.URL),
	)
	_, upstreamErr := hc.CreateList(context.Background(), "probe", json.RawMessage(`{"filterBranches":[]}`))
	if !hubspot.IsUnconfirmed(upstreamErr) {
		t.Fatalf("fixture precondition: a mutating 429 must classify as unconfirmed, got %v", upstreamErr)
	}

	err := mapFlowCloneErr(upstreamErr, "1841232183", false)
	var ise *briefs.InternalServerError
	if !errors.As(err, &ise) {
		t.Fatalf("an unconfirmed create must be a 500, got %T: %v", err, err)
	}
	if !strings.Contains(ise.Message, "a workflow may have been created, so check HubSpot before retrying") {
		t.Fatalf("the 500 must warn that a workflow may exist, got %q", ise.Message)
	}
}
