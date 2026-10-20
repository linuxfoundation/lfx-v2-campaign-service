// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// outcomeErr is a stand-in for the dispatcher's carrier: the token is readable, the wrapped
// text is not reportable. The body text is deliberately hostile so every assertion below
// doubles as a check that no arm echoes the chain (#208).
type outcomeErr struct {
	outcome string
}

func (e *outcomeErr) Error() string {
	return "google-ads POST /campaigns:mutate -> 400 SECRET must not be shown"
}
func (e *outcomeErr) UpstreamCreateOutcome() string { return e.outcome }

// statusOutcomeErr carries both a token and a status, the shape a real google-ads partial
// failure now has.
type statusOutcomeErr struct {
	outcomeErr
	status int
}

func (e *statusOutcomeErr) UpstreamStatus() int { return e.status }

// TestDispatchFailureMessage_OutcomeArms pins one message per create-outcome token.
//
// The five sentences exist because they are not interchangeable: "nothing new was created"
// and "the budget was created and the campaign was not" are opposite instructions to the
// operator, and before the carrier existed both rendered as the same UNCONFIRMED sentence.
func TestDispatchFailureMessage_OutcomeArms(t *testing.T) {
	cases := []struct {
		outcome string
		// wantAll are substrings the message must carry; they pin the CLAIM, not the
		// phrasing.
		wantAll []string
		// wantNone are claims this outcome must not make.
		wantNone []string
	}{
		{
			outcome:  domain.CreateOutcomeBudgetNameTaken,
			wantAll:  []string{"nothing new was created", "change history"},
			wantNone: []string{"something may have been created", "could not be confirmed"},
		},
		{
			outcome:  domain.CreateOutcomeCampaignNameTaken,
			wantAll:  []string{"already exists", "budget for this attempt WAS created"},
			wantNone: []string{"nothing new was created"},
		},
		{
			outcome:  domain.CreateOutcomeBudgetCreatedCampaignRefused,
			wantAll:  []string{"no campaign exists", "accrues no spend"},
			wantNone: []string{"could not be confirmed", "may have been created"},
		},
		{
			outcome:  domain.CreateOutcomeBudgetCreatedCampaignUnconfirmed,
			wantAll:  []string{"created the campaign budget", "could not be confirmed"},
			wantNone: []string{"no campaign exists", "nothing new was created"},
		},
		{
			outcome: domain.CreateOutcomeCampaignCreatedSetupIncomplete,
			// "created the campaign but", not "created the campaign": the latter is a
			// substring of the budget arm's "created the campaign budget", so it would stay
			// green if this arm ever regressed to budget-only wording. Now that both
			// sentences share the "could not confirm" hedge, the campaign's existence is the
			// ONLY thing separating them, and it is what has to be pinned.
			wantAll: []string{"created the campaign but", "could not confirm its setup finished"},
			// The campaign's EXISTENCE is proven by its id; the state of its setup is not.
			// The cascade's last mutate can answer 2xx with a response this client cannot
			// read, past which that step did commit — so asserting the setup is unfinished
			// would be this token overclaiming exactly as the two the review round caught.
			wantNone: []string{"nothing new was created", "did not finish setting it up", "created the campaign budget"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.outcome, func(t *testing.T) {
			got := DispatchFailureMessage(model.ProviderGoogleAds, &outcomeErr{outcome: tc.outcome}, false)

			for _, want := range tc.wantAll {
				if !strings.Contains(got, want) {
					t.Errorf("message %q is missing %q", got, want)
				}
			}
			for _, never := range tc.wantNone {
				if strings.Contains(got, never) {
					t.Errorf("message %q makes a claim this outcome contradicts: %q", got, never)
				}
			}
			if !strings.HasPrefix(got, string(model.ProviderGoogleAds)) {
				t.Errorf("message %q does not name the provider", got)
			}
			// #208: the carrier's own text must never reach the operator.
			if strings.Contains(got, "SECRET") || strings.Contains(got, "/campaigns:mutate") {
				t.Errorf("message %q echoes the error chain", got)
			}
		})
	}
}

// TestDispatchFailureMessage_UnknownOutcomeFallsBackToTheStatusWording is the safety property
// that lets an outcome arm be a plain switch: a token this service does not recognise can only
// leave the reader on its pre-existing sentence, never put words in the operator's mouth.
func TestDispatchFailureMessage_UnknownOutcomeFallsBackToTheStatusWording(t *testing.T) {
	err := &statusOutcomeErr{outcomeErr: outcomeErr{outcome: "something_a_later_version_invented"}, status: 503}

	got := DispatchFailureMessage(model.ProviderGoogleAds, err, false)
	want := "google-ads campaign creation could not be confirmed (the platform answered HTTP 503); something may have been created — verify on the platform before retrying"
	if got != want {
		t.Errorf("message = %q, want the unchanged status wording %q", got, want)
	}
}

// TestDispatchFailureMessage_UnclassifiedOutcomeIsUnchanged pins that CreateOutcomeUnconfirmed
// and a missing carrier produce the SAME message — the one this branch produced before the
// carrier existed. It is the regression guard for every provider that does not classify.
func TestDispatchFailureMessage_UnclassifiedOutcomeIsUnchanged(t *testing.T) {
	plain := fmt.Errorf("google-ads dispatch: %w", &outcomeErr{outcome: ""})
	tagged := &outcomeErr{outcome: domain.CreateOutcomeUnconfirmed}

	want := "google-ads campaign creation could not be confirmed; something may have been created — verify on the platform before retrying"
	for name, err := range map[string]error{"no carrier": plain, "unconfirmed token": tagged} {
		if got := DispatchFailureMessage(model.ProviderGoogleAds, err, false); got != want {
			t.Errorf("%s: message = %q, want %q", name, got, want)
		}
	}
}

// TestDispatchFailureMessage_OutcomeDoesNotReachThePreCreateBranch pins the boundary the
// carrier must not cross: preCreate is decided by the orchestrator from whether a partial came
// back, and a classified POST-request outcome has no business answering a question about a
// create that never happened.
func TestDispatchFailureMessage_OutcomeDoesNotReachThePreCreateBranch(t *testing.T) {
	got := DispatchFailureMessage(model.ProviderGoogleAds, &outcomeErr{outcome: domain.CreateOutcomeBudgetNameTaken}, true)

	if strings.Contains(got, "change history") || strings.Contains(got, "nothing new was created") {
		t.Errorf("pre-create message %q rendered a post-request outcome arm", got)
	}
}
