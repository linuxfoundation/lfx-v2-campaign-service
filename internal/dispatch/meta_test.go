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
