// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/llm"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/emailstage"
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
			blob:      json.RawMessage(`{"eventName":"KubeCon EU 2026","location":"Barcelona","startDate":"June 17","endDate":"June 20"}`),
			wantName:  "KubeCon EU 2026",
			wantError: false,
		},
		{
			name:      "partial details with only event_name",
			blob:      json.RawMessage(`{"eventName":"Linux Foundation Summit"}`),
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
			blob:      json.RawMessage(`{"eventName":"   "}`),
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
// A UI-written brief carries the scraped combined string in `dates` and leaves
// `startDate`/`endDate` empty -- those are paid-platform config fields. Reading only the pair
// therefore produced "Date TBD" for every email while the real dates sat in the brief, and the
// system prompt tells the model never to invent dates, so it could only omit them.
//
// Verified on a live brief: dates="19-20 November 2026", startDate="", endDate="".
func TestResolveEventDates(t *testing.T) {
	cases := []struct {
		name    string
		details emailCopyEventDetails
		want    string
	}{
		{"combined only, the shape a UI brief actually has", emailCopyEventDetails{Dates: "19-20 November 2026"}, "19-20 November 2026"},
		{"structured pair wins when set", emailCopyEventDetails{StartDate: "2026-11-19", EndDate: "2026-11-20", Dates: "ignored"}, "2026-11-19 - 2026-11-20"},
		{"combined is trimmed", emailCopyEventDetails{Dates: "  19-20 November 2026  "}, "19-20 November 2026"},
		{"whitespace-only combined is not a date", emailCopyEventDetails{Dates: "   "}, "Date TBD"},
		{"nothing at all", emailCopyEventDetails{}, "Date TBD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveEventDates(tc.details); got != tc.want {
				t.Errorf("resolveEventDates() = %q, want %q", got, tc.want)
			}
		})
	}
}

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
		// UTF-8 multibyte sequences (rune boundaries)
		{"こんにちは", "こんに", 3},      // 3 runes (Japanese), not truncated mid-rune
		{"café ☕", "café", 4},    // 4 runes, emoji excluded
		{"💻 computer", "💻 c", 3}, // 3 runes including emoji
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
	repo := newFakeBriefRepo()
	// Add a brief so ready() succeeds but don't wire an LLM client.
	repo.briefs["proj-123/brief-456"] = &model.CampaignBrief{
		ID:           "brief-456",
		ProjectID:    "proj-123",
		EventDetails: json.RawMessage(`{"eventName":"Test","location":"Boston","startDate":"2026-09-01","endDate":"2026-09-02"}`),
	}
	svc := newTestBriefService(repo)
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
	// Should be ServiceUnavailable (503) with the specific message.
	var unavail *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavail) {
		t.Errorf("expected ConnServiceUnavailableError, got %T: %v", err, err)
	}
	if unavail.Message != "AI model is not configured; email copy generation is unavailable" {
		t.Errorf("expected specific nil-client message, got: %s", unavail.Message)
	}
}

// TestGenerateEmailCopy_BriefNotFound verifies a missing brief maps to 404 via mapBriefErr,
// exercising the handler's briefRepo.GetBrief error path end to end.
func TestGenerateEmailCopy_BriefNotFound(t *testing.T) {
	repo := newFakeBriefRepo()
	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// t.Error, not t.Fatal: this runs on the HANDLER goroutine, where Fatal calls
			// runtime.Goexit on the wrong goroutine instead of failing the test cleanly.
			t.Error("LLM should not be called when the brief lookup fails")
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
			t.Error("LLM should not be called when event details are invalid") // handler goroutine -- see above
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
		EventDetails: json.RawMessage(`{"eventName":"KubeCon EU 2026"}`),
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
			`{"eventName":"KubeCon EU 2026","location":"Barcelona","startDate":"June 17","endDate":"June 20"}`),
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

