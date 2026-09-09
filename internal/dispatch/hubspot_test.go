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
// builtHubSpotAudience stamps BuiltInPortalID with the portal the fake server reports
// (`{"hubId":8112310}`), because Dispatch now refuses a send whose audience cannot be proven to
// belong to the portal it authenticates against. Leaving it empty would make every dispatch test
// exercise the provenance refusal instead of the path it was written for — see
// builtHubSpotAudienceInPortal for the tests that want a different portal on purpose.
func builtHubSpotAudience(masterList string, suppression []string) []*model.CampaignAudience {
	return builtHubSpotAudienceInPortal(masterList, suppression, "8112310")
}

// builtHubSpotAudienceInPortal is the explicit form: an audience built in a NAMED portal, or in
// none at all when portalID is empty.
func builtHubSpotAudienceInPortal(masterList string, suppression []string, portalID string) []*model.CampaignAudience {
	raw, _ := json.Marshal(suppression)
	return []*model.CampaignAudience{{
		ID: "aud-1", Platform: model.ProviderHubSpot, Status: model.AudienceBuilt,
		PlatformMasterListID: masterList, SuppressionListIDs: raw,
		BuiltInPortalID: portalID,
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
	// tokenInfoCalls counts hits on the token-info endpoint. Dispatch must make exactly ONE:
	// the cross-portal guard verifies the portal and RETURNS it for the provenance stamp, so a
	// second call would be the duplicate that regression removed.
	tokenInfoCalls int
	taggedHTML     string
	subjectSet     string
	bodyHTMLSet    string
	draftHTML      string
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
			rec.mu.Lock()
			rec.tokenInfoCalls++
			rec.mu.Unlock()
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
		// Answer token-info with the audience's portal, so the cross-portal guard passes and
		// this test reaches the clone it is about of. A canned body for every path would fail
		// the guard first and never exercise the UNCONFIRMED arm.
		if r.URL.Path == hubSpotTokenInfoPath {
			_, _ = io.WriteString(w, `{"hubId":8112310}`)
			return
		}
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
		// token-info must answer, or the cross-portal guard refuses before the clone and this
		// never reaches the set-send-list failure it is written for.
		if r.URL.Path == hubSpotTokenInfoPath {
			_, _ = io.WriteString(w, `{"hubId":8112310}`)
			return
		}
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

// TestHubSpot_CreateCampaignTagsDomainSentinels pins the translation the SERVICE depends on.
//
// The service classifies a create outcome from domain sentinels alone — it must not import a
// platform client to read unexported error types, which would invert service → dispatch →
// platform. That only works if this layer actually tags them, and nothing else in the chain can
// notice if it stops: the service tests would go on passing against sentinels nobody produces.
func TestHubSpot_CreateCampaignTagsDomainSentinels(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		wantTags    []error
		notWantTags []error
	}{
		{
			name:        "403 is a permission rejection",
			status:      http.StatusForbidden,
			wantTags:    []error{domain.ErrPlatformPermission, domain.ErrPlatformRejected},
			notWantTags: nil,
		},
		{
			name:        "400 is a definite rejection but not a permission one",
			status:      http.StatusBadRequest,
			wantTags:    []error{domain.ErrPlatformRejected},
			notWantTags: []error{domain.ErrPlatformPermission},
		},
		{
			// A 500 MAY have committed the campaign, so it must stay untagged and be treated as
			// unconfirmed upstream. Tagging it would tell the operator nothing was created.
			name:        "500 is left unclassified",
			status:      http.StatusInternalServerError,
			wantTags:    nil,
			notWantTags: []error{domain.ErrPlatformRejected, domain.ErrPlatformPermission},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"message":"nope"}`)
			}))
			defer srv.Close()

			d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{},
				fakeAudienceReader{}, hubspot.WithBaseURL(srv.URL))
			_, err := d.CreateCampaign(context.Background(), "cncf", model.ProviderHubSpot, "KubeCon NA 2027")
			if err == nil {
				t.Fatalf("a %d was reported as success", tc.status)
			}
			for _, want := range tc.wantTags {
				if !errors.Is(err, want) {
					t.Errorf("a %d is not tagged %v, so the service cannot classify it: %v", tc.status, want, err)
				}
			}
			for _, notWant := range tc.notWantTags {
				if errors.Is(err, notWant) {
					t.Errorf("a %d is wrongly tagged %v: %v", tc.status, notWant, err)
				}
			}
		})
	}
}

// TestHubSpot_CreateCampaignTagsANeverSentFailure pins the arm the status table above cannot
// reach: a failure that happens BEFORE any request leaves this process.
//
// It matters because the default is deliberately fail-closed. An untagged error is treated as
// unconfirmed, which is right for anything that might have reached HubSpot — but a dial failure
// or an already-cancelled context proves the campaign was never created, and reporting that as
// "may already exist" sends the operator to hunt for something that does not exist.
func TestHubSpot_CreateCampaignTagsANeverSentFailure(t *testing.T) {
	// A context cancelled before the call begins is the cheapest way to reach preSendError
	// deterministically — no network, no timing.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request should reach HubSpot when the context is already cancelled")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{},
		fakeAudienceReader{}, hubspot.WithBaseURL(srv.URL))
	_, err := d.CreateCampaign(ctx, "cncf", model.ProviderHubSpot, "KubeCon NA 2027")
	if err == nil {
		t.Fatal("a cancelled context was reported as a successful create")
	}
	// The SPECIFIC sentinel, which is what the service branches on. Asserting the broader
	// rejection tag would pass even if this arm stopped tagging never-sent at all, and the
	// service would then answer the name-rejection remedy for a dial failure.
	if !errors.Is(err, domain.ErrPlatformNeverSent) {
		t.Errorf("a never-sent failure is not tagged ErrPlatformNeverSent, so the service cannot "+
			"tell it apart from a rejection and answers the wrong remedy: %v", err)
	}
	// And NOT tagged rejected: the two are mutually exclusive events — HubSpot refusing on the
	// merits versus HubSpot never seeing the request. Reporting both made `definite_rejection`
	// true for a DNS failure in the create's own telemetry.
	if errors.Is(err, domain.ErrPlatformRejected) {
		t.Errorf("a never-sent failure also tagged as a definite rejection, which corrupts "+
			"rejection telemetry and any generic rejection handling: %v", err)
	}
	if errors.Is(err, domain.ErrPlatformPermission) {
		t.Errorf("a never-sent failure tagged as a permission refusal: %v", err)
	}
}

// TestHubSpot_SearchCampaignsCrossesTheSeam covers the dispatcher adapter itself.
//
// The client tests derive Capped and the service tests map it onto the wire, but nothing
// exercised the adapter BETWEEN them. Dropping page.Capped here would leave both of those suites
// green while turning a capped, incomplete search into a plain empty answer — and empty is
// exactly what the UI acts on by offering to create a campaign, in a namespace shared by every
// project configured against that same HubSpot portal — which is not necessarily the LF's own,
// since connections are per project. Every field is asserted for the same reason: a field lost at
// this seam cannot be seen from either side of it.
func TestHubSpot_SearchCampaignsCrossesTheSeam(t *testing.T) {
	const searchPath = "/crm/v3/objects/0-35/search"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == searchPath {
			// total (7) deliberately exceeds the returned rows (1): that is what makes the page
			// capped, and the capped flag is the whole point of this test.
			_, _ = io.WriteString(w, `{"total":7,"results":[{"id":"c-1","properties":{"hs_name":"KubeCon NA 2026","hs_utm":"kubecon-na-2026","hs_start_date":"2026-11-10"}}]}`)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{},
		fakeAudienceReader{}, hubspot.WithBaseURL(srv.URL))

	page, err := d.SearchCampaigns(context.Background(), "cncf", model.ProviderHubSpot, "KubeCon")
	if err != nil {
		t.Fatalf("SearchCampaigns: %v", err)
	}
	if !page.Capped {
		t.Error("Capped was dropped crossing the dispatcher: an incomplete search now reads as a " +
			"complete one, and an absent campaign is what licenses a duplicate create")
	}
	if len(page.Campaigns) != 1 {
		t.Fatalf("got %d campaigns, want 1", len(page.Campaigns))
	}
	got := page.Campaigns[0]
	for _, f := range []struct{ name, got, want string }{
		{"ID", got.ID, "c-1"},
		{"Name", got.Name, "KubeCon NA 2026"},
		{"UTM", got.UTM, "kubecon-na-2026"},
		{"StartDate", got.StartDate, "2026-11-10"},
	} {
		if f.got != f.want {
			t.Errorf("%s = %q, want %q — lost crossing the dispatcher seam", f.name, f.got, f.want)
		}
	}
}

// TestHubSpot_LatePermissionFailureCarriesTheCredentialOrigin pins that a 401/403 discovered on
// the FIRST REAL CALL -- after credential resolution returned cleanly -- still says which row the
// token came from.
//
// The distinction only became reachable when credsSource.systemConn stopped refusing the email
// channel: a project with no HubSpot connection of its own now runs on the shared LF row. Defects
// that resolution finds ITSELF are tagged by res.systemScoped inside resolveHubSpotClientWithCreds,
// but a permission failure is invisible until the platform answers, and the narrow
// resolveHubSpotClient wrapper discards the resolved before that can happen. Untagged, one expired
// or under-scoped LF token is reported to every fallback foundation as THEIR configuration fault --
// telling each to fix a connection they do not have, while the single operator who can repair it
// hears from nobody.
//
// Both paths are covered because they carry the origin by DIFFERENT mechanisms, and only one of
// them is systemScoped: SearchCampaigns emits ErrConnectionNotUsable, which systemScoped upgrades;
// CreateCampaign emits the platform-rejection taxonomy, which systemScoped is gated against and
// silently passes through, so it joins ErrSystemConnectionOrigin directly.
func TestHubSpot_LatePermissionFailureCarriesTheCredentialOrigin(t *testing.T) {
	newDispatcher := func(t *testing.T, srvURL string, systemOwned bool) *HubSpotDispatcher {
		t.Helper()
		owner := "cncf"
		if systemOwned {
			owner = model.SystemProjectID
		}
		return NewHubSpotDispatcher(
			&scopedConnReader{rows: map[string]*model.Connection{owner: activeHubSpotConn(goodHubSpotCreds)}},
			identityEncryptor{}, fakeAudienceReader{}, hubspot.WithBaseURL(srvURL))
	}

	for _, tc := range []struct {
		name string
		call func(*HubSpotDispatcher) error
		// tagWhenProjectOwned is the sentinel the ordinary (project-owned) case must still carry.
		// Asserting it in BOTH rows is what keeps the origin split additive: a change that
		// tagged the system case by REPLACING the existing classification would pass a
		// system-only assertion and break every consumer switching on the original tag.
		tagWhenProjectOwned error
	}{
		{
			name: "SearchCampaigns",
			call: func(d *HubSpotDispatcher) error {
				_, err := d.SearchCampaigns(context.Background(), "cncf", model.ProviderHubSpot, "KubeCon")
				return err
			},
			tagWhenProjectOwned: domain.ErrConnectionNotUsable,
		},
		{
			name: "SearchEmails",
			call: func(d *HubSpotDispatcher) error {
				_, err := d.SearchEmails(context.Background(), "cncf", model.ProviderHubSpot, "KubeCon")
				return err
			},
			tagWhenProjectOwned: domain.ErrConnectionNotUsable,
		},
		{
			name: "CreateCampaign",
			call: func(d *HubSpotDispatcher) error {
				_, err := d.CreateCampaign(context.Background(), "cncf", model.ProviderHubSpot, "KubeCon NA 2027")
				return err
			},
			tagWhenProjectOwned: domain.ErrPlatformPermission,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"insufficient scope"}`)
			}))
			defer srv.Close()

			t.Run("the LF row's failure is marked system-owned", func(t *testing.T) {
				err := tc.call(newDispatcher(t, srv.URL, true))
				if err == nil {
					t.Fatal("a 403 was reported as success")
				}
				if !errors.Is(err, domain.ErrSystemConnectionOrigin) && !errors.Is(err, domain.ErrSystemConnectionNotUsable) {
					t.Errorf("a 403 on the shared LF token is not marked system-owned, so every "+
						"fallback project is told to fix a connection it does not have: %v", err)
				}
				if !errors.Is(err, tc.tagWhenProjectOwned) {
					t.Errorf("the original classification was REPLACED rather than added to; "+
						"consumers switching on %v now miss this error: %v", tc.tagWhenProjectOwned, err)
				}
			})

			t.Run("a project's own failure stays the project's", func(t *testing.T) {
				err := tc.call(newDispatcher(t, srv.URL, false))
				if err == nil {
					t.Fatal("a 403 was reported as success")
				}
				if !errors.Is(err, tc.tagWhenProjectOwned) {
					t.Errorf("want %v, got %v", tc.tagWhenProjectOwned, err)
				}
				if errors.Is(err, domain.ErrSystemConnectionOrigin) || errors.Is(err, domain.ErrSystemConnectionNotUsable) {
					t.Errorf("a project's OWN connection defect was attributed to the LF system row, "+
						"which sends the one person who can fix it to the wrong place: %v", err)
				}
			})
		})
	}
}

