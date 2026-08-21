// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

const goodMetaCreds = `{"AccessToken":"tok"}`

func activeMetaConn(creds string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderMetaAds,
		AccountID:            "act_777",
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"page_id": "987654321"},
		Status:               model.StatusActive,
	}
}

// ---- pre-create paths -----------------------------------------------------

func TestMeta_PreCreateErrorsReleaseClaim(t *testing.T) {
	cases := []struct {
		name string
		repo connReader
		enc  domain.Encryptor
	}{
		{"missing connection", fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}},
		{"no stored credentials", fakeConnReader{conn: &model.Connection{Provider: model.ProviderMetaAds, Status: model.StatusActive}}, identityEncryptor{}},
		{"decrypt fails", fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, errEncryptor{}},
		{"empty access token", fakeConnReader{conn: activeMetaConn(`{"AccessToken":""}`)}, identityEncryptor{}},
		{"inactive connection", fakeConnReader{conn: &model.Connection{Provider: model.ProviderMetaAds, AccountID: "act_1", EncryptedCredentials: []byte(goodMetaCreds), ProviderConfig: map[string]string{"page_id": "p"}, Status: model.StatusInactive}}, identityEncryptor{}},
		{"missing page id", fakeConnReader{conn: &model.Connection{Provider: model.ProviderMetaAds, AccountID: "act_1", EncryptedCredentials: []byte(goodMetaCreds), Status: model.StatusActive}}, identityEncryptor{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewMetaDispatcher(tc.repo, tc.enc)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, nil)
			var nuc interface{ NoUpstreamCreate() bool }
			if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("a pre-create failure must be NoUpstreamCreate, got %T: %v", err, err)
			}
		})
	}
}

// TestMeta_UnusableReasonsAreClassified pins the sentinel/reason contract shared by
// resolveMetaCredentials across all three entry points — Dispatch, ToggleStatus, and
// ReadMetrics must each surface the same domain.ErrConnectionNotUsable + specific reason
// sentinel for a given stored-connection defect, mirroring
// TestGoogleAds_ListAccounts_UnusableReasonsAreClassifiedWithoutPlaintext. Account-id and
// page-id checks are intentionally NOT covered here — those are Dispatch-only (see
// TestMeta_DispatchRequiresAccountID and TestMeta_ToggleStatus_NoPageIDNeeded /
// TestMeta_ToggleStatus_NoAccountIDNeeded).
func TestMeta_UnusableReasonsAreClassified(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.Connection)
		want   error
	}{
		{
			name:   "inactive",
			mutate: func(c *model.Connection) { c.Status = model.StatusInactive },
			want:   domain.ErrConnectionInactive,
		},
		{
			name:   "undecodable credentials",
			mutate: func(c *model.Connection) { c.EncryptedCredentials = []byte(`{"AccessToken":`) },
			want:   domain.ErrCredentialsUndecodable,
		},
		{
			name:   "incomplete credentials",
			mutate: func(c *model.Connection) { c.EncryptedCredentials = []byte(`{"AccessToken":""}`) },
			want:   domain.ErrCredentialsIncomplete,
		},
	}
	entryPoints := []struct {
		name string
		call func(d *MetaDispatcher, conn *model.Connection) error
	}{
		{"Dispatch", func(d *MetaDispatcher, _ *model.Connection) error {
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, nil)
			return err
		}},
		{"ToggleStatus", func(d *MetaDispatcher, _ *model.Connection) error {
			return d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, metaToggleCampaign("23847290", "999"), model.CampaignRunPaused)
		}},
		{"ReadMetrics", func(d *MetaDispatcher, _ *model.Connection) error {
			camp := &model.Campaign{Platform: model.ProviderMetaAds, PlatformCampaignID: "777"}
			_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMetaAds, camp, model.MetricsWindowLast30Days)
			return err
		}},
	}
	for _, tc := range cases {
		for _, ep := range entryPoints {
			t.Run(tc.name+"/"+ep.name, func(t *testing.T) {
				conn := activeMetaConn(goodMetaCreds)
				tc.mutate(conn)
				d := NewMetaDispatcher(fakeConnReader{conn: conn}, identityEncryptor{})
				err := ep.call(d, conn)
				if err == nil {
					t.Fatal("expected a connection error, got nil")
				}
				if !errors.Is(err, tc.want) {
					t.Errorf("error = %v, want errors.Is(err, %v)", err, tc.want)
				}
				if !errors.Is(err, domain.ErrConnectionNotUsable) {
					t.Errorf("error = %v, want errors.Is(err, domain.ErrConnectionNotUsable)", err)
				}
			})
		}
	}
}

// TestMeta_DispatchRequiresPageID pins the SENTINELS on the missing-page-id refusal, which
// nothing else does. TestMeta_PreCreateErrorsReleaseClaim carries a "missing page id" case
// but asserts only NoUpstreamCreate, and TestMeta_UnusableReasonsAreClassified excludes the
// page-id check by design (it is Dispatch-only, so it is not part of the contract shared
// across the three entry points). Between them, dropping either sentinel from the refusal
// would leave the whole suite green while turning the async job's logged reason into
// `unclassified` — and the reason token is the ONLY thing that reaches a human here, since
// dispatchPlatform collapses every dispatcher error into one job-result string.
func TestMeta_DispatchRequiresPageID(t *testing.T) {
	conn := activeMetaConn(goodMetaCreds)
	conn.ProviderConfig = nil
	d := NewMetaDispatcher(fakeConnReader{conn: conn}, identityEncryptor{})
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, nil)
	if !errors.Is(err, domain.ErrProviderConfigInvalid) {
		t.Errorf("error = %v, want errors.Is(err, domain.ErrProviderConfigInvalid): without "+
			"this sentinel the async failure log reads `unclassified` for a missing page id", err)
	}
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Errorf("error = %v, want errors.Is(err, domain.ErrConnectionNotUsable)", err)
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a missing page id must be NoUpstreamCreate, got %T: %v", err, err)
	}
}

// TestMeta_DispatchRequiresAccountID proves Dispatch (unlike ToggleStatus/ReadMetrics)
// refuses a connection with no account selected, tagged as account_not_selected — it builds
// Graph paths as /{accountID}/campaigns and needs a real one.
func TestMeta_DispatchRequiresAccountID(t *testing.T) {
	conn := activeMetaConn(goodMetaCreds)
	conn.AccountID = ""
	d := NewMetaDispatcher(fakeConnReader{conn: conn}, identityEncryptor{})
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, nil)
	if !errors.Is(err, domain.ErrAccountNotSelected) {
		t.Errorf("error = %v, want errors.Is(err, domain.ErrAccountNotSelected)", err)
	}
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Errorf("error = %v, want errors.Is(err, domain.ErrConnectionNotUsable)", err)
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a missing account id must be NoUpstreamCreate, got %T: %v", err, err)
	}
}

// TestMeta_ToggleStatus_NoAccountIDNeeded proves a status update works on a connection
// with no account id selected — the pause/resume/read paths target an existing campaign by
// platform id and never read AccountConfig.AccountID, so account selection can be cleared
// via PUT after a campaign was already created without blocking these operations.
func TestMeta_ToggleStatus_NoAccountIDNeeded(t *testing.T) {
	conn := activeMetaConn(goodMetaCreds)
	conn.AccountID = ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(
		fakeConnReader{conn: conn}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, &model.Campaign{PlatformCampaignID: "23847290"}, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus must work without an account id: %v", err)
	}
}

