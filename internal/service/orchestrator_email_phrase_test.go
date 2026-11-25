// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// phraseSearcherRecording records what SearchEmailsAnyPhrase was handed.
type phraseSearcherRecording struct {
	gotDeadline time.Time
	hadDeadline bool
	gotPhrases  []string
	result      []model.MarketingEmail
}

func (d *phraseSearcherRecording) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (d *phraseSearcherRecording) SearchEmails(context.Context, string, model.Provider, string) ([]model.MarketingEmail, error) {
	return []model.MarketingEmail{}, nil
}

func (d *phraseSearcherRecording) SearchEmailsAnyPhrase(ctx context.Context, _ string, _ model.Provider, phrases []string) ([]model.MarketingEmail, error) {
	d.gotDeadline, d.hadDeadline = ctx.Deadline()
	d.gotPhrases = phrases
	return d.result, nil
}

// The phrase walk reads as many pages as a filtered search, so it needs the same bound: without
// it a slow portal pins the request goroutine for the whole walk.
func TestOrchestrator_SearchEmailsAnyPhraseBoundsThePlatformCall(t *testing.T) {
	disp := &phraseSearcherRecording{result: []model.MarketingEmail{{ID: "1"}}}
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
		model.ProviderHubSpot: disp,
	})

	beforeCall := time.Now()
	emails, err := orch.SearchEmailsAnyPhrase(context.Background(), "proj-1", model.ProviderHubSpot, []string{"KubeCon", "CloudNativeCon"})
	afterCall := time.Now()
	if err != nil {
		t.Fatalf("SearchEmailsAnyPhrase: %v", err)
	}
	if len(emails) != 1 {
		t.Fatalf("emails = %+v, want the searcher's one row", emails)
	}
	if len(disp.gotPhrases) != 2 || disp.gotPhrases[0] != "KubeCon" {
		t.Errorf("phrases not forwarded verbatim: %v", disp.gotPhrases)
	}
	if !disp.hadDeadline {
		t.Fatal("the phrase searcher received a context with NO deadline")
	}
	if disp.gotDeadline.Before(beforeCall.Add(emailSearchCallTimeout)) || disp.gotDeadline.After(afterCall.Add(emailSearchCallTimeout)) {
		t.Errorf("deadline %v is not call time + emailSearchCallTimeout (%v)", disp.gotDeadline, emailSearchCallTimeout)
	}
}

// A dispatcher that can search but not by phrase is the contract's "not supported", not a crash
// and not a silent unfiltered listing.
func TestOrchestrator_SearchEmailsAnyPhraseUnsupported(t *testing.T) {
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
		model.ProviderHubSpot: &emailSearcherRecordingDeadline{},
	})
	_, err := orch.SearchEmailsAnyPhrase(context.Background(), "proj-1", model.ProviderHubSpot, []string{"KubeCon"})
	if !errors.Is(err, ErrEmailSearchUnsupported) {
		t.Fatalf("err = %v, want ErrEmailSearchUnsupported", err)
	}
}

// The (nil, nil) guard SearchEmails has: an empty picker must never be reported by accident.
func TestOrchestrator_SearchEmailsAnyPhraseRejectsNilResult(t *testing.T) {
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
		model.ProviderHubSpot: &phraseSearcherRecording{result: nil},
	})
	if _, err := orch.SearchEmailsAnyPhrase(context.Background(), "proj-1", model.ProviderHubSpot, []string{"KubeCon"}); err == nil {
		t.Fatal("a nil result with no error must be refused")
	}
}
