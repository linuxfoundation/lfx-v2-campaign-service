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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// #550 gap 14: an ad an operator paused through toggle-meta-ad-status stays paused across a
// campaign resume, and the per-ad lever mirrors the ad-set lever.

// adCascadeServer answers a campaign status cascade under campaign 23847290 / ad set 999 / account
// 777 with ads 111 and 222. A GET of an ad node answers that ad at adStatus[id] ("" answers 500).
func adCascadeServer(t *testing.T, adStatus map[string]string) (*httptest.Server, func() []cascadeReq) {
	t.Helper()
	var mu sync.Mutex
	var seen []cascadeReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ads") {
			seen = append(seen, cascadeReq{r.Method, r.URL.Path, ""})
			_, _ = io.WriteString(w, `{"data":[{"id":"111"},{"id":"222"}]}`)
			return
		}
		if r.Method == http.MethodGet {
			seen = append(seen, cascadeReq{r.Method, r.URL.Path, ""})
			id := strings.TrimPrefix(r.URL.Path, "/")
			st, ok := adStatus[id]
			if !ok || st == "" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":{"message":"unavailable","code":2}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"`+id+`","adset_id":"999","campaign_id":"23847290","account_id":"777","status":"`+st+`"}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		st, _ := body["status"].(string)
		seen = append(seen, cascadeReq{r.Method, r.URL.Path, st})
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []cascadeReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]cascadeReq(nil), seen...)
	}
}

// adPausedOnRecord returns c with deliberate pauses of adIDs recorded by the per-ad lever's helper.
func adPausedOnRecord(t *testing.T, c *model.Campaign, adIDs ...string) *model.Campaign {
	t.Helper()
	for _, id := range adIDs {
		res, err := model.WithMetaUserPausedAd(c.Result, id, true)
		if err != nil {
			t.Fatal(err)
		}
		c.Result = res
	}
	return c
}

// TestMeta_ToggleStatus_ActivateLeavesADeliberatelyPausedAdPaused: the held ad is read first, then
// not posted; its sibling, the ad set and the campaign are activated.
func TestMeta_ToggleStatus_ActivateLeavesADeliberatelyPausedAdPaused(t *testing.T) {
	srv, seen := adCascadeServer(t, map[string]string{"111": meta.StatusPaused})
	d := cascadeDispatcher(srv)
	row := adPausedOnRecord(t, metaToggleCampaign("23847290", "999"), "111")
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, row, model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	reqs := seen()
	if len(reqs) == 0 || reqs[0].method != http.MethodGet || reqs[0].path != "/111" {
		t.Fatalf("requests = %+v, want the held ad read first", reqs)
	}
	posts := postsByPath(reqs)
	if _, touched := posts["/111"]; touched {
		t.Fatalf("the deliberately paused ad was posted (%q); it must stay PAUSED", posts["/111"])
	}
	for _, p := range []string{"/23847290", "/999", "/222"} {
		if posts[p] != meta.StatusActive {
			t.Errorf("POST %s = %q, want ACTIVE", p, posts[p])
		}
	}
}

// TestMeta_ToggleStatus_HeldAdAndHeldAdSetCompose: both holds apply in one activation.
func TestMeta_ToggleStatus_HeldAdAndHeldAdSetCompose(t *testing.T) {
	srv, seen := adCascadeServer(t, map[string]string{"222": meta.StatusPaused, "999": meta.StatusPaused})
	d := cascadeDispatcher(srv)
	row := adPausedOnRecord(t, pausedOnRecord(t, metaToggleCampaign("23847290", "999"), "999"), "222")
	// The ad-set pre-read decodes the same node shape (adset_id is ignored there).
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, row, model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	posts := postsByPath(seen())
	if _, ok := posts["/999"]; ok {
		t.Errorf("held ad set was posted")
	}
	if _, ok := posts["/222"]; ok {
		t.Errorf("held ad was posted")
	}
	if posts["/111"] != meta.StatusActive || posts["/23847290"] != meta.StatusActive {
		t.Errorf("posts = %v, want ad 111 and the campaign ACTIVE", posts)
	}
}

// TestMeta_ToggleStatus_HeldAdThatIsActiveRefusesTheActivation: Meta reports the held ad ACTIVE,
// so nothing is written and the error is ErrMetaHeldAdActive; an unreadable held ad writes nothing.
func TestMeta_ToggleStatus_HeldAdThatIsActiveRefusesTheActivation(t *testing.T) {
	for name, tc := range map[string]struct {
		status string
		want   error
	}{
		"active":     {meta.StatusActive, domain.ErrMetaHeldAdActive},
		"unreadable": {"", nil},
	} {
		t.Run(name, func(t *testing.T) {
			srv, seen := adCascadeServer(t, map[string]string{"111": tc.status})
			d := cascadeDispatcher(srv)
			row := adPausedOnRecord(t, metaToggleCampaign("23847290", "999"), "111")
			err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, row, model.CampaignRunActive)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || (tc.want == nil && errors.Is(err, domain.ErrMetaHeldAdActive)) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if posts := postsByPath(seen()); len(posts) != 0 {
				t.Fatalf("posts = %v, want none", posts)
			}
		})
	}
}