// TestGenerateEmailCopy_RejectsIncompleteCopy covers the last guard in GenerateEmailCopy: a
// response that parses cleanly but leaves a required field blank.
//
// The tests above it cover transport failure and unparseable output — both cases where
// something is visibly wrong. This one is the case where nothing is: `{"subject":""}` is valid
// JSON, `parseEmailCopyResponse` returns no error, and every length limit is satisfied. Without
// the emptiness check the endpoint would answer 200 with a subject-less email, and the failure
// would surface as a send with a blank subject line rather than as an error anyone could act on.
// A prompt edit or a parsing change is exactly what would regress it.
//
// Each field is exercised separately: an `||` chain is easy to narrow to one field by accident,
// and a test that only blanks the subject would still pass if the other three checks were lost.
func TestGenerateEmailCopy_RejectsIncompleteCopy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"subject", `{"subject":"","preheader":"x","body":"<p>x</p>","cta":"Register"}`},
		{"preheader", `{"subject":"Join us","preheader":"","body":"<p>x</p>","cta":"Register"}`},
		{"body", `{"subject":"Join us","preheader":"x","body":"","cta":"Register"}`},
		{"cta", `{"subject":"Join us","preheader":"x","body":"<p>x</p>","cta":""}`},
		// Whitespace-only, which is a different case for exactly one field. truncateString
		// strips trailing whitespace, so a blank-but-not-empty subject, preheader or CTA has
		// already arrived here empty and the == "" test caught it. The body does NOT go
		// through truncateString — an oversized body is rejected rather than cut, because
		// truncating HTML at a rune boundary corrupts markup — so a body of spaces was the
		// one shape that reached this check non-empty, passed it, and returned 200 with a
		// blank email. All four are exercised so the trim cannot later be narrowed to body
		// alone and still pass.
		{"whitespace subject", `{"subject":"   ","preheader":"x","body":"<p>x</p>","cta":"Register"}`},
		{"whitespace preheader", `{"subject":"Join us","preheader":"  ","body":"<p>x</p>","cta":"Register"}`},
		{"whitespace body", `{"subject":"Join us","preheader":"x","body":"   \n\t ","cta":"Register"}`},
		{"whitespace cta", `{"subject":"Join us","preheader":"x","body":"<p>x</p>","cta":" "}`},
	} {
		t.Run("blank "+tc.name, func(t *testing.T) {
			repo := newFakeBriefRepo()
			repo.briefs[briefKey("proj-123", "brief-456")] = &model.CampaignBrief{
				ID:           "brief-456",
				ProjectID:    "proj-123",
				EventDetails: json.RawMessage(`{"eventName":"KubeCon EU 2026"}`),
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				encoded, err := json.Marshal(tc.content)
				if err != nil {
					t.Errorf("marshal fake LLM content: %v", err)
					return
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(encoded) + `},"finish_reason":"stop"}]}`))
			}))
			defer srv.Close()

			svc := newTestBriefService(repo)
			svc.SetLLMClient(newTestLLMClient(t, srv))

			result, err := svc.GenerateEmailCopy(context.Background(), &briefs.GenerateEmailCopyPayload{
				ProjectID:   "proj-123",
				BriefID:     "brief-456",
				BearerToken: strPtr("token"),
			})
			if result != nil {
				t.Errorf("result = %+v, want nil — copy with a blank %s was returned to the caller", result, tc.name)
			}
			var unavail *briefs.ConnServiceUnavailableError
			if !errors.As(err, &unavail) {
				t.Fatalf("err = %T (%v), want *briefs.ConnServiceUnavailableError", err, err)
			}
			// The message is what tells an operator which of the three 503s this is —
			// unreachable model, unreadable response, or incomplete copy.
			if unavail.Message != "the AI platform generated incomplete copy" {
				t.Errorf("Message = %q, want %q", unavail.Message, "the AI platform generated incomplete copy")
			}
		})
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

// TestGenerateEmailCopy_RejectsOverlongBody verifies that an oversized body HTML
// is rejected as a generation failure (503) rather than silently corrupted by truncation.
// Truncating HTML at an arbitrary rune boundary can cut inside tags or entities,
// leaving malformed markup. Oversized bodies must be rejected outright.
func TestGenerateEmailCopy_RejectsOverlongBody(t *testing.T) {
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("proj-123", "brief-456")] = &model.CampaignBrief{
		ID:           "brief-456",
		ProjectID:    "proj-123",
		EventDetails: json.RawMessage(`{"eventName":"Test Event"}`),
	}

	// Create a response with a body exceeding 8000 characters.
	longBody := `<p>` + repeatStr(`This is email content that exceeds the safe limit. `, 200) + `</div>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		content := `{"subject":"Test","preheader":"Test","body":"` + longBody + `","cta":"Click"}`
		encoded, _ := json.Marshal(content)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(encoded) + `},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, srv))

	result, err := svc.GenerateEmailCopy(context.Background(), &briefs.GenerateEmailCopyPayload{
		ProjectID:   "proj-123",
		BriefID:     "brief-456",
		BearerToken: strPtr("token"),
	})

	// Should reject as ServiceUnavailable (503), not return a silently corrupted copy.
	if result != nil {
		t.Error("expected nil result when body is overlong; got result instead of rejection")
	}
	var unavail *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavail) {
		t.Errorf("expected ConnServiceUnavailableError for overlong body, got %T: %v", err, err)
	}
}

