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
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// LFXV2-2665 product decision (2026-10-13): a deliberate pause of the recorded ad set survives a
// campaign pause/resume. These tests pin the dispatcher's half: the campaign toggle honours the
// record, the ad-set read reports it, and the ad-set toggle runs its beforeWrite hook exactly at
// the write.

type cascadeReq struct{ method, path, status string }

// cascadeServer records every request of a campaign status cascade; ads under any ad set are
// 111 and 222. A GET of an ad set node (the held-activation pre-read) answers with the ad set
// reported at status under campaign 23847290 and account 777; status "" answers 500.
func cascadeServer(t *testing.T) (*httptest.Server, func() []cascadeReq) {
	return cascadeServerWithAdSet(t, meta.StatusPaused)
}

func cascadeServerWithAdSet(t *testing.T, status string) (*httptest.Server, func() []cascadeReq) {
	t.Helper()
	var mu sync.Mutex
	var seen []cascadeReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ads") {
			mu.Lock()
			seen = append(seen, cascadeReq{r.Method, r.URL.Path, ""})
			mu.Unlock()
			_, _ = io.WriteString(w, `{"data":[{"id":"111"},{"id":"222"}]}`)
			return
		}
		if r.Method == http.MethodGet {
			mu.Lock()
			seen = append(seen, cascadeReq{r.Method, r.URL.Path, ""})
			mu.Unlock()
			if status == "" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":{"message":"unavailable","code":2}}`)
				return
			}
			id := strings.TrimPrefix(r.URL.Path, "/")
			_, _ = io.WriteString(w, `{"id":"`+id+`","campaign_id":"23847290","account_id":"777","status":"`+status+`"}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		status, _ := body["status"].(string)
		mu.Lock()
		seen = append(seen, cascadeReq{r.Method, r.URL.Path, status})
		mu.Unlock()
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []cascadeReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]cascadeReq(nil), seen...)
	}
}

func cascadeDispatcher(srv *httptest.Server) *MetaDispatcher {
	return NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
}

// pausedOnRecord returns c with a deliberate pause of adSetID recorded, written by the same model
// helper the ad-set lever uses.
func pausedOnRecord(t *testing.T, c *model.Campaign, adSetID string) *model.Campaign {
	t.Helper()
	res, err := model.WithMetaUserPausedAdSet(c.Result, adSetID)
	if err != nil {
		t.Fatal(err)
	}
	c.Result = res
	return c
}

func postsByPath(reqs []cascadeReq) map[string]string {
	out := map[string]string{}
	for _, r := range reqs {
		if r.method == http.MethodPost {
			out[r.path] = r.status
		}
	}
	return out
}

func TestMeta_ToggleStatus_ActivateLeavesADeliberatelyPausedAdSetPaused(t *testing.T) {
	srv, seen := cascadeServer(t)
	d := cascadeDispatcher(srv)
	row := pausedOnRecord(t, metaToggleCampaign("23847290", "999"), "999")
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, row, model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	posts := postsByPath(seen())
	if _, touched := posts["/999"]; touched {
		t.Fatalf("the deliberately paused ad set was posted (%q); it must stay PAUSED", posts["/999"])
	}
	for _, p := range []string{"/23847290", "/111", "/222"} {
		if posts[p] != meta.StatusActive {
			t.Errorf("POST %s = %q, want ACTIVE (the campaign and the ads are still activated)", p, posts[p])
		}
	}
}

func TestMeta_ToggleStatus_ActivateWithoutARecordStillActivatesTheAdSet(t *testing.T) {
	for name, row := range map[string]func(t *testing.T) *model.Campaign{
		"no record": func(*testing.T) *model.Campaign { return metaToggleCampaign("23847290", "999") },
		// A record naming any ad set but the recorded one does not hold the recorded one.
		"record names another ad set": func(t *testing.T) *model.Campaign {
			return pausedOnRecord(t, metaToggleCampaign("23847290", "999"), "998")
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv, seen := cascadeServer(t)
			d := cascadeDispatcher(srv)
			if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, row(t), model.CampaignRunActive); err != nil {
				t.Fatalf("ToggleStatus: %v", err)
			}
			if got := postsByPath(seen())["/999"]; got != meta.StatusActive {
				t.Fatalf("POST /999 = %q, want ACTIVE", got)
			}
		})
	}
}

func TestMeta_ToggleStatus_PauseStillCascadesToAHeldAdSet(t *testing.T) {
	srv, seen := cascadeServer(t)
	d := cascadeDispatcher(srv)
	row := pausedOnRecord(t, metaToggleCampaign("23847290", "999"), "999")
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

// The record is written into the create path's own Result blob: it must not disturb what the
// dispatcher reads from it, and the model's reader of the recorded ad set must agree with
// metaAdSetID on a blob the create path's type produced.
func TestMeta_PauseRecordRoundTripsThroughTheCreateResultBlob(t *testing.T) {
	raw, err := json.Marshal(meta.CampaignResult{CampaignID: "555", AccountID: "act_777", AdSetID: "888", Ads: []meta.AdResult{{Variant: 1, AdID: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Campaign{PlatformCampaignID: "555", Result: raw}
	if got := model.MetaRecordedAdSetID(row.Result); got != metaAdSetID(row) || got != "888" {
		t.Fatalf("model reads recorded %q, dispatcher reads %q", got, metaAdSetID(row))
	}
	pausedOnRecord(t, row, "888")
	if metaAdSetID(row) != "888" || metaCreationAccountID(row) != "act_777" || !model.MetaAdSetHeldPaused(row.Result, "888") {
		t.Fatalf("after recording: ad set %q, account %q, held %v", metaAdSetID(row), metaCreationAccountID(row), model.MetaAdSetHeldPaused(row.Result, "888"))
	}
	var blob meta.CampaignResult
	if err := json.Unmarshal(row.Result, &blob); err != nil || len(blob.Ads) != 1 || blob.CampaignID != "555" {
		t.Fatalf("blob after recording = %+v, %v", blob, err)
	}
	cleared, err := model.WithMetaUserPausedAdSet(row.Result, "")
	if err != nil || model.MetaAdSetHeldPaused(cleared, "888") || strings.Contains(string(cleared), model.MetaResultUserPausedAdSetKey) {
		t.Fatalf("cleared = %s, %v", cleared, err)
	}
}

func TestMeta_ReadMetaAdSets_ReportsTheDeliberatePause(t *testing.T) {
	d, _ := adSetDispatcher(t, adSetReadRoutes())
	got, err := d.ReadMetaAdSets(context.Background(), "proj", model.ProviderMetaAds, pausedOnRecord(t, adSetRow(), "888"), model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("ReadMetaAdSets: %v", err)
	}
	if !got.AdSets[0].DeliberatelyPaused || got.AdSets[1].DeliberatelyPaused {
		t.Fatalf("deliberately_paused = %v/%v, want only the recorded ad set 888", got.AdSets[0].DeliberatelyPaused, got.AdSets[1].DeliberatelyPaused)
	}
	plain, err := d.ReadMetaAdSets(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), model.MetricsWindowLast7Days)
	if err != nil || plain.AdSets[0].DeliberatelyPaused {
		t.Fatalf("without a record: %+v, %v", plain, err)
	}
}

func TestMeta_ToggleMetaAdSetStatus_BeforeWriteRunsOnceImmediatelyBeforeTheWrite(t *testing.T) {
	d, api := adSetDispatcher(t, adSetToggleRoutes(adSetStateActive))
	calls := 0
	var requestsAtHook int
	hook := func(context.Context) error {
		calls++
		requestsAtHook = len(api.requests())
		return nil
	}
	res, err := d.ToggleMetaAdSetStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetaAdSetStatusPaused, hook)
	if err != nil || res.Outcome != model.MetaAdSetApplied {
		t.Fatalf("got %+v, %v", res, err)
	}
	if calls != 1 || requestsAtHook != 1 || api.count(http.MethodPost) != 1 {
		t.Fatalf("hook calls = %d, requests before it = %d (want the read only), posts = %d", calls, requestsAtHook, api.count(http.MethodPost))
	}
}

func TestMeta_ToggleMetaAdSetStatus_BeforeWriteIsNotRunWhenNothingIsSent(t *testing.T) {
	t.Run("already in state", func(t *testing.T) {
		d, _ := adSetDispatcher(t, adSetToggleRoutes(adSetStatePaused))
		called := false
		res, err := d.ToggleMetaAdSetStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetaAdSetStatusPaused, func(context.Context) error { called = true; return nil })
		if err != nil || res.Outcome != model.MetaAdSetAlreadyInState || called {
			t.Fatalf("got %+v, %v, hook called %v", res, err, called)
		}
	})
	t.Run("ad set of another campaign", func(t *testing.T) {
		d, _ := adSetDispatcher(t, adSetToggleRoutes(`{"id":"888","campaign_id":"556","account_id":"777","status":"ACTIVE"}`))
		called := false
		_, err := d.ToggleMetaAdSetStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetaAdSetStatusPaused, func(context.Context) error { called = true; return nil })
		if err == nil || called {
			t.Fatalf("err = %v, hook called %v", err, called)
		}
	})
}

func TestMeta_ToggleMetaAdSetStatus_BeforeWriteFailureSendsNothing(t *testing.T) {
	d, api := adSetDispatcher(t, adSetToggleRoutes(adSetStateActive))
	sentinel := errors.New("row write failed")
	_, err := d.ToggleMetaAdSetStatus(context.Background(), "proj", model.ProviderMetaAds, adSetRow(), "888", model.MetaAdSetStatusPaused, func(context.Context) error { return sentinel })
	if !errors.Is(err, sentinel) || meta.IsOutcomeUnconfirmed(err) {
		t.Fatalf("err = %v, want the hook's error as a definite (not unconfirmed) failure", err)
	}
	if n := api.count(http.MethodPost); n != 0 {
		t.Fatalf("a failed hook still sent %d write(s)", n)
	}
}

func TestMetaClient_ActivateCampaignHoldingAdSet(t *testing.T) {
	srv, seen := cascadeServer(t)
	c := meta.NewClient(meta.Credentials{AccessToken: "tok"}, meta.AccountConfig{AccountID: "act_777"}, meta.WithBaseURL(srv.URL))
	if err := c.ActivateCampaignHoldingAdSet(context.Background(), "23847290", "999"); err != nil {
		t.Fatalf("ActivateCampaignHoldingAdSet: %v", err)
	}
	reqs := seen()
	posts := postsByPath(reqs)
	if _, touched := posts["/999"]; touched || posts["/23847290"] != meta.StatusActive || posts["/111"] != meta.StatusActive || posts["/222"] != meta.StatusActive {
		t.Fatalf("posts = %+v", posts)
	}
	// The campaign flip is LAST, as in the full cascade.
	if last := reqs[len(reqs)-1]; last.path != "/23847290" {
		t.Fatalf("last request = %+v, want the campaign flip", last)
	}
}

// The record is the row's, not Meta's. Before a holding ACTIVATE the held ad set is READ: only a
// PAUSED (or DELETED/ARCHIVED, which cannot serve) ad set lets the activation proceed. ACTIVE is a
// definite refusal (ErrMetaHeldAdSetActive → 409) and an unreadable state an ordinary error — and
// on both, NOTHING is activated: no campaign, ad set or ad write at all.
func TestMeta_ToggleStatus_HeldActivationReadsTheAdSetFirst(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		proceeds bool
		check    func(t *testing.T, err error)
	}{
		{"paused proceeds", meta.StatusPaused, true, nil},
		{"archived cannot serve, proceeds", meta.StatusArchived, true, nil},
		{"active is refused", meta.StatusActive, false, func(t *testing.T, err error) {
			if !errors.Is(err, domain.ErrMetaHeldAdSetActive) {
				t.Fatalf("err = %v; want ErrMetaHeldAdSetActive", err)
			}
		}},
		{"unreadable is refused", "", false, func(t *testing.T, err error) {
			if err == nil || errors.Is(err, domain.ErrMetaHeldAdSetActive) || meta.IsOutcomeUnconfirmed(err) {
				t.Fatalf("err = %v; want an ordinary (nothing-changed) error", err)
			}
		}},
		{"unrecognised status is refused", "IN_PROCESS", false, func(t *testing.T, err error) {
			if err == nil || errors.Is(err, domain.ErrMetaHeldAdSetActive) {
				t.Fatalf("err = %v; want an ordinary error", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, seen := cascadeServerWithAdSet(t, tc.status)
			d := cascadeDispatcher(srv)
			row := pausedOnRecord(t, metaToggleCampaign("23847290", "999"), "999")
			err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, row, model.CampaignRunActive)
			reqs := seen()
			if len(reqs) == 0 || reqs[0].method != http.MethodGet || reqs[0].path != "/999" {
				t.Fatalf("first request = %+v; want the held ad set read first", reqs)
			}
			posts := postsByPath(reqs)
			if tc.proceeds {
				if err != nil {
					t.Fatalf("ToggleStatus: %v", err)
				}
				if _, touched := posts["/999"]; touched || posts["/23847290"] != meta.StatusActive {
					t.Fatalf("posts = %+v; want the campaign activated and the ad set left alone", posts)
				}
				return
			}
			tc.check(t, err)
			if len(posts) != 0 {
				t.Fatalf("a refused activation still wrote %+v", posts)
			}
		})
	}
}

// Without a record nothing extra is read: the ordinary cascade is unchanged.
func TestMeta_ToggleStatus_UnheldActivationDoesNotReadTheAdSet(t *testing.T) {
	srv, seen := cascadeServerWithAdSet(t, "")
	d := cascadeDispatcher(srv)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, metaToggleCampaign("23847290", "999"), model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	for _, r := range seen() {
		if r.method == http.MethodGet && r.path == "/999" {
			t.Fatalf("an unheld activation read the ad set: %+v", seen())
		}
	}
}
