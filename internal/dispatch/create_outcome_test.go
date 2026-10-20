// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// TestWithCreateOutcome_TagsWithoutReplacing pins the property the whole design rests on: the
// tag ADDS a question to the error and changes nothing else in the chain, so every errors.Is/As
// a caller already makes through a google-ads dispatch failure keeps resolving — including the
// *apiError's HTTP status, which service.DispatchFailureMessage falls back to when it does not
// recognise the token.
func TestWithCreateOutcome_TagsWithoutReplacing(t *testing.T) {
	sentinel := errors.New("campaigns:mutate refused")
	inner := fmt.Errorf("google-ads dispatch: %w", fmt.Errorf("budget 111 created: %w", sentinel))

	tagged := withCreateOutcome(inner, domain.CreateOutcomeBudgetCreatedCampaignRefused)

	got, ok := domain.UpstreamCreateOutcome(tagged)
	if !ok || got != domain.CreateOutcomeBudgetCreatedCampaignRefused {
		t.Errorf("UpstreamCreateOutcome = (%q, %v), want (%q, true)", got, ok, domain.CreateOutcomeBudgetCreatedCampaignRefused)
	}
	if !errors.Is(tagged, sentinel) {
		t.Error("tagging broke errors.Is to the underlying cause")
	}
	if tagged.Error() != inner.Error() {
		t.Errorf("tagging changed the error text: %q, want %q", tagged.Error(), inner.Error())
	}
}

// TestWithCreateOutcome_NoTagWhenNothingToSay covers both declining paths. An unclassified
// failure must be indistinguishable from one that was never offered to the classifier,
// otherwise a reader finds a carrier holding nothing and has to special-case it.
func TestWithCreateOutcome_NoTagWhenNothingToSay(t *testing.T) {
	if got := withCreateOutcome(nil, domain.CreateOutcomeUnconfirmed); got != nil {
		t.Errorf("withCreateOutcome(nil, ...) = %v, want nil", got)
	}

	plain := errors.New("x")
	got := withCreateOutcome(plain, "")
	if got != plain { //nolint:errorlint // identity is the assertion: no wrapper was added.
		t.Errorf("an empty outcome must return the error unchanged, got %#v", got)
	}
	if _, ok := domain.UpstreamCreateOutcome(got); ok {
		t.Error("an empty outcome must leave no carrier for a reader to find")
	}
}

// TestWithCreateOutcome_SurvivesContextCancellation pins the ctx.Err() arms (every create path
// has one between the budget and the campaign): those wrap a context error, and the tag must
// not hide it from the cancellation checks DispatchFailureMessage and the orchestrator make.
func TestWithCreateOutcome_SurvivesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tagged := withCreateOutcome(
		fmt.Errorf("google-ads dispatch: %w", fmt.Errorf("aborted after budget 111 created: %w", ctx.Err())),
		domain.CreateOutcomeBudgetCreatedCampaignUnconfirmed,
	)

	if !errors.Is(tagged, context.Canceled) {
		t.Error("tagging hid context.Canceled from the chain")
	}
}