// TestHubSpot_DispatchRefusesAnAudienceFromAnotherPortal pins the cross-portal guard, in both
// forms, and pins that each refuses BEFORE anything is created.
//
// The hazard only became reachable when the reserved-scope fallback began serving the email
// channel: a project with no HubSpot connection builds its audience against the LF portal, then
// connects its own portal, and Dispatch resolves credentials afresh — preferring that new
// connection. The email is cloned in the project's portal while SetSendList is handed list ids
// that exist only in the LF portal. HubSpot answers about ids it cannot see, so the send is a
// partial or a hard failure, and neither row explains why.
//
// Both arms assert notCreated: a refusal after CloneEmail would orphan a draft, which is the
// same reason the master/suppression pre-flight runs before the clone.
func TestHubSpot_DispatchRefusesAnAudienceFromAnotherPortal(t *testing.T) {
	cfg := json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`)

	t.Run("built in a different portal", func(t *testing.T) {
		srv, rec := hubspotServer(t)
		d := NewHubSpotDispatcher(
			fakeConnReader{conn: activeHubSpotConn(`{"PrivateAppToken":"pat-good"}`)},
			identityEncryptor{},
			fakeAudienceReader{auds: builtHubSpotAudienceInPortal("26724", nil, "99999999")},
			hubspot.WithBaseURL(srv.URL))

		camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, cfg)
		if err == nil {
			t.Fatal("dispatch succeeded with an audience built in another portal; its send list does not exist there")
		}
		assertRefusedBeforeCreate(t, camp, err, rec)
		if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
			t.Errorf("err = %v, want ErrCampaignAccountMismatch: the caller must be told to rebuild, not to retry", err)
		}
		// Both portals named, so an operator can see which way the connection moved.
		if !strings.Contains(err.Error(), "99999999") || !strings.Contains(err.Error(), "8112310") {
			t.Errorf("err = %v, want it to name BOTH the audience's portal and the send's", err)
		}
	})

	t.Run("no portal recorded", func(t *testing.T) {
		srv, rec := hubspotServer(t)
		d := NewHubSpotDispatcher(
			fakeConnReader{conn: activeHubSpotConn(`{"PrivateAppToken":"pat-good"}`)},
			identityEncryptor{},
			fakeAudienceReader{auds: builtHubSpotAudienceInPortal("26724", nil, "")},
			hubspot.WithBaseURL(srv.URL))

		camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, cfg)
		if err == nil {
			t.Fatal("dispatch succeeded with an audience recording no portal; an unprovable tenant must fail closed")
		}
		assertRefusedBeforeCreate(t, camp, err, rec)
		// The NARROWER sentinel: there is no portal to reconnect to, so the remedy is a rebuild.
		// Every audience built before built_in_portal_id existed is in this state, and the column
		// is deliberately not backfilled.
		if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) {
			t.Errorf("err = %v, want ErrCampaignProvenanceUnknown for an unrecorded portal", err)
		}
		if !strings.Contains(err.Error(), "rebuild") {
			t.Errorf("err = %v, want it to say rebuild — there is no portal to reconnect to", err)
		}
	})
}

// assertRefusedBeforeCreate pins that a refusal happened with NOTHING created upstream.
//
// The sentinel alone does not prove that. Moving the guard below CloneEmail would keep every
// errors.Is assertion green while orphaning a draft in HubSpot — a refusal that leaves real state
// behind is a different, worse outcome than one that does not, and only the recorder can tell
// them apart. NoUpstreamCreate is what the orchestrator reads to release the dispatch claim.
func assertRefusedBeforeCreate(t *testing.T, camp *model.Campaign, err error, rec *hubspotRec) {
	t.Helper()
	if camp != nil {
		t.Errorf("campaign = %+v, want nil: a pre-create refusal must return no campaign", camp)
	}
	if rec.SawClone() {
		t.Error("CloneEmail was called before the refusal — a draft is now orphaned in the portal")
	}
	if rec.SawSendList() {
		t.Error("SetSendList was called before the refusal")
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("err = %v, want NoUpstreamCreate() true so the orchestrator releases the claim", err)
	}
}

// TestHubSpot_DispatchRefusesWhenPortalIdentityIsUnreadable pins the FAIL-CLOSED half of the
// cross-portal guard, which nothing else covers.
//
// An earlier version permitted the dispatch when token-info failed, reasoning that a metadata
// outage should not block a send that is otherwise ready. That is wrong in exactly the case the
// guard exists for: a token authenticated against the WRONG portal, plus a transient lookup
// failure, clones the email there and hands it list ids from the audience's portal — the unsafe
// partial send, reached through the guard's own fallback.
//
// An unreadable identity is not a match; it is an unknown. Refusing before any mutation lets the
// caller retry once token-info answers, which costs a delay rather than an orphaned draft.
func TestHubSpot_DispatchRefusesWhenPortalIdentityIsUnreadable(t *testing.T) {
	rec := &hubspotRec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Everything works EXCEPT portal identity.
		if r.URL.Path == hubSpotTokenInfoPath {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/marketing/v3/emails/clone" {
			rec.mu.Lock()
			rec.sawClone = true
			rec.mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"999","name":"n","state":"DRAFT"}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	d := NewHubSpotDispatcher(
		fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)},
		identityEncryptor{},
		fakeAudienceReader{auds: builtHubSpotAudienceInPortal("26724", nil, "8112310")},
		hubspot.WithBaseURL(srv.URL))

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot,
		json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`))
	if err == nil {
		t.Fatal("dispatch succeeded while portal identity was unreadable; the send list could not be proven to exist there")
	}
	if rec.SawClone() {
		t.Error("CloneEmail ran despite an unprovable portal — a draft is orphaned, which is what failing closed prevents")
	}
	if camp != nil {
		t.Errorf("campaign = %+v, want nil", camp)
	}
	if !strings.Contains(err.Error(), "retry") {
		t.Errorf("err = %v, want it to say retry: the identity may be readable later, unlike a real mismatch", err)
	}
}

