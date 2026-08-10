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

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/llm"
)

// newTestLLMClient builds a real llm.Client against an httptest.Server, using the
// WithHTTPClient testing seam rather than a mock — see client.go's doc comment on why
// this package deliberately exposes no interface to mock.
func newTestLLMClient(t *testing.T, srv *httptest.Server) *llm.Client {
	t.Helper()
	c, err := llm.NewClient(llm.Config{
		ProxyURL: srv.URL,
		APIKey:   "test-key",
	}, llm.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("llm.NewClient: %v", err)
	}
	return c
}

// TestDecodeEmailCopyEventDetails verifies the event details decoder handles valid and
// invalid inputs correctly, mirroring the opportunistic pattern.
func TestDecodeEmailCopyEventDetails(t *testing.T) {
	tests := []struct {
		name      string
		blob      json.RawMessage
		wantName  string
		wantError bool
	}{
		{
			name:      "valid details",
			blob:      json.RawMessage(`{"event_name":"KubeCon EU 2026","location":"Barcelona","start_date":"June 17","end_date":"June 20"}`),
			wantName:  "KubeCon EU 2026",
			wantError: false,
		},
		{
			name:      "partial details with only event_name",
			blob:      json.RawMessage(`{"event_name":"Linux Foundation Summit"}`),
			wantName:  "Linux Foundation Summit",
			wantError: false,
		},
		{
			name:      "empty blob",
			blob:      json.RawMessage(``),
			wantError: true,
		},
		{
			name:      "invalid json",
			blob:      json.RawMessage(`{invalid}`),
			wantError: true,
		},
		{
			name:      "missing event_name",
			blob:      json.RawMessage(`{"location":"Barcelona"}`),
			wantError: true,
		},
		{
			name:      "whitespace-only event_name",
			blob:      json.RawMessage(`{"event_name":"   "}`),
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeEmailCopyEventDetails(tt.blob)
			if (err != nil) != tt.wantError {
				t.Errorf("decodeEmailCopyEventDetails() error = %v, wantError %v", err, tt.wantError)
				return
			}
			if !tt.wantError && got.EventName != tt.wantName {
				t.Errorf("decodeEmailCopyEventDetails() EventName = %q, want %q", got.EventName, tt.wantName)
			}
		})
	}
}

// TestFormatEventDates verifies date formatting handles various input combinations.
func TestFormatEventDates(t *testing.T) {
	tests := []struct {
		startDate, endDate, want string
	}{
		{"", "", "Date TBD"},
		{"June 17", "", "June 17"},
		{"", "June 20", "June 20"},
		{"June 17", "June 17", "June 17"},
		{"June 17", "June 20", "June 17 - June 20"},
		{"  June 17  ", "  June 20  ", "June 17 - June 20"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatEventDates(tt.startDate, tt.endDate)
			if got != tt.want {
				t.Errorf("formatEventDates(%q, %q) = %q, want %q", tt.startDate, tt.endDate, got, tt.want)
			}
		})
	}
}

