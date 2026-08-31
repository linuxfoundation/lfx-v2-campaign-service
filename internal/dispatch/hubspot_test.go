// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

const goodHubSpotCreds = `{"PrivateAppToken":"pat-123"}`

func activeHubSpotConn(creds string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderHubSpot,
		AccountID:            "8112310",
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"portal_id": "8112310"},
		Status:               model.StatusActive,
	}
}

// fakeAudienceReader is an in-memory audienceReader for the dispatcher tests.
type fakeAudienceReader struct {
	auds []*model.CampaignAudience
	err  error
}

func (f fakeAudienceReader) ListAudiences(context.Context, string, string) ([]*model.CampaignAudience, error) {
	return f.auds, f.err
}

// builtHubSpotAudience returns a newest-first list with one BUILT HubSpot audience.
func builtHubSpotAudience(masterList string, suppression []string) []*model.CampaignAudience {
	raw, _ := json.Marshal(suppression)
	return []*model.CampaignAudience{{
		ID: "aud-1", Platform: model.ProviderHubSpot, Status: model.AudienceBuilt,
		PlatformMasterListID: masterList, SuppressionListIDs: raw,
	}}
}

// hubspotServer fakes the HubSpot API for the clone + set-send-list flow. It records the
// send-list payload so a test can assert the master/suppression ids reached the wire.
// hubspotRec captures what the fake server saw. Every field is written by the HANDLER goroutine
// and read by the TEST goroutine, so all access is mutex-guarded: httptest.Server.Close only
// synchronizes at the deferred Close, which runs AFTER the assertions (same guard as
// meta_test.go).
type hubspotRec struct {
	mu           sync.Mutex
	sendListBody map[string]any
	sawClone     bool
	sawSendList  bool
	taggedHTML   string
	subjectSet   string
	bodyHTMLSet  string
	draftHTML    string
	// extraWidget makes the draft report TWO rich-text widgets, the shape applyEmailContent
	// refuses to rewrite. Set before Dispatch; never mutated concurrently with a read.
	extraWidget bool
	// emptyExtraWidget adds a second rich-text widget with an EMPTY body. The client USED TO omit
	// such widgets from the map it returned, so a guard counting only populated widgets saw 1 and
	// rewrote the populated body — the ambiguity the single-widget guard exists to refuse. Empty
	// rich-text widgets are now returned like any other, and this fixture pins that they count.
	emptyExtraWidget bool
	// onlyEmptyWidget makes the draft's SINGLE rich-text widget empty -- the most unambiguous
	// shape there is, and the one an operator most expects the generated body to fill.
	onlyEmptyWidget bool
	// imageWidget adds a header IMAGE module beside the rich-text block -- the ordinary template
	// shape. It has a body object but no `html` key, so counting object-bodied modules reported
	// two widgets and the body write silently no-opped.
	imageWidget bool
}

func (r *hubspotRec) markClone() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sawClone = true
}

func (r *hubspotRec) markSendList(body map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sawSendList = true
	r.sendListBody = body
}

// markSubject records a PATCH that set the draft's subject (LFXV2-2775 content apply).
func (r *hubspotRec) markSubject(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subjectSet = v
}

// markBody records the html written to the draft's single rich-text widget.
func (r *hubspotRec) markBody(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodyHTMLSet = v
	r.draftHTML = v
}

// setDraft records html written by a path that is not the content apply (the UTM tagger).
func (r *hubspotRec) setDraft(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draftHTML = v
}

// currentBody is the draft's html as it stands, seeded with the template's own body.
func (r *hubspotRec) currentBody() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draftHTML == "" {
		return `<a href="https://events.lfx.dev/reg">Register</a>`
	}
	return r.draftHTML
}

func (r *hubspotRec) snapshotContent() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.subjectSet, r.bodyHTMLSet
}

func (r *hubspotRec) markTagged(raw string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.taggedHTML = raw
}

// SawClone / SawSendList / SendListBody / TaggedHTML read the captures under the lock.
func (r *hubspotRec) SawClone() bool    { r.mu.Lock(); defer r.mu.Unlock(); return r.sawClone }
func (r *hubspotRec) SawSendList() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.sawSendList }
func (r *hubspotRec) SendListBody() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sendListBody
}
func (r *hubspotRec) TaggedHTML() string { r.mu.Lock(); defer r.mu.Unlock(); return r.taggedHTML }

