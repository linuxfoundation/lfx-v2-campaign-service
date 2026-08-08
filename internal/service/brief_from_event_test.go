// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
)

func TestBriefFromEventDetails(t *testing.T) {
	tests := []struct {
		name        string
		projectID   string
		eventSlug   string
		details     eventurl.EventDetails
		programType model.ProgramType
		wantErr     bool
		wantErrMsg  string
		// Assertions on the returned brief (only if !wantErr)
		wantProjectID    string
		wantProgramType  model.ProgramType
		wantEventSlug    string
		wantURL          string
		wantStatus       model.BriefStatus
		wantEventName    string // extracted from EventDetails blob
		wantDescription  string // extracted from EventDetails blob
		wantLocation     string // extracted from EventDetails blob
		wantCopyNil      bool   // copy should be left empty
		wantKeywordsNil  bool   // keywords should be left empty
		wantTargetingNil bool   // targeting should be left empty
		wantPlatformsNil bool   // platforms should be left empty
	}{
		{
			name:        "complete event mapping",
			projectID:   "cncf",
			eventSlug:   "kubecon-eu-2026",
			programType: model.ProgramEvents,
			details: eventurl.EventDetails{
				Name:          "KubeCon Europe 2026",
				Description:   "The Cloud Native Computing Foundation's flagship conference",
				Location:      "Barcelona, Spain",
				StartDate:     "2026-04-20",
				EndDate:       "2026-04-23",
				Image:         "https://example.com/kubecon-2026.jpg",
				URL:           "https://example.com/events/kubecon-eu-2026",
				ExtractedFrom: "jsonld",
			},
			wantErr:          false,
			wantProjectID:    "cncf",
			wantProgramType:  model.ProgramEvents,
			wantEventSlug:    "kubecon-eu-2026",
			wantURL:          "https://example.com/events/kubecon-eu-2026",
			wantStatus:       model.BriefDraft,
			wantEventName:    "KubeCon Europe 2026",
			wantDescription:  "The Cloud Native Computing Foundation's flagship conference",
			wantLocation:     "Barcelona, Spain",
			wantCopyNil:      true,
			wantKeywordsNil:  true,
			wantTargetingNil: true,
			wantPlatformsNil: true,
		},
		{
			name:        "partial event: missing optional fields",
			projectID:   "tlf",
			eventSlug:   "open-source-summit-2026",
			programType: model.ProgramEvents,
			details: eventurl.EventDetails{
				Name: "Open Source Summit 2026",
				URL:  "https://example.com/oss-summit-2026",
				// Missing description, location, dates, image
				ExtractedFrom: "opengraph",
			},
			wantErr:          false,
			wantProjectID:    "tlf",
			wantProgramType:  model.ProgramEvents,
			wantEventSlug:    "open-source-summit-2026",
			wantURL:          "https://example.com/oss-summit-2026",
			wantStatus:       model.BriefDraft,
			wantEventName:    "Open Source Summit 2026",
			wantDescription:  "",
			wantLocation:     "",
			wantCopyNil:      true,
			wantKeywordsNil:  true,
			wantTargetingNil: true,
			wantPlatformsNil: true,
		},
		{
			name:        "missing event name (required)",
			projectID:   "cncf",
			eventSlug:   "unknown-event",
			programType: model.ProgramEvents,
			details: eventurl.EventDetails{
				// Name is empty — this should fail
				URL: "https://example.com/event",
			},
			wantErr:    true,
			wantErrMsg: "event details missing required eventName",
		},
		{
			name:        "education program type",
			projectID:   "lf",
			eventSlug:   "intro-to-linux",
			programType: model.ProgramEducation,
			details: eventurl.EventDetails{
				Name:          "Introduction to Linux",
				Description:   "Beginner course on Linux fundamentals",
				URL:           "https://example.com/courses/intro-to-linux",
				ExtractedFrom: "fallback",
			},
			wantErr:          false,
			wantProjectID:    "lf",
			wantProgramType:  model.ProgramEducation,
			wantEventSlug:    "intro-to-linux",
			wantURL:          "https://example.com/courses/intro-to-linux",
			wantStatus:       model.BriefDraft,
			wantEventName:    "Introduction to Linux",
			wantDescription:  "Beginner course on Linux fundamentals",
			wantCopyNil:      true,
			wantKeywordsNil:  true,
			wantTargetingNil: true,
			wantPlatformsNil: true,
		},
		{
			name:        "all optional fields empty except name",
			projectID:   "cncf",
			eventSlug:   "minimal-event",
			programType: model.ProgramEvents,
			details: eventurl.EventDetails{
				Name: "Minimal Event",
				// Everything else empty — valid since only name is required
			},
			wantErr:          false,
			wantProjectID:    "cncf",
			wantProgramType:  model.ProgramEvents,
			wantEventSlug:    "minimal-event",
			wantURL:          "",
			wantStatus:       model.BriefDraft,
			wantEventName:    "Minimal Event",
			wantDescription:  "",
			wantLocation:     "",
			wantCopyNil:      true,
			wantKeywordsNil:  true,
			wantTargetingNil: true,
			wantPlatformsNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BriefFromEventDetails(tt.projectID, tt.eventSlug, tt.details, tt.programType)

			if tt.wantErr {
				if err == nil {
					t.Errorf("BriefFromEventDetails: expected error, got nil")
				}
				if tt.wantErrMsg != "" && err.Error() != tt.wantErrMsg {
					t.Errorf("BriefFromEventDetails error message: got %q, want %q", err.Error(), tt.wantErrMsg)
				}
				return
			}

			if err != nil {
				t.Errorf("BriefFromEventDetails: unexpected error: %v", err)
				return
			}

			if got == nil {
				t.Errorf("BriefFromEventDetails: expected brief, got nil")
				return
			}

			// Check top-level fields
			if got.ProjectID != tt.wantProjectID {
				t.Errorf("ProjectID: got %q, want %q", got.ProjectID, tt.wantProjectID)
			}
			if got.ProgramType != tt.wantProgramType {
				t.Errorf("ProgramType: got %q, want %q", got.ProgramType, tt.wantProgramType)
			}
			if got.EventSlug != tt.wantEventSlug {
				t.Errorf("EventSlug: got %q, want %q", got.EventSlug, tt.wantEventSlug)
			}
			if got.URL != tt.wantURL {
				t.Errorf("URL: got %q, want %q", got.URL, tt.wantURL)
			}
			if got.Status != tt.wantStatus {
				t.Errorf("Status: got %q, want %q", got.Status, tt.wantStatus)
			}

			// Check that human-authored fields are left empty
			if tt.wantCopyNil && len(got.Copy) > 0 {
				t.Errorf("Copy: expected nil, got %s", string(got.Copy))
			}
			if tt.wantKeywordsNil && len(got.Keywords) > 0 {
				t.Errorf("Keywords: expected nil, got %s", string(got.Keywords))
			}
			if tt.wantTargetingNil && len(got.Targeting) > 0 {
				t.Errorf("Targeting: expected nil, got %s", string(got.Targeting))
			}
			if tt.wantPlatformsNil && len(got.Platforms) > 0 {
				t.Errorf("Platforms: expected nil, got %s", string(got.Platforms))
			}

			// Verify EventDetails blob by decoding it
			if len(got.EventDetails) > 0 {
				var decoded eventurl.EventDetails
				if err := json.Unmarshal(got.EventDetails, &decoded); err != nil {
					t.Errorf("EventDetails decode failed: %v", err)
					return
				}

				if decoded.Name != tt.wantEventName {
					t.Errorf("EventDetails.Name: got %q, want %q", decoded.Name, tt.wantEventName)
				}
				if decoded.Description != tt.wantDescription {
					t.Errorf("EventDetails.Description: got %q, want %q", decoded.Description, tt.wantDescription)
				}
				if decoded.Location != tt.wantLocation {
					t.Errorf("EventDetails.Location: got %q, want %q", decoded.Location, tt.wantLocation)
				}
			}
		})
	}
}