// TestGenerateEmailCopy_RejectsOversizedPrompt verifies that event details large enough
// to create oversized event details (>2400 runes across the three fields) are rejected as 400
// calling the LLM. This prevents unbounded input-token cost and large allocations from
// unsized event_details fields.
func TestGenerateEmailCopy_RejectsOversizedPrompt(t *testing.T) {
	repo := newFakeBriefRepo()
	// Create a brief with event details sized to exceed the 2400-rune input limit.
	hugeName := repeatStr("KubeCon ", 500) // ~4000 chars
	repo.briefs[briefKey("proj-123", "brief-456")] = &model.CampaignBrief{
		ID:           "brief-456",
		ProjectID:    "proj-123",
		EventDetails: json.RawMessage(`{"eventName":"` + hugeName + `"}`),
	}
	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("LLM should not be called when prompt exceeds size limit") // handler goroutine -- see above
		}))))

	payload := &briefs.GenerateEmailCopyPayload{
		ProjectID:   "proj-123",
		BriefID:     "brief-456",
		BearerToken: strPtr("token"),
	}

	result, err := svc.GenerateEmailCopy(context.Background(), payload)

	// Should reject as BadRequest (400), not call the LLM.
	if result != nil {
		t.Error("expected nil result when prompt is oversized")
	}
	var badReq *briefs.BadRequestError
	if !errors.As(err, &badReq) {
		t.Errorf("expected BadRequestError for oversized prompt, got %T: %v", err, err)
	}
}

