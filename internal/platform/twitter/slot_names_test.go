// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// The composer's label tokens (multi-campaign cs-4). An unlabelled slot-1 input composes the
// legacy name; the tactic and slot token go INSIDE the Targeting segment and the stage token
// replaces the Funnel segment, so every name keeps the data pipeline's exact 8 segments. Tokens
// are trimmed and pipe-stripped like every other segment, so none can inject a field.
func TestBuildTwitterCampaignName_LabelTokens(t *testing.T) {
	const head = "Events | KubeCon | Global | Awareness | "
	for _, tc := range []struct {
		name                     string
		stage, tactic, slotToken string
		want                     string
	}{
		{"unlabelled slot 1: the legacy name", "", "", "", head + "Prospecting | Promoted Post | cncf | MoFU"},
		{"labelled slot 1", "ToFU", "prospecting", "", head + "Prospecting prospecting | Promoted Post | cncf | ToFU"},
		{"labelled slot 2", "BoFU", "retargeting", "#2", head + "Prospecting retargeting #2 | Promoted Post | cncf | BoFU"},
		{"unlabelled slot 2: only the slot token", "", "", "#2", head + "Prospecting #2 | Promoted Post | cncf | MoFU"},
		{"tactic without a stage keeps the legacy funnel", "", "lookalike", "", head + "Prospecting lookalike | Promoted Post | cncf | MoFU"},
		{"pipe-injected tokens cannot add a segment", "To|FU", " keyword | BoFU ", "#2 | x", head + "Prospecting keyword - BoFU #2 - x | Promoted Post | cncf | To-FU"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := CampaignInput{EventName: "KubeCon", Project: "cncf", StageToken: tc.stage, Tactic: tc.tactic, SlotToken: tc.slotToken}
			got := buildTwitterCampaignName(in)
			if got != tc.want {
				t.Errorf("name = %q, want %q", got, tc.want)
			}
			if n := strings.Count(got, " | "); n != 7 {
				t.Errorf("name %q has %d separators, want 7 (8 segments)", got, n)
			}
		})
	}
}

// A labelled or later-slot campaign name is held to X's 255-rune entity-name limit like every
// other name, and refused before the FIRST upstream request (the account verify). The control:
// the same event and project, unlabelled, compose a name of exactly 255 runes, which the check
// lets through to X; the labels alone push it over.
func TestCreateCampaign_OverlongLabelledNameRefusedBeforeAnyRequest(t *testing.T) {
	newInput := func(event string) CampaignInput {
		return CampaignInput{
			EventName: event, Project: strings.Repeat("p", 60), BudgetUsd: 500,
			StartDate: "2099-03-01", EndDate: "2099-03-10", TweetID: "1234567890",
			RegistrationURL: "https://events.lf.org/kubecon",
		}
	}
	overhead := utf8.RuneCountInString(buildTwitterCampaignName(newInput("")))
	atTheBound := strings.Repeat("E", maxEntityNameLen-overhead)
	if utf8.RuneCountInString(atTheBound) > maxEventNameLen {
		t.Fatalf("fixture: a %d-rune event is over the event-name bound", utf8.RuneCountInString(atTheBound))
	}

	run := func(in CampaignInput) (int32, *CampaignResult, error) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			// The control only needs to prove the create got past the name check to X.
			http.Error(w, `{"errors":[{"message":"stop"}]}`, http.StatusBadRequest)
		}))
		defer srv.Close()
		c := NewClient(
			Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
			AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
			WithBaseURL(srv.URL), WithWriteDelay(0),
		)
		c.nonceFn = func() string { return "n" }
		c.timeFn = staticTime
		res, err := c.CreateCampaign(context.Background(), in)
		return requests.Load(), res, err
	}

	t.Run("control: an unlabelled name at the bound reaches X", func(t *testing.T) {
		if got := utf8.RuneCountInString(buildTwitterCampaignName(newInput(atTheBound))); got != maxEntityNameLen {
			t.Fatalf("fixture: unlabelled name is %d runes, want %d", got, maxEntityNameLen)
		}
		if n, _, err := run(newInput(atTheBound)); n == 0 {
			t.Fatalf("no request was sent (err %v) for an unlabelled name at the bound", err)
		}
	})
	for _, tc := range []struct {
		name    string
		mutate  func(*CampaignInput)
		wantLen int
	}{
		{"labelled slot 2", func(in *CampaignInput) { in.StageToken, in.Tactic, in.SlotToken = "MoFU", "retargeting", "#2" }, maxEntityNameLen + len(" retargeting #2")},
		{"labelled slot 1, tactic only", func(in *CampaignInput) { in.Tactic = "keyword" }, maxEntityNameLen + len(" keyword")},
		{"unlabelled slot 2", func(in *CampaignInput) { in.SlotToken = "#2" }, maxEntityNameLen + len(" #2")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newInput(atTheBound)
			tc.mutate(&in)
			n, res, err := run(in)
			if err == nil || res != nil {
				t.Fatalf("CreateCampaign = %+v, %v; want a nil result and a refusal", res, err)
			}
			if n != 0 {
				t.Errorf("%d upstream request(s) before the refusal; want 0", n)
			}
			msg, ok := domain.AsPreflightRefusal(err)
			want := "invalid campaign name: composed name is " + strconv.Itoa(tc.wantLen) + " characters, exceeds X's 255-character limit"
			if !ok || msg != want {
				t.Errorf("refusal = %q (preflight %v)\nwant      %q", msg, ok, want)
			}
		})
	}
}