// extractWidgetHTML pulls the single widget's html out of a content PATCH payload.
func extractWidgetHTML(body map[string]any) string {
	content, _ := body["content"].(map[string]any)
	widgets, _ := content["widgets"].(map[string]any)
	for _, w := range widgets {
		wm, _ := w.(map[string]any)
		bm, _ := wm["body"].(map[string]any)
		if h, ok := bm["html"].(string); ok {
			return h
		}
	}
	return ""
}

func hubspotServer(t *testing.T) (*httptest.Server, *hubspotRec) {
	t.Helper()
	rec := &hubspotRec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == hubSpotTokenInfoPath:
			// The provenance lookup Dispatch makes before it creates anything: the portal
			// the TOKEN authenticates against, which is what gets recorded in Result.
			_, _ = io.WriteString(w, `{"hubId":8112310}`)
		case r.Method == http.MethodPost && r.URL.Path == "/marketing/v3/emails/clone":
			rec.markClone()
			_, _ = io.WriteString(w, `{"id":"999","name":"KubeCon NA 2026 — brief-1","state":"DRAFT"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/marketing/v3/emails/") && strings.HasSuffix(r.URL.Path, "/draft"):
			// STATEFUL: returns whatever was last written, so a reader sees the effect of an
			// earlier write. A stub that always replayed the template made write ORDER
			// unobservable — and order is the whole claim of the content-vs-tagging test.
			html := rec.currentBody()
			body1 := html
			if rec.onlyEmptyWidget {
				body1 = "   "
			}
			widgets := map[string]any{"module_1": map[string]any{"body": map[string]any{"html": body1}}}
			// A template with a SECOND rich-text widget, when the test asks for one. There is no
			// safe way to pick which of two the generated body replaces, so `applyEmailContent`
			// must decline rather than guess -- see its `len(widgets) != 1` guard.
			if rec.imageWidget {
				widgets["module_hdr"] = map[string]any{"body": map[string]any{"src": "https://img.example/logo.png", "alt": "logo"}}
			}
			if rec.emptyExtraWidget {
				widgets["module_2"] = map[string]any{"body": map[string]any{"html": "   "}}
			}
			if rec.extraWidget {
				widgets["module_2"] = map[string]any{"body": map[string]any{"html": "<p>second block</p>"}}
			}
			payload, _ := json.Marshal(map[string]any{
				"content": map[string]any{"widgets": widgets},
			})
			_, _ = w.Write(payload)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/marketing/v3/emails/") && strings.HasSuffix(r.URL.Path, "/draft"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			// The send-list PATCH and the UTM PATCH hit the same path; tell them apart by
			// which key the payload carries rather than by call order.
			switch {
			case body["content"] != nil:
				// Both the content apply (LFXV2-2775) and the UTM tagger PATCH `content`.
				// Tell them apart by what the html contains: a tagged body carries utm_
				// parameters, a freshly applied body does not.
				html := extractWidgetHTML(body)
				if strings.Contains(html, "utm_") {
					rec.markTagged(string(raw))
					rec.setDraft(html)
				} else {
					rec.markBody(html)
				}
			case body["subject"] != nil:
				rec.markSubject(fmt.Sprint(body["subject"]))
			default:
				rec.markSendList(body)
			}
			_, _ = io.WriteString(w, `{"id":"999","name":"KubeCon NA 2026 — brief-1","state":"DRAFT"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// ---- pre-create paths: must release the claim -----------------------------

// hubSpotTokenInfoPath mirrors the platform client's private-apps token-info endpoint.
// Duplicated rather than exported: these tests assert the wire path this dispatcher
// actually causes, and reading the constant from the package under test would make
// that assertion vacuous.
const hubSpotTokenInfoPath = "/oauth/v2/private-apps/get/access-token-info"

func TestHubSpot_PreCreateErrorsReleaseClaim(t *testing.T) {
	builtAuds := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	cfg := json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`)
	cases := map[string]struct {
		repo   connReader
		enc    domain.Encryptor
		aud    audienceReader
		config json.RawMessage
	}{
		"missing connection":     {fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}, builtAuds, cfg},
		"decrypt fails":          {fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, errEncryptor{}, builtAuds, cfg},
		"incomplete credentials": {fakeConnReader{conn: activeHubSpotConn(`{"PrivateAppToken":""}`)}, identityEncryptor{}, builtAuds, cfg},
		"inactive connection":    {fakeConnReader{conn: &model.Connection{Provider: model.ProviderHubSpot, AccountID: "1", EncryptedCredentials: []byte(goodHubSpotCreds), Status: model.StatusInactive}}, identityEncryptor{}, builtAuds, cfg},
		"missing sourceEmailId":  {fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, builtAuds, json.RawMessage(`{"hubspotConfig":{}}`)},
		"no audience":            {fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, fakeAudienceReader{auds: nil}, cfg},
		"audience not built":     {fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, fakeAudienceReader{auds: []*model.CampaignAudience{{Platform: model.ProviderHubSpot, Status: model.AudienceBuilding}}}, cfg},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := NewHubSpotDispatcher(tc.repo, tc.enc, tc.aud)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, tc.config)
			if err == nil {
				t.Fatalf("%s: expected a pre-create error", name)
			}
			if camp != nil {
				t.Errorf("%s: a pre-create failure must return a nil campaign (claim released), got %+v", name, camp)
			}
			var nuc interface{ NoUpstreamCreate() bool }
			if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("%s: a pre-create failure must be NoUpstreamCreate (claim released), got %T: %v", name, err, err)
			}
		})
	}
}

// TestHubSpot_DispatchClonesAndSetsSendList drives the happy path: clone the template + set the
// send list to the built audience's master list + suppression ids, and map the cloned email to
// the campaign.
func TestHubSpot_DispatchClonesAndSetsSendList(t *testing.T) {
	srv, rec := hubspotServer(t)
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", []string{"9001", "9002"})}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "999" {
		t.Fatalf("adapter must map the cloned email id, got %+v", camp)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want %q", camp.Status, campaignStatusCreated)
	}
	if len(camp.Result) == 0 {
		t.Error("result blob should be populated with the cloned email")
	}
	// The portal the TOKEN authenticates against, recorded at create time. Without it the row
	// cannot say which portal its bare-numeric email id means, and ReadMetrics — which refuses
	// rather than guessing — can never read this campaign again.
	var blob struct {
		PortalID string `json:"portalId"`
	}
	if err := json.Unmarshal(camp.Result, &blob); err != nil {
		t.Fatalf("result blob is not valid JSON: %v", err)
	}
	if blob.PortalID != "8112310" {
		t.Errorf("result portalId = %q, want the portal the token resolves to (8112310)", blob.PortalID)
	}
	if !rec.SawClone() || !rec.SawSendList() {
		t.Fatalf("expected both a clone (%v) and a set-send-list (%v) call", rec.SawClone(), rec.SawSendList())
	}
	// The master list id must reach the send-list payload (the field name is the client's, so
	// assert the value is present somewhere in the recorded body).
	body, _ := json.Marshal(rec.SendListBody())
	if !strings.Contains(string(body), "26724") {
		t.Errorf("send-list payload must carry the audience master list id 26724, got %s", body)
	}
}

// TestHubSpot_AppliesGeneratedContent: subject and body from the config reach the draft, and the
// body is written BEFORE the UTM tagger so the tags survive (LFXV2-2775).
func TestHubSpot_AppliesGeneratedContent(t *testing.T) {
	srv, rec := hubspotServer(t)
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	cfg := json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555","subject":"Three days in Amsterdam","bodyHtml":"<p>Join us</p><a href=\"https://events.lfx.dev/reg\">Register</a>"}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	subject, body := rec.snapshotContent()
	if subject != "Three days in Amsterdam" {
		t.Errorf("subject = %q, want the generated subject", subject)
	}
	if !strings.Contains(body, "Join us") {
		t.Errorf("body = %q, want the generated body", body)
	}

	// ORDER is the claim, not merely that both ran: the tagger rewrites the body's links, so a
	// body written afterwards would discard every utm_ parameter it had just added.
	tagged := rec.TaggedHTML()
	if !strings.Contains(tagged, "utm_") {
		t.Errorf("the tagger must run AFTER the body is applied, so the final draft carries utm parameters; got %q", tagged)
	}
	if !strings.Contains(tagged, "Join us") {
		t.Errorf("the tagged html must be the GENERATED body, not the template's; got %q", tagged)
	}
}

// A template whose ONLY rich-text block is empty must still receive the generated body.
//
// It is the most unambiguous shape there is -- one block, nothing to overwrite -- and it was the
// one case the guard refused: GetEmailHTMLWidgets omitted empty bodies, so `total` was 1 while the
// writable map was empty, leaving the widget unaddressable. Every rich-text widget is now
// returned, empty included, and the caller decides.
func TestHubSpot_SingleEmptyWidgetReceivesTheBody(t *testing.T) {
	srv, rec := hubspotServer(t)
	rec.onlyEmptyWidget = true
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	cfg := json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555","subject":"Three days in Amsterdam","bodyHtml":"<p>Join us</p>"}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if _, body := rec.snapshotContent(); !strings.Contains(body, "Join us") {
		t.Errorf("body = %q, want the generated body written into the single empty rich-text block", body)
	}
}

// A header IMAGE beside the rich-text block must NOT count as a second widget.
//
// This is the ordinary template shape, and it was silently broken: an image module decodes into
// `struct{ HTML string }` perfectly happily with HTML empty, so counting object-bodied modules
// reported two widgets, the single-widget guard declined, and the generated body was never
// written -- with only an info log to say so. The fix identifies a rich-text widget by the
// PRESENCE of the `html` key, which an image body does not carry.
//
// The inverse of TestHubSpot_EmptySecondWidgetKeepsItsBody: that one pins an undercount, this one
// an overcount, and the same key check has to answer both.
func TestHubSpot_HeaderImageDoesNotBlockTheBodyWrite(t *testing.T) {
	srv, rec := hubspotServer(t)
	rec.imageWidget = true
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	cfg := json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555","subject":"Three days in Amsterdam","bodyHtml":"<p>Join us</p>"}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	_, body := rec.snapshotContent()
	if !strings.Contains(body, "Join us") {
		t.Errorf("body = %q, want the generated body written despite a header image module", body)
	}
}

// An EMPTY second rich-text widget is still a second widget.
//
// `GetEmailHTMLWidgets` USED TO omit widgets whose body trims to empty, so a guard counting only
// the widgets it CAN write saw 1 for a template with one populated body and one empty block, and
// rewrote the populated one — the exact ambiguity the single-widget guard exists to refuse. An
// empty block is one an operator can see and fill; it is part of the template's structure, not
// an absence, so every rich-text widget is now returned and this test pins that it counts.
//
// This is the case the populated-second-widget test above cannot reach: there the map itself has
// two entries, so a count of either kind refuses.
func TestHubSpot_EmptySecondWidgetKeepsItsBody(t *testing.T) {
	srv, rec := hubspotServer(t)
	rec.emptyExtraWidget = true
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	cfg := json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555","subject":"Three days in Amsterdam","bodyHtml":"<p>Join us</p>"}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	subject, body := rec.snapshotContent()
	if subject != "Three days in Amsterdam" {
		t.Errorf("subject = %q, want the subject applied even where the body cannot be", subject)
	}
	if body != "" {
		t.Errorf("body = %q, want no body write when an empty second rich-text block exists", body)
	}
}

// A template with TWO rich-text widgets must keep its own body. There is no safe way to choose
// which block the generated body replaces, and writing the wrong one destroys content the
// operator did not choose to replace -- the single destructive outcome in this path.
//
// The subject still applies: it is one field with one meaning, so it carries no such ambiguity.
func TestHubSpot_MultiWidgetTemplateKeepsItsBody(t *testing.T) {
	srv, rec := hubspotServer(t)
	rec.extraWidget = true
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	cfg := json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555","subject":"Three days in Amsterdam","bodyHtml":"<p>Join us</p>"}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	subject, body := rec.snapshotContent()
	if subject != "Three days in Amsterdam" {
		t.Errorf("subject = %q, want the generated subject applied even for a multi-widget template", subject)
	}
	// The BODY is what must not be written. Asserting it is empty pins "no content PATCH was
	// issued for the body" rather than merely "the template survived by luck".
	if body != "" {
		t.Errorf("body = %q, want no body write on a multi-widget template", body)
	}
}

// TestHubSpot_ContentAbsentLeavesTemplateCopy: omitting subject/bodyHtml must change nothing —
// this is every campaign that predates LFXV2-2775.
func TestHubSpot_ContentAbsentLeavesTemplateCopy(t *testing.T) {
	srv, rec := hubspotServer(t)
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	subject, body := rec.snapshotContent()
	if subject != "" {
		t.Errorf("no subject was configured, so none may be PATCHed; got %q", subject)
	}
	if body != "" {
		t.Errorf("no body was configured, so the template's body must be left alone; got %q", body)
	}
}

// TestHubSpot_StagesWithoutEventName: email staging must proceed even when the brief has no
// eventName (unlike the ad adapters, which require it). The clone name falls back to the event
// slug / brief id.
func TestHubSpot_StagesWithoutEventName(t *testing.T) {
	srv, rec := hubspotServer(t)
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	// A brief whose details carry NO eventName (only a url).
	brief := &model.CampaignBrief{ID: "brief-2", ProjectID: "cncf", EventSlug: "kubecon-na-2026", URL: "https://events.example/kc"}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))
	camp, err := d.Dispatch(context.Background(), brief, model.ProviderHubSpot, json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`))
	if err != nil {
		t.Fatalf("staging must succeed without an eventName: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "999" || !rec.SawClone() {
		t.Fatalf("expected a cloned email, got %+v (sawClone=%v)", camp, rec.SawClone())
	}
}

// TestHubSpot_MasterInSuppressionRefusedBeforeClone: when the audience master list is also in its
// suppression set (which would exclude the whole audience), the dispatcher must refuse BEFORE any
// HubSpot call — otherwise the clone would be created and then orphaned when SetSendList rejects.
func TestHubSpot_MasterInSuppressionRefusedBeforeClone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no HubSpot call should happen when master is in the suppression set: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", []string{"26724"})} // master also suppressed
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`))
	if err == nil {
		t.Fatal("a master-in-suppression audience must be refused")
	}
	if camp != nil {
		t.Errorf("a pre-clone refusal must return a nil campaign (nothing created), got %+v", camp)
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a pre-clone conflict must be NoUpstreamCreate (claim released), got %T: %v", err, err)
	}
}

// TestHubSpot_CloneUnconfirmedRetainsClaim: a clone that returns 2xx with no id (UNCONFIRMED —
// a draft may exist) must retain the claim (non-nil name-only partial) with an UNCONFIRMED error.
func TestHubSpot_CloneUnconfirmedRetainsClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"clone but no id"}`) // 2xx, no id → UNCONFIRMED
	}))
	defer srv.Close()
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`))
	if err == nil {
		t.Fatal("expected an error on an unconfirmed clone")
	}
	if camp == nil {
		t.Fatal("an UNCONFIRMED clone must return a non-nil partial (claim retained), got nil")
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
		t.Errorf("an UNCONFIRMED clone must NOT be NoUpstreamCreate (claim retained): %v", err)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error should say UNCONFIRMED, got: %v", err)
	}
	// The partial MUST carry a non-empty Result (the orchestrator persists an id-less orphan
	// only when PlatformCampaignID != "" OR len(Result) > 0) so the maybe-created draft is
	// reconcilable by name; and its status must be `unconfirmed`, not `created`.
	if len(camp.Result) == 0 {
		t.Error("an UNCONFIRMED partial must populate Result (else the orchestrator drops the id-less orphan)")
	}
	if camp.Status != campaignStatusUnconfirmed {
		t.Errorf("status = %q, want %q for an unconfirmed clone", camp.Status, campaignStatusUnconfirmed)
	}
	if camp.CampaignName == "" {
		t.Error("the partial must carry the deterministic clone name for reconcile")
	}
}

// TestHubSpot_SendListFailureIsPartial: the clone succeeds but SetSendList fails — the email
// exists, so the dispatcher must return a non-nil campaign (claim retained) with an error so the
// caller reconciles rather than reporting a clean success.
func TestHubSpot_SendListFailureIsPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/marketing/v3/emails/clone" {
			_, _ = io.WriteString(w, `{"id":"999","name":"n","state":"DRAFT"}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway) // set-send-list fails
	}))
	defer srv.Close()
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud,
		hubspot.WithBaseURL(srv.URL))
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`))
	if err == nil {
		t.Fatal("expected an error when set-send-list fails")
	}
	if camp == nil || camp.PlatformCampaignID != "999" {
		t.Fatalf("a post-clone failure must return the cloned campaign (claim retained), got %+v", camp)
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
		t.Errorf("a post-clone failure must NOT be NoUpstreamCreate (the email exists): %v", err)
	}
}

// TestHubSpot_TagsEmailLinksWithUTM pins that the staged email's links reach HubSpot TAGGED.
// Without this the email sends with bare links, so its sessions land in the warehouse as
// direct/unattributed traffic and the marketing dashboards cannot see the email channel at all
// — the gap this feature exists to close.
func TestHubSpot_TagsEmailLinksWithUTM(t *testing.T) {
	srv, rec := hubspotServer(t)
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot,
		json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if rec.TaggedHTML() == "" {
		t.Fatal("the draft's links were never written back tagged: the email would send unattributed")
	}
	for _, want := range []string{"utm_source=email", "utm_medium=LF-Events", "utm_campaign="} {
		if !strings.Contains(rec.TaggedHTML(), want) {
			t.Errorf("tagged body missing %q\ngot: %s", want, rec.TaggedHTML())
		}
	}
	// The original destination must survive tagging — a rewritten link that loses its target
	// is far worse than an untagged one.
	if !strings.Contains(rec.TaggedHTML(), "events.lfx.dev/reg") {
		t.Errorf("the link destination was lost\ngot: %s", rec.TaggedHTML())
	}
}

// TestHubSpot_UTMCampaignOverrideReachesTheLinks pins the config override, which lets several
// briefs' emails roll up to one campaign in reporting.
func TestHubSpot_UTMCampaignOverrideReachesTheLinks(t *testing.T) {
	srv, rec := hubspotServer(t)
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot,
		json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555","utmCampaign":"q1-events-push"}}`)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(rec.TaggedHTML(), "utm_campaign=q1-events-push") {
		t.Errorf("the configured campaign must win over the derived one\ngot: %s", rec.TaggedHTML())
	}
}