// TestMeta_ToggleStatus_PauseStillCascadesToAHeldAd: PAUSE posts every ad, held or not, and reads
// nothing first.
func TestMeta_ToggleStatus_PauseStillCascadesToAHeldAd(t *testing.T) {
	srv, seen := adCascadeServer(t, nil)
	d := cascadeDispatcher(srv)
	row := adPausedOnRecord(t, metaToggleCampaign("23847290", "999"), "111")
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, row, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	posts := postsByPath(seen())
	for _, p := range []string{"/23847290", "/999", "/111", "/222"} {
		if posts[p] != meta.StatusPaused {
			t.Errorf("POST %s = %q, want PAUSED", p, posts[p])
		}
	}
}

// ---- the per-ad lever ---------------------------------------------------------

const (
	adStateActive = `{"id":"901","adset_id":"888","campaign_id":"555","account_id":"777","status":"ACTIVE"}`
	adStatePaused = `{"id":"901","adset_id":"888","campaign_id":"555","account_id":"777","status":"PAUSED"}`
)

func adToggleRoutes(state string) map[string]settingsRoute {
	return map[string]settingsRoute{
		"GET /901":  {status: 200, body: state},
		"POST /901": {status: 200, body: `{"success":true}`},
	}
}

func TestMeta_ToggleMetaAdStatus_AppliesWithExactlyOneWrite(t *testing.T) {
	d, api := adSetDispatcher(t, adToggleRoutes(adStateActive))
	hook := 0
	res, err := d.ToggleMetaAdStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", "901", model.MetaAdSetStatusPaused,
		func(context.Context) error {
			hook++
			if n := api.count(http.MethodPost); n != 0 {
				t.Errorf("the hook ran after %d write(s); it must run before the write", n)
			}
			return nil
		})
	if err != nil || res.Outcome != model.MetaAdSetApplied || res.PreviousStatus != "ACTIVE" || res.AdID != "901" || hook != 1 {
		t.Fatalf("got %+v, %v, hook %d", res, err, hook)
	}
	reqs := api.requests()
	if len(reqs) != 2 || reqs[0].Method != http.MethodGet || reqs[1].Path != "/901" || reqs[1].Body != `{"status":"PAUSED"}` {
		t.Fatalf("requests = %+v, want the read then ONE POST /901 {status:PAUSED}", reqs)
	}
}

func TestMeta_ToggleMetaAdStatus_AlreadyInStateSendsNoWriteAndNoHook(t *testing.T) {
	d, api := adSetDispatcher(t, adToggleRoutes(adStatePaused))
	res, err := d.ToggleMetaAdStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", "901", model.MetaAdSetStatusPaused,
		func(context.Context) error { t.Error("hook ran with nothing to send"); return nil })
	if err != nil || res.Outcome != model.MetaAdSetAlreadyInState {
		t.Fatalf("got %+v, %v", res, err)
	}
	if n := api.count(http.MethodPost); n != 0 {
		t.Fatalf("ALREADY_IN_STATE sent %d write(s)", n)
	}
}

func TestMeta_ToggleMetaAdStatus_HookFailureSendsNothing(t *testing.T) {
	d, api := adSetDispatcher(t, adToggleRoutes(adStateActive))
	_, err := d.ToggleMetaAdStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", "901", model.MetaAdSetStatusPaused,
		func(context.Context) error { return errors.New("db down") })
	if err == nil || !strings.Contains(err.Error(), "nothing was sent") || api.count(http.MethodPost) != 0 {
		t.Fatalf("err = %v, posts = %d", err, api.count(http.MethodPost))
	}
}