// TestMeta_ReadMetrics_NoAccountIDNeeded is ReadMetrics' half of the same contract as
// TestMeta_ToggleStatus_NoAccountIDNeeded.
func TestMeta_ReadMetrics_NoAccountIDNeeded(t *testing.T) {
	conn := activeMetaConn(goodMetaCreds)
	conn.AccountID = ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"1","clicks":"1","spend":"1.00"}]}`)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	camp := &model.Campaign{Platform: model.ProviderMetaAds, PlatformCampaignID: "777"}
	if _, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMetaAds, camp, model.MetricsWindowLast30Days); err != nil {
		t.Fatalf("ReadMetrics must work without an account id: %v", err)
	}
}

func TestMeta_BadConfigIsPreCreate(t *testing.T) {
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{})
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, json.RawMessage(`{bad`))
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a malformed config must be a pre-create error, got %T: %v", err, err)
	}
}

// TestMeta_ClientPreCreateRejectionReleasesClaim exercises the `result == nil`
// RELEASE branch (meta.go: "failed before any upstream create" -> notCreated), which
// the other pre-create tests don't reach — they fail during envelope decode or before
// the client is called. Here the connection is active and the config is syntactically
// valid and passes the dispatcher's own checks, so the flow reaches the real Meta
// client; the client then rejects it BEFORE its first upstream create because it
// carries no ad variants (client.go: "at least one ad variant is required"), returning
// (nil, err). The adapter must map that to a NoUpstreamCreate error so the orchestrator
// RELEASES the claim (nothing was created upstream) — the release half of the
// client-result contract.
func TestMeta_ClientPreCreateRejectionReleasesClaim(t *testing.T) {
	// A server that fails any request, proving the rejection happens BEFORE the client
	// issues its first create (no request should reach here).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("client must reject the variant-less config before any upstream HTTP call")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	// Valid budget/dates/objective but NO variants — passes envelope decode + the
	// dispatcher's checks, reaches the client, and is rejected pre-create.
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if camp != nil {
		t.Errorf("a pre-create rejection must return a nil campaign, got %+v", camp)
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a client pre-create rejection must be NoUpstreamCreate (release the claim), got %T: %v", err, err)
	}
}

// ---- happy path through an httptest meta API ------------------------------

func TestMeta_DispatchSuccessMapsResult(t *testing.T) {
	// Capture every mutating request path + body so we can assert the full mapping
	// contract, not just the returned id. A mapping regression (a dropped pixel,
	// placements, lifetime budget, account/page id, or geo target) must fail this test
	// rather than quietly create a materially different paid campaign.
	type captured struct {
		path string
		body string
	}
	var (
		mu                   sync.Mutex
		reqs                 []captured
		creativeCount, adCnt int32
	)
	record := func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, captured{path: r.URL.Path, body: string(b)})
		mu.Unlock()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			// The by-name reconcile lookup. It does NOT fire on this path today — the
			// dispatcher leaves ReconcileByName unset, which
			// TestMeta_DispatchNeverOptsIntoNameReconciliation pins. This arm exists
			// so that opting in would not fail here with a confusing 404 instead.
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			record(r)
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			record(r)
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			record(r)
			n := atomic.AddInt32(&creativeCount, 1)
			_, _ = io.WriteString(w, `{"id":"creative_`+strconv.Itoa(int(n))+`"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			record(r)
			n := atomic.AddInt32(&adCnt, 1)
			_, _ = io.WriteString(w, `{"id":"ad_`+strconv.Itoa(int(n))+`"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(clock),
	)
	// NON-DEFAULT values for every mapped field: a lifetime budget, an explicit
	// objective ("conversions" → OUTCOME_SALES + a numeric pixel promoted object), two
	// geo targets, an explicit facebook-ONLY placement (InstagramFeed:false, overriding
	// the client default that enables both feeds), and two variants (→ two creatives +
	// two ads). currencyOffset set → preflight skips FX derivation.
	//
	// NOTE the placement keys: metaConfig.Placements is a meta.Placement, which has NO
	// json tags, so the JSON keys are the Go field names (FacebookFeed/InstagramFeed) —
	// lowercase "facebook"/"instagram" would be silently ignored and the client would
	// apply its both-feeds default. We assert below that instagram is actually absent.
	cfg := json.RawMessage(`{"metaConfig":{
		"budget":2500,"lifetimeBudget":true,"startDate":"2099-01-01","endDate":"2099-02-01",
		"objective":"conversions","geoTargets":["US","GB"],"currencyOffset":100,
		"pixelId":"555000111","placements":{"FacebookFeed":true,"InstagramFeed":false},
		"variants":[
			{"headline":"KubeCon 2099","primaryText":"Join us — it's great","description":"Cloud native event"},
			{"headline":"Register now","primaryText":"Early bird pricing","description":"Save your seat"}
		]
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "120100000000123" {
		t.Fatalf("adapter must map the upstream campaign id, got %+v", camp)
	}
	if camp.CampaignName == "" || len(camp.Result) == 0 {
		t.Error("campaign name + result blob should be populated")
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("success status = %q, want %q", camp.Status, campaignStatusCreated)
	}
	// Persistence-contract columns populated from the config (config uses a LIFETIME
	// budget of 2500, so BudgetType must be lifetime — not left NULL or daily).
	if camp.BudgetAmount == nil || *camp.BudgetAmount != 2500 {
		t.Errorf("BudgetAmount = %v, want 2500", camp.BudgetAmount)
	}
	if camp.BudgetType == nil || *camp.BudgetType != model.BudgetLifetime {
		t.Errorf("BudgetType = %v, want lifetime (lifetimeBudget:true)", camp.BudgetType)
	}
	if camp.StartDate == nil || camp.StartDate.Format("2006-01-02") != "2099-01-01" {
		t.Errorf("StartDate = %v, want 2099-01-01", camp.StartDate)
	}
	if camp.EndDate == nil || camp.EndDate.Format("2006-01-02") != "2099-02-01" {
		t.Errorf("EndDate = %v, want 2099-02-01", camp.EndDate)
	}
	if len(camp.ConfigSnapshot) == 0 {
		t.Error("ConfigSnapshot should capture the validated meta config")
	}

	// Per-variant fan-out: two variants → two creatives + two ads.
	if got := atomic.LoadInt32(&creativeCount); got != 2 {
		t.Errorf("adcreatives created = %d, want 2 (one per variant)", got)
	}
	if got := atomic.LoadInt32(&adCnt); got != 2 {
		t.Errorf("ads created = %d, want 2 (one per variant)", got)
	}

	// Assert the mapped fields landed in the outbound request bodies. We match on the
	// account/page ids and each config field so a dropped mapping fails loudly.
	mu.Lock()
	defer mu.Unlock()
	find := func(suffix string) string {
		for _, rq := range reqs {
			if strings.HasSuffix(rq.path, suffix) {
				return rq.body
			}
		}
		return ""
	}
	// Every mutating request must target the connection's ad account (act_777) in its path.
	for _, rq := range reqs {
		if !strings.Contains(rq.path, "act_777") {
			t.Errorf("request path %q does not target the connection account act_777", rq.path)
		}
	}
	// Campaign body carries the mapped objective ("conversions" → OUTCOME_SALES).
	campBody := find("/campaigns")
	if !strings.Contains(campBody, "OUTCOME_SALES") {
		t.Errorf("campaign body missing objective OUTCOME_SALES (from \"conversions\")\nbody: %s", campBody)
	}
	// Ad-set body carries budget (lifetime, in minor units = 2500*100), geo countries,
	// and the pixel promoted object (conversions objective).
	adsetBody := find("/adsets")
	if !strings.Contains(adsetBody, `"lifetime_budget"`) {
		t.Errorf("adset body should use lifetime_budget (lifetimeBudget:true)\nbody: %s", adsetBody)
	}
	if strings.Contains(adsetBody, `"daily_budget"`) {
		t.Errorf("adset body must NOT use daily_budget when lifetimeBudget:true\nbody: %s", adsetBody)
	}
	if !strings.Contains(adsetBody, "250000") { // 2500 * currencyOffset(100)
		t.Errorf("adset body missing minor-unit budget 250000 (2500 * offset 100)\nbody: %s", adsetBody)
	}
	for _, want := range []string{"US", "GB"} { // geo targets
		if !strings.Contains(adsetBody, want) {
			t.Errorf("adset body missing geo target %q\nbody: %s", want, adsetBody)
		}
	}
	if !strings.Contains(adsetBody, "555000111") { // numeric pixel on promoted_object
		t.Errorf("adset body missing the pixel id 555000111\nbody: %s", adsetBody)
	}
	// facebook-only placement (InstagramFeed:false) → targeting must include facebook
	// and must NOT include instagram. Asserting the ABSENCE is what proves the override
	// was honored (a silently-ignored placement key would leave the default both-feeds
	// on and instagram would appear here).
	if !strings.Contains(adsetBody, "facebook") {
		t.Errorf("adset targeting missing the facebook placement\nbody: %s", adsetBody)
	}
	if strings.Contains(adsetBody, "instagram") {
		t.Errorf("adset targeting must NOT include instagram (InstagramFeed:false)\nbody: %s", adsetBody)
	}
	// The connection's page id (987654321) rides on each creative's object_story_spec.
	creativeBody := find("/adcreatives")
	if !strings.Contains(creativeBody, "987654321") {
		t.Errorf("creative object_story_spec missing the connection page id 987654321\nbody: %s", creativeBody)
	}
}

// TestMeta_DispatchNeverOptsIntoNameReconciliation pins the DORMANCY of the by-name
// reconcile at the caller boundary, which is where it is actually decided.
//
// meta.Client gates the lookup on CampaignInput.ReconcileByName, and
// TestCreateCampaignWithoutReconcileByNameDoesNotLookUpOrReuse (in the meta package)
// proves the gate holds when the flag is unset. Neither of those says anything about
// what THIS dispatcher passes — and the dispatcher is the only production caller. It
// must leave the flag false: buildCampaignName is event/region/objective/project only,
// so a lookup here would reuse a campaign belonging to a different brief that happens
// to share those four segments, and would defeat the documented delete → re-dispatch
// flow by silently reusing a campaign created with the wrong budget. Setting the flag
// is LFXV2-2665's reconcile path, which knows it is resuming one dispatch generation;
// a blanket dispatch never does.
func TestMeta_DispatchNeverOptsIntoNameReconciliation(t *testing.T) {
	var lookups int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			// The by-name lookup, for both the campaign and the ad set. Answer it
			// "no match" so the dispatch still completes and the assertion below is
			// about the CALL being made, not about the create failing for some
			// unrelated reason.
			atomic.AddInt32(&lookups, 1)
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(clock),
	)
	cfg := json.RawMessage(`{"metaConfig":{
		"budget":2500,"startDate":"2099-01-01","endDate":"2099-02-01","currencyOffset":100,
		"variants":[{"headline":"KubeCon 2099","primaryText":"Join us","description":"Cloud native event"}]
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "120100000000123" {
		t.Fatalf("dispatch must have reached the create; got %+v", camp)
	}
	if got := atomic.LoadInt32(&lookups); got != 0 {
		t.Errorf("dispatch issued %d by-name lookup(s), want 0: the ordinary dispatch path must "+
			"not set meta.CampaignInput.ReconcileByName — the campaign name is not brief-unique, "+
			"so reuse can attach a second brief to one upstream campaign and can silently re-run "+
			"a budget that delete → re-dispatch was meant to correct", got)
	}
}

func TestMeta_DegradedSuccessSetsCreatedDegraded(t *testing.T) {
	// Two variants requested, but the SECOND ad POST fails (Meta rejects it). Meta
	// treats per-variant ad failures as non-fatal, so CreateCampaign returns
	// (result, nil) with AdCount=1 < 2 — a DEGRADED success. The adapter must persist
	// created_degraded, not a clean created (which would let idempotency block a
	// re-dispatch while the missing ad is only visible inside the result blob).
	var adCnt int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			// The by-name reconcile lookup. It does NOT fire on this path today — the
			// dispatcher leaves ReconcileByName unset, which
			// TestMeta_DispatchNeverOptsIntoNameReconciliation pins. This arm exists
			// so that opting in would not fail here with a confusing 404 instead.
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			// First ad succeeds; the second is rejected → AdCount ends at 1 of 2.
			if atomic.AddInt32(&adCnt, 1) == 1 {
				_, _ = io.WriteString(w, `{"id":"ad_1"}`)
			} else {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"rejected"}}`)
			}
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"],"currencyOffset":100,"variants":[
		{"headline":"A","primaryText":"first","description":"d1"},
		{"headline":"B","primaryText":"second","description":"d2"}
	]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if err != nil {
		t.Fatalf("a degraded success (campaign created, one ad failed) must NOT error: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "120100000000123" {
		t.Fatalf("the created campaign must still be mapped, got %+v", camp)
	}
	if camp.Status != campaignStatusCreatedDegraded {
		t.Errorf("status = %q, want %q (fewer ads created than requested is a degraded success)", camp.Status, campaignStatusCreatedDegraded)
	}
}

func TestMeta_ConfigHSTokenTakesPrecedence(t *testing.T) {
	// config.hsToken is a documented top-level field and must drive utm_campaign,
	// taking precedence over any brief token — not be silently ignored.
	var creativeBody string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			// The by-name reconcile lookup. It does NOT fire on this path today — the
			// dispatcher leaves ReconcileByName unset, which
			// TestMeta_DispatchNeverOptsIntoNameReconciliation pins. This arm exists
			// so that opting in would not fail here with a confusing 404 instead.
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			creativeBody = string(b) // the creative carries the utm link
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	cfg := json.RawMessage(`{"hsToken":"HS-FROM-CONFIG","metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"],"currencyOffset":100,"variants":[{"headline":"A","primaryText":"first","description":"d1"}]}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	got := creativeBody
	mu.Unlock()
	if !strings.Contains(got, "HS-FROM-CONFIG") {
		t.Errorf("config.hsToken must drive utm_campaign; creative link did not carry it: %q", got)
	}
}

func TestMeta_AmbiguousCreateRetainsClaim(t *testing.T) {
	// A 5xx on the campaign POST is ambiguous → the meta client returns a non-nil
	// name-only partial (empty CampaignID). The adapter must retain the claim.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status") {
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway) // ambiguous 5xx on the campaign POST
	}))
	defer srv.Close()
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(clock),
	)
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"],"currencyOffset":100,"variants":[{"headline":"KubeCon 2099","primaryText":"Join us — it's great","description":"x"}]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if err == nil {
		t.Fatal("expected an error from an ambiguous create")
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
		t.Error("an ambiguous create must NOT be NoUpstreamCreate — the claim must be retained")
	}
	if camp == nil {
		t.Fatal("an ambiguous create must return a non-nil campaign for orphan recording")
	}
	// The name-only reconciliation contract: no upstream id was confirmed, but the
	// deterministic campaign name + result blob must survive so the orphan can be
	// reconciled on retry. A regression that dropped these — or wrongly populated an
	// upstream id — must fail here.
	if camp.PlatformCampaignID != "" {
		t.Errorf("an ambiguous create must NOT report an upstream campaign id, got %q", camp.PlatformCampaignID)
	}
	if camp.CampaignName == "" {
		t.Error("an ambiguous create must retain the deterministic campaign name for reconciliation")
	}
	if len(camp.Result) == 0 {
		t.Error("an ambiguous create must retain the result blob for reconciliation")
	}
}

// metaToggleCampaign builds a persisted *model.Campaign carrying the ad set id in Result, as
// the meta create path stores it (CampaignResult.AdSetID, no json tag → field name).
func metaToggleCampaign(campaignID, adSetID string) *model.Campaign {
	return &model.Campaign{
		PlatformCampaignID: campaignID,
		Result:             []byte(`{"CampaignID":"` + campaignID + `","AdSetID":"` + adSetID + `"}`),
	}
}

// TestMeta_ToggleStatus_CascadesToTree verifies the dispatcher POSTs the status to the
// campaign, its ad set, AND each ad discovered under the ad set (all three are PAUSED at
// creation, so a partial toggle would not serve).
func TestMeta_ToggleStatus_CascadesToTree(t *testing.T) {
	// Capture requests over a channel so handler writes happen-before test reads (race-safe).
	type req struct{ method, path, status string }
	gotCh := make(chan req, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ads") {
			gotCh <- req{r.Method, r.URL.Path, ""}
			_, _ = io.WriteString(w, `{"data":[{"id":"555"},{"id":"666"}]}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		status, _ := body["status"].(string)
		gotCh <- req{r.Method, r.URL.Path, status}
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, metaToggleCampaign("23847290", "999"), model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	close(gotCh)
	var seen []req
	for r := range gotCh {
		seen = append(seen, r)
	}
	// campaign POST, ad set POST, ads GET, then one POST per discovered ad (555, 666).
	if len(seen) != 5 {
		t.Fatalf("issued %d requests, want 5 (campaign, adset, ads GET, 2 ad POSTs): %+v", len(seen), seen)
	}
	wantPost := map[string]bool{"/23847290": false, "/999": false, "/555": false, "/666": false}
	sawAdsGet := false
	for _, r := range seen {
		if r.method == http.MethodGet && strings.HasSuffix(r.path, "/999/ads") {
			sawAdsGet = true
			continue
		}
		if _, ok := wantPost[r.path]; !ok {
			t.Errorf("unexpected request: %+v", r)
			continue
		}
		if r.status != "ACTIVE" {
			t.Errorf("POST %s status = %q, want ACTIVE", r.path, r.status)
		}
		wantPost[r.path] = true
	}
	if !sawAdsGet {
		t.Error("did not issue GET /999/ads to discover the ads")
	}
	for p, hit := range wantPost {
		if !hit {
			t.Errorf("expected a POST to %s", p)
		}
	}
	// An unsupported run state is rejected before any call.
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, metaToggleCampaign("23847290", "999"), "RUNNING"); err == nil {
		t.Error("expected an error for an unsupported run status")
	}
}

// TestMeta_ToggleStatus_ActivateWithoutAdSetRejected: activating a legacy/incomplete
// "created" campaign that has no stored ad set id must be refused before any HTTP call and
// classified as ErrCampaignNotProvisioned (→ 409), not a 503 platform failure.
func TestMeta_ToggleStatus_ActivateWithoutAdSetRejected(t *testing.T) {
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count++
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, metaToggleCampaign("23847290", ""), model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error activating a campaign with no ad set id")
	}
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Errorf("error = %v, want ErrCampaignNotProvisioned (a 409 state error, not 503)", err)
	}
	if count != 0 {
		t.Errorf("issued %d requests, want 0 (rejected before any HTTP call)", count)
	}
}

