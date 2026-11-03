// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

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
func TestBuildCampaignName_LabelTokens(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		stage, tactic, slotToken string
		want                     string
	}{
		{"unlabelled slot 1: the legacy name", "", "", "", "Events | KubeCon | LinkedIn | Traffic | Prospecting | Static | cncf | MoFU"},
		{"labelled slot 1", "ToFU", "prospecting", "", "Events | KubeCon | LinkedIn | Traffic | Prospecting prospecting | Static | cncf | ToFU"},
		{"labelled slot 2", "BoFU", "retargeting", "#2", "Events | KubeCon | LinkedIn | Traffic | Prospecting retargeting #2 | Static | cncf | BoFU"},
		{"unlabelled slot 2: only the slot token", "", "", "#2", "Events | KubeCon | LinkedIn | Traffic | Prospecting #2 | Static | cncf | MoFU"},
		{"tactic without a stage keeps the legacy funnel", "", "competitor", "", "Events | KubeCon | LinkedIn | Traffic | Prospecting competitor | Static | cncf | MoFU"},
		{"pipe-injected tokens cannot add a segment", "To|FU", " keyword | BoFU ", "#2 | x", "Events | KubeCon | LinkedIn | Traffic | Prospecting keyword - BoFU #2 - x | Static | cncf | To-FU"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := CampaignInput{StageToken: tc.stage, Tactic: tc.tactic, SlotToken: tc.slotToken}
			got := buildCampaignName("KubeCon", "Traffic", "cncf", in)
			if got != tc.want {
				t.Errorf("name = %q, want %q", got, tc.want)
			}
			if n := strings.Count(got, " | "); n != 7 {
				t.Errorf("name %q has %d separators, want 7 (8 segments)", got, n)
			}
		})
	}
}

// A labelled or later-slot campaign name is held to LinkedIn's 255-character name limit like
// every other name, and refused before the FIRST upstream request (the campaign-group lookup).
// The control: the same event and project, unlabelled, compose a name of exactly 255 characters,
// which the check lets through to LinkedIn; the labels alone push it over.
func TestCreateCampaign_OverlongLabelledNameRefusedBeforeAnyRequest(t *testing.T) {
	newInput := func(event string) CampaignInput {
		in := validCampaignInput()
		in.EventName = event
		return in
	}
	overhead := utf8.RuneCountInString(buildCampaignName("", "Traffic", "tlf", CampaignInput{}))
	atTheBound := strings.Repeat("E", maxNameLen-overhead)

	run := func(in CampaignInput) (int32, *CampaignResult, error) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			// The control only needs to prove the create got past the name check to LinkedIn.
			http.Error(w, `{"message":"stop"}`, http.StatusBadRequest)
		}))
		defer srv.Close()
		c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
		res, err := c.CreateCampaign(context.Background(), in)
		return requests.Load(), res, err
	}

	t.Run("control: an unlabelled name at the bound reaches LinkedIn", func(t *testing.T) {
		if got := utf8.RuneCountInString(buildCampaignName(atTheBound, "Traffic", "tlf", CampaignInput{})); got != maxNameLen {
			t.Fatalf("fixture: unlabelled name is %d characters, want %d", got, maxNameLen)
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
		{"labelled slot 2", func(in *CampaignInput) { in.StageToken, in.Tactic, in.SlotToken = "MoFU", "retargeting", "#2" }, maxNameLen + len(" retargeting #2")},
		{"labelled slot 1, tactic only", func(in *CampaignInput) { in.Tactic = "keyword" }, maxNameLen + len(" keyword")},
		{"unlabelled slot 2", func(in *CampaignInput) { in.SlotToken = "#2" }, maxNameLen + len(" #2")},
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
			want := "campaign name is " + strconv.Itoa(tc.wantLen) + " characters, exceeds the 255-character limit; shorten the event name or project"
			if !ok || msg != want {
				t.Errorf("refusal = %q (preflight %v)\nwant      %q", msg, ok, want)
			}
		})
	}
}