func TestMeta_ToggleMetaAdStatus_RefusalsSendNoRequestAtAll(t *testing.T) {
	for _, tc := range []struct {
		name, adSet, ad, status string
		row                     func(t *testing.T) *model.Campaign
		want                    error
	}{
		{"invalid ad set id", "08", "901", model.MetaAdSetStatusPaused, func(*testing.T) *model.Campaign { return adSetRow() }, domain.ErrMetaAdSetInvalid},
		{"invalid ad id", "888", "9x", model.MetaAdSetStatusPaused, func(*testing.T) *model.Campaign { return adSetRow() }, domain.ErrMetaAdInvalid},
		{"ACTIVATE on an adopted row", "888", "901", model.MetaAdSetStatusActive, adoptedAdSetRow, domain.ErrCampaignNotProvisioned},
		{"ACTIVATE under another ad set", "889", "901", model.MetaAdSetStatusActive, func(*testing.T) *model.Campaign { return adSetRow() }, domain.ErrMetaAdNotRecorded},
		{"unknown provenance", "888", "901", model.MetaAdSetStatusPaused, func(*testing.T) *model.Campaign {
			r := adSetRow()
			r.Result = json.RawMessage(`{"CampaignID":"555","AdSetID":"888"}`)
			return r
		}, domain.ErrCampaignProvenanceUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, api := adSetDispatcher(t, adToggleRoutes(adStatePaused))
			res, err := d.ToggleMetaAdStatus(context.Background(), "proj", model.ProviderMetaAds, tc.row(t), tc.adSet, tc.ad, tc.status, nil)
			if res != nil || !errors.Is(err, tc.want) {
				t.Fatalf("got %+v, %v; want %v", res, err, tc.want)
			}
			if n := len(api.requests()); n != 0 {
				t.Fatalf("a refusal sent %d request(s)", n)
			}
		})
	}
}

