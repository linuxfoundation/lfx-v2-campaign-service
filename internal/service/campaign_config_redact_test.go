// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

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
	// The userinfo URL is spliced in so the literal is not a credential-shaped fixture for
	// the secret scanners.
	userinfoURL := "https://bob:" + "SECRET_PASSWORD@api.example.org/v1"
	if err := json.Unmarshal([]byte(strings.Replace(`{
		"post_url": "https://events.example.org/reg?access_token=SECRET_QUERY",
		"nested": {
			"image": "https://cdn.example.org/img.png#SECRET_FRAGMENT",
			"list": [
				"USERINFO_URL",
				"see example.org/reset/SECRET_PATH now",
				"www.example.org/r?ticket=SECRET_SCHEMELESS",
				42, 1.5, true, false, null
			],
			"https://keys.example.org/?k=SECRET_KEY": "plain text"
		},
		"budget": 1000,
		"enabled": true,
		"nothing": null
	}`, "USERINFO_URL", userinfoURL, 1)), &config); err != nil {
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
	for _, secret := range []string{"SECRET_QUERY", "SECRET_FRAGMENT", "SECRET_PASSWORD", "bob", "SECRET_PATH", "SECRET_SCHEMELESS", "SECRET_KEY", "/reg", "/reset", "img.png"} {
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
			// Keys are caller-typed too and are redacted like values.
			"https://keys.example.org": "plain text",
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

// Two keys that redact to the same string must both survive: the first in sorted original-key
// order keeps the redacted key, later ones get a deterministic `#n` suffix.
func TestRedactedConfigSnapshot_KeyCollisions(t *testing.T) {
	in := map[string]any{
		"https://a.example/y":          "second",
		"https://a.example/x?t=SECRET": "first",
		"https://a.example#frag":       "zeroth",
		"https://a.example#2":          "taken",
		"other":                        float64(1),
	}
	// Sorted originals: "https://a.example#2", "https://a.example#frag",
	// "https://a.example/x?t=SECRET", "https://a.example/y", "other". Each of the first four
	// redacts to "https://a.example"; the first keeps it and the rest are suffixed in order.
	want := `{"https://a.example":"taken","https://a.example#2":"zeroth","https://a.example#3":"first","https://a.example#4":"second","other":1}`
	for i := 0; i < 20; i++ { // map order is randomized; the output must not be
		if got := string(redactedConfigSnapshot(in)); got != want {
			t.Fatalf("got  %s\nwant %s", got, want)
		}
	}
}

// Links the http-only passes did not recognise reach neither the stored values nor the stored
// keys (PR #267 review): a non-http scheme with an authority, a host-less file URL, userinfo in
// front of a bracketed IPv6 host, and a username carrying RFC 3986 sub-delims.
func TestRedactedConfigSnapshot_NonHTTPAndUserinfoShapes(t *testing.T) {
	// Spliced so the literals are not credential-shaped fixtures for the secret scanners.
	bracketUserinfo := "https://bob:" + "pw@[2001:db8::1]/reset/SECRET_PATH?token=SECRET_QUERY"
	bangUserinfo := "admin!:" + "pw@events.example/reset/SECRET_TOKEN"
	for _, tc := range []struct{ name, in, want string }{
		{"ftp ipv6", "ftp://[2001:db8::1]/reset/SECRET?token=SECRET", "ftp://[2001:db8::1]"},
		{"file url", "file:///private/RESET_SECRET", ""},
		{"ftp in prose", "get ftp://files.example.org/reset/SECRET now", "get ftp://files.example.org now"},
		{"userinfo before bracketed host", bracketUserinfo, ""},
		{"sub-delim username", bangUserinfo, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// As a value.
			got := string(redactedConfigSnapshot(map[string]any{"v": tc.in}))
			if strings.Contains(got, "SECRET") || strings.Contains(got, "pw") {
				t.Errorf("value: snapshot still carries the secret: %s", got)
			}
			wantJSON, _ := json.Marshal(map[string]any{"v": tc.want})
			if got != string(wantJSON) {
				t.Errorf("value: got %s, want %s", got, wantJSON)
			}
			// As an object key.
			got = string(redactedConfigSnapshot(map[string]any{tc.in: 1}))
			if strings.Contains(got, "SECRET") || strings.Contains(got, "pw") {
				t.Errorf("key: snapshot still carries the secret: %s", got)
			}
			wantJSON, _ = json.Marshal(map[string]any{tc.want: 1})
			if got != string(wantJSON) {
				t.Errorf("key: got %s, want %s", got, wantJSON)
			}
		})
	}
}

// Many keys redacting to one base must not cost quadratic work (PR #267 review): 20,000 keys
// sharing a host finish quickly and every value survives under its own suffix.
func TestRedactedConfigSnapshot_ManyCollisionsAreLinear(t *testing.T) {
	const n = 20000
	in := make(map[string]any, n)
	for i := 0; i < n; i++ {
		in["https://a.example/"+strconv.Itoa(i)] = float64(i)
	}
	start := time.Now()
	raw := redactedConfigSnapshot(in)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("redacting %d colliding keys took %v", n, d)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("got %d keys, want %d: a colliding value was merged or dropped", len(got), n)
	}
	if _, ok := got["https://a.example#"+strconv.Itoa(n)]; !ok {
		t.Errorf("the last colliding key should carry suffix #%d", n)
	}
}
