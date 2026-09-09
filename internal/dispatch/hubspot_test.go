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
	"slices"
	"sort"
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

// hubspotServer fakes the HubSpot API for the clone + set-send-list + content flow.
//
// The draft it serves is a DRAG_AND_DROP email, because that is what every template in the LF
// portal is: a `content.flexAreas` layout tree naming module ids, beside a `content.widgets` map
// holding the modules themselves. The fake models the two HubSpot behaviours a simpler stub hid,
// and which together produced a staged email with nothing inside it at all:
//
//   - Content is AUTHORITATIVE, not merged. A content PATCH REPLACES the widget map, so a payload
//     naming two widgets of a 33-widget draft leaves a draft holding two widgets. `Dropped()`
//     reports what a payload discarded, and the client is expected to discard nothing.
//   - Reading order lives in flexAreas and nowhere else. The widget map is a JSON object with no
//     order of its own, so a fake serving widgets alone could not tell a caller that reads the
//     layout from one that iterates the map at random.
//
// It is STATEFUL: a reader sees the effect of an earlier write, which is what makes write ORDER
// observable — the whole claim of the content-vs-tagging test.
//
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
	bodyWidget   string
	// widgets is content.widgets as the draft currently holds it: module id -> the whole module
	// object, body and scaffolding included. Seeded by seed(), then REPLACED by every content
	// PATCH, exactly as HubSpot treats it.
	widgets map[string]map[string]any
	// layout is the reading order content.flexAreas places the module ids in.
	layout []string
	// dropped names every widget a content PATCH removed from the draft. On a drag-and-drop
	// email that is destroyed content, not a harmless omission.
	dropped []string
	// contentKeys is the key set of the last content PATCH's `content` object. flexAreas absent
	// from it is the same destruction by another route — the layout tree would be gone.
	contentKeys []string
	// extraWidget adds a SECOND populated rich-text block below the first. The generated body
	// belongs in the first one; this block must survive untouched.
	extraWidget bool
	// emptyExtraWidget adds a second rich-text block with an EMPTY body. An empty block is one an
	// operator can see and fill, so it counts as a block — and it is not the first one.
	emptyExtraWidget bool
	// onlyEmptyWidget makes the draft's SINGLE rich-text block empty -- the most unambiguous
	// shape there is, and the one an operator most expects the generated body to fill.
	onlyEmptyWidget bool
	// imageWidget adds a header IMAGE module above the rich-text block -- the ordinary template
	// shape. It has a body object but no `html` key, so counting object-bodied modules reported
	// two blocks and the body write silently no-opped.
	imageWidget bool
	// reversedLayout places module_2 ABOVE module_1 while the map's keys sort the other way, so
	// only a caller that reads flexAreas can name the block at the top of the email.
	reversedLayout bool
}

// richModule is a rich-text drag-and-drop module: a body carrying `html`, plus the scaffolding
// a placed module needs. The scaffolding is here so that a write which dropped it would be
// visible — sending a bare `{body:{html}}` for a placed module is what made HubSpot discard the
// two widgets the old client did name.
func richModule(html string) map[string]any {
	return map[string]any{
		"path":           "@hubspot/rich_text",
		"module_id":      1,
		"schema_version": 2,
		"hs_wrapper_css": map[string]any{"padding-top": "10px"},
		"body":           map[string]any{"html": html},
	}
}

// seed builds the draft the flags describe, once, on first access. Callers hold the lock.
func (r *hubspotRec) seed() {
	if r.widgets != nil {
		return
	}
	body1 := `<a href="https://events.lfx.dev/reg">Register</a>`
	if r.onlyEmptyWidget {
		body1 = "   "
	}
	r.widgets = map[string]map[string]any{"module_1": richModule(body1)}
	r.layout = []string{"module_1"}

	if r.imageWidget {
		r.widgets["module_hdr"] = map[string]any{
			"path":      "@hubspot/email/dnd/image",
			"module_id": 2,
			"body":      map[string]any{"src": "https://img.example/logo.png", "alt": "logo"},
		}
		r.layout = append([]string{"module_hdr"}, r.layout...)
	}
	if r.emptyExtraWidget {
		r.widgets["module_2"] = richModule("   ")
		r.layout = append(r.layout, "module_2")
	}
	if r.extraWidget {
		r.widgets["module_2"] = richModule("<p>second block</p>")
		r.layout = append(r.layout, "module_2")
	}
	if r.reversedLayout {
		r.widgets["module_2"] = richModule("<p>top block</p>")
		r.layout = []string{"module_2", "module_1"}
	}
	// A template-level module the layout does not place, and which is not rich text at all. The
	// real template's preview_text is exactly this, and it was the ONE widget that survived the
	// destructive write — so a fixture without it cannot tell "everything survived" from
	// "everything unplaced was lost".
	r.widgets["preview_text"] = map[string]any{"body": map[string]any{"value": "See you there"}}
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

// draftPayload renders the draft as the GET .../draft response.
func (r *hubspotRec) draftPayload() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seed()
	sections := make([]any, 0, len(r.layout))
	for _, id := range r.layout {
		sections = append(sections, map[string]any{
			"id":      "section_" + id,
			"columns": []any{map[string]any{"id": "column_" + id, "width": 12, "widgets": []string{id}}},
		})
	}
	payload, _ := json.Marshal(map[string]any{
		"id":                "999",
		"emailTemplateMode": "DRAG_AND_DROP",
		"content": map[string]any{
			"templatePath":  "@hubspot/email/dnd/Start_from_scratch.html",
			"styleSettings": map[string]any{"backgroundColor": "#ffffff"},
			"flexAreas":     map[string]any{"main": map[string]any{"boxed": true, "sections": sections}},
			"widgets":       r.widgets,
		},
	})
	return payload
}