// TestMeta_ToggleMetaAdStatus_OwnershipAndStateAreReadBeforeTheWrite: an ad of another ad set,
// campaign or account, or one DELETED, is refused after the read with no write; PAUSE of an ad of a
// non-recorded ad set is allowed.
func TestMeta_ToggleMetaAdStatus_OwnershipAndStateAreReadBeforeTheWrite(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		want        error
	}{
		{"another ad set", `{"id":"901","adset_id":"777001","campaign_id":"555","account_id":"777","status":"ACTIVE"}`, domain.ErrMetaAdNotInAdSet},
		{"another campaign", `{"id":"901","adset_id":"888","campaign_id":"556","account_id":"777","status":"ACTIVE"}`, domain.ErrMetaAdNotInAdSet},
		{"another account", `{"id":"901","adset_id":"888","campaign_id":"555","account_id":"778","status":"ACTIVE"}`, domain.ErrMetaAdNotInAdSet},
		{"deleted", `{"id":"901","adset_id":"888","campaign_id":"555","account_id":"777","status":"DELETED"}`, domain.ErrMetaAdUnwritable},
		{"archived", `{"id":"901","adset_id":"888","campaign_id":"555","account_id":"777","status":"ARCHIVED"}`, domain.ErrMetaAdUnwritable},
		{"no adset_id", `{"id":"901","campaign_id":"555","account_id":"777","status":"ACTIVE"}`, nil},
		{"unknown status", `{"id":"901","adset_id":"888","campaign_id":"555","account_id":"777","status":"IN_PROCESS"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, api := adSetDispatcher(t, adToggleRoutes(tc.state))
			res, err := d.ToggleMetaAdStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", "901", model.MetaAdSetStatusPaused, nil)
			if res != nil || err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("got %+v, %v; want %v", res, err, tc.want)
			}
			if n := api.count(http.MethodPost); n != 0 {
				t.Fatalf("sent %d write(s)", n)
			}
		})
	}
	d, api := adSetDispatcher(t, map[string]settingsRoute{
		"GET /901":  {status: 200, body: `{"id":"901","adset_id":"889","campaign_id":"555","account_id":"777","status":"ACTIVE"}`},
		"POST /901": {status: 200, body: `{"success":true}`},
	})
	if res, err := d.ToggleMetaAdStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "889", "901", model.MetaAdSetStatusPaused, nil); err != nil || res.Outcome != model.MetaAdSetApplied || api.count(http.MethodPost) != 1 {
		t.Fatalf("pause under another ad set = %+v, %v", res, err)
	}
}

// TestMeta_ToggleMetaAdStatus_WriteClassification: Meta's answer to the single write is classified
// like the ad-set write's.
func TestMeta_ToggleMetaAdStatus_WriteClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		status      int
		unconfirmed bool
	}{
		{"2xx without success", `{}`, 200, true},
		{"5xx", `{"error":{"message":"boom","code":2}}`, 500, true},
		{"definite refusal", `{"error":{"message":"bad","type":"OAuthException","code":100}}`, 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := adSetDispatcher(t, map[string]settingsRoute{
				"GET /901":  {status: 200, body: adStateActive},
				"POST /901": {status: tc.status, body: tc.body},
			})
			_, err := d.ToggleMetaAdStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", "901", model.MetaAdSetStatusPaused, nil)
			var u interface{ Unconfirmed() bool }
			if err == nil || (errors.As(err, &u) && u.Unconfirmed()) != tc.unconfirmed {
				t.Fatalf("err = %v, want unconfirmed %v", err, tc.unconfirmed)
			}
		})
	}
}

// TestMeta_ReadMetaAds_ReportsTheDeliberateAdPause: list-meta-ads marks an ad of the recorded ad set
// whose pause is on record, and never one of another ad set.
func TestMeta_ReadMetaAds_ReportsTheDeliberateAdPause(t *testing.T) {
	routes := map[string]settingsRoute{
		"GET /888":              {status: 200, body: adSetStatePaused},
		"GET /888/ads":          {status: 200, body: `{"data":[{"id":"901","name":"A","status":"PAUSED","effective_status":"PAUSED","adset_id":"888","campaign_id":"555","account_id":"777"},{"id":"902","name":"B","status":"ACTIVE","effective_status":"ACTIVE","adset_id":"888","campaign_id":"555","account_id":"777"}],"paging":{}}`},
		"GET /act_777":          {status: 200, body: `{"currency":"USD","id":"act_777"}`},
		"GET /act_777/insights": {status: 200, body: `{"data":[],"paging":{}}`},
	}
	d, _ := adSetDispatcher(t, routes)
	row := adPausedOnRecord(t, adSetRow(), "901")
	got, err := d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, row, "888", model.MetricsWindowLast7Days)
	if err != nil || len(got.Ads) != 2 {
		t.Fatalf("ReadMetaAds = %+v, %v", got, err)
	}
	if !got.Ads[0].DeliberatelyPaused || got.Ads[1].DeliberatelyPaused {
		t.Fatalf("deliberately paused = %v, %v; want only ad 901", got.Ads[0].DeliberatelyPaused, got.Ads[1].DeliberatelyPaused)
	}
	other := adSetRow()
	other.Result = json.RawMessage(`{"AccountID":"act_777","CampaignID":"555","AdSetID":"777001","UserPausedAdIDs":["901"]}`)
	got, err = d.ReadMetaAds(context.Background(), "proj", model.ProviderMetaAds, other, "888", model.MetricsWindowLast7Days)
	if err != nil || got.Ads[0].DeliberatelyPaused {
		t.Fatalf("an ad of a non-recorded ad set was marked: %+v, %v", got, err)
	}
}

// TestMetaClient_ActivateCampaignHolding: held ads are skipped on ACTIVATE (the ad set is posted
// unless held too) and the campaign flip is still last; the full PAUSE cascade ignores holds.
func TestMetaClient_ActivateCampaignHolding(t *testing.T) {
	srv, seen := adCascadeServer(t, nil)
	c := meta.NewClient(meta.Credentials{AccessToken: "tok"}, meta.AccountConfig{AccountID: "act_777"}, meta.WithBaseURL(srv.URL))
	if err := c.ActivateCampaignHolding(context.Background(), "23847290", "999", false, []string{"222"}); err != nil {
		t.Fatalf("ActivateCampaignHolding: %v", err)
	}
	reqs := seen()
	posts := postsByPath(reqs)
	if _, touched := posts["/222"]; touched || posts["/111"] != meta.StatusActive || posts["/999"] != meta.StatusActive || posts["/23847290"] != meta.StatusActive {
		t.Fatalf("posts = %+v", posts)
	}
	if last := reqs[len(reqs)-1]; last.path != "/23847290" {
		t.Fatalf("last request = %+v, want the campaign flip", last)
	}
}

// TestMetaClient_UpdateAdStatusOnce_RefusesBeforeSending: a malformed id or status sends nothing.
func TestMetaClient_UpdateAdStatusOnce_RefusesBeforeSending(t *testing.T) {
	srv, seen := adCascadeServer(t, nil)
	c := meta.NewClient(meta.Credentials{AccessToken: "tok"}, meta.AccountConfig{AccountID: "act_777"}, meta.WithBaseURL(srv.URL))
	if err := c.UpdateAdStatusOnce(context.Background(), "9/../1", meta.StatusPaused); !errors.Is(err, meta.ErrInvalidAdID) {
		t.Fatalf("bad id err = %v", err)
	}
	if err := c.UpdateAdStatusOnce(context.Background(), "901", "DELETED"); err == nil {
		t.Fatal("a DELETED status was accepted")
	}
	if _, err := c.GetAdState(context.Background(), "0901"); !errors.Is(err, meta.ErrInvalidAdID) {
		t.Fatalf("GetAdState bad id err = %v", err)
	}
	if n := len(seen()); n != 0 {
		t.Fatalf("%d request(s) sent", n)
	}
}
