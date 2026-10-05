// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

func newConfigEditService(snapshot json.RawMessage) (*BriefService, *campaignEditRepo) {
	camps := &campaignEditRepo{cur: &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Version: 5,
		CampaignName: "old", Status: "created", ConfigSnapshot: snapshot,
	}}
	s := &BriefService{
		briefs:    &fakeBriefRepo{briefs: map[string]*model.CampaignBrief{}},
		campaigns: camps, jobs: newFakeJobRepo(),
		orch: NewOrchestrator(camps, newFakeJobRepo(), nil),
	}
	return s, camps
}

// TestBriefService_UpdateCampaign_RedactsCallerConfig pins the update path's generic
// redaction. `config` is Goa `Any` and no dispatch adapter sees it here, so before this every
// string a caller sent reached the UNENCRYPTED config_snapshot verbatim — including a token in
// a link's query, fragment, userinfo or path.
func TestBriefService_UpdateCampaign_RedactsCallerConfig(t *testing.T) {
	s, camps := newConfigEditService(json.RawMessage(`{"before":true}`))
	// Decoded the way Goa decodes an Any body: plain encoding/json into interface{}.
	var config any
	if err := json.Unmarshal([]byte(`{
		"post_url": "https://events.example.org/reg?access_token=SECRET_QUERY",
		"nested": {
			"image": "https://cdn.example.org/img.png#SECRET_FRAGMENT",
			"list": [
				"https://bob:SECRET_PASSWORD@api.example.org/v1",
				"see example.org/reset/SECRET_PATH now",
				"www.example.org/r?ticket=SECRET_SCHEMELESS",
				42, 1.5, true, false, null
			],
			"https://keys.example.org/?k=key_is_not_redacted": "plain text"
		},
		"budget": 1000,
		"enabled": true,
		"nothing": null
	}`), &config); err != nil {
		t.Fatal(err)
	}
	v := "5"
	if _, err := s.UpdateCampaign(context.Background(), &briefs.UpdateCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &v,
		Campaign: &briefs.CampaignUpdateInput{CampaignName: "old", Status: "created", Config: config},
	}); err != nil {
		t.Fatalf("UpdateCampaign: %v", err)
	}
	if camps.got == nil {
		t.Fatal("ReplaceCampaign was not called")
	}
	stored := string(camps.got.ConfigSnapshot)
	for _, secret := range []string{"SECRET_QUERY", "SECRET_FRAGMENT", "SECRET_PASSWORD", "bob", "SECRET_PATH", "SECRET_SCHEMELESS", "/reg", "/reset", "img.png"} {
		if strings.Contains(stored, secret) {
			t.Errorf("config_snapshot still contains %q: %s", secret, stored)
		}
	}
	// The index document is built from the persisted row; it must not carry the secrets either.
	for _, p := range camps.indexPayloads {
		if bytes.Contains(p, []byte("SECRET_")) {
			t.Errorf("index payload carries a secret: %s", p)
		}
	}

	dec := json.NewDecoder(strings.NewReader(stored))
	dec.UseNumber()
	var got map[string]any
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("stored snapshot is not a JSON object: %v (%s)", err, stored)
	}
	want := map[string]any{
		"post_url": "https://events.example.org",
		"nested": map[string]any{
			"image": "https://cdn.example.org",
			"list": []any{
				"",
				"see example.org now",
				"www.example.org",
				json.Number("42"), json.Number("1.5"), true, false, nil,
			},
			// Keys are structure, not content: left as written.
			"https://keys.example.org/?k=key_is_not_redacted": "plain text",
		},
		"budget":  json.Number("1000"),
		"enabled": true,
		"nothing": nil,
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Errorf("stored snapshot\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

// A nil config leaves the stored snapshot exactly as it was — the omitted-config edit must not
// wipe it, redacted or otherwise.
func TestBriefService_UpdateCampaign_NilConfigKeepsSnapshot(t *testing.T) {
	orig := json.RawMessage(`{"post_url":"https://events.example.org"}`)
	s, camps := newConfigEditService(orig)
	v := "5"
	if _, err := s.UpdateCampaign(context.Background(), &briefs.UpdateCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &v,
		Campaign: &briefs.CampaignUpdateInput{CampaignName: "renamed", Status: "created"},
	}); err != nil {
		t.Fatalf("UpdateCampaign: %v", err)
	}
	if !bytes.Equal(camps.got.ConfigSnapshot, orig) {
		t.Errorf("snapshot = %s, want unchanged %s", camps.got.ConfigSnapshot, orig)
	}
}

// The contract still accepts any JSON value, not only an object: a bare string or array is
// redacted the same way rather than refused.
func TestRedactedConfigSnapshot_NonObjectValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"bare string", "https://a.example/x?token=S", `"https://a.example"`},
		{"array", []any{"https://u:p@a.example", float64(3)}, `["",3]`},
		{"number", float64(7), `7`},
		{"bool", true, `true`},
		{"typed struct", struct {
			URL string `json:"url"`
		}{"https://a.example/reset/S"}, `{"url":"https://a.example"}`},
	} {
		if got := string(redactedConfigSnapshot(tc.in)); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
	if got := redactedConfigSnapshot(nil); got != nil {
		t.Errorf("nil: got %s, want nil", got)
	}
}
