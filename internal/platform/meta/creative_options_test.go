// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// TestCreateCampaign_CallToActionReachesEveryCreative pins the button on the creative's
// link_data.call_to_action: LEARN_MORE when omitted or blank (the pre-#550 payload), else the
// upper-cased allow-listed value, on every variant.
func TestCreateCampaign_CallToActionReachesEveryCreative(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "LEARN_MORE"}, {"  ", "LEARN_MORE"}, {"sign_up", "SIGN_UP"}, {" Buy_Tickets ", "BUY_TICKETS"}, {"LEARN_MORE", "LEARN_MORE"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			creativeCap := newBodyCapture()
			srv := metaFieldServer(t, newBodyCapture(), creativeCap)
			defer srv.Close()
			in := metaFieldInput()
			in.CallToAction = tc.in
			in.Variants = []AdVariant{{PrimaryText: "Join us", Headline: "KubeCon"}, {PrimaryText: "Come", Headline: "KubeCon 2"}}
			if _, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			// Every creative body, in order: get() keeps only the last, so drain the channel here.
			var bodies []map[string]any
			for len(creativeCap.ch) > 0 {
				bodies = append(bodies, <-creativeCap.ch)
			}
			if len(bodies) != 2 {
				t.Fatalf("%d creatives sent, want 2", len(bodies))
			}
			for v, body := range bodies {
				spec, _ := body["object_story_spec"].(map[string]any)
				link, _ := spec["link_data"].(map[string]any)
				cta, _ := link["call_to_action"].(map[string]any)
				if cta["type"] != tc.want {
					t.Fatalf("creative %d call_to_action.type = %v, want %s", v+1, cta["type"], tc.want)
				}
				if value, _ := cta["value"].(map[string]any); value["link"] != link["link"] {
					t.Fatalf("creative %d call_to_action.value.link = %v, want the ad link %v", v+1, value["link"], link["link"])
				}
			}
		})
	}
}

// TestCreateCampaign_ConversionEventSetsTheCustomEventType: under conversions the promoted object
// carries the chosen event, PURCHASE when omitted (the pre-#550 payload), next to the pixel.
func TestCreateCampaign_ConversionEventSetsTheCustomEventType(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "PURCHASE"}, {"complete_registration", "COMPLETE_REGISTRATION"}, {" Lead ", "LEAD"}, {"SUBMIT_APPLICATION", "SUBMIT_APPLICATION"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			adsetCap := newBodyCapture()
			srv := metaFieldServer(t, adsetCap, newBodyCapture())
			defer srv.Close()
			in := metaFieldInput()
			in.Objective, in.PixelID, in.ConversionEvent = "conversions", "1234567890", tc.in
			if _, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			po, _ := adsetCap.get()["promoted_object"].(map[string]any)
			if po["custom_event_type"] != tc.want || po["pixel_id"] != "1234567890" {
				t.Fatalf("promoted_object = %v, want custom_event_type %s with the pixel", po, tc.want)
			}
		})
	}
}

// TestCreateCampaign_CreativeOptionRefusalsSendNothing: an unknown button or event, and an event on
// an objective that promotes no pixel, are config-invalid pre-flight refusals with no request sent.
func TestCreateCampaign_CreativeOptionRefusalsSendNothing(t *testing.T) {
	for _, tc := range []struct {
		name, objective, cta, event, want string
	}{
		{"REGISTER_NOW is not a Meta button", "traffic", "REGISTER_NOW", "", "metaConfig.callToAction: \"REGISTER_NOW\" is not one of LEARN_MORE"},
		{"an app-install button", "traffic", "INSTALL_APP", "", "is not one of"},
		{"unknown event", "conversions", "", "ADD_TO_CART", "metaConfig.conversionEvent: \"ADD_TO_CART\" is not one of PURCHASE"},
		{"event on traffic", "traffic", "", "LEAD", "applies only to the conversions objective (objective is \"traffic\")"},
		{"event on the default objective", "", "", "LEAD", "objective is \"traffic\""},
		{"event on awareness", "Awareness", "", "PURCHASE", "objective is \"awareness\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.String())
			}))
			defer srv.Close()
			in := metaFieldInput()
			in.Objective, in.PixelID, in.CallToAction, in.ConversionEvent = tc.objective, "1234567890", tc.cta, tc.event
			res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateCampaign = %+v, %v; want a config-invalid pre-flight refusal containing %q", res, err, tc.want)
			}
		})
	}
}

// TestCreateCampaign_ConversionEventIsRecordedOnlyForACreatedAdSet: the result names the event the
// ad set THIS run created carries (PURCHASE by default), and nothing when the ad set was reused by
// name, whose event this run never set (PR #559 review) — so the persisted result can be trusted by
// the conversions metrics read.
func TestCreateCampaign_ConversionEventIsRecordedOnlyForACreatedAdSet(t *testing.T) {
	for _, tc := range []struct {
		name, event, want string
		objective         string
	}{
		{"chosen event", "complete_registration", "COMPLETE_REGISTRATION", "conversions"},
		{"default event", "", "PURCHASE", "conversions"},
		{"no event off conversions", "", "", "traffic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := metaFieldServer(t, newBodyCapture(), newBodyCapture())
			defer srv.Close()
			in := metaFieldInput()
			in.Objective, in.PixelID, in.ConversionEvent = tc.objective, "1234567890", tc.event
			res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
			if err != nil || res.ConversionEvent != tc.want {
				t.Fatalf("CreateCampaign = %+v, %v; want ConversionEvent %q", res, err, tc.want)
			}
		})
	}

	t.Run("reused ad set", func(t *testing.T) {
		rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body := `{"id":"120100000000012"}`
			switch {
			case req.Method == http.MethodGet && strings.Contains(req.URL.RawQuery, "filtering") && strings.HasSuffix(req.URL.Path, "/campaigns"):
				body = `{"data":[{"id":"120200000000123","status":"PAUSED","objective":"OUTCOME_SALES"}]}`
			case req.Method == http.MethodGet && strings.Contains(req.URL.RawQuery, "filtering") && strings.HasSuffix(req.URL.Path, "/adsets"):
				body = `{"data":[{"id":"120300000000456","status":"PAUSED"}],"paging":{}}`
			case req.Method == http.MethodGet:
				body = `{"name":"x"}`
			case req.Method == http.MethodPost && (strings.HasSuffix(req.URL.Path, "/campaigns") || strings.HasSuffix(req.URL.Path, "/adsets")):
				t.Errorf("unexpected create %s", req.URL.Path)
			case strings.HasSuffix(req.URL.Path, "/adcreatives"):
				body = `{"id":"creative_1"}`
			case strings.HasSuffix(req.URL.Path, "/ads"):
				body = `{"id":"ad_1"}`
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})
		c := NewClient(Credentials{AccessToken: "t"}, AccountConfig{AccountID: "act_1", PageID: "100", CurrencyOffset: 100},
			WithBaseURL("http://meta.test"), WithHTTPClient(&http.Client{Transport: rt}), WithClock(fixedMetaClock()))
		in := metaFieldInput()
		in.Objective, in.PixelID, in.ConversionEvent, in.ReconcileByName = "conversions", "1234567890", "LEAD", true
		res, err := c.CreateCampaign(context.Background(), in)
		if err != nil || res.AdSetID != "120300000000456" || res.ConversionEvent != "" {
			t.Fatalf("CreateCampaign = %+v, %v; want the reused ad set and no recorded event", res, err)
		}
	})
}