// TestHubSpot_TaggingFailureDoesNotFailTheDispatch pins the best-effort contract. By the time
// tagging runs the email is cloned AND pointed at the right audience — a working campaign.
// Failing the dispatch would turn a reporting gap into a failed send and still leave the
// configured draft behind.
func TestHubSpot_TaggingFailureDoesNotFailTheDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == hubSpotTokenInfoPath:
			_, _ = io.WriteString(w, `{"hubId":8112310}`)
		case r.Method == http.MethodPost && r.URL.Path == "/marketing/v3/emails/clone":
			_, _ = io.WriteString(w, `{"id":"999","name":"n","state":"DRAFT"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/draft"):
			// The draft read fails: tagging cannot proceed.
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/draft"):
			_, _ = io.WriteString(w, `{"id":"999","name":"n","state":"DRAFT"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot,
		json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`))
	if err != nil {
		t.Fatalf("a tagging failure must NOT fail the dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "999" {
		t.Fatalf("the staged email must still be returned, got %+v", camp)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want %q — the campaign is complete without tagging", camp.Status, campaignStatusCreated)
	}
}

// TestHubSpot_DispatchBoundsThePortalLookupBelowProviderCallTimeout: the best-effort provenance
// lookup must carry its OWN short deadline (portalLookupTimeout), not the caller's context —
// otherwise sustained throttling on the token-info endpoint (the client's own retry policy can
// wait up to retryMax*maxRetryWait = 180s) could burn the entire 2-minute providerCallTimeout
// before CloneEmail ever runs, handing it an already-cancelled context. Asserted by reading the
// deadline the token-info REQUEST actually carried, not by waiting one out.
func TestHubSpot_DispatchBoundsThePortalLookupBelowProviderCallTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == hubSpotTokenInfoPath:
			_, _ = io.WriteString(w, `{"hubId":8112310}`)
		case r.Method == http.MethodPost && r.URL.Path == "/marketing/v3/emails/clone":
			_, _ = io.WriteString(w, `{"id":"999","name":"n","state":"DRAFT"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/draft"):
			_, _ = io.WriteString(w, `{"content":{"widgets":{}}}`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/draft"):
			_, _ = io.WriteString(w, `{"id":"999","name":"n","state":"DRAFT"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	// Captures the DEADLINE the outgoing http.Request's context carried, per path — the
	// server-side r.Context() (an httptest connection-lifetime context) can't reveal this;
	// only the client-side request the RoundTripper sees has the ctx this code actually set.
	deadlines := map[string]time.Time{}
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if dl, ok := req.Context().Deadline(); ok {
			deadlines[req.URL.Path] = dl
		}
		return http.DefaultTransport.RoundTrip(req)
	})

	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud,
		hubspot.WithBaseURL(srv.URL), hubspot.WithHTTPClient(&http.Client{Transport: rt}))

	before := time.Now()
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot,
		json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	portalDeadline, ok := deadlines[hubSpotTokenInfoPath]
	if !ok {
		t.Fatal("the token-info request must carry a deadline, not the caller's un-timeboxed context")
	}
	if d := portalDeadline.Sub(before); d <= 0 || d > portalLookupTimeout+time.Second {
		t.Errorf("token-info deadline was %v out from Dispatch start, want within (0, portalLookupTimeout=%v]", d, portalLookupTimeout)
	}
	// The clone call, by contrast, is a MUTATING step and must NOT be truncated to the short
	// provenance-lookup budget. It still carries A deadline — every attempt gets its own
	// context.WithTimeout(ctx, c.requestTimeout) inside doRequest, unconditionally, regardless
	// of what the caller's context looked like (see client.go) — so presence/absence of a
	// deadline isn't the right signal. What must hold is that it is bounded by the CLIENT's
	// own per-attempt requestTimeout, not truncated down to the much shorter portalLookupTimeout.
	cloneDeadline, ok := deadlines["/marketing/v3/emails/clone"]
	if !ok {
		t.Fatal("the clone request must carry a deadline (doRequest's per-attempt timeout)")
	}
	if d := cloneDeadline.Sub(before); d <= portalLookupTimeout {
		t.Errorf("clone deadline was only %v out from Dispatch start, want materially more than portalLookupTimeout=%v — it must not have inherited the short portal-lookup budget", d, portalLookupTimeout)
	}
	// providerCallTimeout lives in internal/service (orchestrator.go) and cannot be imported
	// here without a cycle; its value (2m) is duplicated in this assertion so the two stay
	// honest with each other. If that constant changes, this must change too.
	const providerCallTimeoutForTest = 2 * time.Minute
	if portalLookupTimeout >= providerCallTimeoutForTest {
		t.Fatalf("portalLookupTimeout (%v) must stay well under providerCallTimeout (%v) or the guard is pointless", portalLookupTimeout, providerCallTimeoutForTest)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// ConfigSnapshot must not carry the generated copy.
//
// The snapshot column is UNENCRYPTED and is returned through the API, and its purpose is
// provenance: what this campaign was cloned from, and how its links attribute. The generated
// subject and body are caller-supplied content whose `href`s can carry query tokens, and the
// HubSpot draft is the system of record for them -- so persisting them here would put arbitrary
// caller content in a column nobody reading a reconcile row expects to hold any.
//
// Passing `cfg` wholesale to applyCampaignConfig did exactly that.
func TestHubSpot_ConfigSnapshotOmitsTheGeneratedCopy(t *testing.T) {
	srv, _ := hubspotServer(t)
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	// A body carrying a tokenised link -- the shape that makes this a leak rather than bloat.
	cfg := json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555","utmCampaign":"kubecon","subject":"Three days in Amsterdam","bodyHtml":"<a href=\"https://x.example/rsvp?token=SECRET-LINK-TOKEN\">RSVP</a>"}}`)
	out, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snapshot := string(out.ConfigSnapshot)
	if strings.Contains(snapshot, "SECRET-LINK-TOKEN") || strings.Contains(snapshot, "bodyHtml") {
		t.Errorf("ConfigSnapshot carried the generated body: %s", snapshot)
	}
	if strings.Contains(snapshot, "Three days in Amsterdam") || strings.Contains(snapshot, "subject") {
		t.Errorf("ConfigSnapshot carried the generated subject: %s", snapshot)
	}
	// The provenance fields it EXISTS for must survive, or this test would pass on an empty snapshot.
	if !strings.Contains(snapshot, "555") {
		t.Errorf("ConfigSnapshot lost the source template id: %s", snapshot)
	}
	if !strings.Contains(snapshot, "kubecon") {
		t.Errorf("ConfigSnapshot lost the utm campaign: %s", snapshot)
	}
}
