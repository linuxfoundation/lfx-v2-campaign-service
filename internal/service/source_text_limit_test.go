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
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// A survey's source document is its ONLY factual basis, so one longer than the limit is refused
// with a 400 naming the field and the limit -- never cut silently, which would drop a finding,
// closing date or incentive from the copy with nothing telling the operator.

func wantSourceTextRefusal(t *testing.T, err error) {
	t.Helper()
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("error = %T (%v), want a 400", err, err)
	}
	if !strings.Contains(bad.Message, "event_details.sourceText") || !strings.Contains(bad.Message, "3000") {
		t.Errorf("message = %q, want it to name event_details.sourceText and the limit", bad.Message)
	}
}

func TestSourceTextTooLong_RefusesOnlyPastTheLimit(t *testing.T) {
	if err := sourceTextTooLong(strings.Repeat("s", maxFactSourceTextRunes)); err != nil {
		t.Errorf("a document exactly at the limit was refused: %v", err)
	}
	// Measured as the prompt renders it: whitespace runs collapse and the ends are trimmed, so
	// padding does not count against the operator.
	if err := sourceTextTooLong("  " + strings.Repeat("s", maxFactSourceTextRunes) + "\n\n"); err != nil {
		t.Errorf("surrounding whitespace counted against the limit: %v", err)
	}
	// Runes, not bytes: a non-ASCII document at the limit passes.
	if err := sourceTextTooLong(strings.Repeat("é", maxFactSourceTextRunes)); err != nil {
		t.Errorf("a non-ASCII document at the limit was refused: %v", err)
	}
	wantSourceTextRefusal(t, sourceTextTooLong(strings.Repeat("s", maxFactSourceTextRunes+1)))
}

func TestBriefWrites_RefuseAnOversizedSourceDocument(t *testing.T) {
	details := func(n int) any { return map[string]any{"sourceText": strings.Repeat("s", n)} }
	input := func(n int) *briefs.BriefInput {
		return &briefs.BriefInput{
			ProgramType: "events", EventSlug: "cncf-survey-2026",
			DeliveryType: strPtr("survey"), Stage: strPtr("Survey Promo"),
			EventDetails: details(n),
		}
	}

	t.Run("create", func(t *testing.T) {
		s := newTestBriefService(newFakeBriefRepo())
		if _, err := s.CreateBrief(context.Background(), &briefs.CreateBriefPayload{ProjectID: "cncf", Brief: input(maxFactSourceTextRunes)}); err != nil {
			t.Fatalf("a document at the limit was refused: %v", err)
		}
		_, err := s.CreateBrief(context.Background(), &briefs.CreateBriefPayload{ProjectID: "cncf", Brief: input(maxFactSourceTextRunes + 1)})
		wantSourceTextRefusal(t, err)
	})

	t.Run("update", func(t *testing.T) {
		repo := newFakeBriefRepo()
		repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
			ID: "b1", ProjectID: "cncf", EventSlug: "cncf-survey-2026",
			DeliveryType: model.DeliverySurvey, Stage: "Survey Promo", Status: model.BriefDraft, Version: 1,
		}
		s := newTestBriefService(repo)
		ifMatch := "1"
		in := input(maxFactSourceTextRunes + 1)
		in.DeliveryType, in.Stage = nil, nil
		_, err := s.UpdateBrief(context.Background(), &briefs.UpdateBriefPayload{ProjectID: "cncf", BriefID: "b1", IfMatch: &ifMatch, Brief: in})
		wantSourceTextRefusal(t, err)
		if stored := repo.briefs[briefKey("cncf", "b1")]; len(stored.EventDetails) != 0 {
			t.Error("the oversized document was written despite the refusal")
		}
	})
}

func TestGenerateEmailCopy_RefusesAnOversizedSourceDocument(t *testing.T) {
	// A brief stored before the save-time check must not reach the model either.
	repo := newFakeBriefRepo()
	blob, _ := json.Marshal(map[string]any{
		"eventName":  "CNCF Annual Survey 2026",
		"sourceText": strings.Repeat("s", maxFactSourceTextRunes+1),
	})
	repo.briefs[briefKey("proj-123", "b-survey")] = &model.CampaignBrief{
		ID: "b-survey", ProjectID: "proj-123", DeliveryType: model.DeliverySurvey, Stage: "Survey Promo",
		EventDetails: blob,
	}
	svc := newTestBriefService(repo)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the model was called with a source document that would have been cut")
	}))
	t.Cleanup(srv.Close)
	svc.SetLLMClient(newTestLLMClient(t, srv))

	_, err := svc.GenerateEmailCopy(context.Background(), &briefs.GenerateEmailCopyPayload{
		ProjectID: "proj-123", BriefID: "b-survey", BearerToken: strPtr("token"), Stage: strPtr("Survey Promo"),
	})
	wantSourceTextRefusal(t, err)
}

func TestGenerateWizardContent_RefusesAnOversizedSourceDocument(t *testing.T) {
	h := newWizardHarness(t, wizardModelJSON)
	blob, _ := json.Marshal(map[string]any{
		"eventName":  "CNCF Annual Survey 2026",
		"sourceText": strings.Repeat("s", maxFactSourceTextRunes+1),
	})
	h.briefs.briefs[briefKey(wizardTestProject, wizardTestBrief)] = &model.CampaignBrief{
		ID: wizardTestBrief, ProjectID: wizardTestProject,
		DeliveryType: model.DeliverySurvey, Stage: "Survey Promo", EventDetails: blob,
	}
	started := h.start(t)
	if _, err := h.svc.PlanEmailWizard(context.Background(), &briefs.PlanEmailWizardPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	}); err != nil {
		t.Fatalf("PlanEmailWizard: %v", err)
	}
	_, err := h.svc.GenerateWizardContent(context.Background(), &briefs.GenerateWizardContentPayload{
		ProjectID: wizardTestProject, BriefID: wizardTestBrief, SessionID: started.SessionID,
	})
	wantSourceTextRefusal(t, err)
}
