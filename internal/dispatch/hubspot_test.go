// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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

func hubspotServer(t *testing.T) (*httptest.Server, *hubspotRec) {
	t.Helper()
	rec := &hubspotRec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/marketing/v3/emails/clone":
			rec.markClone()
			_, _ = io.WriteString(w, `{"id":"999","name":"KubeCon NA 2026 — brief-1","state":"DRAFT"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/marketing/v3/emails/") && strings.HasSuffix(r.URL.Path, "/draft"):
			// The draft body the UTM tagger reads: one rich-text widget with a bare link.
			_, _ = io.WriteString(w, `{"content":{"widgets":{"module_1":{"body":{"html":"<a href=\"https://events.lfx.dev/reg\">Register</a>"}}}}}`)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/marketing/v3/emails/") && strings.HasSuffix(r.URL.Path, "/draft"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			// The send-list PATCH and the UTM PATCH hit the same path; tell them apart by
			// which key the payload carries rather than by call order.
			if _, isContent := body["content"]; isContent {
				rec.markTagged(string(raw))
			} else {
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
