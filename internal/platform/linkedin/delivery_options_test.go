// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Audience Expansion and the Audience Network (#667).

func boolp(b bool) *bool { return &b }

func TestValidateDeliveryOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    DeliveryOptions
		format  string
		refused bool
	}{
		{"none, default format", DeliveryOptions{}, "", false},
		{"both on, single image", DeliveryOptions{boolp(true), boolp(true)}, FormatSingleImage, false},
		{"audience network on, text", DeliveryOptions{nil, boolp(true)}, FormatText, true},
		{"audience network off, text", DeliveryOptions{nil, boolp(false)}, FormatText, false},
		{"expansion on, text", DeliveryOptions{boolp(true), nil}, FormatText, false},
		{"unknown format is left to NormalizeFormat", DeliveryOptions{nil, boolp(true)}, "carousel", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDeliveryOptions(tc.opts, tc.format)
			if got := err != nil; got != tc.refused {
				t.Fatalf("ValidateDeliveryOptions = %v, refused %v want %v", err, got, tc.refused)
			}
			if err != nil && !errors.Is(err, ErrDeliveryOptionInvalid) {
				t.Errorf("err = %v, want ErrDeliveryOptionInvalid", err)
			}
		})
	}
}

// TestDeliveryOptionsApply pins the body: offsiteDeliveryEnabled always (false unless chosen),
// audienceExpansionEnabled only when chosen. The zero value is the body every create has sent.
func TestDeliveryOptionsApply(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts DeliveryOptions
		want map[string]any
	}{
		{"zero value", DeliveryOptions{}, map[string]any{"offsiteDeliveryEnabled": false}},
		{"both on", DeliveryOptions{boolp(true), boolp(true)}, map[string]any{"offsiteDeliveryEnabled": true, "audienceExpansionEnabled": true}},
		{"both off", DeliveryOptions{boolp(false), boolp(false)}, map[string]any{"offsiteDeliveryEnabled": false, "audienceExpansionEnabled": false}},
		{"expansion only", DeliveryOptions{boolp(true), nil}, map[string]any{"offsiteDeliveryEnabled": false, "audienceExpansionEnabled": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{}
			tc.opts.apply(body)
			if !reflect.DeepEqual(body, tc.want) {
				t.Errorf("body = %v, want %v", body, tc.want)
			}
		})
	}
}

// TestCreateCampaign_AudienceNetworkOnTextRefusedBeforeAnyRequest: the client's own check, for a
// caller that skips the dispatcher's.
func TestCreateCampaign_AudienceNetworkOnTextRefusedBeforeAnyRequest(t *testing.T) {
	s := &textAdServer{}
	in := textAdInput()
	in.DeliveryOptions = DeliveryOptions{OffsiteDeliveryEnabled: boolp(true)}
	res, err := newTextAdClient(t, s).CreateCampaign(context.Background(), in)
	if res != nil {
		t.Errorf("a partial result for a refused create: %+v", res)
	}
	if msg, pre := domain.AsPreflightRefusal(err); !pre || !strings.Contains(msg, "Sponsored Content only") {
		t.Fatalf("CreateCampaign = %v, want the pre-flight Audience Network refusal", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 0 {
		t.Errorf("requests sent for a refused create: %v", s.calls)
	}
}

// TestCreateCampaign_DeliveryOptionsReachTheTextCampaign: a text create sends the chosen expansion
// and the Audience Network off.
func TestCreateCampaign_DeliveryOptionsReachTheTextCampaign(t *testing.T) {
	s := &textAdServer{}
	in := textAdInput()
	in.DeliveryOptions = DeliveryOptions{AudienceExpansionEnabled: boolp(true)}
	if _, err := newTextAdClient(t, s).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.campaign["audienceExpansionEnabled"] != true || s.campaign["offsiteDeliveryEnabled"] != false {
		t.Errorf("campaign body = %v, want audienceExpansionEnabled true and offsiteDeliveryEnabled false", s.campaign)
	}
}

// TestCreateCampaign_DeliveryOptionsWarnOnAReusedCampaign: a campaign found by name keeps its
// settings, so a chosen option is reported as not applied rather than silently dropped.
func TestCreateCampaign_DeliveryOptionsWarnOnAReusedCampaign(t *testing.T) {
	first := &textAdServer{}
	ok, err := newTextAdClient(t, first).CreateCampaign(context.Background(), textAdInput())
	if err != nil {
		t.Fatalf("clean create: %v", err)
	}
	for _, tc := range []struct {
		name string
		opts DeliveryOptions
		warn bool
	}{
		{"none chosen", DeliveryOptions{}, false},
		{"expansion chosen", DeliveryOptions{AudienceExpansionEnabled: boolp(true)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			el, _ := json.Marshal(map[string]any{"elements": []map[string]any{{
				"name": ok.CampaignName, "status": "PAUSED", "id": 555, "type": "TEXT_AD",
				"objectiveType": "WEBSITE_CONVERSION", "optimizationTargetType": "NONE",
				"campaignGroup": "urn:li:sponsoredCampaignGroup:100",
			}}, "metadata": map[string]any{}})
			s := &textAdServer{existingCampaign: string(el)}
			in := textAdInput()
			in.DeliveryOptions = tc.opts
			res, err := newTextAdClient(t, s).CreateCampaign(context.Background(), in)
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			if res.CampaignID != "555" {
				t.Fatalf("campaign = %q, want the reused 555", res.CampaignID)
			}
			got := strings.Contains(strings.Join(res.Steps, "\n"), "NOT applied to reused campaign 555; its existing settings")
			if got != tc.warn {
				t.Errorf("delivery-options warning present %v, want %v; steps %v", got, tc.warn, res.Steps)
			}
		})
	}
}

func TestGetCampaignSettings_DeliveryOptions(t *testing.T) {
	for _, tc := range []struct {
		name               string
		extra              string
		expansion, offsite *bool
	}{
		{"not reported", "", nil, nil},
		{"both reported", `,"audienceExpansionEnabled":true,"offsiteDeliveryEnabled":false`, boolp(true), boolp(false)},
		{"audience network only", `,"offsiteDeliveryEnabled":true`, nil, boolp(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newLeverStub(t)
			s.campaignBody = strings.TrimSuffix(fullCampaignJSON, "}") + tc.extra + "}"
			got, err := c.GetCampaignSettings(context.Background(), "777")
			if err != nil {
				t.Fatalf("GetCampaignSettings: %v", err)
			}
			if !reflect.DeepEqual(got.AudienceExpansionEnabled, tc.expansion) || !reflect.DeepEqual(got.OffsiteDeliveryEnabled, tc.offsite) {
				t.Errorf("expansion %v, offsite %v; want %v, %v", got.AudienceExpansionEnabled, got.OffsiteDeliveryEnabled, tc.expansion, tc.offsite)
			}
		})
	}
}
