// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// The composer's label tokens (multi-campaign cs-3). An unlabelled slot-1 input composes the
// legacy name; the tactic and slot token go INSIDE the Targeting segment and the stage token
// replaces the Funnel segment, so every name keeps the data pipeline's exact 8 segments. Tokens
// are pipe-stripped like every other segment, so none can inject a field.
func TestBuildRedditCampaignName_LabelTokens(t *testing.T) {
	base := CampaignInput{EventName: "KubeCon", Project: "cncf"}
	for _, tc := range []struct {
		name                     string
		stage, tactic, slotToken string
		want                     string
	}{
		{"unlabelled slot 1: the legacy name", "", "", "", "Events | KubeCon | NA | Traffic | Intent | Social | cncf | ToFU"},
		{"labelled slot 1", "MoFU", "retargeting", "", "Events | KubeCon | NA | Traffic | Intent retargeting | Social | cncf | MoFU"},
		{"labelled slot 2", "BoFU", "keyword", "#2", "Events | KubeCon | NA | Traffic | Intent keyword #2 | Social | cncf | BoFU"},
		{"unlabelled slot 2: only the slot token", "", "", "#2", "Events | KubeCon | NA | Traffic | Intent #2 | Social | cncf | ToFU"},
		{"tactic without a stage keeps the legacy funnel", "", "lookalike", "", "Events | KubeCon | NA | Traffic | Intent lookalike | Social | cncf | ToFU"},
		{"pipe-injected tokens cannot add a segment", "Mo|FU", " keyword | BoFU ", "#2 | x", "Events | KubeCon | NA | Traffic | Intent keyword - BoFU #2 - x | Social | cncf | Mo-FU"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.StageToken, in.Tactic, in.SlotToken = tc.stage, tc.tactic, tc.slotToken
			got := buildRedditCampaignName(in, "traffic", "NA")
			if got != tc.want {
				t.Errorf("name = %q, want %q", got, tc.want)
			}
			if n := strings.Count(got, " | "); n != 7 {
				t.Errorf("name %q has %d separators, want 7 (8 segments)", got, n)
			}
		})
	}
}

// A labelled or later-slot name is held to Reddit's 200-rune name limit like every other name,
// and refused before the FIRST upstream request (no token exchange, no account GET). The
// control: the same event and project, unlabelled, compose a name of exactly 200 runes, which the
// check lets through; the labels alone push it over.
func TestCreateCampaign_OverlongLabelledNameRefusedBeforeAnyRequest(t *testing.T) {
	newInput := func() CampaignInput {
		in := validRedditInput()
		in.Project = strings.Repeat("p", maxProjectRunes)
		// validRedditInput's legacy name with a 120-rune event and 40-rune project is 212 runes
		// (TestCreateCampaign_ComposedNameRefusedBeforeAnyRequest), so a 108-rune event is 200.
		in.EventName = strings.Repeat("E", maxEventNameRunes-12)
		return in
	}
	if n := len([]rune(buildRedditCampaignName(newInput(), "traffic", "NA"))); n != redditMaxNameRunes {
		t.Fatalf("control: the unlabelled name is %d runes, want exactly %d", n, redditMaxNameRunes)
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*CampaignInput)
		wantMsg string
	}{
		{"labelled slot 2", func(in *CampaignInput) { in.StageToken, in.Tactic, in.SlotToken = "MoFU", "content-promo", "#2" },
			"composed campaign name is too long: 217 characters (max 200)"},
		{"unlabelled slot 2", func(in *CampaignInput) { in.SlotToken = "#2" },
			"composed campaign name is too long: 203 characters (max 200)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
			}))
			defer tokenSrv.Close()
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected", http.StatusNotFound)
			}))
			defer apiSrv.Close()
			c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

			in := newInput()
			tc.mutate(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil || res != nil {
				t.Fatalf("CreateCampaign = %+v, %v; want a nil result and a refusal", res, err)
			}
			if n := requests.Load(); n != 0 {
				t.Errorf("%d upstream request(s) before the refusal; want 0", n)
			}
			msg, ok := domain.AsPreflightRefusal(err)
			if !ok || msg != tc.wantMsg {
				t.Errorf("refusal = %q (preflight %v), want %q", msg, ok, tc.wantMsg)
			}
		})
	}
}