// TestHubSpot_DispatchReadsThePortalOnce pins that the cross-portal guard's verified portal is
// REUSED for the campaign's provenance stamp rather than looked up a second time.
//
// The guard and the stamp both need the same fact -- which portal this token authenticates
// against -- and both used to ask the network for it. That is two retrying round trips per
// dispatch, each bounded at portalLookupTimeout, to learn one thing. The cost is the smaller half:
// the two calls could also DISAGREE in the direction that matters, with the guard proving the
// portal and the stamp then failing to read it, producing a campaign created with no provenance
// for a fact the process had already established. ReadMetrics refuses an unprovenanced campaign,
// so that send was unmeasurable.
//
// Asserting the count rather than the absence of a code path is deliberate: a future edit that
// reintroduces the lookup anywhere on this path fails here, wherever it puts it.
func TestHubSpot_DispatchReadsThePortalOnce(t *testing.T) {
	srv, rec := hubspotServer(t)
	d := NewHubSpotDispatcher(
		fakeConnReader{conn: activeHubSpotConn(`{"PrivateAppToken":"pat-good"}`)},
		identityEncryptor{},
		// Built in the SAME portal the fixture's token reports, so the guard passes and dispatch
		// runs to completion -- the path where a second lookup used to happen.
		fakeAudienceReader{auds: builtHubSpotAudienceInPortal("26724", nil, "8112310")},
		hubspot.WithBaseURL(srv.URL))

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot,
		json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	rec.mu.Lock()
	calls := rec.tokenInfoCalls
	rec.mu.Unlock()
	if calls != 1 {
		t.Errorf("token-info was called %d times, want exactly 1: the guard verifies the portal and "+
			"returns it, so the provenance stamp must not ask again", calls)
	}

	// And the stamp must actually carry the verified value -- reusing it is only a win if the
	// campaign ends up provenanced. An empty stamp here would make ReadMetrics refuse.
	if camp == nil {
		t.Fatal("dispatch returned no campaign")
	}
	if got := hubSpotCreationPortalID(camp); got != "8112310" {
		t.Errorf("the campaign recorded portal %q, want the verified 8112310 — reusing the guard's "+
			"value is only a win if the stamp actually carries it, or ReadMetrics refuses the send", got)
	}
}