// applyContent replays a content PATCH the way HubSpot does: the submitted content REPLACES the
// draft's, and what it did not name is gone. It then classifies the write by what changed — a
// tagged body carries utm_ parameters, a freshly applied one does not — since the content apply
// and the UTM tagger PATCH the same path.
func (r *hubspotRec) applyContent(raw string, content map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seed()

	keys := make([]string, 0, len(content))
	for k := range content {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	r.contentKeys = keys

	before := r.richBodies()
	submitted, _ := content["widgets"].(map[string]any)
	next := make(map[string]map[string]any, len(submitted))
	for key, v := range submitted {
		wm, _ := v.(map[string]any)
		next[key] = wm
	}
	for key := range r.widgets {
		if _, kept := next[key]; !kept {
			r.dropped = append(r.dropped, key)
		}
	}
	r.widgets = next

	for key, html := range r.richBodies() {
		if before[key] == html {
			continue
		}
		if strings.Contains(html, "utm_") {
			r.taggedHTML = raw
			continue
		}
		r.bodyHTMLSet = html
		r.bodyWidget = key
	}
}

// richBodies is the draft's rich-text bodies keyed by module id. Callers hold the lock.
func (r *hubspotRec) richBodies() map[string]string {
	out := make(map[string]string, len(r.widgets))
	for key, w := range r.widgets {
		body, _ := w["body"].(map[string]any)
		if html, ok := body["html"].(string); ok {
			out[key] = html
		}
	}
	return out
}

// snapshotContent is the subject and the body html the content apply wrote.
func (r *hubspotRec) snapshotContent() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.subjectSet, r.bodyHTMLSet
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

// BodyWidget is the module id the content apply chose to write into.
func (r *hubspotRec) BodyWidget() string { r.mu.Lock(); defer r.mu.Unlock(); return r.bodyWidget }

// Dropped names the widgets a content PATCH removed from the draft. Any name here is content an
// operator lost.
func (r *hubspotRec) Dropped() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dropped...)
}

// ContentKeys is the key set of the last content PATCH's content object.
func (r *hubspotRec) ContentKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.contentKeys...)
}