// TestMeta_ToggleStatus_5xxIsUnconfirmed verifies a 5xx surfaces as Unconfirmed().
// TestMeta_ToggleStatus_5xxAfterMutationIsUnconfirmed: a 5xx that lands AFTER an upstream
// mutation may have committed is Unconfirmed. On PAUSE the campaign gate is flipped FIRST, so a
// subsequent 5xx (here, on ad discovery) is a partial application → Unconfirmed (verify/retry).
func TestMeta_ToggleStatus_5xxAfterMutationIsUnconfirmed(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		if n == 1 { // the PAUSE campaign flip succeeds first
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"success":true}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway) // then ad discovery 5xx — a partial application
	}))
	defer srv.Close()
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, metaToggleCampaign("23847290", "999"), model.CampaignRunPaused)
	if err == nil {
		t.Fatal("expected an error on a 5xx after the campaign was paused")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a 5xx AFTER a mutation must be Unconfirmed(), got %T: %v", err, err)
	}
}

// TestMeta_ToggleStatus_ActivateDiscovery5xxIsClean: on ACTIVATE, ad discovery (a GET) runs
// BEFORE any mutation, so a 5xx there applied nothing — it must be a CLEAN, definite failure
// (NOT Unconfirmed), so the operator gets a deterministic retry-safe error rather than a
// spurious "verify" 503. This is the discover-first contract.
func TestMeta_ToggleStatus_ActivateDiscovery5xxIsClean(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // first call is discovery on activate → clean
	}))
	defer srv.Close()
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, metaToggleCampaign("23847290", "999"), model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error on a 5xx discovery")
	}
	var unconf interface{ Unconfirmed() bool }
	if errors.As(err, &unconf) && unconf.Unconfirmed() {
		t.Errorf("a pre-mutation discovery 5xx on activate must be CLEAN, not Unconfirmed: %v", err)
	}
}