// TestGenerateEmailCopy_AcceptsSizeablePrompt verifies that normally-sized event details
// (within the 2400-rune input limit) pass validation and reach the LLM.
func TestGenerateEmailCopy_AcceptsSizeablePrompt(t *testing.T) {
	repo := newFakeBriefRepo()
	// Create a brief with large but acceptable event details.
	reasonablyLongName := repeatStr("x", 500) // 500 runes is well within the 2400 limit
	repo.briefs[briefKey("proj-123", "brief-456")] = &model.CampaignBrief{
		ID:           "brief-456",
		ProjectID:    "proj-123",
		EventDetails: json.RawMessage(`{"eventName":"` + reasonablyLongName + `","location":"Barcelona","startDate":"2026-06-17"}`),
	}

	// atomic for the same reason as in TestGenerateEmailCopy_PromptLimitCountsRunesNotBytes:
	// written on the handler's goroutine, read on the test's.
	var llmCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCalled.Store(true)
		w.Header().Set("Content-Type", "application/json")
		content := `{"subject":"Join Us","preheader":"Event details","body":"<p>Register now</p>","cta":"Register"}`
		encoded, _ := json.Marshal(content)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(encoded) + `},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, srv))

	result, err := svc.GenerateEmailCopy(context.Background(), &briefs.GenerateEmailCopyPayload{
		ProjectID:   "proj-123",
		BriefID:     "brief-456",
		BearerToken: strPtr("token"),
	})

	if !llmCalled.Load() {
		t.Error("LLM was not called; prompt size validation rejected a valid prompt")
	}
	if err != nil {
		t.Fatalf("expected success with sized prompt, got error: %v", err)
	}
	if result == nil {
		t.Error("expected non-nil result on success")
	}
}

// TestGenerateEmailCopy_PromptLimitCountsRunesNotBytes pins the unit of the prompt bound.
//
// The limit is stated to the caller as a character count and every other bound in this file
// counts runes, but the check was written with len(), which counts BYTES. The gap is invisible
// in ASCII and total elsewhere: a Japanese event name costs three bytes per character, so a
// caller naming their event in Japanese got a third of the advertised budget, and a caller
// naming it in English got all of it. The name below is 998 runes and 2994 bytes: composed with
// the 962-rune system prompt and the user template it is ~2100 runes — comfortably inside the
// limit — but ~4100 bytes, so counting bytes rejects a prompt well within the documented
// allowance and the LLM is never called.
func TestGenerateEmailCopy_PromptLimitCountsRunesNotBytes(t *testing.T) {
	repo := newFakeBriefRepo()
	// 998 runes / 2994 bytes (every rune here is three bytes in UTF-8).
	multibyteName := repeatStr("東京開発者会議", 142) + "東京開発" // 142*7 + 4
	repo.briefs[briefKey("proj-123", "brief-456")] = &model.CampaignBrief{
		ID:           "brief-456",
		ProjectID:    "proj-123",
		EventDetails: json.RawMessage(`{"eventName":"` + multibyteName + `"}`),
	}

	// atomic, not a plain bool: the flag is written on the httptest handler's goroutine and
	// read on the test's, and the atomic is what synchronizes them. Reading the response body
	// does NOT establish the edge — the client can consume bytes the handler has written while
	// the handler is still running, so a body read orders nothing with respect to the handler's
	// return. The atomic supplies the ordering the body read does not.
	var llmCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		llmCalled.Store(true)
		w.Header().Set("Content-Type", "application/json")
		content := `{"subject":"Join Us","preheader":"Event details","body":"<p>Register now</p>","cta":"Register"}`
		encoded, err := json.Marshal(content)
		if err != nil {
			t.Errorf("marshal fake LLM content: %v", err)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(encoded) + `},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, srv))

	result, err := svc.GenerateEmailCopy(context.Background(), &briefs.GenerateEmailCopyPayload{
		ProjectID:   "proj-123",
		BriefID:     "brief-456",
		BearerToken: strPtr("token"),
	})
	if err != nil {
		t.Fatalf("GenerateEmailCopy() error = %v; a %d-rune (%d-byte) name is within the "+
			"2400-RUNE input limit and must not be rejected",
			err, utf8.RuneCountInString(multibyteName), len(multibyteName))
	}
	if !llmCalled.Load() {
		t.Error("the LLM was never called: the prompt bound rejected a prompt inside its own limit")
	}
	if result == nil {
		t.Error("expected non-nil result for a prompt within the limit")
	}
}

// TestGenerateEmailCopy_RejectsOversizedInputBeforeComposing pins WHERE the bound is enforced.
//
// A size guard placed after the formatting it protects performs the allocation it exists to
// prevent: composeEmailCopyPrompt Sprintf's the three unbounded event-detail fields into a new
// string, so a stored event name of arbitrary size is copied in full and only then measured and
// refused. event_details is declared Any in design/brief.go, so nothing upstream bounds it.
//
// Both checks return the same 400 with the same message — deliberately, since the caller's
// remedy is identical — so the outcome cannot distinguish them. The log line can: the
// pre-composition check reports the size of the INPUT fields, the post-composition one the size
// of the composed prompt. Asserting on the former is what makes deleting the pre-check fail.
func TestGenerateEmailCopy_RejectsOversizedInputBeforeComposing(t *testing.T) {
	repo := newFakeBriefRepo()
	hugeName := repeatStr("KubeCon ", 500) // 4000 runes, over the 2400 limit on its own
	repo.briefs[briefKey("proj-123", "brief-456")] = &model.CampaignBrief{
		ID:           "brief-456",
		ProjectID:    "proj-123",
		EventDetails: json.RawMessage(`{"eventName":"` + hugeName + `"}`),
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {
			t.Error("the LLM must not be called for an oversized prompt")
		}))))

	result, err := svc.GenerateEmailCopy(context.Background(), &briefs.GenerateEmailCopyPayload{
		ProjectID:   "proj-123",
		BriefID:     "brief-456",
		BearerToken: strPtr("token"),
	})
	if result != nil {
		t.Errorf("result = %+v, want nil for an oversized prompt", result)
	}
	var badReq *briefs.BadRequestError
	if !errors.As(err, &badReq) {
		t.Fatalf("err = %T (%v), want *briefs.BadRequestError", err, err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "event details exceed prompt size limit") {
		t.Errorf("log = %q\nwant the PRE-composition rejection; the oversized name reached "+
			"composeEmailCopyPrompt and was formatted into a new string before being refused", logged)
	}
	if !strings.Contains(logged, "input_size=") {
		t.Errorf("log = %q, want an input_size field naming what was measured", logged)
	}
}