// TestBriefFromEventDetails_ReversibilityAfterMarshal verifies that the mapped
// brief's EventDetails blob round-trips: the original EventDetails should
// deserialize identically from the stored JSON.
func TestBriefFromEventDetails_ReversibilityAfterMarshal(t *testing.T) {
	original := eventurl.EventDetails{
		Name:          "Test Event",
		Description:   "A test event for verification",
		Location:      "Test Location",
		StartDate:     "2026-10-01",
		EndDate:       "2026-10-03",
		Image:         "https://example.com/test.jpg",
		URL:           "https://example.com/test",
		ExtractedFrom: "jsonld",
	}

	brief, err := BriefFromEventDetails("cncf", "test-event", original, model.ProgramEvents)
	if err != nil {
		t.Fatalf("BriefFromEventDetails: unexpected error: %v", err)
	}

	var decoded eventurl.EventDetails
	if err := json.Unmarshal(brief.EventDetails, &decoded); err != nil {
		t.Fatalf("failed to unmarshal EventDetails: %v", err)
	}

	if decoded != original {
		t.Errorf("EventDetails round-trip mismatch:\noriginal: %+v\ndecoded:  %+v", original, decoded)
	}
}

// TestBriefFromEventDetails_FieldsMissingName verifies that when a field is
// missing in EventDetails (the parser returned "" for it), the stored JSON
// reflects that accurately.
func TestBriefFromEventDetails_PartialEventStoresEmpty(t *testing.T) {
	partial := eventurl.EventDetails{
		Name: "Event Name",
		// Description, Location, etc. intentionally empty
	}

	brief, err := BriefFromEventDetails("cncf", "partial", partial, model.ProgramEvents)
	if err != nil {
		t.Fatalf("BriefFromEventDetails: unexpected error: %v", err)
	}

	var decoded eventurl.EventDetails
	if err := json.Unmarshal(brief.EventDetails, &decoded); err != nil {
		t.Fatalf("failed to unmarshal EventDetails: %v", err)
	}

	// Verify that empty fields are preserved, not fabricated
	if decoded.Description != "" {
		t.Errorf("Description: expected empty, got %q", decoded.Description)
	}
	if decoded.Location != "" {
		t.Errorf("Location: expected empty, got %q", decoded.Location)
	}
	if decoded.StartDate != "" {
		t.Errorf("StartDate: expected empty, got %q", decoded.StartDate)
	}
	if decoded.EndDate != "" {
		t.Errorf("EndDate: expected empty, got %q", decoded.EndDate)
	}
	if decoded.Image != "" {
		t.Errorf("Image: expected empty, got %q", decoded.Image)
	}
}