// TestMeta_ToggleStatus_NoPageIDNeeded proves a status update works with a connection that
// has an access token + account id but NO page_id (Dispatch requires page_id; a toggle must
// not) — locking in that contract against a future refactor.
func TestMeta_ToggleStatus_NoPageIDNeeded(t *testing.T) {
	conn := &model.Connection{
		Provider:             model.ProviderMetaAds,
		AccountID:            "act_1",
		EncryptedCredentials: []byte(goodMetaCreds), // {"AccessToken":"tok"} — no page_id in ProviderConfig
		Status:               model.StatusActive,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(
		fakeConnReader{conn: conn}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, &model.Campaign{PlatformCampaignID: "23847290"}, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus must work without a page_id: %v", err)
	}
}

// ---- ReadMetrics --------------------------------------------------------

func TestMeta_ReadMetrics_HappyPath(t *testing.T) {
	var mu sync.Mutex
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"1000","clicks":"40","spend":"25.00"}]}`)
	}))
	defer srv.Close()

	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	camp := &model.Campaign{Platform: model.ProviderMetaAds, PlatformCampaignID: "777"}
	m, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMetaAds, camp, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadMetrics: %v", err)
	}
	if m.CampaignID != "777" || m.Window != model.MetricsWindowLast30Days || m.Impressions != 1000 || m.Clicks != 40 || m.CostMicros != 25_000_000 {
		t.Errorf("got %+v", m)
	}
	if want := 0.04; m.Ctr != want {
		t.Errorf("Ctr = %v, want %v", m.Ctr, want)
	}
	mu.Lock()
	path := gotPath
	mu.Unlock()
	if !strings.HasPrefix(path, "/777/insights?") || !strings.Contains(path, "date_preset=last_30d") {
		t.Errorf("request path = %s", path)
	}
}

// TestMeta_ReadMetrics_ConnectionUnresolvedPropagates pins that a broken/inactive
// connection surfaces as a plain error (NOT wrapped with notCreated, unlike Dispatch) — a
// metrics read has no create-claim semantics to protect. Mirrors the Google Ads dispatcher.
func TestMeta_ReadMetrics_ConnectionUnresolvedPropagates(t *testing.T) {
	d := NewMetaDispatcher(fakeConnReader{err: errors.New("no connection")}, identityEncryptor{})
	camp := &model.Campaign{Platform: model.ProviderMetaAds, PlatformCampaignID: "777"}
	if _, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMetaAds, camp, model.MetricsWindowLast30Days); err == nil {
		t.Fatal("expected an error when the connection cannot be resolved")
	}
}

func TestMeta_ReadMetrics_InactiveConnectionErrors(t *testing.T) {
	conn := &model.Connection{
		Provider:             model.ProviderMetaAds,
		AccountID:            "act_777",
		EncryptedCredentials: []byte(goodMetaCreds),
		Status:               model.StatusInactive,
	}
	d := NewMetaDispatcher(fakeConnReader{conn: conn}, identityEncryptor{})
	camp := &model.Campaign{Platform: model.ProviderMetaAds, PlatformCampaignID: "777"}
	if _, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMetaAds, camp, model.MetricsWindowLast30Days); err == nil {
		t.Fatal("expected an error for an inactive connection")
	}
}

// ---- account discovery ----------------------------------------------------

// recordedPath carries a request path from the httptest handler back to the test body.
// The handler runs on the server's own goroutine, so a bare *string shared across that
// boundary is a data race that `go test -race` fails — the same reason the mutex in
// TestMeta_ConfigHSTokenTakesPrecedence exists.
type recordedPath struct {
	mu   sync.Mutex
	path string
}

func (p *recordedPath) set(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.path = s
}

func (p *recordedPath) get() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.path
}

// metaAccountsServer serves one page of GET /me/adaccounts with the given JSON entries and
// records the path it was asked for.
func metaAccountsServer(t *testing.T, entries string, gotPath *recordedPath) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotPath != nil {
			gotPath.set(r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[`+entries+`]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMeta_ListAccounts_AsksAboutTheTokenNotTheAccount pins the account-agnostic request
// path.
//
// The connection fixture carries `act_777`, and every other Meta call this dispatcher makes
// is scoped to that id. Discovery must not be: the question is "which accounts does this
// token reach?", and scoping it to one of the answers would narrow the response to a subset
// of the question. A regression here does not fail loudly — it returns a plausible one-account
// list — so the path itself is the assertion.
func TestMeta_ListAccounts_AsksAboutTheTokenNotTheAccount(t *testing.T) {
	var rec recordedPath
	srv := metaAccountsServer(t, `{"id":"act_111","name":"Alpha","account_status":1}`, &rec)

	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL))
	accounts, err := d.ListAccounts(context.Background(), "proj", model.ProviderMetaAds)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	gotPath := rec.get()
	if !strings.HasSuffix(gotPath, "/me/adaccounts") {
		t.Errorf("path = %q, want the account-agnostic /me/adaccounts", gotPath)
	}
	if strings.Contains(gotPath, "act_777") {
		t.Errorf("path = %q is scoped to the connection's stored account; discovery asks about the token", gotPath)
	}
	if len(accounts) != 1 || accounts[0].ID != "act_111" {
		t.Fatalf("accounts = %+v, want the one discovered act_ id verbatim", accounts)
	}
}

// TestMeta_ListAccounts_WorksBeforeAnAccountIsChosen pins the omission that makes discovery
// useful. The resolver deliberately does not check the account id: a connection that has one
// does not need to ask which accounts exist, so requiring one would make the endpoint
// reachable only by callers who no longer have the question.
//
// Before LFXV2-3061 only re-pointing could reach this, because MetaAdsConnectionConfig required
// account_id at create; the resolver was already correct for first-time bootstrap, which is why
// that ticket changed the config and the account-needing paths rather than this function. Both
// callers reach it now, and the assertion is unchanged — the record that the omission was
// deliberate rather than a gap the config happened to cover.
func TestMeta_ListAccounts_WorksBeforeAnAccountIsChosen(t *testing.T) {
	srv := metaAccountsServer(t, `{"id":"act_222","name":"Beta","account_status":1}`, nil)

	c := activeMetaConn(goodMetaCreds)
	c.AccountID = "" // not chosen yet — the exact state discovery exists to resolve

	d := NewMetaDispatcher(fakeConnReader{conn: c}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	accounts, err := d.ListAccounts(context.Background(), "proj", model.ProviderMetaAds)
	if err != nil {
		t.Fatalf("discovery must work with no account id chosen yet, got: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "act_222" {
		t.Errorf("accounts = %+v, want the one discovered account", accounts)
	}
}

// TestMeta_ListAccounts_AttributesSystemRowDefects is the Meta half of what
// TestSystemScopedCoversEveryCallerNotJustDiscovery pins for Google Ads: every stored-state
// defect the discovery resolver detects belongs to whichever row it was READ FROM.
//
// A project with no connection of its own falls back to the LF system row. Untagged, all
// three defects below reach the handler as a plain ErrConnectionNotUsable, which it answers
// 400 — "the stored meta ads connection cannot be used as configured" — to a caller whose
// project owns no connection and cannot address the reserved scope. The correct answer is the
// 500 that pages whoever installed the LF credential. The mirror half matters just as much:
// the project's OWN broken row must stay a 400, or every fixable connection becomes a page.
func TestMeta_ListAccounts_AttributesSystemRowDefects(t *testing.T) {
	defects := map[string]func() *model.Connection{
		"connection not active": func() *model.Connection {
			c := activeMetaConn(goodMetaCreds)
			c.Status = model.StatusInactive
			return c
		},
		"credentials undecodable": func() *model.Connection { return activeMetaConn(`not json`) },
		"credentials incomplete":  func() *model.Connection { return activeMetaConn(`{"AccessToken":""}`) },
	}

	for name, conn := range defects {
		t.Run(name, func(t *testing.T) {
			dispatcherFor := func(scope string) *MetaDispatcher {
				return NewMetaDispatcher(&scopedConnReader{
					rows: map[string]*model.Connection{scope: conn()},
				}, identityEncryptor{})
			}

			_, err := dispatcherFor(model.SystemProjectID).ListAccounts(
				context.Background(), "cncf", model.ProviderMetaAds)
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Fatalf("err = %v, want ErrConnectionNotUsable", err)
			}
			if !errors.Is(err, domain.ErrSystemConnectionNotUsable) {
				t.Errorf("err = %v, want it attributed to the SYSTEM connection — otherwise the "+
					"caller is told to edit a row it does not own", err)
			}

			_, err = dispatcherFor("cncf").ListAccounts(
				context.Background(), "cncf", model.ProviderMetaAds)
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Fatalf("err = %v, want ErrConnectionNotUsable", err)
			}
			if errors.Is(err, domain.ErrSystemConnectionNotUsable) {
				t.Errorf("err = %v, must not be attributed to the system account", err)
			}
		})
	}
}

// TestMeta_SystemScopedCoversEveryCallerOfResolveMetaCredentials is the same invariant as
// the test above, extended to the three callers it does NOT reach — and it exists because
// those three were broken.
//
// resolveMetaCredentials applies systemScoped in a defer, which is the right shape: a fourth
// not-usable return added later cannot forget it. But the defer read the NAMED RETURN `res`,
// and every not-usable return sets res to nil on its way out. systemScoped is a no-op on a
// nil receiver, so the tag was dropped from precisely the errors that need it, on every
// caller of this resolver: create, toggle and metrics. Nothing failed. The error was still
// correct in every other respect — right sentinels, right message, right status class — so
// the only symptom was a project on the LF fallback being told to go and edit a connection it
// does not own, while the operator who installed the LF credential was never paged.
//
// Discovery masked it until this commit: resolveMetaDiscoveryClient carried its own copy of
// the three checks, over a plain local rather than a named return, so ListAccounts tagged
// correctly while the other three did not. Collapsing the duplicate onto the shared resolver
// is what surfaced the defect, which is the argument for the collapse: two copies meant one
// path's correctness said nothing about the other's.
//
// Every caller is exercised, discovery included, so the path that was already right cannot
// regress while attention is on the three that were not.
func TestMeta_SystemScopedCoversEveryCallerOfResolveMetaCredentials(t *testing.T) {
	defects := map[string]func() *model.Connection{
		"connection not active": func() *model.Connection {
			c := activeMetaConn(goodMetaCreds)
			c.Status = model.StatusInactive
			return c
		},
		"credentials undecodable": func() *model.Connection { return activeMetaConn(`not json`) },
		"credentials incomplete":  func() *model.Connection { return activeMetaConn(`{"AccessToken":""}`) },
	}

	// testBrief()'s ProjectID is "cncf", so every caller below resolves the same scope.
	callers := map[string]func(*MetaDispatcher) error{
		"create/Dispatch": func(d *MetaDispatcher) error {
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, nil)
			return err
		},
		"toggle/ToggleStatus": func(d *MetaDispatcher) error {
			return d.ToggleStatus(context.Background(), "cncf", model.ProviderMetaAds,
				metaToggleCampaign("23847290", "999"), model.CampaignRunPaused)
		},
		"metrics/ReadMetrics": func(d *MetaDispatcher) error {
			camp := &model.Campaign{Platform: model.ProviderMetaAds, PlatformCampaignID: "777"}
			_, err := d.ReadMetrics(context.Background(), "cncf", model.ProviderMetaAds, camp,
				model.MetricsWindowLast30Days)
			return err
		},
		"discovery/ListAccounts": func(d *MetaDispatcher) error {
			_, err := d.ListAccounts(context.Background(), "cncf", model.ProviderMetaAds)
			return err
		},
	}

	for defectName, conn := range defects {
		for callerName, call := range callers {
			t.Run(defectName+"/"+callerName, func(t *testing.T) {
				dispatcherFor := func(scope string) *MetaDispatcher {
					return NewMetaDispatcher(&scopedConnReader{
						rows: map[string]*model.Connection{scope: conn()},
					}, identityEncryptor{})
				}

				err := call(dispatcherFor(model.SystemProjectID))
				if !errors.Is(err, domain.ErrConnectionNotUsable) {
					t.Fatalf("err = %v, want ErrConnectionNotUsable", err)
				}
				if !errors.Is(err, domain.ErrSystemConnectionNotUsable) {
					t.Errorf("err = %v, want it attributed to the SYSTEM connection — this caller "+
						"sends the project to fix a row it does not own", err)
				}

				// The mirror half: the project's OWN broken row must not pick up the marker,
				// or every 400 naming a connection the caller can actually fix becomes a page.
				err = call(dispatcherFor("cncf"))
				if !errors.Is(err, domain.ErrConnectionNotUsable) {
					t.Fatalf("err = %v, want ErrConnectionNotUsable", err)
				}
				if errors.Is(err, domain.ErrSystemConnectionNotUsable) {
					t.Errorf("err = %v, must not be attributed to the system account", err)
				}
			})
		}
	}
}

// TestMeta_ListAccounts_KnownBadAccountsAreLabelledNotDropped pins the picker contract.
//
// Dropping a disabled account answers "your token reaches no ad accounts" about an account
// sitting right there, sending the user to look for a permissions problem that does not
// exist. Returning it with the reason in the label is the only outcome that lets them see why
// the account they expected cannot be used — and it is the same map CreateCampaign's preflight
// refuses on, so the picker and the create path cannot disagree.
//
// The last two cases are the ones a naive "label everything non-1" would get wrong: status 0
// means Meta omitted the field, which is not a claim of disabled, and an unnamed account must
// fall back to its id because a blank row in a picker is unpickable.
func TestMeta_ListAccounts_KnownBadAccountsAreLabelledNotDropped(t *testing.T) {
	srv := metaAccountsServer(t, `
		{"id":"act_1","name":"Active One","account_status":1},
		{"id":"act_2","name":"Disabled One","account_status":2},
		{"id":"act_3","name":"Unsettled One","account_status":3},
		{"id":"act_4","name":"Closed One","account_status":101},
		{"id":"act_5","name":"Status Absent"},
		{"id":"act_6","account_status":2}`, nil)

	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL))
	accounts, err := d.ListAccounts(context.Background(), "proj", model.ProviderMetaAds)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	want := []model.AccessibleAccount{
		{ID: "act_1", Label: "Active One"},
		{ID: "act_2", Label: "Disabled One (disabled)"},
		{ID: "act_3", Label: "Unsettled One (unsettled)"},
		{ID: "act_4", Label: "Closed One (closed)"},
		{ID: "act_5", Label: "Status Absent"},
		{ID: "act_6", Label: "act_6 (disabled)"},
	}
	if len(accounts) != len(want) {
		t.Fatalf("got %d accounts, want %d — a known-bad account was filtered out: %+v", len(accounts), len(want), accounts)
	}
	for i, w := range want {
		if accounts[i] != w {
			t.Errorf("account %d = %+v, want %+v", i, accounts[i], w)
		}
	}
}

// TestMeta_ListAccounts_UndecodableBlobDropsTheUnmarshalCause pins the one value in the
// resolver derived from DECRYPTED plaintext.
//
// The error reaches the discovery handler, which logs it and describes the not-usable arm to
// the caller. Today's encoding/json happens not to quote the offending bytes for a struct of
// string fields, but that is a behaviour rather than a documented guarantee and it does not
// hold for every field type — a number decoded into a numeric field appears verbatim.
//
// The assertion is EXACT EQUALITY against the whole expected message, not a "does not
// contain the secret" check. The latter would not bind: it passes with the cause appended,
// and the point is to remove the class rather than to catch one instance of it. Equality is
// what makes any newly appended text fail, whatever it says.
//
// The expected text carries the project id. That is deliberate and not a weakening: the
// project id is a caller-supplied path parameter, not a value derived from the decrypted
// blob, and it is the one thing that tells an operator reading a log WHICH connection has to
// be re-entered. What must never appear is the *json.SyntaxError / *json.UnmarshalTypeError
// cause, and this assertion still rejects it.
func TestMeta_ListAccounts_UndecodableBlobDropsTheUnmarshalCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("reached Meta with an undecodable credential blob")
	}))
	defer srv.Close()

	c := activeMetaConn(`{"AccessToken": SUPER-SECRET-PLAINTEXT}`)
	d := NewMetaDispatcher(fakeConnReader{conn: c}, identityEncryptor{}, meta.WithBaseURL(srv.URL))

	_, err := d.ListAccounts(context.Background(), "proj", model.ProviderMetaAds)
	if err == nil {
		t.Fatal("an undecodable credential blob must not reach Meta")
	}
	want := domain.ErrConnectionNotUsable.Error() + ": " + domain.ErrCredentialsUndecodable.Error() +
		": meta credentials for project proj are not valid JSON"
	if err.Error() != want {
		t.Errorf("error = %q, want exactly %q — anything further appended would be the decode cause, "+
			"which is derived from decrypted plaintext", err.Error(), want)
	}
	if strings.Contains(err.Error(), "SUPER-SECRET-PLAINTEXT") {
		t.Error("the decrypted blob leaked into the error text")
	}
	if !errors.Is(err, domain.ErrConnectionNotUsable) || !errors.Is(err, domain.ErrCredentialsUndecodable) {
		t.Error("both sentinels must survive: the first is how the handler answers 400 rather than 503, " +
			"the second is the fixed token it logs in place of the cause")
	}
}

// TestMeta_ListAccounts_StillRejectsUnusableConnections pins the other half of dropping the
// account-id requirement: the rest of the connection contract must survive.
//
// Each case must satisfy errors.Is(err, domain.ErrConnectionNotUsable). That is not
// decoration — it is the ONLY thing the service layer has to tell "this connection needs
// editing" (400) from "Meta did not answer" (503). Without the wrap every case here lands in
// the default arm and becomes a 503: a promise that retrying might help, made about
// conditions that cannot change until a human edits the connection. A substring assertion on
// the message would not catch that, because the message is identical either way.
//
// The missing-connection case is deliberately the opposite: it must NOT be tagged, because
// domain.ErrNotFound is what the handler turns into a 404, and flattening it into "not
// usable" would tell a caller with no connection at all to go and edit one.
func TestMeta_ListAccounts_StillRejectsUnusableConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("reached Meta with an unusable connection")
	}))
	defer srv.Close()

	cases := []struct {
		name        string
		repo        connReader
		wantReason  error
		wantNotUsab bool
	}{
		{
			name: "inactive connection",
			repo: func() connReader {
				c := activeMetaConn(goodMetaCreds)
				c.Status = model.StatusInactive
				return fakeConnReader{conn: c}
			}(),
			wantReason:  domain.ErrConnectionInactive,
			wantNotUsab: true,
		},
		{
			name:        "credentials with no access token",
			repo:        fakeConnReader{conn: activeMetaConn(`{"AccessToken":"   "}`)},
			wantReason:  domain.ErrCredentialsIncomplete,
			wantNotUsab: true,
		},
		{
			name:        "no connection at all",
			repo:        fakeConnReader{err: domain.ErrNotFound},
			wantReason:  domain.ErrNotFound,
			wantNotUsab: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewMetaDispatcher(tc.repo, identityEncryptor{}, meta.WithBaseURL(srv.URL))
			_, err := d.ListAccounts(context.Background(), "proj", model.ProviderMetaAds)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, tc.wantReason) {
				t.Errorf("error %v does not carry %v; the handler classifies on the sentinel, not the text", err, tc.wantReason)
			}
			if got := errors.Is(err, domain.ErrConnectionNotUsable); got != tc.wantNotUsab {
				t.Errorf("errors.Is(err, ErrConnectionNotUsable) = %v, want %v — this decides 400 vs 404/503", got, tc.wantNotUsab)
			}
		})
	}
}

// TestMeta_ToggleStatus_ForeignAccountIs409AndNeverMutates pins the account-provenance guard
// on the TOGGLE path. Meta campaign ids are unique only WITHIN an ad account, so once a
// project's connection is re-pointed the stored id addressed against the new account can
// collide with an unrelated campaign and PAUSE OR ACTIVATE something this project does not
// own. The refusal must be a non-retryable ErrCampaignAccountMismatch (409) raised before Meta
// is contacted, and it must sit above BOTH branches.
//
// The ACTIVATE case pins the ORDERING against the ad-set guard: a foreign-account campaign
// with NO stored ad set must answer the MISMATCH, not "cannot be activated because it has no
// ad set to serve". The latter is a fact about a campaign in a different account, so it would
// explain the wrong campaign — the trap microsoft.go records at the same seam.
//
// Both blob shapes are covered: the explicit AccountID the create path stamps, and the legacy
// row that only carries the account in its Ads Manager URL's act= parameter.
func TestMeta_ToggleStatus_ForeignAccountIs409AndNeverMutates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"AccountID field", `{"CampaignID":"23847290","AdSetID":"999","AccountID":"act_999"}`},
		{"legacy act= url fallback", `{"CampaignID":"23847290","AdSetID":"999","MetaURL":"https://adsmanager.facebook.com/adsmanager/manage/campaigns?act=999"}`},
		// The ordering probe: no ad set id at all, so the not-provisioned guard would fire
		// on ACTIVATE if it ran first.
		{"AccountID field, no ad set", `{"CampaignID":"23847290","AdSetID":"","AccountID":"act_999"}`},
	} {
		for _, status := range []string{model.CampaignRunPaused, model.CampaignRunActive} {
			t.Run(tc.name+"/status="+status, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					t.Errorf("Meta must not be contacted for a campaign owned by another ad account: %s %s", r.Method, r.URL.Path)
				}))
				defer srv.Close()
				// activeMetaConn resolves to act_777; the rows above record act_999.
				d := NewMetaDispatcher(
					fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
					meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
				)
				camp := &model.Campaign{PlatformCampaignID: "23847290", Result: json.RawMessage(tc.result)}
				err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, camp, status)
				if err == nil {
					t.Fatal("expected a mismatch error")
				}
				if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
					t.Errorf("error must wrap ErrCampaignAccountMismatch (409), got %T: %v", err, err)
				}
				// The ordering assertion: the mismatch is the answer, never the narrower
				// provisioning verdict about another account's campaign.
				if errors.Is(err, domain.ErrCampaignNotProvisioned) {
					t.Errorf("a foreign-account campaign must answer the mismatch, not a provisioning verdict: %v", err)
				}
				// Assert the VALUES, in the single act_ vocabulary both sides normalise to.
				if !strings.Contains(err.Error(), "act_999") || !strings.Contains(err.Error(), "act_777") {
					t.Errorf("error must name the created account (act_999) and the resolved one (act_777), got %v", err)
				}
			})
		}
	}
}

// TestMeta_ToggleStatus_MatchingOrUnknownAccountStillToggles is the guard's other half. It
// also pins the act_ NORMALISATION: the connection stores "act_777" while a legacy MetaURL
// carries the bare digits "777", so a raw comparison would report every legacy row as a
// mismatch — a false 409 on a campaign that is perfectly in scope.
func TestMeta_ToggleStatus_MatchingOrUnknownAccountStillToggles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"matching AccountID", `{"CampaignID":"23847290","AdSetID":"999","AccountID":"act_777"}`},
		{"matching bare-digit url fallback", `{"CampaignID":"23847290","AdSetID":"999","MetaURL":"https://adsmanager.facebook.com/adsmanager/manage/campaigns?act=777"}`},
		{"no provenance recorded", `{"CampaignID":"23847290","AdSetID":"999"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var mutations int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ads") {
					_, _ = io.WriteString(w, `{"data":[{"id":"555"}]}`)
					return
				}
				mu.Lock()
				mutations++
				mu.Unlock()
				_, _ = io.WriteString(w, `{"success":true}`)
			}))
			defer srv.Close()
			d := NewMetaDispatcher(
				fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
				meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
			)
			camp := &model.Campaign{PlatformCampaignID: "23847290", Result: json.RawMessage(tc.result)}
			if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, camp, model.CampaignRunActive); err != nil {
				t.Fatalf("ToggleStatus must proceed for a matching/unknown account, got %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			// The guard let the call THROUGH. Asserting only "no error" would also pass if
			// the toggle silently did nothing.
			if mutations == 0 {
				t.Error("a matching/unknown-account campaign must actually be toggled, but no mutation reached Meta")
			}
		})
	}
}