// The stage must reach the MODEL, not merely be accepted by the handler. Asserting only that the
// call succeeded would pass against a stage that was parsed and then dropped -- which is exactly
// how the whole feature would be inert while every test stayed green.
func TestGenerateEmailCopy_StageReachesThePrompt(t *testing.T) {
	cases := []struct {
		name  string
		stage *string
		// wantPhrase comes from the stage's Purpose, wantContent from its ContentPrompt. They are
		// separate fields appended separately, so one assertion cannot pin both.
		wantPhrase  string
		wantContent string
	}{
		// Purpose text unique to each stage's ported ContentPrompt.
		// EVERY stage, not a sample. LFXV2-1940 requires each one's prompt to reach the model, and
		// a two-stage sample cannot catch a template wired to the wrong constant -- the failure
		// that most plausibly survives review, since each stage is a table row that looks right
		// beside its neighbours.
		{"cfp launch", strPtr("CFP Launch"), "Recruit speakers", "PRIMARY OBJECTIVE: Recruit speakers"},
		{"schedule announcement", strPtr("Schedule Announcement"), "speaker lineup", "Showcase learning opportunities"},
		{"registration push", strPtr("Registration Push"), "Drive registrations", "PRIMARY OBJECTIVE: Drive registrations"},
		{"discount offer", strPtr("Discount Offer"), "VIP/alumni", "Offer exclusive rate to segment"},
		{"final countdown", strPtr("Final Countdown"), "anticipation", "PRIMARY OBJECTIVE: Confirm attendance"},
		{"post-event", strPtr("Post-Event"), "extend engagement", "PRIMARY OBJECTIVE: Thank attendees"},
		// Absent stage keeps the pre-stage behaviour: Registration Push, not an error.
		{"absent falls back", nil, "Drive registrations", "PRIMARY OBJECTIVE: Drive registrations"},
		// UNRECOGNISED falls back too, and must reach the service at all -- the design carried a
		// Goa `Enum` that rejected it with a 400 at the decoder, before `Resolve` could run, which
		// contradicted the acceptance criterion. Removing the enum is what this pins; the criterion
		// prefers a caller never blocked by a stage it cannot spell.
		{"unrecognised falls back", strPtr("Fnal Countdown"), "Drive registrations", "PRIMARY OBJECTIVE: Drive registrations"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeBriefRepo()
			repo.briefs[briefKey("proj-123", "brief-456")] = &model.CampaignBrief{
				ID: "brief-456", ProjectID: "proj-123",
				EventDetails: json.RawMessage(`{"eventName":"KubeCon EU 2026","location":"Barcelona","dates":"June 17-20, 2026"}`),
			}

			// atomic, not a bare string: the handler goroutine writes it and the test goroutine
			// reads it, which is a data race `go test -race` reports. The same boundary already
			// uses atomics elsewhere in this file.
			var sentBody atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				sentBody.Store(string(b))
				w.Header().Set("Content-Type", "application/json")
				content, _ := json.Marshal(`{"subject":"s","preheader":"p","body":"<p>b</p>","cta":"c"}`)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(content) + `},"finish_reason":"stop"}]}`))
			}))
			defer srv.Close()

			svc := newTestBriefService(repo)
			svc.SetLLMClient(newTestLLMClient(t, srv))

			if _, err := svc.GenerateEmailCopy(context.Background(), &briefs.GenerateEmailCopyPayload{
				ProjectID: "proj-123", BriefID: "brief-456", BearerToken: strPtr("token"), Stage: tc.stage,
			}); err != nil {
				t.Fatalf("GenerateEmailCopy() error = %v", err)
			}

			body, _ := sentBody.Load().(string)
			if !strings.Contains(body, tc.wantPhrase) {
				t.Errorf("prompt sent upstream does not carry %q for stage %v", tc.wantPhrase, tc.stage)
			}
			// The CONTENT PROMPT too, not only the Purpose. Both are appended, but from separate
			// fields -- so asserting a Purpose phrase alone leaves `tpl.ContentPrompt` free to be
			// dropped or swapped with every case still green, which is the half of the stage that
			// actually shapes the email.
			if !strings.Contains(body, tc.wantContent) {
				t.Errorf("prompt sent upstream does not carry the stage's own content brief %q", tc.wantContent)
			}
		})
	}
}

