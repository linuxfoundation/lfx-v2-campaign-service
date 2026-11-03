// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// The composer's label tokens (multi-campaign cs-3). An unlabelled slot-1 input composes the
// legacy name; the tactic and slot token go INSIDE the Targeting segment and the stage token
// replaces the Funnel segment, so every name keeps the data pipeline's exact 8 segments. Tokens
// are trimmed and pipe-stripped like every other segment, so none can inject a field.
func TestBuildCampaignName_LabelTokens(t *testing.T) {
	base := CampaignInput{EventName: "KubeCon", Project: "cncf", Objective: "traffic"}
	for _, tc := range []struct {
		name                     string
		stage, tactic, slotToken string
		want                     string
	}{
		{"unlabelled slot 1: the legacy name", "", "", "", "Events | KubeCon | NA | Traffic | Intent | Social | cncf | MoFU"},
		{"labelled slot 1", "ToFU", "prospecting", "", "Events | KubeCon | NA | Traffic | Intent prospecting | Social | cncf | ToFU"},
		{"labelled slot 2", "BoFU", "retargeting", "#2", "Events | KubeCon | NA | Traffic | Intent retargeting #2 | Social | cncf | BoFU"},
		{"unlabelled slot 2: only the slot token", "", "", "#2", "Events | KubeCon | NA | Traffic | Intent #2 | Social | cncf | MoFU"},
		{"tactic without a stage keeps the legacy funnel", "", "competitor", "", "Events | KubeCon | NA | Traffic | Intent competitor | Social | cncf | MoFU"},
		{"pipe-injected tokens cannot add a segment", "To|FU", " keyword | BoFU ", "#2 | x", "Events | KubeCon | NA | Traffic | Intent keyword - BoFU #2 - x | Social | cncf | To-FU"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.StageToken, in.Tactic, in.SlotToken = tc.stage, tc.tactic, tc.slotToken
			got := buildCampaignName(in, []string{"US"})
			if got != tc.want {
				t.Errorf("name = %q, want %q", got, tc.want)
			}
			if n := strings.Count(got, " | "); n != 7 {
				t.Errorf("name %q has %d separators, want 7 (8 segments)", got, n)
			}
		})
	}
}

// A labelled or later-slot campaign's composed name is bounded at maxLabelledCampaignNameRunes
// and refused before the FIRST upstream request (the account GET). The bound applies to such
// names only: the same event, unlabelled, composes a legacy name past it and still proceeds to
// Meta, exactly as before cs-3.
func TestCreateCampaign_OverlongLabelledNameRefusedBeforeAnyRequest(t *testing.T) {
	newInput := func(event string) CampaignInput {
		return CampaignInput{
			EventName: event, Project: "tlf", Objective: "traffic",
			RegistrationURL: "https://x.example.org/e", GeoTargets: []string{"US"},
			Budget: 10, StartDate: "2026-08-01", EndDate: "2026-08-31",
			Variants: []AdVariant{{PrimaryText: "p", Headline: "h"}},
		}
	}
	overhead := utf8.RuneCountInString(buildCampaignName(newInput(""), []string{"US"}))
	// An event that makes the unlabelled name exactly the bound: any label pushes it over.
	atTheBound := strings.Repeat("E", maxLabelledCampaignNameRunes-overhead)

	run := func(in CampaignInput) (int32, *CampaignResult, error) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			// Answer the account GET, then refuse whatever follows: the control only needs to
			// prove the create got past the name check to Meta.
			if requests.Load() > 1 {
				http.Error(w, `{"error":{"message":"stop","code":100}}`, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"x","account_status":1,"currency":"USD"}`)
		}))
		defer srv.Close()
		c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100}, WithBaseURL(srv.URL), WithClock(fixedMetaClock()))
		res, err := c.CreateCampaign(context.Background(), in)
		return requests.Load(), res, err
	}

	t.Run("control: an unlabelled name past the bound still reaches Meta", func(t *testing.T) {
		in := newInput(atTheBound + "EEEE")
		if n, _, err := run(in); n == 0 {
			t.Fatalf("no request was sent (err %v): the bound must not apply to an unlabelled name", err)
		}
	})
	t.Run("control: a labelled name at the bound passes the check", func(t *testing.T) {
		in := newInput(atTheBound[:len(atTheBound)-len(" content-promo #2")])
		in.StageToken, in.Tactic, in.SlotToken = "ToFU", "content-promo", "#2"
		if got := utf8.RuneCountInString(buildCampaignName(in, []string{"US"})); got != maxLabelledCampaignNameRunes {
			t.Fatalf("fixture: labelled name is %d runes, want %d", got, maxLabelledCampaignNameRunes)
		}
		if n, _, err := run(in); n == 0 {
			t.Fatalf("no request was sent (err %v) for a labelled name at the bound", err)
		}
	})
	for _, tc := range []struct {
		name    string
		mutate  func(*CampaignInput)
		wantLen int
	}{
		{"labelled slot 2", func(in *CampaignInput) { in.StageToken, in.Tactic, in.SlotToken = "MoFU", "retargeting", "#2" }, maxLabelledCampaignNameRunes + len(" retargeting #2")},
		{"labelled slot 1, tactic only", func(in *CampaignInput) { in.Tactic = "keyword" }, maxLabelledCampaignNameRunes + len(" keyword")},
		{"unlabelled slot 2", func(in *CampaignInput) { in.SlotToken = "#2" }, maxLabelledCampaignNameRunes + len(" #2")},
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
			want := "composed campaign name is too long: " + strconv.Itoa(tc.wantLen) + " characters (max 255 for a labelled or additional campaign); shorten the event name"
			if !ok || msg != want {
				t.Errorf("refusal = %q (preflight %v)\nwant      %q", msg, ok, want)
			}
		})
	}
}
