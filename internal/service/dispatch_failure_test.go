// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// secretCause is text no job result may ever contain: what a decrypted credential blob, an
// upstream response body or a request URL could put into an error chain.
const secretCause = "decoded {\"RefreshToken\":\"rt-SECRET\"} from https://api.example/v1?access_token=SECRET-QUERY"

// statusErr stands in for a platform client's HTTP error type. Its Error() deliberately
// carries the body, the way Meta's does when the response has no Graph envelope.
type statusErr struct {
	status int
	body   string
}

func (e *statusErr) Error() string       { return fmt.Sprintf("POST /campaigns -> %d: %s", e.status, e.body) }
func (e *statusErr) UpstreamStatus() int { return e.status }

// preCreateWrap mirrors the dispatchers' notCreated: the orchestrator releases the claim on it.
type preCreateWrap struct{ err error }

func (e *preCreateWrap) Error() string          { return e.err.Error() }
func (e *preCreateWrap) Unwrap() error          { return e.err }
func (e *preCreateWrap) NoUpstreamCreate() bool { return true }

func TestDispatchFailureMessage_Classes(t *testing.T) {
	refusal := domain.PreflightRefusalf("reddit requires a conversion pixel id and none is configured: add it to this project's Reddit connection")
	p := model.ProviderRedditAds
	for _, tc := range []struct {
		name      string
		err       error
		preCreate bool
		want      string
	}{
		{
			name:      "service-authored refusal is verbatim",
			err:       &preCreateWrap{fmt.Errorf("reddit campaign creation failed before any upstream create: %w", refusal)},
			preCreate: true,
			want:      refusal.Error(),
		},
		{
			name:      "no connection",
			err:       &preCreateWrap{fmt.Errorf("no reddit-ads connection for %s: %w: %w", secretCause, domain.ErrNotFound, domain.ErrConnectionAbsent)},
			preCreate: true,
			want:      "no reddit-ads connection is configured for this project; nothing was sent",
		},
		{
			name:      "LF system connection missing outranks no connection",
			err:       &preCreateWrap{fmt.Errorf("%w: %w: %w", domain.ErrSystemConnectionMissing, domain.ErrNotFound, domain.ErrConnectionAbsent)},
			preCreate: true,
			want:      "the LF system reddit-ads connection is not installed; nothing was sent",
		},
		{
			name:      "connection unusable carries only the reason token",
			err:       &preCreateWrap{fmt.Errorf("%w: %w: %s", domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete, secretCause)},
			preCreate: true,
			want:      "this project's reddit-ads connection cannot be used as configured (reason: credentials_incomplete); nothing was sent",
		},
		{
			name:      "connection unusable outranks a refusal in the same chain",
			err:       &preCreateWrap{fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, domain.WrapPreflightRefusal(errors.New(secretCause)))},
			preCreate: true,
			want:      "this project's reddit-ads connection cannot be used as configured; nothing was sent",
		},
		{
			name:      "LF system connection unusable",
			err:       &preCreateWrap{fmt.Errorf("%w: %w: %w", domain.ErrSystemConnectionNotUsable, domain.ErrConnectionNotUsable, domain.ErrConnectionInactive)},
			preCreate: true,
			want:      "the LF system reddit-ads connection is not usable as configured (reason: connection_inactive); nothing was sent",
		},
		{
			name:      "decrypt fault",
			err:       &preCreateWrap{fmt.Errorf("%w: %s", domain.ErrCredentialDecryptionFailed, secretCause)},
			preCreate: true,
			want:      "the stored reddit-ads credentials could not be decrypted; nothing was sent",
		},
		{
			name:      "service defect",
			err:       &preCreateWrap{fmt.Errorf("%w: %w: %s", domain.ErrServiceDefect, domain.ErrConnectionNotUsable, secretCause)},
			preCreate: true,
			want:      "reddit-ads campaign creation was stopped before anything was sent by a defect in this service; the connection is not at fault",
		},
		{
			name:      "token refresh failed",
			err:       &preCreateWrap{domain.MarkTokenRefreshFailure(errors.New(secretCause))},
			preCreate: true,
			want:      "reddit-ads token refresh failed, so nothing was sent; the connection may need re-authorizing",
		},
		{
			// A refresh this call abandoned says nothing about the credential.
			name:      "cancelled token refresh is not a credential verdict",
			err:       &preCreateWrap{domain.MarkTokenRefreshFailure(fmt.Errorf("token: %w", context.Canceled))},
			preCreate: true,
			want:      genericDispatchFailure,
		},
		{
			name:      "timed-out token refresh is not a credential verdict",
			err:       &preCreateWrap{domain.MarkTokenRefreshFailure(fmt.Errorf("token: %w", context.DeadlineExceeded))},
			preCreate: true,
			want:      genericDispatchFailure,
		},
		{
			name:      "definite 4xx before anything was created: status only",
			err:       &preCreateWrap{fmt.Errorf("create: %w", &statusErr{status: 400, body: secretCause})},
			preCreate: true,
			want:      "reddit-ads refused the request (HTTP 400) before anything was created; nothing was created",
		},
		{
			name:      "5xx before anything was created is retryable, not a refusal",
			err:       &preCreateWrap{fmt.Errorf("geo resolve: %w", &statusErr{status: 503, body: secretCause})},
			preCreate: true,
			want:      "reddit-ads answered HTTP 503 before anything was created (a retry may succeed); nothing was created",
		},
		{
			name:      "429 before anything was created is retryable, not a refusal",
			err:       &preCreateWrap{fmt.Errorf("lookup: %w", &statusErr{status: 429, body: secretCause})},
			preCreate: true,
			want:      "reddit-ads answered HTTP 429 before anything was created (a retry may succeed); nothing was created",
		},
		{
			name:      "unconfirmed with a status: status only",
			err:       fmt.Errorf("reddit campaign creation UNCONFIRMED: %w", &statusErr{status: 502, body: secretCause}),
			preCreate: false,
			want:      "reddit-ads campaign creation could not be confirmed (the platform answered HTTP 502); something may have been created — verify on the platform before retrying",
		},
		{
			// The retained verdict outranks everything in the chain, a refusal included: the
			// dispatcher said something may exist, and "nothing was sent" would be a lie.
			name:      "unconfirmed never reports a refusal",
			err:       fmt.Errorf("partial: %w", refusal),
			preCreate: false,
			want:      "reddit-ads campaign creation could not be confirmed; something may have been created — verify on the platform before retrying",
		},
		{
			name:      "unconfirmed never reports a connection class",
			err:       fmt.Errorf("partial: %w", domain.ErrConnectionNotUsable),
			preCreate: false,
			want:      "reddit-ads campaign creation could not be confirmed; something may have been created — verify on the platform before retrying",
		},
		{
			name:      "unclassified keeps the generic string",
			err:       &preCreateWrap{errors.New(secretCause)},
			preCreate: true,
			want:      genericDispatchFailure,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DispatchFailureMessage(p, tc.err, tc.preCreate)
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
			if strings.Contains(got, "SECRET") || strings.Contains(got, "api.example") {
				t.Errorf("message %q leaks the error's text", got)
			}
		})
	}
}