// A caller that sends NO stage must still succeed. Declaring `stage` in the request BODY made the
// body itself required -- Goa emits MissingPayloadError on EOF -- so a pre-stage caller POSTing
// with no body got a 400 instead of the default-stage copy it had always received. It is a query
// parameter for that reason. Verified against the running service: body-less went 400, then 200.
func TestGenerateEmailCopy_NilStageIsNotAnError(t *testing.T) {
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("proj-123", "brief-456")] = &model.CampaignBrief{
		ID: "brief-456", ProjectID: "proj-123",
		EventDetails: json.RawMessage(`{"eventName":"KubeCon EU 2026","location":"Barcelona","dates":"June 17-20, 2026"}`),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		content, _ := json.Marshal(`{"subject":"s","preheader":"p","body":"<p>b</p>","cta":"c"}`)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(content) + `},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, srv))

	got, err := svc.GenerateEmailCopy(context.Background(), &briefs.GenerateEmailCopyPayload{
		ProjectID: "proj-123", BriefID: "brief-456", BearerToken: strPtr("token"), Stage: nil,
	})
	if err != nil {
		t.Fatalf("a nil stage must resolve to the default, got error: %v", err)
	}
	if got == nil || got.Subject == "" {
		t.Error("expected copy back for a caller that named no stage")
	}
}

// The composed-prompt bound guards TEMPLATE growth, not caller input, and this drives it that way.
//
// It cannot be reached by caller input at ALL, for any pair of bounds: "never reject what the
// pre-check accepted" needs the composed bound at or above the worst valid composition, and
// "reachable by caller input" needs it below. The two are mutually exclusive by construction.
//
// An earlier version of this test drove a 2500-rune event name and was a FALSE GREEN once the
// input bound moved to 2400: the PRE-check rejected first, both guards returned the same
// BadRequestError, and nothing could tell them apart. Neutralising the composed guard entirely
// broke no test. The messages now differ, and this injects an oversized STAGE — the only input
// that can actually reach the composed check — so deleting the guard fails here.
func TestGenerateEmailCopy_ComposedBoundIsReachable(t *testing.T) {
	// A stage whose template alone blows the composed budget. Restored after the test so the
	// package's own templates are untouched for everything else.
	const oversized = "Oversized Test Stage"
	original, existed := emailstage.Templates[oversized]
	emailstage.Templates[oversized] = emailstage.Template{
		StageName:     oversized,
		Purpose:       "exercise the composed bound",
		Tone:          "neutral",
		UrgencyLevel:  1,
		ContentPrompt: repeatStr("y", 9000),
	}
	t.Cleanup(func() {
		if existed {
			emailstage.Templates[oversized] = original
			return
		}
		delete(emailstage.Templates, oversized)
	})

	repo := newFakeBriefRepo()
	repo.briefs[briefKey("proj-123", "brief-456")] = &model.CampaignBrief{
		ID: "brief-456", ProjectID: "proj-123",
		// Small, so the PRE-check cannot be what rejects this. That distinction is the whole point.
		EventDetails: json.RawMessage(`{"eventName":"KubeCon","location":"Barcelona","dates":"June 17-20, 2026"}`),
	}
	// An LLM client must be configured, or the call fails on service-unavailable BEFORE the size
	// check and the test would pass for the wrong reason. The server is never reached: the bound
	// rejects the request first, which is the claim.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the composed-prompt bound should have rejected this before any LLM call")
	}))
	defer srv.Close()

	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, srv))

	stage := oversized
	_, err := svc.GenerateEmailCopy(context.Background(), &briefs.GenerateEmailCopyPayload{
		ProjectID: "proj-123", BriefID: "brief-456", BearerToken: strPtr("token"), Stage: &stage,
	})
	if err == nil {
		t.Fatal("expected the composed-prompt bound to reject an oversized stage template")
	}
	// 503, not 400, and the TYPE is now what distinguishes the two guards. This branch is
	// unreachable by caller input, so if it fires a service-owned template has outgrown its
	// budget -- a service defect, not a client error. The pre-check still answers 400, so a
	// pre-check rejection can no longer masquerade as coverage here.
	var unavailable *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("want ConnServiceUnavailableError (503) for a service-side overflow, got %T: %v", err, err)
	}
	var bad *briefs.BadRequestError
	if errors.As(err, &bad) {
		t.Errorf("got a 400 -- the PRE-check rejected first, so this does not exercise the composed bound")
	}

	// The MESSAGE must not promise transience. A compiled-in template exceeding a compiled-in
	// bound returns this same 503 to every retry until a corrected deployment ships, so
	// "temporarily unavailable" would send the caller into a retry loop that cannot succeed.
	// 503 carries the retry hint by convention; only the text can withdraw it.
	if strings.Contains(strings.ToLower(unavailable.Message), "temporar") {
		t.Errorf("the message promises transience for a defect only a deployment fixes: %q", unavailable.Message)
	}
	if !strings.Contains(unavailable.Message, "retrying will not help") {
		t.Errorf("the message must tell the caller a retry cannot clear this: %q", unavailable.Message)
	}
}

// The OMIT rule must outrank a stage brief's own REQUIRED marker.
//
// A brief can mark a section required AND name a placeholder in it -- Registration Push's
// "1. HEADLINE: Early Bird Pricing Ends [DEADLINE]" is required and no deadline is ever supplied.
// Without an explicit precedence the model gets two contradictory instructions and the required
// marker is the more emphatic one, so it invents the fact rather than dropping the section.
//
// Asserts on the composed SYSTEM PROMPT, which is what the model actually reads.
func TestComposeEmailCopyPrompt_OmitOutranksRequired(t *testing.T) {
	sys, _ := composeEmailCopyPrompt(emailCopyPromptVars{
		eventName: "KubeCon Europe 2026",
		location:  "Barcelona",
		dates:     "June 17 - June 20",
	})

	if !strings.Contains(sys, "OUTRANKS THE STAGE BRIEF") {
		t.Error("the system prompt does not say the OMIT rule outranks a REQUIRED section")
	}
	// The rule has to name the conflict concretely, or it reads as generic boilerplate beside a
	// brief that is very specific about what it requires.
	if !strings.Contains(sys, "REQUIRED") {
		t.Error("the precedence rule does not mention the REQUIRED marker it overrides")
	}
	if !strings.Contains(sys, "Drop the section") {
		t.Error("the precedence rule does not say what to do with a required section it cannot fill")
	}
}

// The composed bound must clear EVERY stage's floor plus the full caller allowance.
//
// The two bounds have a contract between them: the pre-check accepts up to maxPromptSize runes of
// caller input, so the composed bound must be at least (worst stage floor + maxPromptSize) or a
// caller is told their input is too large by the SECOND check after the first accepted it -- and
// the second answers 503, blaming the service for the caller's perfectly valid request.
//
// A prose "re-measure when the templates change" note did not hold: the numbers went stale three
// times, most recently when a paragraph added to the shared system prompt grew every stage by
// ~130 runes. This computes the floors instead of trusting a comment.
func TestComposedBoundClearsEveryStageFloor(t *testing.T) {
	// The REAL constants, not copies. Copying them meant reverting the production bound to 7600
	// left this test green -- it pinned its own numbers rather than the ones GenerateEmailCopy
	// uses, which is precisely the regression it claims to prevent.
	const inputBound = maxPromptSize
	const composedBound = maxComposedPromptSize

	worst, worstStage := 0, ""
	for _, name := range emailstage.Names() {
		sys, user := composeEmailCopyPrompt(emailCopyPromptVars{stage: name})
		floor := utf8.RuneCountInString(sys) + utf8.RuneCountInString(user)
		if floor > worst {
			worst, worstStage = floor, name
		}
	}

	if worst+inputBound > composedBound {
		t.Errorf("stage %q floors at %d runes; with the %d-rune input allowance the worst valid composition is %d, above the %d composed bound — valid caller input would be refused with a 503",
			worstStage, worst, inputBound, worst+inputBound, composedBound)
	}
}
