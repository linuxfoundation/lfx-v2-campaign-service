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

// config_snapshot is persisted UNENCRYPTED and indexed. microsoftConfig has no URL field of
// its own; the one caller free-text field reduced in the snapshot is TimeZone (forwarded
// unvalidated). Keyword text is kept VERBATIM, matching googleAdsSnapshotConfig: the prose
// redactor would rewrite legitimate keywords. These tests pin both halves, and that the
// platform still receives every value exactly as the caller wrote it.

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
			_, _ = io.WriteString(w, `{"KeywordIds":[701,702,703,704],"PartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return []microsoft.Option{microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL)}, cap
}

// msSnapKeywords look link-ish. The snapshot stores each through sanitizeSnapshotText (a redacted
// record), while the platform receives every one exactly as written.
var msSnapKeywords = []microsoftKeywordConfig{
	{Text: "k8s.io/docs tutorial", MatchType: "Exact"},
	{Text: "node.js/express", MatchType: "Phrase"},
	{Text: "kubernetes.io", MatchType: "Broad"},
	{Text: "c++ jobs", MatchType: "Broad"},
}

const (
	msSnapTimeZone        = "https://tz.example.net/z/TZPATH?token=TZSECRET#TZFRAG"
	msSnapRegistrationURL = "https://events.example/kc/REGPATH?ticket=REGSECRET"
)

func msSnapshotConfig(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"microsoftConfig": map[string]any{
		"budget":   50,
		"timeZone": msSnapTimeZone,
		"keywords": msSnapKeywords,
	}})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return raw
}

func TestMicrosoft_ConfigSnapshotScrubsTimeZoneKeepsKeywordsVerbatim(t *testing.T) {
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

	// Neither the time zone's path/query/fragment nor the brief's registration URL reaches
	// either persisted blob. Result carries no caller URL by construction (names, ids, Steps,
	// a service-composed deep link); asserting it keeps a future field that echoes one from
	// landing silently.
	for _, leak := range []string{"TZSECRET", "TZFRAG", "TZPATH", "REGSECRET", "REGPATH", "ticket"} {
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
	if len(snap.Keywords) != len(msSnapKeywords) {
		t.Fatalf("snapshot keywords = %+v, want %+v", snap.Keywords, msSnapKeywords)
	}
	for i, kw := range msSnapKeywords {
		want := microsoftKeywordConfig{Text: sanitizeSnapshotText(kw.Text), MatchType: kw.MatchType}
		if snap.Keywords[i] != want {
			t.Errorf("snapshot keyword[%d] = %+v, want the redacted %+v", i, snap.Keywords[i], want)
		}
	}
	if snap.TimeZone != "https://tz.example.net" {
		t.Errorf("snapshot timeZone = %q, want it reduced to scheme+host", snap.TimeZone)
	}
	if snap.Budget != 50 {
		t.Errorf("snapshot budget = %v, want 50 (non-text fields are kept verbatim)", snap.Budget)
	}

	// The platform still receives every value exactly as written: only the STORED copy is
	// scrubbed. Bodies are decoded rather than substring-matched, since encoding/json escapes
	// '+' nowhere but does escape '&', '<' and '>'.
	cap.mu.Lock()
	defer cap.mu.Unlock()
	var kwBody struct {
		Keywords []struct{ Text string }
	}
	if err := json.Unmarshal([]byte(cap.keywordBody), &kwBody); err != nil {
		t.Fatalf("decode POST /Keywords body %q: %v", cap.keywordBody, err)
	}
	if len(kwBody.Keywords) != len(msSnapKeywords) {
		t.Fatalf("POST /Keywords sent %d keywords, want %d: %s", len(kwBody.Keywords), len(msSnapKeywords), cap.keywordBody)
	}
	for i, kw := range msSnapKeywords {
		if kwBody.Keywords[i].Text != kw.Text {
			t.Errorf("POST /Keywords keyword[%d] = %q, want %q", i, kwBody.Keywords[i].Text, kw.Text)
		}
	}
	if !strings.Contains(cap.campaignBody, msSnapTimeZone) {
		t.Errorf("POST /Campaigns body lost the full timeZone %q: %s", msSnapTimeZone, cap.campaignBody)
	}
	if !strings.Contains(cap.adBody, "ticket=REGSECRET") || !strings.Contains(cap.adBody, "REGPATH") {
		t.Errorf("POST /Ads FinalUrls lost the registration URL's query/path: %s", cap.adBody)
	}
}

// TestMicrosoftSnapshotConfig_DoesNotMutateDispatchConfig pins that the snapshot is a copy:
// the config Dispatch hands the client must keep the full timeZone, and the keyword slice
// must come through unchanged on both sides.
func TestMicrosoftSnapshotConfig_DoesNotMutateDispatchConfig(t *testing.T) {
	cfg := microsoftConfig{
		Budget:     10,
		TimeZone:   msSnapTimeZone,
		Keywords:   append([]microsoftKeywordConfig(nil), msSnapKeywords...),
		CpcBid:     1.5,
		GeoTargets: []string{"US"},
	}
	snap := microsoftSnapshotConfig(cfg)
	if cfg.TimeZone != msSnapTimeZone {
		t.Fatalf("microsoftSnapshotConfig mutated the dispatch timeZone: %q", cfg.TimeZone)
	}
	for i, kw := range msSnapKeywords {
		redacted := microsoftKeywordConfig{Text: sanitizeSnapshotText(kw.Text), MatchType: kw.MatchType}
		if cfg.Keywords[i] != kw || snap.Keywords[i] != redacted {
			t.Errorf("keyword[%d]: dispatch %+v (want %+v), snapshot %+v (want %+v)", i, cfg.Keywords[i], kw, snap.Keywords[i], redacted)
		}
	}
	if snap.TimeZone != "https://tz.example.net" {
		t.Errorf("snapshot timeZone = %q, want it reduced to scheme+host", snap.TimeZone)
	}
	if snap.CpcBid != cfg.CpcBid || len(snap.GeoTargets) != 1 || snap.GeoTargets[0] != "US" {
		t.Errorf("non-text fields must be kept verbatim: %+v", snap)
	}

	// A real enum value passes through unchanged; an empty one stays empty.
	if got := microsoftSnapshotConfig(microsoftConfig{TimeZone: "PacificTimeUSCanadaTijuana"}).TimeZone; got != "PacificTimeUSCanadaTijuana" {
		t.Errorf("enum timeZone rewritten to %q", got)
	}
	empty := microsoftSnapshotConfig(microsoftConfig{Budget: 5})
	if empty.Keywords != nil || empty.TimeZone != "" {
		t.Errorf("zero-valued fields must stay zero, got %+v", empty)
	}
}

// A keyword is caller text: any link in it — including one whose secret is in the PATH — is
// redacted before the unencrypted snapshot, and the dispatch config is not mutated.
func TestMicrosoftSnapshotConfig_KeywordLinksRedacted(t *testing.T) {
	in := []string{
		"https://example.test/reset/SECRET?token=VALUE",
		"buy https://shop.example/p#SECRETFRAG now",
		"a.example/r?token=SECRET",
		"example.org/reset/SECRET",
		"www.example.org/reset/SECRET",
		"a.example/r/s/t/SECRET",
		"host.example:8443/reset/SECRET",
		"10.0.0.5/reset/SECRET",
		"bob:SECRET@a.example",
		"kubernetes.io",
		"c++ jobs",
	}
	kws := make([]microsoftKeywordConfig, len(in))
	for i, s := range in {
		kws[i] = microsoftKeywordConfig{Text: s, MatchType: "Exact"}
	}
	orig := append([]string(nil), in...)
	cfg := microsoftConfig{Keywords: kws}
	snap := microsoftSnapshotConfig(cfg)
	if len(snap.Keywords) != len(orig) {
		t.Fatalf("snapshot has %d keywords, want %d", len(snap.Keywords), len(orig))
	}
	for i, k := range snap.Keywords {
		if strings.Contains(k.Text, "SECRET") || strings.Contains(k.Text, "VALUE") {
			t.Errorf("snapshot keyword for %q = %q still carries the secret", orig[i], k.Text)
		}
		if k.Text != sanitizeSnapshotText(orig[i]) {
			t.Errorf("snapshot keyword for %q = %q, want sanitizeSnapshotText's %q", orig[i], k.Text, sanitizeSnapshotText(orig[i]))
		}
		if cfg.Keywords[i].Text != orig[i] {
			t.Errorf("dispatch keyword %d mutated: %q, want %q", i, cfg.Keywords[i].Text, orig[i])
		}
	}
	for _, plain := range []string{"kubernetes.io", "c++ jobs"} {
		found := false
		for _, k := range snap.Keywords {
			found = found || k.Text == plain
		}
		if !found {
			t.Errorf("plain keyword %q was not stored as written", plain)
		}
	}
	if &cfg.Keywords[0] == &snap.Keywords[0] {
		t.Error("snapshot keywords alias the dispatch slice")
	}
}