// TestPreflightRefusalf_NeverRendersAnErrorArgument pins the constructor's guard: an error
// passed as a format argument is the one shape that can smuggle an upstream body into an
// operator-safe message, so it is replaced, never rendered.
func TestPreflightRefusalf_NeverRendersAnErrorArgument(t *testing.T) {
	err := domain.PreflightRefusalf("budget %d refused: %v", 5, errors.New(secretCause))
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("an error argument was rendered into the refusal: %q", err.Error())
	}
	if want := "budget 5 refused: (detail withheld)"; err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}

// refusalDispatcher refuses before sending anything, the way every client's pre-flight does.
type refusalDispatcher struct{ msg string }

func (d refusalDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, &preCreateWrap{fmt.Errorf("reddit campaign creation failed before any upstream create: %w", domain.PreflightRefusalf("%s", d.msg))}
}

// unconfirmedRefusalDispatcher returns a partial ALONGSIDE an error whose chain carries both a
// refusal and an upstream status with a secret body: the shape that must stay UNCONFIRMED.
type unconfirmedRefusalDispatcher struct{}

func (unconfirmedRefusalDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{Status: "unconfirmed", Result: json.RawMessage(`{"campaignName":"n"}`), CampaignName: "n"},
		fmt.Errorf("UNCONFIRMED: %w / %w", domain.PreflightRefusalf("must not be shown"), &statusErr{status: 504, body: secretCause})
}

