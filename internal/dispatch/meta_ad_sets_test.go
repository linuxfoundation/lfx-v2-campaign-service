// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// adSetAPI is a fake Graph API answering by "METHOD path" and recording every request under a
// mutex. The handler never calls t.Fatal; the test reads the record through requests() after the
// call under test has returned.
type adSetAPI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]settingsRoute
	seen   []budgetRequest
}

func newAdSetAPI(t *testing.T, routes map[string]settingsRoute) *adSetAPI {
	t.Helper()
	a := &adSetAPI{routes: routes}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.seen = append(a.seen, budgetRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		rep, ok := a.routes[r.Method+" "+r.URL.Path]
		a.mu.Unlock()
		if !ok {
			rep = settingsRoute{status: http.StatusTeapot, body: `{}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *adSetAPI) requests() []budgetRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]budgetRequest(nil), a.seen...)
}

func (a *adSetAPI) count(method string) int {
	n := 0
	for _, r := range a.requests() {
		if r.Method == method {
			n++
		}
	}
	return n
}

const (
	adSetGraphMissing = `{"error":{"message":"Unsupported get request","type":"GraphMethodException","code":100,"error_subcode":33}}`
	adSetStateActive  = `{"id":"888","campaign_id":"555","account_id":"777","status":"ACTIVE"}`
	adSetStatePaused  = `{"id":"888","campaign_id":"555","account_id":"777","status":"PAUSED"}`
)

func adSetReadRoutes() map[string]settingsRoute {
	return map[string]settingsRoute{
		"GET /555/adsets": {status: 200, body: `{"data":[` +
			`{"id":"888","name":"Leads US","status":"ACTIVE","effective_status":"ACTIVE","daily_budget":"5000","bid_strategy":"LOWEST_COST_WITHOUT_CAP","campaign_id":"555","account_id":"777"},` +
			`{"id":"889","name":"Leads EU","status":"PAUSED","effective_status":"PAUSED","campaign_id":"555","account_id":"777"}` +
			`],"paging":{}}`},
		"GET /act_777": {status: 200, body: `{"currency":"USD"}`},
		"GET /act_777/insights": {status: 200, body: `{"data":[` +
			`{"adset_id":"888","campaign_id":"555","account_currency":"USD","impressions":"1000","clicks":"20","spend":"7.25"}` +
			`],"paging":{}}`},
	}
}

func adSetToggleRoutes(state string) map[string]settingsRoute {
	return map[string]settingsRoute{
		"GET /888":  {status: 200, body: state},
		"POST /888": {status: 200, body: `{"success":true}`},
	}
}

func adSetDispatcher(t *testing.T, routes map[string]settingsRoute, opts ...meta.Option) (*MetaDispatcher, *adSetAPI) {
	t.Helper()
	api := newAdSetAPI(t, routes)
	all := append([]meta.Option{meta.WithBaseURL(api.srv.URL)}, opts...)
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, all...)
	d.settingsNow = pinnedSettingsClock
	return d, api
}

func adSetRow() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderMetaAds, PlatformCampaignID: "555", Status: model.CampaignStatusCreated,
		Result: json.RawMessage(`{"AccountID":"act_777","CampaignID":"555","AdSetID":"888"}`),
	}
}

func adoptedAdSetRow(t *testing.T) *model.Campaign {
	t.Helper()
	ref, err := adoptedRef("555", "Adopted", &meta.CampaignResult{Platform: string(model.ProviderMetaAds), CampaignID: "555", AccountID: "act_777"})
	if err != nil {
		t.Fatal(err)
	}
	return &model.Campaign{ID: "camp-1", Platform: model.ProviderMetaAds, PlatformCampaignID: "555", Status: model.CampaignStatusCreated, Result: ref.Result}
}

// ---- read -----------------------------------------------------------------

func TestMeta_ReadMetaAdSets_RendersBudgetsMetricsAndRecorded(t *testing.T) {
	d, api := adSetDispatcher(t, adSetReadRoutes())
	got, err := d.ReadMetaAdSets(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("ReadMetaAdSets: %v", err)
	}
	if got.PlatformCampaignID != "555" || got.Currency != "USD" || got.Window != model.MetricsWindowLast7Days || !got.ReadAt.Equal(settingsClock) || len(got.AdSets) != 2 {
		t.Fatalf("header = %+v", got)
	}
	a, b := got.AdSets[0], got.AdSets[1]
	if !a.Recorded || b.Recorded {
		t.Errorf("recorded = %v/%v, want only the row's ad set 888", a.Recorded, b.Recorded)
	}
	if a.BudgetType == nil || *a.BudgetType != model.BudgetDaily || derefOr(a.BudgetAmount) != "50.00" ||
		a.Impressions != 1000 || a.Clicks != 20 || a.CostMicros != 7_250_000 || a.Ctr != 0.02 {
		t.Errorf("888 = %+v", a)
	}
	// No ad-set budget (CBO) and no delivery: absent budget, zero counters.
	if b.BudgetType != nil || b.BudgetAmount != nil || b.Impressions != 0 {
		t.Errorf("889 = %+v", b)
	}
	assertSettingsReadOnly(t, api.requests(), "")
}

func TestMeta_ReadMetaAdSets_CurrencyOffsets(t *testing.T) {
	t.Run("zero-decimal", func(t *testing.T) {
		r := adSetReadRoutes()
		r["GET /act_777"] = settingsRoute{status: 200, body: `{"currency":"JPY"}`}
		r["GET /act_777/insights"] = settingsRoute{status: 200, body: `{"data":[],"paging":{}}`}
		d, _ := adSetDispatcher(t, r)
		got, err := d.ReadMetaAdSets(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), model.MetricsWindowLast30Days)
		if err != nil || derefOr(got.AdSets[0].BudgetAmount) != "5000.00" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("unmapped currency leaves the amount absent, never guessed", func(t *testing.T) {
		r := adSetReadRoutes()
		r["GET /act_777"] = settingsRoute{status: 200, body: `{"currency":"ZZZ"}`}
		r["GET /act_777/insights"] = settingsRoute{status: 200, body: `{"data":[],"paging":{}}`}
		d, _ := adSetDispatcher(t, r)
		got, err := d.ReadMetaAdSets(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), model.MetricsWindowLast30Days)
		if err != nil || got.AdSets[0].BudgetAmount != nil || got.AdSets[0].BudgetType == nil {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
}

func TestMeta_ReadMetaAdSets_ProvenanceRefusedBeforeAnyRequest(t *testing.T) {
	t.Run("unknown provenance", func(t *testing.T) {
		var consulted atomic.Bool
		api := newAdSetAPI(t, adSetReadRoutes())
		d := NewMetaDispatcher(unresolvableConn{consulted: &consulted}, identityEncryptor{}, meta.WithBaseURL(api.srv.URL))
		row := adSetRow()
		row.Result = json.RawMessage(`{"CampaignID":"555","AdSetID":"888"}`)
		_, err := d.ReadMetaAdSets(context.Background(), "proj", model.ProviderMetaAds, row, model.MetricsWindowLast30Days)
		assertProvenanceUnknown(t, err)
		if consulted.Load() || len(api.requests()) != 0 {
			t.Fatalf("unknown provenance resolved a connection (%v) or sent %d request(s)", consulted.Load(), len(api.requests()))
		}
	})
	t.Run("account mismatch", func(t *testing.T) {
		d, api := adSetDispatcher(t, adSetReadRoutes())
		row := adSetRow()
		row.Result = json.RawMessage(`{"AccountID":"act_999","CampaignID":"555","AdSetID":"888"}`)
		_, err := d.ReadMetaAdSets(context.Background(), "proj", model.ProviderMetaAds, row, model.MetricsWindowLast30Days)
		assertMismatch(t, err)
		if n := len(api.requests()); n != 0 {
			t.Fatalf("a mismatched account sent %d request(s)", n)
		}
	})
}

func TestMeta_ReadMetaAdSets_AdSetUnderAnotherAccountIsAnIdentityMismatch(t *testing.T) {
	r := adSetReadRoutes()
	r["GET /555/adsets"] = settingsRoute{status: 200, body: `{"data":[{"id":"888","campaign_id":"555","account_id":"999"}],"paging":{}}`}
	d, _ := adSetDispatcher(t, r)
	_, err := d.ReadMetaAdSets(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), model.MetricsWindowLast30Days)
	assertUpstreamIdentityMismatch(t, err)
}

// Graph 100/33 on the campaign: 503 on every HTTP status, never absence.
func TestMeta_ReadMetaAdSets_ObjectMissingIs503NeverAbsent(t *testing.T) {
	for _, status := range []int{400, 403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			r := adSetReadRoutes()
			r["GET /555/adsets"] = settingsRoute{status: status, body: adSetGraphMissing}
			d, _ := adSetDispatcher(t, r)
			got, err := d.ReadMetaAdSets(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), model.MetricsWindowLast30Days)
			if got != nil {
				t.Fatalf("an unverifiable read returned %+v", got)
			}
			assertSettings503(t, err)
		})
	}
}

func TestMeta_ReadMetaAdSets_UnsupportedWindowIsRefusedFirst(t *testing.T) {
	d, api := adSetDispatcher(t, adSetReadRoutes())
	_, err := d.ReadMetaAdSets(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), model.MetricsWindow("last_90_days"))
	if !errors.Is(err, domain.ErrMetricsWindowUnsupported) || len(api.requests()) != 0 {
		t.Fatalf("err = %v, requests = %d", err, len(api.requests()))
	}
}

// ---- toggle ---------------------------------------------------------------

func TestMeta_ToggleMetaAdSetStatus_AppliesWithExactlyOneWrite(t *testing.T) {
	d, api := adSetDispatcher(t, adSetToggleRoutes(adSetStateActive))
	res, err := d.ToggleMetaAdSetStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetaAdSetStatusPaused)
	if err != nil || res.Outcome != model.MetaAdSetApplied || res.PreviousStatus != "ACTIVE" {
		t.Fatalf("got %+v, %v", res, err)
	}
	reqs := api.requests()
	if len(reqs) != 2 || reqs[0].Method != http.MethodGet || reqs[1].Method != http.MethodPost || reqs[1].Path != "/888" || reqs[1].Body != `{"status":"PAUSED"}` {
		t.Fatalf("requests = %+v, want the read then ONE POST /888 {status:PAUSED}", reqs)
	}
}

func TestMeta_ToggleMetaAdSetStatus_AlreadyInStateSendsNoWrite(t *testing.T) {
	d, api := adSetDispatcher(t, adSetToggleRoutes(adSetStatePaused))
	res, err := d.ToggleMetaAdSetStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetaAdSetStatusPaused)
	if err != nil || res.Outcome != model.MetaAdSetAlreadyInState || res.PreviousStatus != "PAUSED" {
		t.Fatalf("got %+v, %v", res, err)
	}
	if n := api.count(http.MethodPost); n != 0 {
		t.Fatalf("ALREADY_IN_STATE still sent %d write(s)", n)
	}
}

func TestMeta_ToggleMetaAdSetStatus_RefusalsSendNoRequestAtAll(t *testing.T) {
	for _, tc := range []struct {
		name   string
		row    func(t *testing.T) *model.Campaign
		adSet  string
		status string
		check  func(t *testing.T, err error)
	}{
		{"invalid ad set id", func(*testing.T) *model.Campaign { return adSetRow() }, "08", model.MetaAdSetStatusPaused,
			func(t *testing.T, err error) {
				if !errors.Is(err, domain.ErrMetaAdSetInvalid) {
					t.Fatalf("err = %v", err)
				}
			}},
		{"unknown provenance", func(*testing.T) *model.Campaign {
			r := adSetRow()
			r.Result = json.RawMessage(`{"CampaignID":"555","AdSetID":"888"}`)
			return r
		}, "888", model.MetaAdSetStatusPaused, assertProvenanceUnknown},
		{"account mismatch", func(*testing.T) *model.Campaign {
			r := adSetRow()
			r.Result = json.RawMessage(`{"AccountID":"act_999","CampaignID":"555","AdSetID":"888"}`)
			return r
		}, "888", model.MetaAdSetStatusPaused, assertMismatch},
		{"ACTIVATE on an adopted row", adoptedAdSetRow, "888", model.MetaAdSetStatusActive,
			func(t *testing.T, err error) {
				if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
					t.Fatalf("err = %v, want ErrCampaignNotProvisioned", err)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, api := adSetDispatcher(t, adSetToggleRoutes(adSetStatePaused))
			res, err := d.ToggleMetaAdSetStatus(context.Background(), "proj", model.ProviderMetaAds, tc.row(t), tc.adSet, tc.status)
			if res != nil {
				t.Fatalf("a refusal returned %+v", res)
			}
			tc.check(t, err)
			if n := len(api.requests()); n != 0 {
				t.Fatalf("a refusal sent %d request(s)", n)
			}
		})
	}
}

// PAUSE is always allowed, adopted or not.
func TestMeta_ToggleMetaAdSetStatus_AdoptedRowCanPause(t *testing.T) {
	d, api := adSetDispatcher(t, adSetToggleRoutes(adSetStateActive))
	res, err := d.ToggleMetaAdSetStatus(context.Background(), "proj", model.ProviderMetaAds, adoptedAdSetRow(t), "888", model.MetaAdSetStatusPaused)
	if err != nil || res.Outcome != model.MetaAdSetApplied || api.count(http.MethodPost) != 1 {
		t.Fatalf("got %+v, %v, %d writes", res, err, api.count(http.MethodPost))
	}
}

func TestMeta_ToggleMetaAdSetStatus_OwnershipRefusedBeforeTheWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		want  error
	}{
		{"another campaign's ad set", `{"id":"888","campaign_id":"556","account_id":"777","status":"PAUSED"}`, domain.ErrMetaAdSetNotInCampaign},
		{"an ad set under another account", `{"id":"888","campaign_id":"555","account_id":"999","status":"PAUSED"}`, domain.ErrMetaAdSetNotInCampaign},
		{"a deleted ad set", `{"id":"888","campaign_id":"555","account_id":"777","status":"DELETED"}`, domain.ErrMetaAdSetUnwritable},
		{"an archived ad set", `{"id":"888","campaign_id":"555","account_id":"777","status":"ARCHIVED"}`, domain.ErrMetaAdSetUnwritable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, api := adSetDispatcher(t, adSetToggleRoutes(tc.state))
			_, err := d.ToggleMetaAdSetStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetaAdSetStatusActive)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if n := api.count(http.MethodPost); n != 0 {
				t.Fatalf("sent %d write(s) to an ad set it may not touch", n)
			}
		})
	}
}

// A failed or unverifiable PRE-WRITE read is definite (nothing was written), Graph 100/33
// included: never UNCONFIRMED, never absence.
func TestMeta_ToggleMetaAdSetStatus_PreWriteReadFailureIsDefinite(t *testing.T) {
	for _, rep := range []settingsRoute{
		{status: 400, body: adSetGraphMissing},
		{status: 500, body: `{"error":{"message":"boom","code":1}}`},
		{status: 200, body: `{"id":"888","campaign_id":"555","account_id":"777","status":"WEIRD"}`},
	} {
		r := adSetToggleRoutes("")
		r["GET /888"] = rep
		d, api := adSetDispatcher(t, r)
		_, err := d.ToggleMetaAdSetStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetaAdSetStatusPaused)
		var u interface{ Unconfirmed() bool }
		if err == nil || errors.As(err, &u) || errors.Is(err, domain.ErrPlatformCampaignAbsent) {
			t.Fatalf("%d: err = %v, want a definite failure", rep.status, err)
		}
		if n := api.count(http.MethodPost); n != 0 {
			t.Fatalf("%d: sent %d write(s)", rep.status, n)
		}
	}
}

func TestMeta_ToggleMetaAdSetStatus_WriteClassification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		write       settingsRoute
		unconfirmed bool
	}{
		{"429 after send", settingsRoute{status: 429, body: `{"error":{"message":"slow down","code":4}}`}, true},
		{"400 throttle code after send", settingsRoute{status: 400, body: `{"error":{"message":"limit","code":80004}}`}, true},
		{"5xx", settingsRoute{status: 503, body: `{"error":{"message":"down","code":2}}`}, true},
		{"2xx without success", settingsRoute{status: 200, body: `{}`}, true},
		{"definite 4xx", settingsRoute{status: 400, body: `{"error":{"message":"Invalid parameter","code":100}}`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := adSetToggleRoutes(adSetStateActive)
			r["POST /888"] = tc.write
			d, api := adSetDispatcher(t, r)
			res, err := d.ToggleMetaAdSetStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetaAdSetStatusPaused)
			if res != nil || err == nil {
				t.Fatalf("got %+v, %v; a failed write is never a result", res, err)
			}
			var u interface{ Unconfirmed() bool }
			if got := errors.As(err, &u) && u.Unconfirmed(); got != tc.unconfirmed {
				t.Fatalf("unconfirmed = %v, want %v (err %v)", got, tc.unconfirmed, err)
			}
			if n := api.count(http.MethodPost); n != 1 {
				t.Fatalf("sent %d writes, want exactly 1 (never retried)", n)
			}
		})
	}
}

// refusePOST lets GETs through and fails every POST with a pre-connect dial error, the shape of a
// request that never left this process.
type refusePOST struct{ next http.RoundTripper }

func (r refusePOST) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	return r.next.RoundTrip(req)
}

func TestMeta_ToggleMetaAdSetStatus_PreSendFailureIsNotSent(t *testing.T) {
	d, api := adSetDispatcher(t, adSetToggleRoutes(adSetStateActive),
		meta.WithHTTPClient(&http.Client{Transport: refusePOST{next: http.DefaultTransport}}))
	_, err := d.ToggleMetaAdSetStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetaAdSetStatusPaused)
	var u interface{ Unconfirmed() bool }
	if err == nil || errors.As(err, &u) {
		t.Fatalf("err = %v, want a definite not-sent failure", err)
	}
	if api.count(http.MethodPost) != 0 || api.count(http.MethodGet) != 1 {
		t.Fatalf("requests = %+v", api.requests())
	}
}