// TestMeta_ReadMetrics_ForeignAccountIs409AndNeverQueries pins the same guard on the READ
// path: GET /{campaignID}/insights under a re-pointed connection returns either nothing — a
// false "no data" — or an unrelated campaign's numbers rendered as this campaign's
// measurement.
func TestMeta_ReadMetrics_ForeignAccountIs409AndNeverQueries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"AccountID field", `{"CampaignID":"777","AccountID":"act_999"}`},
		{"legacy act= url fallback", `{"CampaignID":"777","MetaURL":"https://adsmanager.facebook.com/adsmanager/manage/campaigns?act=999"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				t.Errorf("Meta must not be queried for a campaign owned by another ad account: %s %s", r.Method, r.URL.Path)
			}))
			defer srv.Close()
			d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
			camp := &model.Campaign{Platform: model.ProviderMetaAds, PlatformCampaignID: "777", Result: json.RawMessage(tc.result)}
			got, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMetaAds, camp, model.MetricsWindowLast30Days)
			if err == nil {
				t.Fatal("expected a mismatch error")
			}
			if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Errorf("error must wrap ErrCampaignAccountMismatch (409), got %T: %v", err, err)
			}
			if got != nil {
				t.Errorf("a refused read must return no metrics, got %+v", got)
			}
			if !strings.Contains(err.Error(), "act_999") || !strings.Contains(err.Error(), "act_777") {
				t.Errorf("error must name the created account (act_999) and the resolved one (act_777), got %v", err)
			}
		})
	}
}

// TestMeta_ReadMetrics_MatchingOrUnknownAccountStillReads is the read guard's other half: a
// row that cannot PROVE a mismatch must still be read.
func TestMeta_ReadMetrics_MatchingOrUnknownAccountStillReads(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"matching AccountID", `{"CampaignID":"777","AccountID":"act_777"}`},
		{"matching bare-digit url fallback", `{"CampaignID":"777","MetaURL":"https://adsmanager.facebook.com/adsmanager/manage/campaigns?act=777"}`},
		{"no provenance recorded", `{"CampaignID":"777"}`},
		{"unparseable result blob", `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var queried bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				queried = true
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":[{"impressions":"1000","clicks":"40","spend":"25.00"}]}`)
			}))
			defer srv.Close()
			d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
			camp := &model.Campaign{Platform: model.ProviderMetaAds, PlatformCampaignID: "777", Result: json.RawMessage(tc.result)}
			got, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMetaAds, camp, model.MetricsWindowLast30Days)
			if err != nil {
				t.Fatalf("ReadMetrics must proceed for a matching/unknown account, got %v", err)
			}
			if got == nil || got.Impressions != 1000 {
				t.Errorf("want the platform's metrics (1000 impressions), got %+v", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if !queried {
				t.Error("a matching/unknown-account campaign must actually be read, but Meta was never queried")
			}
		})
	}
}