// TestBriefFromEventDetails_IntegrationWithCreateBrief verifies that a brief
// created from event details can be persisted via the BriefService.CreateBrief
// path and later retrieved with its EventDetails intact.
func TestBriefFromEventDetails_IntegrationWithCreateBrief(t *testing.T) {
	eventDetails := eventurl.EventDetails{
		Name:          "Test Conference",
		Description:   "A test conference",
		Location:      "Test City",
		StartDate:     "2026-05-01",
		EndDate:       "2026-05-03",
		Image:         "https://example.com/conf.jpg",
		URL:           "https://example.com/test-conf",
		ExtractedFrom: "jsonld",
	}

	// Map event details to a brief
	brief, err := BriefFromEventDetails("test-project", "test-conf", eventDetails, model.ProgramEvents)
	if err != nil {
		t.Fatalf("BriefFromEventDetails: unexpected error: %v", err)
	}

	// Verify the brief can be used with CreateBrief
	repo := newFakeBriefRepo()
	svc := newTestBriefService(repo)

	ctx := context.Background()
	created, err := svc.CreateBrief(ctx, &briefs.CreateBriefPayload{
		ProjectID: brief.ProjectID,
		Brief: &briefs.BriefInput{
			ProgramType:  string(brief.ProgramType),
			EventSlug:    brief.EventSlug,
			URL:          &brief.URL,
			EventDetails: unmarshalAny(brief.EventDetails),
		},
	})

	if err != nil {
		t.Fatalf("CreateBrief: unexpected error: %T: %v", err, err)
	}

	if created == nil {
		t.Fatalf("CreateBrief: expected brief, got nil")
	}

	// Verify the stored EventDetails matches the original
	var storedEventDetails eventurl.EventDetails
	if err := json.Unmarshal(repo.briefs[briefKey(brief.ProjectID, created.ID)].EventDetails, &storedEventDetails); err != nil {
		t.Fatalf("failed to unmarshal stored EventDetails: %v", err)
	}

	if storedEventDetails != eventDetails {
		t.Errorf("stored EventDetails mismatch:\noriginal: %+v\nstored:   %+v", eventDetails, storedEventDetails)
	}

	// Verify the event name is readable by dispatchers
	var briefFields struct {
		EventName string `json:"eventName"`
	}
	if err := json.Unmarshal(repo.briefs[briefKey(brief.ProjectID, created.ID)].EventDetails, &briefFields); err != nil {
		t.Fatalf("failed to unmarshal dispatcher fields: %v", err)
	}

	if briefFields.EventName != eventDetails.Name {
		t.Errorf("dispatcher eventName: got %q, want %q", briefFields.EventName, eventDetails.Name)
	}
}