// TestTruncateString verifies string truncation with trailing whitespace stripping.
func TestTruncateString(t *testing.T) {
	tests := []struct {
		input, want string
		maxLen      int
	}{
		{"Hello", "Hello", 10},
		{"Hello World", "Hello", 5},
		{"Hello   ", "Hello", 8}, // trailing spaces get trimmed
		{"Hello   ", "Hello", 5}, // truncated before spaces, no trim needed
		{"", "", 10},
		{"a", "a", 1},
		{"ab", "a", 1},
	}

	for _, tt := range tests {
		t.Run(tt.input+"_"+fmt.Sprint(tt.maxLen), func(t *testing.T) {
			got := truncateString(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateString(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

// TestParseEmailCopyResponse verifies JSON response parsing with fence stripping.
func TestParseEmailCopyResponse(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantSubject   string
		wantCta       string
		wantError     bool
		wantTruncated bool
	}{
		{
			name:        "valid json response",
			raw:         `{"subject":"Join KubeCon","preheader":"Save your spot","body":"<p>Register now</p>","cta":"Register"}`,
			wantSubject: "Join KubeCon",
			wantCta:     "Register",
			wantError:   false,
		},
		{
			name:        "valid json with fences",
			raw:         "```json\n{\"subject\":\"Join KubeCon\",\"preheader\":\"Save your spot\",\"body\":\"<p>Register now</p>\",\"cta\":\"Register\"}\n```",
			wantSubject: "Join KubeCon",
			wantCta:     "Register",
			wantError:   false,
		},
		{
			name:          "truncate long subject",
			raw:           `{"subject":"` + repeatStr("x", 250) + `","preheader":"p","body":"b","cta":"c"}`,
			wantSubject:   repeatStr("x", 200),
			wantCta:       "c",
			wantError:     false,
			wantTruncated: true,
		},
		{
			name:      "invalid json",
			raw:       `{not valid json}`,
			wantError: true,
		},
		{
			name:      "empty response",
			raw:       ``,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEmailCopyResponse(tt.raw)
			if (err != nil) != tt.wantError {
				t.Errorf("parseEmailCopyResponse() error = %v, wantError %v", err, tt.wantError)
				return
			}
			if !tt.wantError {
				if got.Subject != tt.wantSubject {
					t.Errorf("Subject = %q (len %d), want %q (len %d)", got.Subject, len(got.Subject), tt.wantSubject, len(tt.wantSubject))
				}
				if got.Cta != tt.wantCta {
					t.Errorf("Cta = %q, want %q", got.Cta, tt.wantCta)
				}
				if tt.wantTruncated && len(got.Subject) != 200 {
					t.Errorf("Subject not truncated to 200 chars, got %d", len(got.Subject))
				}
			}
		})
	}
}

// repeatStr returns a string repeated n times for testing long inputs.
func repeatStr(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}

// TestComposeEmailCopyPrompt verifies prompt composition from event details.
func TestComposeEmailCopyPrompt(t *testing.T) {
	vars := emailCopyPromptVars{
		eventName: "KubeCon Europe 2026",
		location:  "Barcelona",
		dates:     "June 17 - June 20",
	}

	sys, user := composeEmailCopyPrompt(vars)

	// System prompt should contain constraints.
	if !strings.Contains(sys, "ONLY the event details") {
		t.Error("system prompt missing scrape constraint")
	}
	if !strings.Contains(sys, "Subject") {
		t.Error("system prompt missing subject requirement")
	}

	// User prompt should contain the event details.
	if !strings.Contains(user, "KubeCon Europe 2026") {
		t.Error("user prompt missing event name")
	}
	if !strings.Contains(user, "Barcelona") {
		t.Error("user prompt missing location")
	}
	if !strings.Contains(user, "June 17 - June 20") {
		t.Error("user prompt missing dates")
	}
}

// TestGenerateEmailCopy_NoLLMClient verifies the handler returns 503 when no LLM client is wired.
func TestGenerateEmailCopy_NoLLMClient(t *testing.T) {
	svc := NewBriefService(nil, nil, nil, nil)
	// Don't call SetLLMClient, so it remains nil

	ctx := context.Background()
	payload := &briefs.GenerateEmailCopyPayload{
		ProjectID:   "proj-123",
		BriefID:     "brief-456",
		BearerToken: strPtr("token"),
	}

	result, err := svc.GenerateEmailCopy(ctx, payload)

	if result != nil {
		t.Error("expected nil result when llmClient is nil")
	}
	if err == nil {
		t.Error("expected error when llmClient is nil")
	}
	// Should be ServiceUnavailable (503).
	var unavail *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavail) {
		t.Errorf("expected ConnServiceUnavailableError, got %T: %v", err, err)
	}
}

// TestGenerateEmailCopy_BriefNotFound verifies a missing brief maps to 404 via mapBriefErr,
// exercising the handler's briefRepo.GetBrief error path end to end.
func TestGenerateEmailCopy_BriefNotFound(t *testing.T) {
	repo := newFakeBriefRepo()
	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("LLM should not be called when the brief lookup fails")
		}))))

	payload := &briefs.GenerateEmailCopyPayload{
		ProjectID:   "proj-123",
		BriefID:     "missing-brief",
		BearerToken: strPtr("token"),
	}

	result, err := svc.GenerateEmailCopy(context.Background(), payload)
	if result != nil {
		t.Error("expected nil result when the brief does not exist")
	}
	var notFound *briefs.NotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("expected NotFoundError, got %T: %v", err, err)
	}
}

// TestGenerateEmailCopy_InvalidEventDetails verifies the handler returns 400 when the
// brief's EventDetails blob decodes without an event_name, exercising decodeEmailCopyEventDetails
// through the actual handler path rather than in isolation.
func TestGenerateEmailCopy_InvalidEventDetails(t *testing.T) {
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("proj-123", "brief-456")] = &model.CampaignBrief{
		ID:           "brief-456",
		ProjectID:    "proj-123",
		EventDetails: json.RawMessage(`{"location":"Barcelona"}`), // no event_name
	}
	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("LLM should not be called when event details are invalid")
		}))))

	payload := &briefs.GenerateEmailCopyPayload{
		ProjectID:   "proj-123",
		BriefID:     "brief-456",
		BearerToken: strPtr("token"),
	}

	result, err := svc.GenerateEmailCopy(context.Background(), payload)
	if result != nil {
		t.Error("expected nil result when event details are invalid")
	}
	var badReq *briefs.BadRequestError
	if !errors.As(err, &badReq) {
		t.Errorf("expected BadRequestError, got %T: %v", err, err)
	}
}

