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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// cs-3 of the multi-campaign-per-event plan: a second, labelled Reddit or Meta campaign. Both
// names are the data pipeline's 8-segment ` | ` contract, composed from the CLAIMED ROW (slot
// version and stored labels, carried on the dispatch context):
//   - an unlabelled slot-1 row keeps its exact legacy name (Reddit ToFU, Meta MoFU);
//   - a labelled row puts its stage token in segment 8 and its tactic in segment 5
//     ("Intent <tactic>");
//   - slot 2 on appends "#<n>" to segment 5, so it never shares slot 1's name and the count of
//     segments never changes.

// adWire is a fake Reddit or Meta API that records every campaign name the dispatcher sends,
// mints a distinct campaign id per create, and counts any by-name campaign lookup.
type adWire struct {
	mu        sync.Mutex
	campaigns []string
	lookups   int
	requests  int
}

func (w *adWire) names() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.campaigns...)
}

func (w *adWire) counts() (lookups, requests int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lookups, w.requests
}

func (w *adWire) redditDispatcher(t *testing.T) *RedditDispatcher {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		defer w.mu.Unlock()
		w.requests++
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/campaigns"):
			// Reddit never looks a campaign up by name on create; count it if it ever does.
			w.lookups++
			_ = json.NewEncoder(rw).Encode(map[string]any{"data": []any{}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			var req struct {
				Data struct {
					Name string `json:"name"`
				} `json:"data"`
			}
			_ = json.Unmarshal(body, &req)
			w.campaigns = append(w.campaigns, req.Data.Name)
			_ = json.NewEncoder(rw).Encode(map[string]any{"data": map[string]any{"id": fmt.Sprintf("cmp_%d", len(w.campaigns))}})
		case strings.Contains(r.URL.Path, "ad_groups"):
			_ = json.NewEncoder(rw).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			_ = json.NewEncoder(rw).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		w.mu.Lock()
		w.requests++
		w.mu.Unlock()
		_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	return NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }))
}