// TestMeta_DispatchStampsCreatingAccount closes the loop the guard depends on: it drives a
// REAL create through the client and asserts the persisted Result blob records the account,
// readable by the very function the guard calls.
//
// Meta does have a MetaURL act= fallback, so a lost stamp would not disable the guard
// outright — but it would silently downgrade every new row to the legacy path, and asserting
// through metaCreationAccountID pins reader and writer to the SAME persisted shape (this
// struct is marshalled UNTAGGED, so the key is the Go field name) as well as the act_
// normalisation both sides must agree on.
func TestMeta_DispatchStampsCreatingAccount(t *testing.T) {
	var creativeCount, adCnt int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			n := atomic.AddInt32(&creativeCount, 1)
			_, _ = io.WriteString(w, `{"id":"creative_`+strconv.Itoa(int(n))+`"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			n := atomic.AddInt32(&adCnt, 1)
			_, _ = io.WriteString(w, `{"id":"ad_`+strconv.Itoa(int(n))+`"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"],"currencyOffset":100,"variants":[{"headline":"KubeCon 2099","primaryText":"Join us — it's great","description":"d"}]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// activeMetaConn resolves to act_777, so that is what a created row must record.
	if got := metaCreationAccountID(camp); got != "act_777" {
		t.Errorf("a created row must record its creating account: metaCreationAccountID = %q, want %q (blob: %s)", got, "act_777", camp.Result)
	}
}

// TestMeta_ClearedAccountWithProvenanceStillToggles pins the OTHER absence the guard must not
// punish: an empty CURRENT account id.
//
// Meta is the only platform where this is reachable. Unlike every sibling, ToggleStatus and
// ReadMetrics deliberately do NOT require an account selection — they address the campaign node
// by id and never read AccountConfig.AccountID — so a connection whose account was cleared via
// PUT can still pause a campaign and read its metrics. TestMeta_ToggleStatus_NoAccountIDNeeded
// and its ReadMetrics twin pin that contract, but both pass a campaign with NO Result blob, so
// neither reaches the combination that broke: cleared account AND a row that records
// provenance.
//
// That combination is not rare — via the MetaURL act= fallback it is nearly EVERY historical
// row — so treating "not selected" as "a different account" would 409 pause and metrics for
// campaigns that worked the day before, and would render the message as "resolves to account "
// with an empty name. An absence is not a mismatch on either side of the comparison.
func TestMeta_ClearedAccountWithProvenanceStillToggles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"explicit AccountID", `{"CampaignID":"23847290","AdSetID":"999","AccountID":"act_777"}`},
		{"url act= fallback", `{"CampaignID":"23847290","AdSetID":"999","MetaURL":"https://adsmanager.facebook.com/adsmanager/manage/campaigns?act=777"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := activeMetaConn(goodMetaCreds)
			conn.AccountID = "" // the account selection was cleared via PUT
			var mu sync.Mutex
			var mutations int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ads") {
					_, _ = io.WriteString(w, `{"data":[{"id":"555"}]}`)
					return
				}
				mu.Lock()
				mutations++
				mu.Unlock()
				_, _ = io.WriteString(w, `{"success":true}`)
			}))
			defer srv.Close()
			d := NewMetaDispatcher(
				fakeConnReader{conn: conn}, identityEncryptor{},
				meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
			)
			camp := &model.Campaign{PlatformCampaignID: "23847290", Result: json.RawMessage(tc.result)}
			if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, camp, model.CampaignRunPaused); err != nil {
				t.Fatalf("a cleared account must not 409 a campaign that records provenance, got %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			// The mutation must actually reach Meta — asserting only "no error" would pass if
			// the toggle silently did nothing.
			if mutations == 0 {
				t.Error("the toggle must reach Meta, but no mutation was issued")
			}
		})
	}
}

// TestMeta_ClearedAccountWithProvenanceStillReads is the read half of the same contract.
func TestMeta_ClearedAccountWithProvenanceStillReads(t *testing.T) {
	conn := activeMetaConn(goodMetaCreds)
	conn.AccountID = ""
	var mu sync.Mutex
	var queried bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		queried = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"1000","clicks":"40","spend":"25.00"}]}`)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	camp := &model.Campaign{
		Platform:           model.ProviderMetaAds,
		PlatformCampaignID: "777",
		Result:             json.RawMessage(`{"CampaignID":"777","AccountID":"act_777"}`),
	}
	got, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMetaAds, camp, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("a cleared account must not 409 a metrics read for a campaign that records provenance, got %v", err)
	}
	if got == nil || got.Impressions != 1000 {
		t.Errorf("want the platform's metrics (1000 impressions), got %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !queried {
		t.Error("the read must reach Meta, but it was never queried")
	}
}

// TestMetaNormalizeAccountID pins the one vocabulary both sides of the provenance comparison
// must speak, including the shapes that must normalise to "unknown" rather than to a token.
//
// The malformed cases are the load-bearing ones. A value that names no account must land in
// the guard's "" / proceed arm: returning it non-empty would make it compare unequal to every
// legitimate connection and manufacture a false 409 on a campaign nobody can re-point. They are
// unreachable behind design/connection.go's ^act_[0-9]+$ today — which is exactly why the
// helper must not silently depend on that constraint holding forever.
func TestMetaNormalizeAccountID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// The two real vocabularies that must converge: the connection's prefixed form and
		// the bare digits MetaURL's act= parameter carries.
		{"act_777", "act_777"},
		{"777", "act_777"},
		{"  act_777  ", "act_777"},
		{"  777  ", "act_777"},
		// Absence stays absence.
		{"", ""},
		{"   ", ""},
		// Names no account: a prefix with nothing after it, however it is spelled.
		{"act_", ""},
		{"act_act_777", ""},
		// Not digits: never a Meta account id, so "unknown" rather than a false mismatch.
		{"act_abc", ""},
		{"ACT_777", ""},
		{"act_77 7", ""},
		{"act_-1", ""},
	} {
		if got := normalizeMetaAccountID(tc.in); got != tc.want {
			t.Errorf("normalizeMetaAccountID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMeta_MalformedProvenanceProceedsRatherThanFalseMismatch is the consequence of the above
// stated at the guard: a row whose recorded account names nothing must be waved through as
// "unknown", not reported as a different account.
func TestMeta_MalformedProvenanceProceedsRatherThanFalseMismatch(t *testing.T) {
	for _, blob := range []string{
		`{"CampaignID":"777","AccountID":"act_"}`,
		`{"CampaignID":"777","AccountID":"act_abc"}`,
		`{"CampaignID":"777","MetaURL":"https://adsmanager.facebook.com/adsmanager/manage/campaigns?act=notanumber"}`,
	} {
		camp := &model.Campaign{PlatformCampaignID: "777", Result: json.RawMessage(blob)}
		if err := verifyMetaAccountMatch("probe", camp, "act_777"); err != nil {
			t.Errorf("a row whose provenance names no account must proceed as unknown, got %v (blob: %s)", err, blob)
		}
	}
}

// TestMeta_DispatchMapsVariantImageURL pins the WIRE CONTRACT for the per-variant
// creative image: a caller's `imageUrl` in the dispatch config must land on that
// variant's creative as link_data.picture, the documented by-URL field, with no
// /adimages round-trip (that edge documents only `bytes` and `copy_from`).
//
// This is a dispatcher-level test on purpose. meta.AdVariant carries no json tags,
// so the JSON key is matched case-insensitively against the Go field name — the
// mapping is implicit, and nothing but a test that actually sends `imageUrl` over
// the wire proves the UI's key decodes. A rename of the field would silently drop
// every image, creating link-only ads while still reporting success.
func TestMeta_DispatchMapsVariantImageURL(t *testing.T) {
	type captured struct {
		path string
		body string
	}
	var (
		mu   sync.Mutex
		reqs []captured
	)
	record := func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, captured{path: r.URL.Path, body: string(b)})
		mu.Unlock()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/adimages"):
			record(r)
			_, _ = io.WriteString(w, `{"images":{"hero.png":{"hash":"SHOULD_NOT_BE_USED"}}}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			record(r)
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(clock),
	)

	// `imageUrl` is the camelCase key the UI would send.
	cfg := json.RawMessage(`{"metaConfig":{
		"budget":2500,"startDate":"2099-01-01","endDate":"2099-02-01",
		"objective":"traffic","geoTargets":["US"],"currencyOffset":100,
		"variants":[
			{"headline":"KubeCon 2099","primaryText":"Join us","description":"Cloud native",
			 "imageUrl":"https://cdn.example.org/wire-hero.png"}
		]
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "120100000000123" {
		t.Fatalf("campaign not mapped: %+v", camp)
	}

	mu.Lock()
	defer mu.Unlock()

	find := func(suffix string) string {
		for _, rq := range reqs {
			if strings.HasSuffix(rq.path, suffix) {
				return rq.body
			}
		}
		return ""
	}

	// The undocumented upload edge must never be called.
	if imgBody := find("/adimages"); imgBody != "" {
		t.Errorf("/adimages was called; the url parameter is undocumented: %s", imgBody)
	}

	// The caller's URL landed on the creative as picture...
	creativeBody := find("/adcreatives")
	if creativeBody == "" {
		t.Fatal("no /adcreatives request captured")
	}
	if !strings.Contains(creativeBody, `"picture":"https://cdn.example.org/wire-hero.png"`) {
		t.Errorf("creative body missing link_data.picture — the config's imageUrl never reached the client: %s", creativeBody)
	}
	// ...and no image_hash accompanies it (the two are mutually exclusive).
	if strings.Contains(creativeBody, "image_hash") {
		t.Errorf("creative body sent image_hash alongside picture: %s", creativeBody)
	}
	if strings.Contains(creativeBody, "SHOULD_NOT_BE_USED") {
		t.Errorf("creative body used a hash from the unused upload edge: %s", creativeBody)
	}
	// The image URL must NOT be the creative's click destination — that is the
	// registration/UTM URL. A swap would point the ad at the image file.
	if !strings.Contains(creativeBody, `"link":"https://events.example/kc?`) {
		t.Errorf("creative body missing the registration URL as the link: %s", creativeBody)
	}
}

// The config_snapshot stored for a meta campaign must NOT carry a variant image
// URL's query/fragment. A creative image URL is caller-supplied and may be
// PRE-SIGNED — its signature is a bearer credential — and config_snapshot is
// persisted UNENCRYPTED. This is the SUCCESS path: it fires on every create with
// an image, which is why scrubbing only the error sinks never covered it. Mirrors
// TestReddit_ConfigSnapshotRedactsPostURL.
func TestMeta_ConfigSnapshotRedactsVariantImageURL(t *testing.T) {
	camp := campaignFromMeta(context.Background(),
		&meta.CampaignResult{CampaignID: "cmp_1", CampaignName: "n"},
		metaConfig{
			Budget: 10,
			Variants: []meta.AdVariant{
				{Headline: "h1", ImageURL: "https://cdn.example.org/a.png?X-Amz-Signature=SECRET_SIG&e=1"},
				{Headline: "h2", ImageURL: "https://cdn.example.org/b.png#SECRET_FRAG"},
			},
		},
	)
	if camp.ConfigSnapshot == nil {
		t.Fatal("expected a config snapshot")
	}
	// Assert the persisted VALUE, not that a sanitizer was called.
	s := string(camp.ConfigSnapshot)
	if strings.Contains(s, "SECRET_SIG") || strings.Contains(s, "X-Amz-Signature") {
		t.Errorf("config snapshot carries the pre-signed query/signature, got: %s", s)
	}
	if strings.Contains(s, "SECRET_FRAG") {
		t.Errorf("config snapshot carries the image URL fragment, got: %s", s)
	}
	// The sanitized URL must survive so the snapshot still identifies the image.
	if !strings.Contains(s, "https://cdn.example.org/a.png") {
		t.Errorf("config snapshot lost the sanitized image URL entirely, got: %s", s)
	}
	if !strings.Contains(s, "https://cdn.example.org/b.png") {
		t.Errorf("config snapshot lost the second sanitized image URL, got: %s", s)
	}
}

// Sanitizing the snapshot must not mutate the caller's config: the FULL url still
// has to reach Meta. cfg is passed by value but Variants shares a backing array, so
// an in-place scrub would silently strip the signature from the live request too.
func TestMeta_ConfigSnapshotSanitizeDoesNotMutateCallerConfig(t *testing.T) {
	const full = "https://cdn.example.org/a.png?X-Amz-Signature=SECRET_SIG"
	variants := []meta.AdVariant{{Headline: "h1", ImageURL: full}}
	cfg := metaConfig{Budget: 10, Variants: variants}

	campaignFromMeta(context.Background(), &meta.CampaignResult{CampaignID: "c", CampaignName: "n"}, cfg)

	if got := variants[0].ImageURL; got != full {
		t.Errorf("the caller's variant ImageURL was mutated to %q; Meta must still receive the full signed URL %q", got, full)
	}
}

// ---------------------------------------------------------------------------
// Creative-asset resolution AT THE DISPATCH LEVEL (LFXV2-3295)
// ---------------------------------------------------------------------------

// fakeCreativeAssets is a creativeAssetReader that records what was asked for and
// answers with a fixed asset or error. calls is read by the tests to prove a lookup
// was SCOPED to the dispatching brief, not merely that one happened.
type fakeCreativeAssets struct {
	asset *model.CreativeAsset
	err   error

	mu    sync.Mutex
	calls []string // "projectID/briefID/assetID" per call
}

func (f *fakeCreativeAssets) GetAsset(_ context.Context, projectID, briefID, assetID string) (*model.CreativeAsset, error) {
	f.mu.Lock()
	f.calls = append(f.calls, projectID+"/"+briefID+"/"+assetID)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.asset, nil
}

func (f *fakeCreativeAssets) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// metaConfigWithVariant builds a dispatch config whose single variant carries the
// supplied raw JSON fields, so a test can send `imageAssetId`, `imageUrl`, or both
// exactly as a caller would over the wire.
func metaConfigWithVariant(variantFields string) json.RawMessage {
	// Empty fields must not leave a dangling comma, so the separator is added here
	// rather than baked into each caller's literal.
	variantSuffix := ""
	if strings.TrimSpace(variantFields) != "" {
		variantSuffix = "," + variantFields
	}
	return json.RawMessage(`{"metaConfig":{
		"budget":2500,"startDate":"2099-01-01","endDate":"2099-02-01",
		"objective":"traffic","geoTargets":["US"],"currencyOffset":100,
		"variants":[{"headline":"KubeCon 2099","primaryText":"Join us","description":"Cloud native"` + variantSuffix + `}]
	}}`)
}

// countingMetaServer is a Meta stub that answers the read-only preflight but FAILS
// the test on any mutating call. It is how these tests prove "no upstream create was
// attempted" as an observation of the wire rather than as an inference from an error
// value: a guard that fails to bind reaches one of these arms and the count is
// non-zero, whatever the returned error happens to say.
func countingMetaServer(t *testing.T, mutations *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		default:
			// Any POST — /adimages, /campaigns, /adsets, /adcreatives, /ads — is money or
			// a durable object. Reaching here at all is the failure.
			atomic.AddInt32(mutations, 1)
			t.Errorf("upstream create attempted before the pre-spend guard: %s %s", r.Method, r.URL.Path)
			_, _ = io.WriteString(w, `{"id":"SHOULD_NOT_EXIST"}`)
		}
	}))
}

// assertPreSpendRefusal is the shared assertion for every bad-asset dispatch: the
// dispatch failed, NOTHING was created upstream, and the error carries
// NoUpstreamCreate() so the orchestrator RELEASES the (brief, platform) claim
// instead of stranding it.
//
// The NoUpstreamCreate check goes through errors.As on the same anonymous interface
// the orchestrator uses, not through a *preCreateError type assertion, so the test
// binds the CONTRACT the orchestrator actually consults rather than the concrete
// type that happens to satisfy it today.
func assertPreSpendRefusal(t *testing.T, camp *model.Campaign, err error, mutations int32, wantMsg string) {
	t.Helper()
	if err == nil {
		t.Fatal("Dispatch returned nil error; a bad creative asset must fail the dispatch")
	}
	// (a) no upstream create was attempted.
	if mutations != 0 {
		t.Errorf("%d upstream create call(s) were made; the guard must refuse BEFORE any spend", mutations)
	}
	// A non-nil campaign would tell the orchestrator a paid resource may exist.
	if camp != nil {
		t.Errorf("Dispatch returned a non-nil campaign %+v; nothing was created", camp)
	}
	// (b) the error satisfies NoUpstreamCreate() — (c) which is what releases the claim.
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nuc) {
		t.Fatalf("error does not expose NoUpstreamCreate(); the orchestrator would STRAND the claim: %v", err)
	}
	if !nuc.NoUpstreamCreate() {
		t.Errorf("NoUpstreamCreate() = false; the orchestrator would STRAND the claim: %v", err)
	}
	if wantMsg != "" && !strings.Contains(err.Error(), wantMsg) {
		t.Errorf("error %q does not explain the failure (want it to mention %q)", err.Error(), wantMsg)
	}
}