func decodeOnlyResult(t *testing.T, j *model.CampaignJob) platformResult {
	t.Helper()
	var got []platformResult
	if err := json.Unmarshal(j.Result, &got); err != nil {
		t.Fatalf("unmarshal job result: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d platform results, want 1", len(got))
	}
	return got[0]
}

// TestOrchestrator_PreflightRefusalReachesTheJobResult is issue #208 end to end through the
// orchestrator: the refusal's sentence is the job's `error`, verbatim, and the outcome is what
// it was before — job failed, claim released.
func TestOrchestrator_PreflightRefusalReachesTheJobResult(t *testing.T) {
	const msg = "reddit requires a conversion pixel id and none is configured: add it to this project's Reddit connection (Reddit Ads → Events Manager lists the account's pixel)"
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderRedditAds: refusalDispatcher{msg: msg},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderRedditAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	res := decodeOnlyResult(t, j)
	if res.OK || res.Error != msg {
		t.Errorf("result = %+v, want ok=false error=%q", res, msg)
	}
	camps.mu.Lock()
	defer camps.mu.Unlock()
	if _, ok := camps.existing[slotKey("b1", model.ProviderRedditAds, model.VariantDefault)]; ok {
		t.Error("a pre-flight refusal must still release the claim")
	}
}

// TestOrchestrator_UnconfirmedFailureStaysUnconfirmed: a possibly-created failure keeps every
// part of today's outcome — claim retained, row persisted with its unconfirmed status, job
// failed — and its message says UNCONFIRMED with the status only, never the refusal it happens
// to carry and never the body.
func TestOrchestrator_UnconfirmedFailureStaysUnconfirmed(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderMetaAds: unconfirmedRefusalDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderMetaAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	res := decodeOnlyResult(t, j)
	want := "meta-ads campaign creation could not be confirmed (the platform answered HTTP 504); something may have been created — verify on the platform before retrying"
	if res.OK || res.Error != want {
		t.Errorf("result error = %q, want %q", res.Error, want)
	}
	if strings.Contains(res.Error, "must not be shown") || strings.Contains(res.Error, "SECRET") {
		t.Errorf("result error %q leaks a refusal or the body on the UNCONFIRMED path", res.Error)
	}
	camps.mu.Lock()
	defer camps.mu.Unlock()
	row, ok := camps.existing[slotKey("b1", model.ProviderMetaAds, model.VariantDefault)]
	if !ok {
		t.Fatal("claim was released for a failure that may have created something")
	}
	if row.Status != model.CampaignStatusUnconfirmed {
		t.Errorf("retained row status = %q, want %q (unchanged by #208)", row.Status, model.CampaignStatusUnconfirmed)
	}
}

// TestCallerValue pins how a caller-supplied scalar may appear in an operator-safe refusal.
func TestCallerValue(t *testing.T) {
	long := strings.Repeat("a", 100)
	for _, tc := range []struct{ in, want string }{
		{"2099-01-04", `"2099-01-04"`},
		{"urn:li:image:C4E10AQ", `"urn:li:image:C4E10AQ"`},
		{"https://x.com/a?token=SECRET-208", "(value withheld)"},
		{"x?token=SECRET", "(value withheld)"},
		{"frag#SECRET", "(value withheld)"},
		{"user@SECRET", "(value withheld)"},
		{"line1\nSECRET", "(value withheld)"},
		{"tab\tSECRET", "(value withheld)"},
		{long, `"` + strings.Repeat("a", 64) + `…"`},
		{"bell\x07", "\"bell\uFFFD\""},
		{"", `""`},
	} {
		if got := domain.CallerValue(tc.in); got != tc.want {
			t.Errorf("CallerValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