// WidgetBody is one module's current body field, and whether the draft still carries the module.
func (r *hubspotRec) WidgetBody(key, field string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.widgets[key]
	if !ok {
		return "", false
	}
	body, _ := w["body"].(map[string]any)
	v, _ := body[field].(string)
	return v, true
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
			_, _ = w.Write(rec.draftPayload())
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/marketing/v3/emails/") && strings.HasSuffix(r.URL.Path, "/draft"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			// The send-list PATCH, the subject PATCH and the content PATCHes hit the same path;
			// tell them apart by which key the payload carries rather than by call order.
			switch {
			case body["content"] != nil:
				content, _ := body["content"].(map[string]any)
				rec.applyContent(string(raw), content)
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

// An EMPTY second rich-text block does not stop the write; it is simply not the FIRST block.
//
// This shape used to be refused outright: `GetEmailHTMLWidgets` counted the empty block (rightly
// — an empty block is one an operator can see and fill), the count came to two, and the
// single-block guard declined. Refusing was never safe by comparison, only inert: the operator
// got a draft carrying the template's placeholder copy and no sign the generated body existed.
// Now the body goes to the block at the top and the empty one stays empty.
func TestHubSpot_EmptySecondBlockIsNotTheFirstBlock(t *testing.T) {
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
		t.Errorf("subject = %q, want the generated subject", subject)
	}
	if !strings.Contains(body, "Join us") {
		t.Errorf("body = %q, want the generated body written into the first block", body)
	}
	if got := rec.BodyWidget(); got != "module_1" {
		t.Errorf("wrote into %q, want the first block module_1", got)
	}
	if got, ok := rec.WidgetBody("module_2", "html"); !ok || got != "   " {
		t.Errorf("second block html = %q (present=%v), want it left exactly as the template had it", got, ok)
	}
}

// A template with SEVERAL rich-text blocks gets the generated body in the FIRST one, and keeps
// every other block verbatim.
//
// This is the ordinary case, not an edge one: every template in the LF portal has nine or so
// blocks. It used to be refused on the grounds that choosing between them was a guess — so the
// generated copy an operator reviewed and staged reached no real template at all. The first
// block is the top of the email, which is where a lede goes; the blocks below it are programme
// details, sponsor tiers and footers that the copy was never meant to replace, and this pins
// that they are untouched rather than merely "probably fine".
func TestHubSpot_WritesFirstBlockAndKeepsTheRest(t *testing.T) {
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
		t.Errorf("subject = %q, want the generated subject", subject)
	}
	if !strings.Contains(body, "Join us") {
		t.Errorf("body = %q, want the generated body written into the first block", body)
	}
	if got := rec.BodyWidget(); got != "module_1" {
		t.Errorf("wrote into %q, want the first block module_1", got)
	}
	if got, ok := rec.WidgetBody("module_2", "html"); !ok || got != "<p>second block</p>" {
		t.Errorf("second block html = %q (present=%v), want the template's own copy untouched", got, ok)
	}
}

// FIRST means first in the LAYOUT, not first by map key.
//
// The layout here places module_2 above module_1 while the keys sort the other way, so a caller
// sorting keys — or iterating the map, which Go randomizes — names the wrong block. Only
// content.flexAreas says which block is at the top of the email, and this is the one test that
// can tell the two apart.
func TestHubSpot_FirstBlockComesFromTheLayoutNotTheKeys(t *testing.T) {
	srv, rec := hubspotServer(t)
	rec.reversedLayout = true
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	cfg := json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555","bodyHtml":"<p>Join us</p>"}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if got := rec.BodyWidget(); got != "module_2" {
		t.Errorf("wrote into %q, want module_2 — the block the layout places at the top", got)
	}
	if got, ok := rec.WidgetBody("module_1", "html"); !ok || !strings.Contains(got, "events.lfx.dev/reg") {
		t.Errorf("module_1 html = %q (present=%v), want the template's own copy left below the lede", got, ok)
	}
}

// A content write must DROP NOTHING — not one widget, not one field of the content object.
//
// This is the bug that produced an email with nothing inside it. HubSpot does not merge
// `content` on a drag-and-drop email: it takes what is submitted as authoritative. The client
// PATCHed only the widgets it had rewritten, so the draft came back holding those alone; the
// layout tree still referenced all 32 placed modules, none of which existed any more, and the
// operator opened 14 empty sections. Two writes happen in this flow (the body apply, then the
// UTM tagger), and either one is enough to do it.
//
// The draft here carries a rich-text block, a second rich-text block, a header IMAGE and an
// unplaced template-level module, so the assertion covers a widget that was written, one that
// was not, one that is not rich text at all, and one the layout never places.
func TestHubSpot_ContentWriteDropsNothing(t *testing.T) {
	srv, rec := hubspotServer(t)
	rec.extraWidget = true
	rec.imageWidget = true
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	cfg := json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555","subject":"Three days in Amsterdam","bodyHtml":"<p>Join us</p><a href=\"https://events.lfx.dev/reg\">Register</a>"}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if dropped := rec.Dropped(); len(dropped) != 0 {
		t.Errorf("a content write removed %v from the draft; every widget must be sent back", dropped)
	}
	// The layout tree and the template's styling travel in the same content object. Omitting
	// them is the same loss by another route: the sections would have nothing to lay out.
	keys := rec.ContentKeys()
	for _, want := range []string{"flexAreas", "styleSettings", "templatePath", "widgets"} {
		if !slices.Contains(keys, want) {
			t.Errorf("content PATCH omitted %q; sent %v", want, keys)
		}
	}
	// Per-widget scaffolding matters as much as the widget's presence: HubSpot discarded the two
	// widgets the old client DID name because they arrived as a bare {body:{html}}.
	if got, ok := rec.WidgetBody("module_hdr", "src"); !ok || got != "https://img.example/logo.png" {
		t.Errorf("header image src = %q (present=%v), want the image module preserved whole", got, ok)
	}
	if got, ok := rec.WidgetBody("preview_text", "value"); !ok || got != "See you there" {
		t.Errorf("preview_text value = %q (present=%v), want the unplaced template module preserved", got, ok)
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