// TestMeta_DispatchBadAssetRefusesBeforeAnySpend drives a BAD creative-asset
// reference through the REAL Dispatch path — not resolveVariantAssets in isolation —
// and pins the three properties that make the guard worth having: no upstream create
// was attempted, the error is NoUpstreamCreate, and the claim is therefore released.
//
// This test exists because the helper-level tests could not see any of that. They
// call resolveVariantAssets directly, so they pass identically whether or not its
// error is wrapped in notCreated at the call site and whether or not the call site
// sits before the client is built. Both of those mutations SURVIVED against the
// helper-level tests alone: dropping notCreated strands the (brief, platform) claim,
// and letting a bad asset fall through builds a link-only ad and spends money. Each
// is now killed here.
func TestMeta_DispatchBadAssetRefusesBeforeAnySpend(t *testing.T) {
	const validUUID = "6f1c2d3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"

	cases := []struct {
		name     string
		assets   creativeAssetReader // nil → the store is not configured
		bindRepo bool
		variant  string
		wantMsg  string
	}{
		{
			name:     "asset absent for this brief",
			assets:   &fakeCreativeAssets{err: domain.ErrNotFound},
			bindRepo: true,
			variant:  `"imageAssetId":"` + validUUID + `"`,
			wantMsg:  "does not exist for this brief",
		},
		{
			name:     "malformed asset id",
			assets:   &fakeCreativeAssets{asset: &model.CreativeAsset{Bytes: []byte("x"), MimeType: model.MimeTypePNG}},
			bindRepo: true,
			variant:  `"imageAssetId":"not-a-uuid"`,
			wantMsg:  "not a valid asset id",
		},
		{
			name:     "store not configured but a variant references an asset",
			bindRepo: false,
			variant:  `"imageAssetId":"` + validUUID + `"`,
			wantMsg:  "creative-asset store is not configured",
		},
		{
			name:     "asset resolves with no stored bytes",
			assets:   &fakeCreativeAssets{asset: &model.CreativeAsset{MimeType: model.MimeTypePNG}},
			bindRepo: true,
			variant:  `"imageAssetId":"` + validUUID + `"`,
			wantMsg:  "no stored image bytes",
		},
		{
			name:     "repository error other than not-found",
			assets:   &fakeCreativeAssets{err: errors.New("connection refused")},
			bindRepo: true,
			variant:  `"imageAssetId":"` + validUUID + `"`,
			wantMsg:  "load creative asset",
		},
		{
			// Meta forbids picture and image_hash on ONE creative. Supplying both has no
			// correct interpretation, so it is refused locally, pre-spend — never sent
			// upstream to be rejected after the campaign and ad set already exist.
			name:     "variant supplies BOTH an image url and an image asset id",
			assets:   &fakeCreativeAssets{asset: &model.CreativeAsset{Bytes: []byte("\x89PNG"), MimeType: model.MimeTypePNG}},
			bindRepo: true,
			variant:  `"imageUrl":"https://cdn.example.org/hero.png","imageAssetId":"` + validUUID + `"`,
			wantMsg:  "mutually exclusive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mutations int32
			srv := countingMetaServer(t, &mutations)
			defer srv.Close()

			clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
			d := NewMetaDispatcher(
				fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
				meta.WithBaseURL(srv.URL), meta.WithClock(clock),
			)
			if tc.bindRepo {
				d.SetCreativeAssetRepo(tc.assets)
			}

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, metaConfigWithVariant(tc.variant))
			assertPreSpendRefusal(t, camp, err, atomic.LoadInt32(&mutations), tc.wantMsg)
		})
	}
}

// TestMeta_DispatchResolvesAssetToImageHash is the by-STORED-BYTES happy path, end to
// end through Dispatch: an imageAssetId is resolved to bytes, uploaded to /adimages,
// and the returned hash lands on the creative as link_data.image_hash — with NO
// picture field, since the two are mutually exclusive.
//
// It also pins the lookup SCOPE: the asset is fetched for the dispatching brief's
// project and id, which is what stops one brief's campaign referencing another's
// asset.
func TestMeta_DispatchResolvesAssetToImageHash(t *testing.T) {
	const validUUID = "6f1c2d3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"
	imageBytes := []byte("\x89PNG\r\n\x1a\nFAKEPIXELS")

	var (
		mu           sync.Mutex
		creativeBody string
		uploadBody   []byte
		uploadType   string
		uploadCalls  int32
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/adimages"):
			atomic.AddInt32(&uploadCalls, 1)
			mu.Lock()
			uploadType = r.Header.Get("Content-Type")
			uploadBody, _ = io.ReadAll(r.Body)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"images":{"source":{"hash":"HASH_FROM_UPLOAD","url":"https://scontent.example/x.png"}}}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			creativeBody = string(b)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	assets := &fakeCreativeAssets{asset: &model.CreativeAsset{
		ID: validUUID, ProjectID: "cncf", BriefID: "brief-1",
		Bytes: imageBytes, MimeType: model.MimeTypeJPEG,
	}}

	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(clock),
	)
	d.SetCreativeAssetRepo(assets)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds,
		metaConfigWithVariant(`"imageAssetId":"`+validUUID+`"`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "120100000000123" {
		t.Fatalf("campaign not mapped: %+v", camp)
	}
	// A degraded status would mean the ad was never created.
	if camp.Status != campaignStatusCreated {
		t.Errorf("campaign status = %q, want %q — the asset-backed ad did not get created", camp.Status, campaignStatusCreated)
	}

	// The asset was looked up SCOPED to the dispatching brief.
	if got, want := assets.calls, []string{"cncf/brief-1/" + validUUID}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("asset lookup scope = %v, want %v", got, want)
	}
	if n := atomic.LoadInt32(&uploadCalls); n != 1 {
		t.Errorf("/adimages called %d times, want exactly 1", n)
	}

	mu.Lock()
	defer mu.Unlock()

	// The upload carried the asset's BYTES as a multipart file part — the documented
	// `bytes` create parameter — not a url field.
	if !strings.HasPrefix(uploadType, "multipart/form-data") {
		t.Errorf("upload Content-Type = %q, want multipart/form-data", uploadType)
	}
	if !bytes.Contains(uploadBody, imageBytes) {
		t.Error("upload body does not carry the asset's bytes")
	}
	if !bytes.Contains(uploadBody, []byte(model.MimeTypeJPEG)) {
		t.Errorf("upload body does not label the part with the asset's verified MIME type %q", model.MimeTypeJPEG)
	}

	// The hash from the upload landed on the creative...
	if !strings.Contains(creativeBody, `"image_hash":"HASH_FROM_UPLOAD"`) {
		t.Errorf("creative body missing link_data.image_hash from the upload: %s", creativeBody)
	}
	// ...and no picture accompanies it (Meta forbids both on one creative).
	if strings.Contains(creativeBody, "picture") {
		t.Errorf("creative body sent picture alongside image_hash: %s", creativeBody)
	}
	// The image bytes must never appear in the creative body.
	if strings.Contains(creativeBody, "FAKEPIXELS") {
		t.Errorf("creative body embedded the raw image bytes: %s", creativeBody)
	}

	// The per-variant image_hash is persisted in the campaign result for reconciliation.
	var res meta.CampaignResult
	if uerr := json.Unmarshal(camp.Result, &res); uerr != nil {
		t.Fatalf("unmarshal persisted result: %v", uerr)
	}
	if len(res.Ads) != 1 {
		t.Fatalf("persisted result has %d ads, want 1: %s", len(res.Ads), string(camp.Result))
	}
	if res.Ads[0].ImageHash != "HASH_FROM_UPLOAD" {
		t.Errorf("persisted ad ImageHash = %q, want %q", res.Ads[0].ImageHash, "HASH_FROM_UPLOAD")
	}
	if res.Ads[0].Variant != 1 || res.Ads[0].AdID != "ad_1" || res.Ads[0].CreativeID != "creative_1" {
		t.Errorf("persisted ad identifiers wrong: %+v", res.Ads[0])
	}
}

