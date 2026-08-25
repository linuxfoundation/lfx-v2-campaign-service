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
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"

	"github.com/google/uuid"
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

// Meta has no scalar campaign-level conversions field: the Insights edge reports conversions
// inside the `actions` array as {action_type, value} objects. Reducing that to one number
// requires deciding WHICH action types count as a conversion for this advertiser, which this
// service has no input for — so the count stays absent.
//
// The fixture deliberately CARRIES an actions array in Meta's published shape. A zero here
// would be a fabricated measurement, and this test fails if a future change starts summing
// those values or defaulting the field, either of which would make every Meta campaign
// report a conversion count this service cannot actually justify.
func TestMeta_ReadMetrics_ConversionsAbsentNotZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"1000","clicks":"40","spend":"25.00",`+
			`"actions":[{"action_type":"offsite_conversion.fb_pixel_purchase","value":"12"},`+
			`{"action_type":"lead","value":"3"}]}]}`)
	}))
	defer srv.Close()

	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	camp := &model.Campaign{Platform: model.ProviderMetaAds, PlatformCampaignID: "777"}
	m, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMetaAds, camp, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadMetrics: %v", err)
	}
	if m.Conversions != nil {
		t.Errorf("Conversions = %v for Meta, which exposes no scalar campaign-level "+
			"conversion count; a number derived here would be this service inventing an "+
			"attribution policy it was never given", *m.Conversions)
	}
	// The rest of the read must be unaffected — absence is not a failure.
	if m.Impressions != 1000 || m.Clicks != 40 {
		t.Errorf("got %+v", m)
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

// GetAssetSize answers from the SAME stored asset GetAsset would return, so the fake cannot
// drift into pricing one size and serving another — a mismatch would be a fake-only bug that
// the production CHECK on byte_size (migration 000029) makes impossible for a real row.
func (f *fakeCreativeAssets) GetAssetSize(_ context.Context, _, _, _ string) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	if f.asset == nil {
		return 0, domain.ErrNotFound
	}
	return int64(len(f.asset.Bytes)), nil
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
			// The SIZE read is the first repository call now (it prices the asset so the
			// aggregate reservation can be taken before the blob is resident), so a repo-level
			// failure surfaces from there rather than from the byte read. Both arms refuse
			// pre-spend, which is what this table asserts; the message names whichever read
			// actually failed so an operator is not sent looking at the wrong query.
			name:     "repository error other than not-found",
			assets:   &fakeCreativeAssets{err: errors.New("connection refused")},
			bindRepo: true,
			variant:  `"imageAssetId":"` + validUUID + `"`,
			wantMsg:  "size creative asset",
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

	// The upload carried the asset's BYTES as a multipart FILE part, not a url field.
	// Note the transport is deliberately NOT the documented `bytes` create parameter:
	// `bytes` is Meta's base64 scalar, whereas this request sends a multipart file part
	// (named `source`) to avoid the ~33% base64 inflation. See uploadImage's godoc for
	// why the part NAME is not the contract and the filename is what matters.
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
// COPY, for CALLER ISOLATION. cfg.Variants is reused after resolution — by
// campaignFromMeta for the config snapshot and by Dispatch for the degraded-ad count —
// and the variants slice shares its backing array with the decoded config, so an
// in-place write would hand those later readers a config that no longer matches what
// the caller sent.
//
// It is NOT a persistence guard: AdVariant.ImageBytes is tagged `json:"-"`
// (internal/platform/meta/client.go:2469), so resolved bytes cannot enter the
// marshalled config_snapshot even if the slice were mutated in place.
func TestMeta_ResolveVariantAssetsDoesNotMutateCallerConfig(t *testing.T) {
	const validUUID = "6f1c2d3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{})
	d.SetCreativeAssetRepo(&fakeCreativeAssets{asset: &model.CreativeAsset{
		Bytes: []byte("SECRETPIXELS"), MimeType: model.MimeTypePNG,
	}})

	original := []meta.AdVariant{{Headline: "h1", ImageAssetID: validUUID}}
	resolved, _, err := d.resolveVariantAssets(context.Background(), testBrief(), original)
	if err != nil {
		t.Fatalf("resolveVariantAssets: %v", err)
	}
	if len(resolved[0].ImageBytes) == 0 {
		t.Fatal("resolved variant carries no image bytes")
	}
	if original[0].ImageBytes != nil {
		t.Errorf("the caller's variant was mutated in place; campaignFromMeta and the degraded-ad count would no longer see the config the caller sent")
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

// GetAssetSize answers from the same map GetAsset serves, for the reason above. It deliberately
// does NOT record into calls: callCount() asserts how many times the BYTES were fetched, which
// is the dedupe property those tests pin, and counting the cheap size read would break it.
func (m *multiCreativeAssets) GetAssetSize(_ context.Context, _, _, assetID string) (int64, error) {
	a, ok := m.assets[assetID]
	if !ok {
		return 0, domain.ErrNotFound
	}
	return int64(len(a.Bytes)), nil
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
	out, _, err := d.resolveVariantAssets(context.Background(), testBrief(), in)
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
//
// The fixture carries TEN assets to bound a NINE-asset refusal, and that gap is the point.
// 30 MiB each against the 240 MiB (8-asset) ceiling means the ninth read is the one that
// trips it, so a correct implementation stops there having read exactly 9. An earlier
// version of this test built only 9 variants and asserted `callCount() > 9`, which the
// fixture's own shape made unreachable — the loop could not produce a tenth call, so the
// assertion held no matter what the code did, including an implementation that read every
// asset before checking the total. Supplying a tenth asset is what gives "it stops AT the
// ceiling" something to fail against.
func TestMeta_ResolveVariantAssetsBoundsAggregateBytes(t *testing.T) {
	// Ten assets of 30 MiB each = 300 MiB, past the 240 MiB (8-asset) ceiling, which the
	// running total crosses at the NINTH. The buffers are allocated by the fake, not by the
	// code under test; what is asserted is that resolution REFUSES rather than accumulating
	// them into its own result.
	const perAsset = 30 << 20
	const wantReads = 9 // the read whose running total first exceeds maxVariantAssetBytes
	assets := &multiCreativeAssets{assets: map[string]*model.CreativeAsset{}}
	var in []meta.AdVariant
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("%08d-1111-1111-1111-111111111111", i)
		assets.assets[id] = &model.CreativeAsset{Bytes: make([]byte, perAsset), MimeType: "image/png"}
		in = append(in, meta.AdVariant{ImageAssetID: id})
	}
	// Pin the premise rather than trusting the arithmetic: if the ceiling or the per-asset
	// maximum moves, the read count below is no longer nine and this says so directly
	// instead of failing as a mysterious off-by-one.
	if int64(wantReads)*perAsset <= maxVariantAssetBytes || int64(wantReads-1)*perAsset > maxVariantAssetBytes {
		t.Fatalf("fixture no longer trips the ceiling at read %d: %d assets of %d bytes against a %d-byte ceiling",
			wantReads, len(in), perAsset, maxVariantAssetBytes)
	}

	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{})
	d.SetCreativeAssetRepo(assets)

	out, _, err := d.resolveVariantAssets(context.Background(), testBrief(), in)
	if err == nil {
		t.Fatalf("resolveVariantAssets accepted %d distinct maximum-size assets; the aggregate bound must refuse", len(out))
	}
	if out != nil {
		t.Errorf("a refused resolution must return no variants, got %d", len(out))
	}
	if !strings.Contains(err.Error(), "distinct creative assets") {
		t.Errorf("error = %v, want the aggregate-bytes refusal", err)
	}
	// It must stop AT the ceiling, and the asset that TRIPS it must never be loaded.
	//
	// The expected count is wantReads-1, and that is the property this bound was changed to
	// provide. The size is now read first (one BIGINT, no BYTEA), so the ceiling is crossed on
	// asset N's SIZE and its bytes are never fetched: N-1 byte-reads. Previously the ceiling was
	// checked after GetAsset had already materialised the tripping blob, which is why the peak
	// used to be documented as "the cap plus one maximum-size asset".
	//
	// Asserted as an EQUALITY in both directions: reading fewer would mean it refused before the
	// budget was actually exceeded, and reading wantReads (or more) means the tripping asset —
	// or the whole set — was buffered before the check, the retention this ceiling exists to
	// prevent.
	if n := assets.callCount(); n != wantReads-1 {
		t.Errorf("GetAsset called %d times, want exactly %d — the asset that crosses the ceiling "+
			"must be refused on its SIZE, without its bytes ever being loaded", n, wantReads-1)
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

	out, _, err := d.resolveVariantAssets(context.Background(), testBrief(), in)
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

// TestMeta_ConfigFieldsReachTheWire pins the metaConfig -> meta.CampaignInput hop for the
// Instagram identity and the two EU DSA disclosures, by observing the REQUEST BODIES the
// dispatcher actually produces.
//
// This seam had no coverage. The client-level tests in internal/platform/meta start BELOW
// it — they assert the payload given a CampaignInput — so deleting
// `InstagramUserID: cfg.InstagramUserID` from the struct literal in meta.go, or transposing
// the two DSA fields, left the whole dispatch package green. Either mutation ships an ad
// that is created, spends nothing, and sits unpublishable in Ads Manager ("Please add
// Instagram account"), which is the exact failure this branch exists to remove.
//
// It drives Dispatch through an httptest server rather than re-deriving the mapping: a test
// that builds its own CampaignInput from the same cfg would assert `x == x` and pass against
// a meta.go that dropped the field entirely.
//
// The input is real JSON, so the `json:"..."` tags are pinned too — they are the public
// request contract (docs/api-catalog.md) and a typo there decodes to a silent zero value.
// The three values are DISTINCT so a transposition is observable.
func TestMeta_ConfigFieldsReachTheWire(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string]map[string]any{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if b, err := io.ReadAll(r.Body); err == nil && len(b) > 0 {
			_ = json.Unmarshal(b, &body)
		}
		mu.Lock()
		switch {
		case strings.Contains(r.URL.Path, "/adsets"):
			bodies["adset"] = body
		case strings.Contains(r.URL.Path, "/adcreatives"):
			bodies["creative"] = body
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// A single-segment path is the account preflight (GET /act_<id>?fields=...);
		// url.URL.Path excludes the query string, so the preflight arrives here as the
		// bare "/act_<id>". It needs a currency — CreateCampaign derives the minor-unit
		// offset from it and fails before any mutating call if it is absent. Everything
		// else is a nested edge (/act_<id>/adsets, /<id>/adcreatives) and just needs an id.
		if strings.Count(strings.Trim(r.URL.Path, "/"), "/") == 0 {
			_, _ = io.WriteString(w, `{"currency":"USD","id":"23847290"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"23847290"}`)
	}))
	defer srv.Close()

	// The config is an ENVELOPE with a metaConfig key, not a bare metaConfig — matching
	// docs/api-catalog.md and unmarshalPlatformConfig's contract.
	cfg := []byte(`{"metaConfig": {
		"budget": 100,
		"startDate": "2098-09-01",
		"endDate": "2098-09-30",
		"objective": "traffic",
		"instagramUserId": "17841400000000000",
		"dsaBeneficiary": "The Linux Foundation",
		"dsaPayor": "LF Projects, LLC",
		"variants": [{"Headline": "Join us", "PrimaryText": "An open source event"}]
	}}`)

	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL),
		meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	mu.Lock()
	adset, creative := bodies["adset"], bodies["creative"]
	mu.Unlock()

	if adset == nil {
		t.Fatal("no /adsets request was made; the dispatch did not reach the ad set step")
	}
	if got := adset["dsa_beneficiary"]; got != "The Linux Foundation" {
		t.Errorf("dsa_beneficiary on the wire = %v, want the BENEFICIARY; a value of "+
			"'LF Projects, LLC' means beneficiary and payor are transposed in meta.go", got)
	}
	if got := adset["dsa_payor"]; got != "LF Projects, LLC" {
		t.Errorf("dsa_payor on the wire = %v, want the PAYOR; a value of "+
			"'The Linux Foundation' means beneficiary and payor are transposed in meta.go", got)
	}
	if creative == nil {
		t.Fatal("no /adcreatives request was made; the dispatch did not reach the creative step")
	}
	if got := creative["instagram_user_id"]; got != "17841400000000000" {
		t.Errorf("instagram_user_id on the wire = %v, want the configured IGSID. Absent means "+
			"the metaConfig -> CampaignInput mapping in meta.go dropped it, and Meta will refuse "+
			"to publish with \"Please add Instagram account\"", got)
	}
}

// TestMeta_MalformedAssetIDIsBoundedAndSanitisedInError covers the log-injection and
// unbounded-field surface on the ONE value here that is opaque caller JSON.
//
// imageAssetId has no length or charset bound anywhere on its path, and a rejected value
// is quoted back in an error that reaches operator-facing output AND the orchestrator's
// structured error log (notCreated → the default pre-create arm's slog "error" attribute).
// So a caller controls both how MUCH text and WHAT text lands there.
//
// Asserted as BEHAVIOUR of the rendered error, not as source text: the message must stay
// bounded, must not carry newlines a caller could use to forge a second log entry, and
// must still identify the variant so the operator can act on it.
func TestMeta_MalformedAssetIDIsBoundedAndSanitisedInError(t *testing.T) {
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{})
	d.SetCreativeAssetRepo(&multiCreativeAssets{assets: map[string]*model.CreativeAsset{}})

	t.Run("length is bounded", func(t *testing.T) {
		// 100 KiB of caller-chosen text in a field whose valid form is a 36-char UUID.
		huge := strings.Repeat("A", 100<<10)
		_, _, err := d.resolveVariantAssets(context.Background(), testBrief(),
			[]meta.AdVariant{{ImageAssetID: huge}})
		if err == nil {
			t.Fatal("a malformed asset id must be refused")
		}
		msg := err.Error()
		// The whole message must stay small. The bound is on what the VALUE contributes,
		// so compare against the value's cap plus generous room for the fixed wording.
		if len(msg) > maxAssetIDInError+300 {
			t.Errorf("error is %d bytes for a %d-byte caller value; the quoted id must be truncated", len(msg), len(huge))
		}
		if strings.Contains(msg, huge) {
			t.Error("the full caller value was echoed into the error unchanged")
		}
		if !strings.Contains(msg, "truncated") {
			t.Errorf("a truncated value must say so, got %q", msg)
		}
		// Still actionable: the operator needs to know WHICH variant.
		if !strings.Contains(msg, "variant 1") {
			t.Errorf("error must still name the variant, got %q", msg)
		}
	})

	t.Run("control characters cannot forge a log line", func(t *testing.T) {
		// A newline plus a plausible-looking forged record. If this reaches a line-oriented
		// log sink intact, the caller has written a second entry.
		inject := "aaa\nlevel=INFO msg=\"campaign created successfully\"\rmore"
		_, _, err := d.resolveVariantAssets(context.Background(), testBrief(),
			[]meta.AdVariant{{ImageAssetID: inject}})
		if err == nil {
			t.Fatal("a malformed asset id must be refused")
		}
		msg := err.Error()
		if strings.ContainsAny(msg, "\n\r") {
			t.Errorf("error carries a raw newline/carriage return, so a caller can forge a log entry: %q", msg)
		}
		// The neutralised text may remain; what must not survive is the line break.
		if strings.Contains(msg, "\nlevel=INFO") {
			t.Errorf("the injected log line survived intact: %q", msg)
		}
	})

	t.Run("a valid uuid is still accepted", func(t *testing.T) {
		// The sanitiser must not become a new rejection path: a well-formed id must still
		// reach the repo lookup (and fail there as "does not exist"), not be refused as
		// malformed.
		_, _, err := d.resolveVariantAssets(context.Background(), testBrief(),
			[]meta.AdVariant{{ImageAssetID: "11111111-2222-3333-4444-555555555555"}})
		if err == nil {
			t.Fatal("an absent asset must still be refused")
		}
		if strings.Contains(err.Error(), "not a valid asset id") {
			t.Errorf("a well-formed uuid was rejected as malformed: %v", err)
		}
	})
}

// TestMeta_AssetIDAliasesResolveAsOneAsset covers the identity the dedupe cache keys on.
//
// uuid.Parse accepts FOUR spellings of the same uuid — canonical, braced, URN and
// unhyphenated — so the caller's raw spelling is not a stable identity. Keying the cache
// on it lets a config name ONE asset through several valid aliases and defeat the dedupe:
// each alias misses the map, reads the row again, retains another buffer, and is charged
// against the aggregate budget again. That is the unbounded case the dedupe exists to
// prevent, and it needs no extra stored data to trigger.
//
// Asserted through the OBSERVED read count and the bytes each variant received, so it
// fails if canonicalization is dropped and equally if canonicalization broke resolution.
func TestMeta_AssetIDAliasesResolveAsOneAsset(t *testing.T) {
	const canonical = "11111111-2222-3333-4444-555555555555"
	assets := &multiCreativeAssets{assets: map[string]*model.CreativeAsset{
		canonical: {Bytes: []byte("IMAGE_BYTES"), MimeType: "image/png"},
	}}
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{})
	d.SetCreativeAssetRepo(assets)

	// Four spellings of ONE uuid. Pin the premise: every one of these must actually parse,
	// otherwise this test would be asserting dedupe over values that are simply rejected.
	aliases := []string{
		canonical,
		"{11111111-2222-3333-4444-555555555555}",
		"urn:uuid:11111111-2222-3333-4444-555555555555",
		"11111111222233334444555555555555",
	}
	for _, a := range aliases {
		if _, err := uuid.Parse(a); err != nil {
			t.Fatalf("fixture alias %q does not parse, so this test would not exercise dedupe: %v", a, err)
		}
	}

	in := make([]meta.AdVariant, 0, len(aliases))
	for _, a := range aliases {
		in = append(in, meta.AdVariant{ImageAssetID: a})
	}

	out, _, err := d.resolveVariantAssets(context.Background(), testBrief(), in)
	if err != nil {
		t.Fatalf("resolveVariantAssets error: %v", err)
	}
	if n := assets.callCount(); n != 1 {
		t.Errorf("GetAsset called %d times for 4 ALIASES of one asset id, want 1 — "+
			"alias spellings must not defeat the dedupe", n)
	}
	for i := range out {
		if string(out[i].ImageBytes) != "IMAGE_BYTES" {
			t.Errorf("variant %d (alias %q) bytes = %q, want the resolved asset's bytes",
				i+1, aliases[i], out[i].ImageBytes)
		}
	}
	// All four must alias the ONE resolved buffer, which is the allocation saving.
	for i := 1; i < len(out); i++ {
		if &out[0].ImageBytes[0] != &out[i].ImageBytes[0] {
			t.Errorf("variant %d (alias %q) holds a distinct buffer; aliases must share the single resolved copy",
				i+1, aliases[i])
		}
	}
}

// TestAssetReserver_ConcurrentDispatchesShareOneBudget is the behavioural proof that the
// aggregate bound actually blocks, not just that a constant exists.
//
// The defect it guards: maxVariantAssetBytes caps ONE dispatch, while maxParallelDispatch
// allows five concurrent provider dispatches from a process-wide semaphore with no
// per-provider partition — so five Meta dispatches each held up to their own cap. This
// asserts the property that was missing: a SECOND dispatch cannot hold assets while a first
// one already holds the whole budget.
//
// The proof does not use sleeps or elapsed time. The first reservation is taken and HELD, so
// while it is outstanding the budget is provably exhausted; a second acquire of any non-zero
// size must therefore be refused. Releasing the first must then let the same request through,
// which is what distinguishes a working budget from one that refuses everything.
func TestAssetReserver_ConcurrentDispatchesShareOneBudget(t *testing.T) {
	r := NewAssetReserver(MaxConcurrentVariantAssetBytes, 50*time.Millisecond)

	// A full-size dispatch: the whole budget, which is exactly one maximum config.
	release1, ok := r.reserve(context.Background(), MaxConcurrentVariantAssetBytes)
	if !ok {
		t.Fatalf("a dispatch of exactly MaxConcurrentVariantAssetBytes (%d) was refused — "+
			"the budget must admit one maximum-size config or it rejects legal work",
			MaxConcurrentVariantAssetBytes)
	}

	// While that one is held the budget is exhausted, so a second dispatch — even a small
	// one — must be refused rather than admitted alongside it. Before this bound existed,
	// five of these ran together.
	if _, ok := r.reserve(context.Background(), 1); ok {
		t.Error("a second dispatch was admitted while the whole budget was held: " +
			"concurrent dispatches are not sharing one bound, which is the 1.32 GiB defect")
	}

	// Releasing must actually return the capacity. Without this the test would pass against a
	// reserver that refuses everything after the first acquire, which is not a bound but a
	// deadlock.
	release1()
	release2, ok := r.reserve(context.Background(), MaxConcurrentVariantAssetBytes)
	if !ok {
		t.Fatal("after releasing the first reservation the budget did not come back: " +
			"the release is not returning capacity")
	}
	release2()
}

// TestAssetReserver_AdmitsConcurrentSmallDispatchesTogether pins the other half of the
// property, and it is the half that catches a bound "fixed" by simply refusing more.
//
// A budget that serialized ALL dispatches would pass the test above while making every
// ordinary campaign wait on every other. Real Meta creatives are a few hundred KiB, so the
// common case must still run concurrently: the budget is priced for the worst legal input and
// shared by everything smaller.
func TestAssetReserver_AdmitsConcurrentSmallDispatchesTogether(t *testing.T) {
	r := NewAssetReserver(MaxConcurrentVariantAssetBytes, 50*time.Millisecond)

	// A realistic creative, not a maximum-size one.
	const realistic int64 = 512 << 10 // 512 KiB

	const want = 8
	releases := make([]func(), 0, want)
	for i := range want {
		release, ok := r.reserve(context.Background(), realistic)
		if !ok {
			t.Fatalf("only %d realistic dispatches (%d bytes each) were admitted concurrently, "+
				"want %d — the budget is serializing ordinary campaigns", i, realistic, want)
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
}

// TestResolveVariantAssets_RefusesWhenAggregateBudgetIsExhausted proves the reserver is
// actually CONSULTED by the dispatch path, which the unit tests above cannot show.
//
// A reserver that is constructed, wired and never called would keep every test above green
// while providing no bound at all. This drains the budget out-of-band and then drives the real
// resolve, so a pass requires resolveVariantAssets to have asked for capacity and honoured the
// refusal.
func TestResolveVariantAssets_RefusesWhenAggregateBudgetIsExhausted(t *testing.T) {
	assets := &fakeCreativeAssets{asset: &model.CreativeAsset{
		Bytes: []byte("IMAGE_BYTES"), MimeType: "image/png",
	}}
	d := &MetaDispatcher{creatives: assets}
	d.SetAssetReserver(NewAssetReserver(MaxConcurrentVariantAssetBytes, 20*time.Millisecond))

	// Hold the ENTIRE budget, standing in for another dispatch already running.
	release, ok := d.assets.reserve(context.Background(), MaxConcurrentVariantAssetBytes)
	if !ok {
		t.Fatal("could not take the initial reservation")
	}
	defer release()

	in := []meta.AdVariant{{ImageAssetID: "11111111-1111-4111-8111-111111111111"}}
	out, _, err := d.resolveVariantAssets(context.Background(), testBrief(), in)
	if err == nil {
		t.Fatal("resolveVariantAssets succeeded while the aggregate budget was fully held: " +
			"the reservation is not being taken on the dispatch path")
	}
	// It must fail CLOSED — no bytes handed back — or the refusal would be cosmetic while the
	// memory it was refused for is still resident.
	if out != nil {
		t.Errorf("a refused resolve returned %d variants, want none: bytes must not be retained "+
			"when the budget that accounts for them was refused", len(out))
	}
}

// TestDispatchHoldsAssetReservationForTheWholeDispatch pins the reservation LIFETIME.
//
// WEAKER EVIDENCE THAN THE TESTS ABOVE, and deliberately labelled as such rather than folded
// into a pass count. This is a SOURCE assertion, not a behavioural one: it reads the call site
// instead of observing a release happening too early.
//
// The reason is a real limit, not laziness. The reservation must be held until the Meta client
// has POSTed the bytes to /adimages, which happens deep inside Dispatch — and reaching that
// point requires a decryptable stored connection, a credentials source, and an HTTP round trip
// to Meta. A test that drove it would be asserting the lifetime through several layers that can
// each fail for their own reasons, so a failure would not localise to the lifetime.
//
// It matters because the mutation IS survivable: changing `defer releaseAssets()` to an
// immediate `releaseAssets()` leaves every behavioural test in this file green while handing
// the budget back before the bytes are gone — the exact error DecodeReserver's comment records
// in the opposite direction. Until the lifetime can be observed end to end, this guard is what
// stands between that mutation and a silent regression.
func TestDispatchHoldsAssetReservationForTheWholeDispatch(t *testing.T) {
	src, err := os.ReadFile("meta.go")
	if err != nil {
		t.Fatalf("read meta.go: %v", err)
	}
	body := string(src)

	if !strings.Contains(body, "defer releaseAssets()") {
		t.Error("Dispatch does not `defer releaseAssets()`: the aggregate asset reservation " +
			"must be held until the dispatch returns, because the resolved bytes are POSTed to " +
			"/adimages later in that same call. Releasing at the resolve hands back budget for " +
			"memory that is still resident.")
	}
	// An immediate release is the specific mutation this guards, so name it rather than only
	// checking the defer is present: both could coexist and the bound would still be defeated.
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "releaseAssets()" {
			t.Error("found a bare `releaseAssets()` call: the release must be deferred to the " +
				"end of Dispatch, not invoked as soon as the assets are resolved")
		}
	}
}

// TestResolveVariantAssets_ReservesBeforeMaterialising pins the ORDERING that makes the
// aggregate bound real, and it is the property the first version of this bound did not have.
//
// The original implementation resolved every asset and reserved the total afterwards. Each
// concurrent dispatch therefore materialised its full per-dispatch allowance and only THEN
// blocked on the semaphore, so five dispatches held ~1.2 GiB while queueing politely — the
// budget gated the /adimages phase and bounded resident memory not at all. A reservation taken
// after the allocation it accounts for is not a bound.
//
// This asserts the fix directly: with the budget fully held by another dispatch, the resolve
// must be refused WITHOUT ever calling GetAsset. The size read (one BIGINT, no BYTEA) is what
// makes that possible.
func TestResolveVariantAssets_ReservesBeforeMaterialising(t *testing.T) {
	assets := &multiCreativeAssets{assets: map[string]*model.CreativeAsset{}}
	const id = "11111111-1111-4111-8111-111111111111"
	assets.assets[id] = &model.CreativeAsset{Bytes: make([]byte, 1<<20), MimeType: "image/png"}

	d := &MetaDispatcher{creatives: assets}
	d.SetAssetReserver(NewAssetReserver(MaxConcurrentVariantAssetBytes, 20*time.Millisecond))

	// Another dispatch holds the entire budget.
	release, ok := d.assets.reserve(context.Background(), MaxConcurrentVariantAssetBytes)
	if !ok {
		t.Fatal("could not take the initial reservation")
	}
	defer release()

	_, _, err := d.resolveVariantAssets(context.Background(), testBrief(),
		[]meta.AdVariant{{ImageAssetID: id}})
	if err == nil {
		t.Fatal("resolve succeeded while the aggregate budget was fully held")
	}

	// THE ASSERTION THAT MATTERS. GetAsset is the call that materialises the blob; callCount
	// counts only that read (GetAssetSize deliberately does not record). Zero means the refusal
	// happened before any image bytes entered the process.
	if n := assets.callCount(); n != 0 {
		t.Errorf("GetAsset was called %d times before the budget refusal, want 0 — the bytes were "+
			"materialised and only then charged, so the reservation bounds nothing", n)
	}
}

// TestResolveVariantAssets_ReleasesReservationsOnMidLoopFailure is the regression guard for a
// LEAKED SEMAPHORE PERMIT — a defect that is invisible in a single dispatch and permanent once it
// happens.
//
// The shape: reservations are taken per asset inside the loop, so by the time asset N fails,
// assets 1..N-1 are already holding budget. Two separate bugs discarded them. The GetAssetSize
// error arms returned a no-op releaser instead of the accumulated one, and — compounding it —
// Dispatch's `defer releaseAssets()` sat BELOW its error check, so on a failed resolve it never
// ran at all. Either alone leaks; together they leak on every failure arm in the loop.
//
// A leak here does not fail anything immediately: the dispatch reports its real error and the
// caller sees the right message. The budget is simply smaller forever, and only a LATER dispatch
// pays, by being shed for capacity nobody is using. That is why this is asserted directly on the
// budget rather than on the returned error.
//
// The budget IS observable: reserving exactly MaxConcurrentVariantAssetBytes succeeds only if
// every prior permit came back, so a single full-size acquire is an exact test for "the budget is
// whole". No sleeps and no elapsed-time thresholds.
func TestResolveVariantAssets_ReleasesReservationsOnMidLoopFailure(t *testing.T) {
	const good = "11111111-1111-4111-8111-111111111111"
	const missing = "22222222-2222-4222-8222-222222222222"

	assets := &multiCreativeAssets{assets: map[string]*model.CreativeAsset{
		// Only the FIRST asset exists. The second fails its size read, mid-loop, after the
		// first has already reserved.
		good: {Bytes: make([]byte, 4<<20), MimeType: "image/png"},
	}}

	d := &MetaDispatcher{creatives: assets}
	d.SetAssetReserver(NewAssetReserver(MaxConcurrentVariantAssetBytes, 20*time.Millisecond))

	_, release, err := d.resolveVariantAssets(context.Background(), testBrief(), []meta.AdVariant{
		{ImageAssetID: good},
		{ImageAssetID: missing},
	})
	if err == nil {
		t.Fatal("resolve succeeded although the second asset does not exist")
	}
	// The releaser handed back on an error path must be safe to call and must not double-release
	// (a weighted semaphore panics on over-release, so this would fail loudly if it did).
	release()

	// THE ASSERTION. If the first asset's 4 MiB was leaked, the budget is short by that much and
	// a full-size reservation cannot be taken.
	whole, ok := d.assets.reserve(context.Background(), MaxConcurrentVariantAssetBytes)
	if !ok {
		t.Fatal("the budget did not return to full after a mid-loop failure: the reservations " +
			"taken before the failing asset were leaked, permanently shrinking the process-wide " +
			"dispatch budget")
	}
	whole()
}

// TestResolveVariantAssets_RepeatedFailuresDoNotExhaustTheBudget is the EXHAUSTION test, and it
// is the one that matches how this defect actually surfaces.
//
// A single leaked permit is invisible: the budget is 240 MiB and one leak of a few MiB still
// admits the next dispatch, so a single-shot test can pass while the leak is real. What the
// finding describes is "repeated partial resolves permanently shrink the budget until restart",
// so this runs the failing path enough times that the leaked total would exceed the entire budget
// and then asserts a full-size dispatch is still admissible.
//
// The iteration count is derived, not guessed: enough repetitions that a leak of assetSize each
// time sums past MaxConcurrentVariantAssetBytes, plus a margin.
func TestResolveVariantAssets_RepeatedFailuresDoNotExhaustTheBudget(t *testing.T) {
	const good = "11111111-1111-4111-8111-111111111111"
	const missing = "22222222-2222-4222-8222-222222222222"
	const assetSize = 8 << 20 // 8 MiB per leaked reservation

	assets := &multiCreativeAssets{assets: map[string]*model.CreativeAsset{
		good: {Bytes: make([]byte, assetSize), MimeType: "image/png"},
	}}

	d := &MetaDispatcher{creatives: assets}
	d.SetAssetReserver(NewAssetReserver(MaxConcurrentVariantAssetBytes, 20*time.Millisecond))

	iterations := int(MaxConcurrentVariantAssetBytes/assetSize) + 2
	for i := range iterations {
		_, release, err := d.resolveVariantAssets(context.Background(), testBrief(), []meta.AdVariant{
			{ImageAssetID: good},
			{ImageAssetID: missing},
		})
		if err == nil {
			t.Fatalf("iteration %d: resolve succeeded although the second asset does not exist", i)
		}
		release()
	}

	// After more leaked bytes than the budget holds, a full-size dispatch must still be
	// admissible. If reservations leak, this is where a real deployment starts shedding
	// dispatches for capacity nothing is using — and it never recovers without a restart.
	whole, ok := d.assets.reserve(context.Background(), MaxConcurrentVariantAssetBytes)
	if !ok {
		t.Fatalf("after %d failed resolves the budget can no longer admit a full-size dispatch: "+
			"each failure leaked its reservations, so the process-wide dispatch budget is "+
			"permanently exhausted until restart", iterations)
	}
	whole()
}

// TestResolveVariantAssets_ReleaseIsIdempotent pins that the SUCCESS-path releaser is safe to
// call more than once, because a weighted semaphore PANICS on over-release rather than failing
// quietly — "semaphore: released more than held" would take the process down.
//
// The success path is the reachable case, and it is the only one. On an ERROR the callee's own
// defer releases and hands back a no-op, so a second call there is free no matter what. On
// SUCCESS the callee returns the real releaseAll and the caller owns it; anything that called it
// twice — a future retry wrapper, a second defer added during a refactor — would over-release.
// `releases = nil` inside releaseAll is what makes the second call a no-op.
//
// Stated plainly: on TODAY's code releaseAll cannot run twice, because exactly one deferred call
// in Dispatch owns it. This guards the shape rather than a live bug, and it is cheap.
func TestResolveVariantAssets_ReleaseIsIdempotent(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"

	assets := &multiCreativeAssets{assets: map[string]*model.CreativeAsset{
		id: {Bytes: make([]byte, 4<<20), MimeType: "image/png"},
	}}
	d := &MetaDispatcher{creatives: assets}
	d.SetAssetReserver(NewAssetReserver(MaxConcurrentVariantAssetBytes, 20*time.Millisecond))

	// SUCCESS, so the returned releaser is the real accumulated one, not a no-op.
	_, release, err := d.resolveVariantAssets(context.Background(), testBrief(),
		[]meta.AdVariant{{ImageAssetID: id}})
	if err != nil {
		t.Fatalf("resolveVariantAssets: %v", err)
	}

	// Both must be safe. Without `releases = nil` the second call panics the process.
	release()
	release()

	// And the budget must be exactly whole — not over-released into a budget larger than real,
	// which would silently raise the memory ceiling this bound exists to hold.
	whole, ok := d.assets.reserve(context.Background(), MaxConcurrentVariantAssetBytes)
	if !ok {
		t.Fatal("budget not whole after a double release")
	}
	whole()
}

// TestResolveVariantAssets_SuccessPathReleasesExactlyOnce checks the OTHER half of the lifecycle:
// on success the callee must NOT release (the bytes outlive the call and the caller owns them),
// and the caller's single deferred call must return the whole reservation.
//
// Without this, a defer that released on every path — not just the error path — would look
// correct in the leak tests above while handing budget back while the bytes were still resident,
// which is the bound inverted rather than fixed.
func TestResolveVariantAssets_SuccessPathReleasesExactlyOnce(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const size = 16 << 20

	assets := &multiCreativeAssets{assets: map[string]*model.CreativeAsset{
		id: {Bytes: make([]byte, size), MimeType: "image/png"},
	}}
	d := &MetaDispatcher{creatives: assets}
	d.SetAssetReserver(NewAssetReserver(MaxConcurrentVariantAssetBytes, 20*time.Millisecond))

	out, release, err := d.resolveVariantAssets(context.Background(), testBrief(),
		[]meta.AdVariant{{ImageAssetID: id}})
	if err != nil {
		t.Fatalf("resolveVariantAssets: %v", err)
	}
	if len(out) != 1 || len(out[0].ImageBytes) != size {
		t.Fatalf("resolved bytes = %d, want %d", len(out[0].ImageBytes), size)
	}

	// STILL HELD at this point: the caller has the bytes, so the budget must reflect them.
	// Asking for the whole budget must fail while this dispatch holds its share.
	if _, ok := d.assets.reserve(context.Background(), MaxConcurrentVariantAssetBytes); ok {
		t.Error("the full budget was available while a successful resolve still held its assets: " +
			"the reservation was released before the bytes stopped being resident")
	}

	release()

	whole, ok := d.assets.reserve(context.Background(), MaxConcurrentVariantAssetBytes)
	if !ok {
		t.Fatal("the budget did not return to full after the caller released")
	}
	whole()
}

// TestResolveVariantAssets_ReleasesReservationsOnPanic covers the exit a `defer` exists for and
// that an error-keyed guard silently misses.
//
// The obvious shape for the unwind is `defer func(){ if err != nil { releaseAll() } }()` with a
// named error return. It is wrong in one place: on a PANIC no return statement runs, so err is
// still nil and the release is skipped — leaking every reservation taken so far, on the one path
// where the process is already in trouble. Keying on "did this hand the reservation to the
// caller" instead covers it, because the success flag is only set immediately before the single
// successful return.
//
// The panic is injected through the repository fake rather than by editing the loop, so this
// tests the real function. The reservation is asserted returned by taking the whole budget after
// recovering.
func TestResolveVariantAssets_ReleasesReservationsOnPanic(t *testing.T) {
	const good = "11111111-1111-4111-8111-111111111111"
	const boom = "22222222-2222-4222-8222-222222222222"

	assets := &panickingCreativeAssets{
		inner: &multiCreativeAssets{assets: map[string]*model.CreativeAsset{
			good: {Bytes: make([]byte, 8<<20), MimeType: "image/png"},
		}},
		panicOn: boom,
	}
	d := &MetaDispatcher{creatives: assets}
	d.SetAssetReserver(NewAssetReserver(MaxConcurrentVariantAssetBytes, 20*time.Millisecond))

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the fixture did not panic, so this test proves nothing about the panic path")
			}
		}()
		//nolint:errcheck // the call panics by construction; the recover above is the assertion
		_, _, _ = d.resolveVariantAssets(context.Background(), testBrief(), []meta.AdVariant{
			{ImageAssetID: good},
			{ImageAssetID: boom},
		})
	}()

	// The first asset reserved 8 MiB before the panic. If the unwind is keyed on the error
	// return rather than on success, that reservation is gone for the life of the process.
	whole, ok := d.assets.reserve(context.Background(), MaxConcurrentVariantAssetBytes)
	if !ok {
		t.Fatal("the budget did not return to full after a panic mid-resolve: reservations taken " +
			"before the panic were leaked, because the unwind is keyed on the error return and a " +
			"panic sets no error")
	}
	whole()
}

// panickingCreativeAssets panics on ONE asset id and delegates everything else, so a test can
// reach the panic path of a real loop without editing the code under test.
type panickingCreativeAssets struct {
	inner   *multiCreativeAssets
	panicOn string
}

func (p *panickingCreativeAssets) GetAsset(ctx context.Context, projectID, briefID, assetID string) (*model.CreativeAsset, error) {
	if assetID == p.panicOn {
		panic("creative-asset store exploded")
	}
	return p.inner.GetAsset(ctx, projectID, briefID, assetID)
}

func (p *panickingCreativeAssets) GetAssetSize(ctx context.Context, projectID, briefID, assetID string) (int64, error) {
	if assetID == p.panicOn {
		panic("creative-asset store exploded")
	}
	return p.inner.GetAssetSize(ctx, projectID, briefID, assetID)
}