func (w *adWire) metaDispatcher(t *testing.T) *MetaDispatcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rw.Header().Set("Content-Type", "application/json")
		w.mu.Lock()
		defer w.mu.Unlock()
		w.requests++
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			// The by-name reconcile lookup (campaign or ad set), gated off on the dispatch path.
			w.lookups++
			_, _ = io.WriteString(rw, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(rw, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			var req struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(body, &req)
			w.campaigns = append(w.campaigns, req.Name)
			_, _ = io.WriteString(rw, fmt.Sprintf(`{"id":"1201000000001%02d"}`, len(w.campaigns)))
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(rw, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(rw, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(rw, `{"id":"ad_1"}`)
		default:
			http.Error(rw, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
}

var (
	redditMultiCfg = json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"]}}`)
	metaMultiCfg   = json.RawMessage(`{"metaConfig":{"budget":2500,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"],"currencyOffset":100,
		"variants":[{"headline":"KubeCon 2099","primaryText":"Join us","description":"Cloud native event"}]}}`)
)

// multiPlatform is one of the two cs-3 platforms, with its dispatch wiring and the legacy
// funnel token its unlabelled names keep.
type multiPlatform struct {
	provider     model.Provider
	legacyFunnel string
	dispatch     func(t *testing.T, w *adWire, ctx context.Context, brief *model.CampaignBrief) (*model.Campaign, error)
}

func cs3Platforms() []multiPlatform {
	return []multiPlatform{
		{model.ProviderRedditAds, "ToFU", func(t *testing.T, w *adWire, ctx context.Context, brief *model.CampaignBrief) (*model.Campaign, error) {
			return w.redditDispatcher(t).Dispatch(ctx, brief, model.ProviderRedditAds, redditMultiCfg)
		}},
		{model.ProviderMetaAds, "MoFU", func(t *testing.T, w *adWire, ctx context.Context, brief *model.CampaignBrief) (*model.Campaign, error) {
			return w.metaDispatcher(t).Dispatch(ctx, brief, model.ProviderMetaAds, metaMultiCfg)
		}},
	}
}

// slotCtx is the dispatch context the orchestrator builds for a claimed row: its slot version
// and, when row is non-nil, its stored labels.
func slotCtx(slot int, row *model.Campaign) context.Context {
	ctx := model.WithDispatchSlotVersion(context.Background(), slot)
	if row != nil {
		ctx = model.WithDispatchLabels(ctx, row)
	}
	return ctx
}

func stagedRow(stage *model.FunnelStage, tactic *model.Tactic) *model.Campaign {
	return &model.Campaign{FunnelStage: stage, Tactic: tactic}
}

func stagePtr(s model.FunnelStage) *model.FunnelStage { return &s }
func tacticPtr(t model.Tactic) *model.Tactic         { return &t }

// AC 11 and AC 12 (Reddit, Meta): literal golden names per claimed row, each dispatched twice (a
// retry of the same row composes the same name), every one exactly 8 segments with segment 8 in
// {ToFU, MoFU, BoFU}.
func TestRedditMeta_NamesComeFromTheClaimedRow(t *testing.T) {
	const head = "Events | KubeCon NA 2026 | NA | Traffic | "
	for _, p := range cs3Platforms() {
		legacy := head + "Intent | Social | cncf | " + p.legacyFunnel
		for _, tc := range []struct {
			name string
			slot int
			row  *model.Campaign
			want string
		}{
			{"legacy row, slot 1: the pre-label name, byte for byte", model.FirstSlotVersion, &model.Campaign{}, legacy},
			{"no labels on the context at all: the same legacy name", model.FirstSlotVersion, nil, legacy},
			{"legacy row, slot 2: only the slot token", 2, &model.Campaign{}, head + "Intent #2 | Social | cncf | " + p.legacyFunnel},
			{"labelled row, slot 1: stage and tactic, no slot token", model.FirstSlotVersion,
				stagedRow(stagePtr(model.FunnelStageTop), tacticPtr(model.TacticProspecting)), head + "Intent prospecting | Social | cncf | ToFU"},
			{"labelled row, slot 2: stage, tactic and the slot token", 2,
				stagedRow(stagePtr(model.FunnelStageMid), tacticPtr(model.TacticRetargeting)), head + "Intent retargeting #2 | Social | cncf | MoFU"},
			{"stage without a tactic, slot 3", 3,
				stagedRow(stagePtr(model.FunnelStageLower), nil), head + "Intent #3 | Social | cncf | BoFU"},
			{"tactic without a stage keeps the legacy funnel", 2,
				stagedRow(nil, tacticPtr(model.TacticLookalike)), head + "Intent lookalike #2 | Social | cncf | " + p.legacyFunnel},
			{"content-promotion is written content-promo (keeps the pipeline's Motion parser off it)", 2,
				stagedRow(stagePtr(model.FunnelStageTop), tacticPtr(model.TacticContentPromotion)), head + "Intent content-promo #2 | Social | cncf | ToFU"},
			{"an unlisted stored tactic never reaches the name", 2,
				stagedRow(stagePtr(model.FunnelStageTop), tacticPtr(model.Tactic("retarget | BoFU"))), head + "Intent #2 | Social | cncf | ToFU"},
		} {
			t.Run(string(p.provider)+"/"+tc.name, func(t *testing.T) {
				w := &adWire{}
				for attempt := 1; attempt <= 2; attempt++ {
					camp, err := p.dispatch(t, w, slotCtx(tc.slot, tc.row), testBrief())
					if err != nil {
						t.Fatalf("attempt %d: Dispatch: %v", attempt, err)
					}
					if camp.CampaignName != tc.want {
						t.Errorf("attempt %d: row campaign name %q, want %q", attempt, camp.CampaignName, tc.want)
					}
				}
				sent := w.names()
				if len(sent) != 2 || sent[0] != tc.want || sent[1] != tc.want {
					t.Fatalf("names sent = %q, want %q twice", sent, tc.want)
				}
				if n := strings.Count(sent[0], " | "); n != 7 {
					t.Errorf("name %q has %d separators, want exactly 7 (the pipeline's 8 segments)", sent[0], n)
				}
				segs := strings.Split(sent[0], " | ")
				if f := segs[len(segs)-1]; f != "ToFU" && f != "MoFU" && f != "BoFU" {
					t.Errorf("segment 8 = %q, want ToFU, MoFU or BoFU", f)
				}
			})
		}
	}
}

// AC 10 (Reddit, Meta): slot 2 never binds to slot 1. Slot 1 and slot 2 — unlabelled, where only
// the slot token separates them, and labelled — each send their own campaign create under their
// own name and get their own upstream campaign; no dispatch looks a campaign up by name (Reddit
// never does; Meta's lookup is gated off and the dispatcher never opts in).
func TestRedditMeta_Slot2NeverBindsSlot1(t *testing.T) {
	for _, p := range cs3Platforms() {
		for _, slot2 := range []struct {
			name string
			row  *model.Campaign
		}{
			{"unlabelled slot 2", &model.Campaign{}},
			{"labelled slot 2", stagedRow(stagePtr(model.FunnelStageMid), tacticPtr(model.TacticRetargeting))},
		} {
			t.Run(string(p.provider)+"/"+slot2.name, func(t *testing.T) {
				w := &adWire{}
				first, err := p.dispatch(t, w, slotCtx(model.FirstSlotVersion, &model.Campaign{}), testBrief())
				if err != nil {
					t.Fatalf("slot 1: %v", err)
				}
				second, err := p.dispatch(t, w, slotCtx(2, slot2.row), testBrief())
				if err != nil {
					t.Fatalf("slot 2: %v", err)
				}
				if second.PlatformCampaignID == "" || second.PlatformCampaignID == first.PlatformCampaignID {
					t.Errorf("slot 2 bound upstream campaign %q, slot 1 is %q: want a campaign of its own", second.PlatformCampaignID, first.PlatformCampaignID)
				}
				sent := w.names()
				if len(sent) != 2 || sent[0] == sent[1] {
					t.Errorf("campaign creates = %q, want two creates under different names", sent)
				}
				if lookups, _ := w.counts(); lookups != 0 {
					t.Errorf("%d by-name campaign lookup(s); the dispatch path must never look a campaign up by name", lookups)
				}
			})
		}
	}
}

// A labelled or additional Meta campaign whose composed name is over the bound is refused before
// any Meta request, as a pre-create failure that releases the claim. The same event, unlabelled,
// still dispatches: the bound never refuses a create that worked before cs-3. (Reddit's
// 200-rune name limit already applies to every composed name; the client test
// TestCreateCampaign_OverlongLabelledNameRefusedBeforeAnyRequest pins its labelled case, which
// needs a longer project slug than a brief carries here.)
func TestMeta_OverlongLabelledNameIsRefusedBeforeAnyRequest(t *testing.T) {
	brief := testBrief()
	brief.EventDetails = json.RawMessage(`{"eventName":"` + strings.Repeat("E", 240) + `","registrationUrl":"https://events.example/kc"}`)

	t.Run("control: unlabelled slot 1 still dispatches", func(t *testing.T) {
		w := &adWire{}
		if _, err := w.metaDispatcher(t).Dispatch(slotCtx(model.FirstSlotVersion, &model.Campaign{}), brief, model.ProviderMetaAds, metaMultiCfg); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
	})
	t.Run("labelled slot 2 is refused before any request", func(t *testing.T) {
		w := &adWire{}
		row := stagedRow(stagePtr(model.FunnelStageMid), tacticPtr(model.TacticRetargeting))
		camp, err := w.metaDispatcher(t).Dispatch(slotCtx(2, row), brief, model.ProviderMetaAds, metaMultiCfg)
		if err == nil || camp != nil {
			t.Fatalf("Dispatch = %+v, %v; want a nil campaign and a refusal", camp, err)
		}
		if !strings.Contains(err.Error(), "composed campaign name is too long") {
			t.Errorf("err = %v, want the composed-name refusal", err)
		}
		var nuc interface{ NoUpstreamCreate() bool }
		if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
			t.Errorf("the refusal must be NoUpstreamCreate (claim released), got %T: %v", err, err)
		}
		if _, requests := w.counts(); requests != 0 {
			t.Errorf("%d Meta request(s) before the refusal; want 0", requests)
		}
	})
}