// TestMeta_DispatchNoAssetMakesNoUploadCall pins that the by-URL and link-only paths
// are UNAFFECTED by the asset machinery: neither consults the creative-asset store
// nor calls /adimages. A variant with no imageAssetId must not acquire an upload
// round-trip just because the capability now exists.
func TestMeta_DispatchNoAssetMakesNoUploadCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		variant string
	}{
		{"link-only variant", ``},
		{"by-url variant", `"imageUrl":"https://cdn.example.org/hero.png"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var uploadCalls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
					_, _ = io.WriteString(w, `{"data":[]}`)
				case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
					_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
				case strings.HasSuffix(r.URL.Path, "/adimages"):
					atomic.AddInt32(&uploadCalls, 1)
					_, _ = io.WriteString(w, `{"images":{"x":{"hash":"SHOULD_NOT_BE_USED"}}}`)
				case strings.HasSuffix(r.URL.Path, "/campaigns"):
					_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
				case strings.HasSuffix(r.URL.Path, "/adsets"):
					_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
				case strings.HasSuffix(r.URL.Path, "/adcreatives"):
					_, _ = io.WriteString(w, `{"id":"creative_1"}`)
				case strings.HasSuffix(r.URL.Path, "/ads"):
					_, _ = io.WriteString(w, `{"id":"ad_1"}`)
				default:
					http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
				}
			}))
			defer srv.Close()

			// The store is bound and would answer, so a zero call count proves the
			// by-URL/link-only paths never consult it — not that nothing was wired.
			assets := &fakeCreativeAssets{asset: &model.CreativeAsset{Bytes: []byte("x"), MimeType: model.MimeTypePNG}}
			clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
			d := NewMetaDispatcher(
				fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
				meta.WithBaseURL(srv.URL), meta.WithClock(clock),
			)
			d.SetCreativeAssetRepo(assets)

			if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, metaConfigWithVariant(tc.variant)); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if n := atomic.LoadInt32(&uploadCalls); n != 0 {
				t.Errorf("/adimages was called %d times for a variant with no imageAssetId", n)
			}
			if n := assets.callCount(); n != 0 {
				t.Errorf("the creative-asset store was consulted %d times for a variant with no imageAssetId", n)
			}
		})
	}
}

// TestMeta_ResolveVariantAssetsDoesNotMutateCallerConfig proves resolution returns a
// COPY. cfg.Variants is reused after resolution — by campaignFromMeta for the config
// snapshot and by Dispatch for the degraded-ad count — and the variants slice shares
// its backing array with the decoded config, so an in-place write would put
// multi-megabyte image bytes into the persisted config_snapshot.
func TestMeta_ResolveVariantAssetsDoesNotMutateCallerConfig(t *testing.T) {
	const validUUID = "6f1c2d3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{})
	d.SetCreativeAssetRepo(&fakeCreativeAssets{asset: &model.CreativeAsset{
		Bytes: []byte("SECRETPIXELS"), MimeType: model.MimeTypePNG,
	}})

	original := []meta.AdVariant{{Headline: "h1", ImageAssetID: validUUID}}
	resolved, err := d.resolveVariantAssets(context.Background(), testBrief(), original)
	if err != nil {
		t.Fatalf("resolveVariantAssets: %v", err)
	}
	if len(resolved[0].ImageBytes) == 0 {
		t.Fatal("resolved variant carries no image bytes")
	}
	if original[0].ImageBytes != nil {
		t.Errorf("the caller's variant was mutated in place; image bytes would reach the persisted config snapshot")
	}
}

// multiCreativeAssets serves a DIFFERENT asset per id (and records every lookup), which
// is what the dedupe and aggregate-bound tests need — the shared fakeCreativeAssets
// returns one asset for any id, so it cannot distinguish "resolved twice" from
// "resolved once and reused".
type multiCreativeAssets struct {
	assets map[string]*model.CreativeAsset

	mu    sync.Mutex
	calls []string // assetID per call, in order
}

func (m *multiCreativeAssets) GetAsset(_ context.Context, _, _, assetID string) (*model.CreativeAsset, error) {
	m.mu.Lock()
	m.calls = append(m.calls, assetID)
	m.mu.Unlock()
	a, ok := m.assets[assetID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return a, nil
}

func (m *multiCreativeAssets) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// TestMeta_ResolveVariantAssetsDedupesByAssetID is the memory bound's load-bearing half.
//
// Nothing caps how many variants a config may carry and each asset may be 30 MiB, so the
// earlier implementation performed one DB read and retained one full buffer PER VARIANT —
// and the cheapest way to trigger that was the SAME asset id repeated, which requires no
// extra stored data. This asserts the repeat costs exactly ONE read, and that every
// variant still receives the bytes (dedupe must not silently drop the image from the
// later variants, which would build link-only ads that spend money).
func TestMeta_ResolveVariantAssetsDedupesByAssetID(t *testing.T) {
	const id = "11111111-1111-1111-1111-111111111111"
	assets := &multiCreativeAssets{assets: map[string]*model.CreativeAsset{
		id: {Bytes: []byte("IMAGE_BYTES"), MimeType: "image/png"},
	}}
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{})
	d.SetCreativeAssetRepo(assets)

	// Five variants, all naming the SAME asset.
	in := []meta.AdVariant{
		{ImageAssetID: id}, {ImageAssetID: id}, {ImageAssetID: id},
		{ImageAssetID: id}, {ImageAssetID: id},
	}
	out, err := d.resolveVariantAssets(context.Background(), testBrief(), in)
	if err != nil {
		t.Fatalf("resolveVariantAssets error: %v", err)
	}
	if n := assets.callCount(); n != 1 {
		t.Errorf("GetAsset called %d times for 5 variants naming one asset, want 1 — a repeated id must cost one read and one buffer, not N", n)
	}
	for i := range out {
		if string(out[i].ImageBytes) != "IMAGE_BYTES" {
			t.Errorf("variant %d bytes = %q, want the resolved asset's bytes — dedupe must not drop the image", i+1, out[i].ImageBytes)
		}
		if out[i].ImageMIME != "image/png" {
			t.Errorf("variant %d mime = %q, want image/png", i+1, out[i].ImageMIME)
		}
	}
	// The repeated variants must ALIAS one buffer rather than hold five copies; that
	// aliasing is the allocation saving the dedupe exists for.
	if len(out) > 1 && &out[0].ImageBytes[0] != &out[1].ImageBytes[0] {
		t.Errorf("repeated variants hold distinct buffers; they must alias the single resolved copy")
	}
}

// TestMeta_ResolveVariantAssetsBoundsAggregateBytes proves the aggregate ceiling refuses a
// config naming more DISTINCT asset bytes than one dispatch may hold, and that it refuses
// BEFORE retaining them. Distinct ids defeat dedupe, so this is the case the byte budget
// (not the dedupe) has to catch.
func TestMeta_ResolveVariantAssetsBoundsAggregateBytes(t *testing.T) {
	// Nine assets of 30 MiB each = 270 MiB, past the 240 MiB (8-asset) ceiling. The
	// buffers are allocated by the fake, not by the code under test; what is asserted is
	// that resolution REFUSES rather than accumulating them into its own result.
	const perAsset = 30 << 20
	assets := &multiCreativeAssets{assets: map[string]*model.CreativeAsset{}}
	var in []meta.AdVariant
	for i := 0; i < 9; i++ {
		id := fmt.Sprintf("%08d-1111-1111-1111-111111111111", i)
		assets.assets[id] = &model.CreativeAsset{Bytes: make([]byte, perAsset), MimeType: "image/png"}
		in = append(in, meta.AdVariant{ImageAssetID: id})
	}
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{})
	d.SetCreativeAssetRepo(assets)

	out, err := d.resolveVariantAssets(context.Background(), testBrief(), in)
	if err == nil {
		t.Fatalf("resolveVariantAssets accepted %d distinct maximum-size assets; the aggregate bound must refuse", len(out))
	}
	if out != nil {
		t.Errorf("a refused resolution must return no variants, got %d", len(out))
	}
	if !strings.Contains(err.Error(), "distinct creative assets") {
		t.Errorf("error = %v, want the aggregate-bytes refusal", err)
	}
	// It must stop AT the ceiling rather than reading every asset first.
	if n := assets.callCount(); n > 9 {
		t.Errorf("GetAsset called %d times, want at most 9", n)
	}
}

// TestMeta_ResolveVariantAssetsAcceptsRealisticCampaign is the other side of the bound:
// it must not refuse a legitimate campaign. Real Meta creatives are a few hundred KiB
// (Meta's recommended feed image is 1936x1936), so a many-variant A/B test of realistic
// images sits far under the ceiling and must resolve cleanly.
func TestMeta_ResolveVariantAssetsAcceptsRealisticCampaign(t *testing.T) {
	const realistic = 400 << 10 // 400 KiB, a generous real-world PNG
	assets := &multiCreativeAssets{assets: map[string]*model.CreativeAsset{}}
	var in []meta.AdVariant
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("%08d-2222-2222-2222-222222222222", i)
		assets.assets[id] = &model.CreativeAsset{Bytes: make([]byte, realistic), MimeType: "image/png"}
		in = append(in, meta.AdVariant{ImageAssetID: id})
	}
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{})
	d.SetCreativeAssetRepo(assets)

	out, err := d.resolveVariantAssets(context.Background(), testBrief(), in)
	if err != nil {
		t.Fatalf("50 realistic (400 KiB) creatives were refused, but the bound must not reject a legitimate campaign: %v", err)
	}
	if len(out) != 50 {
		t.Fatalf("resolved %d variants, want 50", len(out))
	}
	for i := range out {
		if len(out[i].ImageBytes) != realistic {
			t.Errorf("variant %d did not receive its bytes", i+1)
		}
	}
}
