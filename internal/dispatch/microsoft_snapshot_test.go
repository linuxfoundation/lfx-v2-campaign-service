// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// config_snapshot is persisted UNENCRYPTED and indexed, so the Microsoft adapter must not
// copy a caller's link — query, fragment or path — into it. microsoftConfig has no URL field
// of its own; the free-text fields a link can ride in are Keywords[].Text and TimeZone (the
// latter is forwarded unvalidated). These tests pin that both are scrubbed in the snapshot
// while the platform still receives every value exactly as the caller wrote it.

// msSnapshotCapture records the raw bodies the fake Microsoft API received.
type msSnapshotCapture struct {
	mu           sync.Mutex
	campaignBody string
	keywordBody  string
	adBody       string
}

func msSnapshotServers(t *testing.T) ([]microsoft.Option, *msSnapshotCapture) {
	t.Helper()
	cap := &msSnapshotCapture{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case strings.HasSuffix(p, "/Ads/QueryByAdGroupId"):
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case strings.HasSuffix(p, "/Campaigns"):
			cap.mu.Lock()
			cap.campaignBody = string(body)
			cap.mu.Unlock()
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/AdGroups"):
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Ads"):
			cap.mu.Lock()
			cap.adBody = string(body)
			cap.mu.Unlock()
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Keywords"):
			cap.mu.Lock()
			cap.keywordBody = string(body)
			cap.mu.Unlock()
			_, _ = io.WriteString(w, `{"KeywordIds":[701,702,703],"PartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return []microsoft.Option{microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL)}, cap
}

const (
	msSnapKeywordSchemeful  = "https://events.example.org/reg/KWPATH?access_token=KWSECRET1#KWFRAG1"
	msSnapKeywordSchemeless = "events.example.org/reg?sig=KWSECRET2#KWFRAG2"
	msSnapKeywordPlain      = "kubernetes training"
	msSnapTimeZone          = "https://tz.example.net/z/TZPATH?token=TZSECRET#TZFRAG"
	msSnapRegistrationURL   = "https://events.example/kc/REGPATH?ticket=REGSECRET"
)

func msSnapshotConfig(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"microsoftConfig": map[string]any{
		"budget":   50,
		"timeZone": msSnapTimeZone,
		"keywords": []map[string]string{
			{"text": msSnapKeywordSchemeful, "matchType": "Exact"},
			{"text": msSnapKeywordSchemeless, "matchType": "Phrase"},
			{"text": msSnapKeywordPlain, "matchType": "Broad"},
		},
	}})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return raw
}

func TestMicrosoft_ConfigSnapshotStripsURLsFromFreeTextFields(t *testing.T) {
	opts, cap := msSnapshotServers(t)
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
	brief := testBrief()
	brief.EventDetails = json.RawMessage(`{"eventName":"KubeCon NA 2026","registrationUrl":"` + msSnapRegistrationURL + `","project":"cncf"}`)

	camp, err := d.Dispatch(context.Background(), brief, model.ProviderMicrosoftAds, msSnapshotConfig(t))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(camp.ConfigSnapshot) == 0 {
		t.Fatal("ConfigSnapshot is empty; nothing was persisted to assert on")
	}

	// Nothing secret-bearing — query, fragment or path — survives into either persisted blob.
	// Result carries no caller URL by construction (names, ids, Steps, a service-composed deep
	// link); asserting it here keeps a future field that echoes one from landing silently.
	for _, leak := range []string{
		"KWSECRET1", "KWSECRET2", "KWFRAG1", "KWFRAG2", "KWPATH", "access_token", "sig=",
		"TZSECRET", "TZFRAG", "TZPATH", "REGSECRET", "REGPATH", "ticket",
	} {
		if strings.Contains(string(camp.ConfigSnapshot), leak) {
			t.Errorf("config_snapshot carries %q: %s", leak, camp.ConfigSnapshot)
		}
		if strings.Contains(string(camp.Result), leak) {
			t.Errorf("result blob carries %q: %s", leak, camp.Result)
		}
	}

	var snap microsoftConfig
	if err := json.Unmarshal(camp.ConfigSnapshot, &snap); err != nil {
		t.Fatalf("config_snapshot must be valid JSON: %v", err)
	}
	wantKW := []microsoftKeywordConfig{
		{Text: "https://events.example.org", MatchType: "Exact"},
		{Text: "events.example.org", MatchType: "Phrase"},
		{Text: msSnapKeywordPlain, MatchType: "Broad"}, // ordinary keyword text is untouched
	}
	if len(snap.Keywords) != len(wantKW) {
		t.Fatalf("snapshot keywords = %+v, want %+v", snap.Keywords, wantKW)
	}
	for i := range wantKW {
		if snap.Keywords[i] != wantKW[i] {
			t.Errorf("snapshot keyword[%d] = %+v, want %+v", i, snap.Keywords[i], wantKW[i])
		}
	}
	if snap.TimeZone != "https://tz.example.net" {
		t.Errorf("snapshot timeZone = %q, want it reduced to scheme+host", snap.TimeZone)
	}
	if snap.Budget != 50 {
		t.Errorf("snapshot budget = %v, want 50 (non-text fields are kept verbatim)", snap.Budget)
	}

	// The platform still receives every value exactly as written: only the STORED copy is
	// scrubbed. encoding/json escapes '&', '<', '>' but none appear in these values.
	cap.mu.Lock()
	defer cap.mu.Unlock()
	for _, want := range []string{msSnapKeywordSchemeful, msSnapKeywordSchemeless, msSnapKeywordPlain} {
		if !strings.Contains(cap.keywordBody, want) {
			t.Errorf("POST /Keywords body lost the full keyword %q: %s", want, cap.keywordBody)
		}
	}
	if !strings.Contains(cap.campaignBody, msSnapTimeZone) {
		t.Errorf("POST /Campaigns body lost the full timeZone %q: %s", msSnapTimeZone, cap.campaignBody)
	}
	if !strings.Contains(cap.adBody, "ticket=REGSECRET") || !strings.Contains(cap.adBody, "REGPATH") {
		t.Errorf("POST /Ads FinalUrls lost the registration URL's query/path: %s", cap.adBody)
	}
}

// TestMicrosoftSnapshotConfig_DoesNotMutateDispatchConfig pins the copy: Keywords shares its
// backing array with the config Dispatch hands the client, so rewriting it in place would
// strip the URL from what Microsoft receives on any path that reads cfg after the snapshot.
func TestMicrosoftSnapshotConfig_DoesNotMutateDispatchConfig(t *testing.T) {
	cfg := microsoftConfig{
		Budget:   10,
		TimeZone: msSnapTimeZone,
		Keywords: []microsoftKeywordConfig{
			{Text: msSnapKeywordSchemeful, MatchType: "Exact"},
			{Text: msSnapKeywordSchemeless, MatchType: "Phrase"},
		},
		CpcBid:     1.5,
		GeoTargets: []string{"US"},
	}
	snap := microsoftSnapshotConfig(cfg)
	if cfg.TimeZone != msSnapTimeZone ||
		cfg.Keywords[0].Text != msSnapKeywordSchemeful || cfg.Keywords[1].Text != msSnapKeywordSchemeless {
		t.Fatalf("microsoftSnapshotConfig mutated the dispatch config: %+v", cfg)
	}
	if snap.Keywords[0].Text == cfg.Keywords[0].Text {
		t.Errorf("snapshot keyword was not sanitized: %q", snap.Keywords[0].Text)
	}
	if snap.CpcBid != cfg.CpcBid || len(snap.GeoTargets) != 1 || snap.GeoTargets[0] != "US" {
		t.Errorf("non-text fields must be kept verbatim: %+v", snap)
	}

	// An omitted keywords field stays nil (not an empty non-nil slice), and an empty timeZone
	// stays empty.
	empty := microsoftSnapshotConfig(microsoftConfig{Budget: 5})
	if empty.Keywords != nil || empty.TimeZone != "" {
		t.Errorf("zero-valued text fields must stay zero, got %+v", empty)
	}
}