// TestGenerateEmailCopy_LLMError verifies an LLM call failure maps to 503, using a real
// llm.Client (via WithHTTPClient) pointed at an httptest.Server that always errors — not a mock.
func TestGenerateEmailCopy_LLMError(t *testing.T) {
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("proj-123", "brief-456")] = &model.CampaignBrief{
		ID:           "brief-456",
		ProjectID:    "proj-123",
		EventDetails: json.RawMessage(`{"event_name":"KubeCon EU 2026"}`),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, srv))

	payload := &briefs.GenerateEmailCopyPayload{
		ProjectID:   "proj-123",
		BriefID:     "brief-456",
		BearerToken: strPtr("token"),
	}

	result, err := svc.GenerateEmailCopy(context.Background(), payload)
	if result != nil {
		t.Error("expected nil result when the LLM call fails")
	}
	var unavail *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavail) {
		t.Errorf("expected ConnServiceUnavailableError, got %T: %v", err, err)
	}
}

// TestGenerateEmailCopy_HappyPath verifies the full flow: a valid brief and a valid LLM JSON
// response produce a populated, correctly truncated EmailCopy.
func TestGenerateEmailCopy_HappyPath(t *testing.T) {
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("proj-123", "brief-456")] = &model.CampaignBrief{
		ID:        "brief-456",
		ProjectID: "proj-123",
		EventDetails: json.RawMessage(
			`{"event_name":"KubeCon EU 2026","location":"Barcelona","start_date":"June 17","end_date":"June 20"}`),
	}
	longSubject := repeatStr("x", 250)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		content := `{"subject":"` + longSubject + `","preheader":"Save your spot","body":"<p>Register now</p>","cta":"Register"}`
		encoded, err := json.Marshal(content)
		if err != nil {
			t.Fatalf("marshal fake LLM content: %v", err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(encoded) + `},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, srv))

	payload := &briefs.GenerateEmailCopyPayload{
		ProjectID:   "proj-123",
		BriefID:     "brief-456",
		BearerToken: strPtr("token"),
	}

	result, err := svc.GenerateEmailCopy(context.Background(), payload)
	if err != nil {
		t.Fatalf("GenerateEmailCopy() error = %v, want nil", err)
	}
	if result == nil {
		t.Fatal("expected a non-nil EmailCopy on the happy path")
	}
	if len(result.Subject) != 200 {
		t.Errorf("Subject not truncated to 200 chars, got %d", len(result.Subject))
	}
	if result.Preheader != "Save your spot" {
		t.Errorf("Preheader = %q, want %q", result.Preheader, "Save your spot")
	}
	if result.Body != "<p>Register now</p>" {
		t.Errorf("Body = %q, want %q", result.Body, "<p>Register now</p>")
	}
	if result.Cta != "Register" {
		t.Errorf("Cta = %q, want %q", result.Cta, "Register")
	}
}

// TestDecodeEmailCopyEventDetails_FailsWithoutName verifies that decoding fails
// when event_name is missing, even if other fields are present.
func TestDecodeEmailCopyEventDetails_FailsWithoutName(t *testing.T) {
	// This test verifies the scrape-not-invent principle: without an event name,
	// copy generation has no core claim to make.
	details, err := decodeEmailCopyEventDetails(json.RawMessage(`{"location":"Barcelona"}`))

	if err == nil {
		t.Error("expected error when event_name is missing, got nil")
	}
	if details.EventName != "" {
		t.Errorf("EventName should be empty on error, got %q", details.EventName)
	}
}

// TestFormatEventDates_RangeFormat verifies date range formatting.
func TestFormatEventDates_RangeFormat(t *testing.T) {
	// Mutation test: reverting the "June 17 - June 20" format to "June 17, June 20"
	// should make this test fail.
	got := formatEventDates("June 17", "June 20")
	if got != "June 17 - June 20" {
		t.Errorf("date range format = %q, want %q", got, "June 17 - June 20")
	}
}

// TestTruncateString_EnforcesLimit verifies truncation is applied correctly.
func TestTruncateString_EnforcesLimit(t *testing.T) {
	// Mutation test: changing the max of 5 to 10 should make this test fail.
	input := "This is a long string that needs truncating"
	got := truncateString(input, 5)
	if len(got) > 5 {
		t.Errorf("truncated length = %d exceeds max 5", len(got))
	}
	if got != "This" {
		t.Errorf("truncated value = %q, want %q", got, "This")
	}
}

// TestParseEmailCopyResponse_EnforcesMaxLengths verifies response limits are enforced.
func TestParseEmailCopyResponse_EnforcesMaxLengths(t *testing.T) {
	// Create a response with fields that exceed the documented limits.
	tooLongSubject := repeatStr("x", 300)
	raw := `{"subject":"` + tooLongSubject + `","preheader":"p","body":"b","cta":"c"}`

	result, err := parseEmailCopyResponse(raw)
	if err != nil {
		t.Errorf("parseEmailCopyResponse() error = %v, want nil", err)
		return
	}

	// Mutation test: changing max subject length from 200 to 250 should fail this.
	if len(result.Subject) > 200 {
		t.Errorf("Subject truncation not applied: %d chars (want <= 200)", len(result.Subject))
	}
}
