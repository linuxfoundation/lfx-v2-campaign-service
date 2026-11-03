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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// cs-4 of the multi-campaign-per-event plan: a second, labelled LinkedIn or X campaign. Both
// names are the data pipeline's 8-segment ` | ` contract, composed from the CLAIMED ROW (slot
// version and stored labels, carried on the dispatch context):
//   - an unlabelled slot-1 row keeps its exact legacy name (both MoFU);
//   - a labelled row puts its stage token in segment 8 and its tactic in segment 5
//     ("Prospecting <tactic>");
//   - slot 2 on appends "#<n>" to segment 5, so it never shares slot 1's name and the count of
//     segments never changes.
//
// Unlike Reddit and Meta, both clients REUSE a campaign by that name on every create (LinkedIn's
// findCampaignByNameInGroup, X's findCampaignByName). The fakes below are therefore stateful:
// they remember every campaign created and answer each by-name lookup with them, the way the
// platform does, so a slot-2 name equal to slot 1's would be found and reused.

// fakeCampaign is one campaign a stateful fake platform has created.
type fakeCampaign struct{ id, name string }

// liWire is a stateful fake LinkedIn Marketing API: it creates campaign groups and campaigns,
// answers the by-name searches with what it has created, and records which campaign every
// creative was pushed onto.
type liWire struct {
	mu        sync.Mutex
	groups    []fakeCampaign
	campaigns []fakeCampaign
	creates   []string // campaign names POSTed, in order
	creatives []string // the campaign URN each creative was created under
	requests  int
}

func (w *liWire) snapshot() (creates, creatives []string, requests int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.creates...), append([]string(nil), w.creatives...), w.requests
}

func (w *liWire) dispatcher(t *testing.T) *LinkedInDispatcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rw.Header().Set("Content-Type", "application/json")
		w.mu.Lock()
		defer w.mu.Unlock()
		w.requests++
		var req struct {
			Name     string `json:"name"`
			Campaign string `json:"campaign"`
		}
		_ = json.Unmarshal(body, &req)
		elements := func(items []fakeCampaign, group bool) {
			out := make([]map[string]any, 0, len(items))
			for _, it := range items {
				el := map[string]any{"id": json.Number(it.id), "name": it.name, "status": "ACTIVE"}
				if !group {
					el["status"] = "PAUSED"
					el["campaignGroup"] = "urn:li:sponsoredCampaignGroup:" + w.groups[0].id
				}
				out = append(out, el)
			}
			_ = json.NewEncoder(rw).Encode(map[string]any{"elements": out, "metadata": map[string]any{}})
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaignGroups/"):
			// The single-group read before a reused group's campaign create: an open-ended
			// schedule covers every campaign, so no extension is written.
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_, _ = io.WriteString(rw, `{"id":`+id+`,"runSchedule":{"start":1}}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaignGroups"):
			elements(w.groups, true)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "adCampaigns"):
			elements(w.campaigns, false)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(rw, `{"elements":[],"metadata":{}}`)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			id := fmt.Sprintf("%d", 100+len(w.groups))
			w.groups = append(w.groups, fakeCampaign{id, req.Name})
			_, _ = io.WriteString(rw, `{"id":"urn:li:sponsoredCampaignGroup:`+id+`"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			id := fmt.Sprintf("%d", 200+len(w.campaigns))
			w.campaigns = append(w.campaigns, fakeCampaign{id, req.Name})
			w.creates = append(w.creates, req.Name)
			rw.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:"+id)
			_, _ = io.WriteString(rw, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(rw, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			w.creatives = append(w.creatives, req.Campaign)
			_, _ = io.WriteString(rw, fmt.Sprintf(`{"id":"urn:li:sponsoredCreative:%d"}`, 400+len(w.creatives)))
		default:
			http.Error(rw, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
}

// xWire is a stateful fake X Ads API: it creates campaigns and line items, answers the by-name
// lookups with what it has created, and records which line item each promoted tweet went to.
type xWire struct {
	mu        sync.Mutex
	campaigns []fakeCampaign
	lineItems map[string][]fakeCampaign // by campaign id
	creates   []string                  // campaign names POSTed, in order
	lineFor   []string                  // the campaign id each line item was created under
	promoted  []string                  // the line item id each promoted tweet was attached to
	requests  int
}

func (w *xWire) snapshot() (creates, lineFor, promoted []string, requests int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.creates...), append([]string(nil), w.lineFor...), append([]string(nil), w.promoted...), w.requests
}

func (w *xWire) dispatcher(t *testing.T) *TwitterDispatcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.requests++
		if w.lineItems == nil {
			w.lineItems = map[string][]fakeCampaign{}
		}
		q := r.URL.Query()
		list := func(items []fakeCampaign, parent string) {
			out := make([]map[string]any, 0, len(items))
			for _, it := range items {
				el := map[string]any{"id": it.id, "name": it.name, "funding_instrument_id": "fi1"}
				if parent != "" {
					el["campaign_id"] = parent
				}
				out = append(out, el)
			}
			_ = json.NewEncoder(rw).Encode(map[string]any{"data": out})
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = io.WriteString(rw, `{"data":{"name":"LF Events"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/campaigns"):
			list(w.campaigns, "")
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/line_items"):
			parent := q.Get("campaign_ids")
			list(w.lineItems[parent], parent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			id := fmt.Sprintf("cmp_%d", len(w.campaigns)+1)
			w.campaigns = append(w.campaigns, fakeCampaign{id, q.Get("name")})
			w.creates = append(w.creates, q.Get("name"))
			_, _ = io.WriteString(rw, `{"data":{"id":"`+id+`"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/line_items"):
			parent := q.Get("campaign_id")
			id := fmt.Sprintf("li_%d", len(w.lineFor)+1)
			w.lineItems[parent] = append(w.lineItems[parent], fakeCampaign{id, q.Get("name")})
			w.lineFor = append(w.lineFor, parent)
			_, _ = io.WriteString(rw, `{"data":{"id":"`+id+`"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/promoted_tweets"):
			w.promoted = append(w.promoted, q.Get("line_item_id"))
			_, _ = io.WriteString(rw, `{"data":[{"id":"pt1"}]}`)
		default:
			http.Error(rw, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
}

var (
	linkedInMultiCfg = json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
	twitterMultiCfg = json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890"}}`)
)

// cs4Platform is one of the two cs-4 platforms: its dispatch wiring and the fixed segments
// around its name's Targeting (5) and Funnel (8) segments.
type cs4Platform struct {
	provider model.Provider
	head     string // segments 1-4 and the separator before segment 5
	tail     string // the Ad Format and Project segments between segment 5 and segment 8
	// run dispatches one claimed row through a fresh dispatcher over the platform's shared
	// stateful fake, so a second run sees what the first created.
	run func(t *testing.T, ctx context.Context, brief *model.CampaignBrief) (*model.Campaign, error)
}

func cs4Platforms(li *liWire, x *xWire) []cs4Platform {
	return []cs4Platform{
		{model.ProviderLinkedInAds, "Events | KubeCon NA 2026 | LinkedIn | Traffic | ", " | Static | cncf | ",
			func(t *testing.T, ctx context.Context, brief *model.CampaignBrief) (*model.Campaign, error) {
				return li.dispatcher(t).Dispatch(ctx, brief, model.ProviderLinkedInAds, linkedInMultiCfg)
			}},
		{model.ProviderTwitterAds, "Events | KubeCon NA 2026 | Global | Awareness | ", " | Promoted Post | cncf | ",
			func(t *testing.T, ctx context.Context, brief *model.CampaignBrief) (*model.Campaign, error) {
				return x.dispatcher(t).Dispatch(ctx, brief, model.ProviderTwitterAds, twitterMultiCfg)
			}},
	}
}

// campaignCreates reports the campaign names POSTed to whichever fake p uses.
func campaignCreates(p model.Provider, li *liWire, x *xWire) []string {
	if p == model.ProviderLinkedInAds {
		c, _, _ := li.snapshot()
		return c
	}
	c, _, _, _ := x.snapshot()
	return c
}

// AC 11 and AC 12 (LinkedIn, X): literal golden names per claimed row, every one exactly 8
// segments with segment 8 in {ToFU, MoFU, BoFU}. Each row is dispatched twice: the retry composes
// the same name, so it finds and reuses the campaign the first attempt created (one create, the
// same upstream id) — the by-name reuse lookup is keyed on the row's own composed name.
func TestLinkedInX_NamesComeFromTheClaimedRow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		slot      int
		row       *model.Campaign
		targeting string
		funnel    string
	}{
		{"legacy row, slot 1: the pre-label name, byte for byte", model.FirstSlotVersion, &model.Campaign{}, "Prospecting", "MoFU"},
		{"no labels on the context at all: the same legacy name", model.FirstSlotVersion, nil, "Prospecting", "MoFU"},
		{"legacy row, slot 2: only the slot token", 2, &model.Campaign{}, "Prospecting #2", "MoFU"},
		{"labelled row, slot 1: stage and tactic, no slot token", model.FirstSlotVersion,
			stagedRow(stagePtr(model.FunnelStageTop), tacticPtr(model.TacticProspecting)), "Prospecting prospecting", "ToFU"},
		{"labelled row, slot 2: stage, tactic and the slot token", 2,
			stagedRow(stagePtr(model.FunnelStageMid), tacticPtr(model.TacticRetargeting)), "Prospecting retargeting #2", "MoFU"},
		{"stage without a tactic, slot 3", 3, stagedRow(stagePtr(model.FunnelStageLower), nil), "Prospecting #3", "BoFU"},
		{"tactic without a stage keeps the legacy funnel", 2, stagedRow(nil, tacticPtr(model.TacticLookalike)), "Prospecting lookalike #2", "MoFU"},
		{"content-promotion is written content-promo (keeps the pipeline's Motion parser off it)", 2,
			stagedRow(stagePtr(model.FunnelStageTop), tacticPtr(model.TacticContentPromotion)), "Prospecting content-promo #2", "ToFU"},
		{"an unlisted stored tactic never reaches the name", 2,
			stagedRow(stagePtr(model.FunnelStageTop), tacticPtr(model.Tactic("retarget | BoFU"))), "Prospecting #2", "ToFU"},
	} {
		li, x := &liWire{}, &xWire{}
		for _, p := range cs4Platforms(li, x) {
			want := p.head + tc.targeting + p.tail + tc.funnel
			t.Run(string(p.provider)+"/"+tc.name, func(t *testing.T) {
				var firstID string
				for attempt := 1; attempt <= 2; attempt++ {
					camp, err := p.run(t, slotCtx(tc.slot, tc.row), testBrief())
					if err != nil {
						t.Fatalf("attempt %d: Dispatch: %v", attempt, err)
					}
					if camp.CampaignName != want {
						t.Errorf("attempt %d: row campaign name %q, want %q", attempt, camp.CampaignName, want)
					}
					if attempt == 1 {
						firstID = camp.PlatformCampaignID
					} else if camp.PlatformCampaignID != firstID {
						t.Errorf("retry bound upstream campaign %q, want the first attempt's %q", camp.PlatformCampaignID, firstID)
					}
				}
				sent := campaignCreates(p.provider, li, x)
				if len(sent) != 1 || sent[0] != want {
					t.Fatalf("campaign creates = %q, want exactly one, %q", sent, want)
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

// AC 10 (LinkedIn, X): slot 2 never binds to slot 1. Slot 1 is created first and stays live in
// the fake account, where each client's by-name lookup sees it. Slot 2 — unlabelled, where only
// the slot token separates the names, and labelled — must not find it: it creates its own
// campaign under its own name, and its children go onto THAT campaign (LinkedIn creatives; X line
// item and promoted tweet), never onto slot 1's.
func TestLinkedInX_Slot2NeverBindsSlot1(t *testing.T) {
	for _, slot2 := range []struct {
		name string
		row  *model.Campaign
	}{
		{"unlabelled slot 2", &model.Campaign{}},
		{"labelled slot 2", stagedRow(stagePtr(model.FunnelStageMid), tacticPtr(model.TacticRetargeting))},
	} {
		t.Run("linkedin-ads/"+slot2.name, func(t *testing.T) {
			w := &liWire{}
			first, err := w.dispatcher(t).Dispatch(slotCtx(model.FirstSlotVersion, &model.Campaign{}), testBrief(), model.ProviderLinkedInAds, linkedInMultiCfg)
			if err != nil {
				t.Fatalf("slot 1: %v", err)
			}
			second, err := w.dispatcher(t).Dispatch(slotCtx(2, slot2.row), testBrief(), model.ProviderLinkedInAds, linkedInMultiCfg)
			if err != nil {
				t.Fatalf("slot 2: %v", err)
			}
			if second.PlatformCampaignID == "" || second.PlatformCampaignID == first.PlatformCampaignID {
				t.Fatalf("slot 2 bound upstream campaign %q, slot 1 is %q: want a campaign of its own", second.PlatformCampaignID, first.PlatformCampaignID)
			}
			creates, creatives, _ := w.snapshot()
			if len(creates) != 2 || creates[0] == creates[1] {
				t.Errorf("campaign creates = %q, want two creates under different names", creates)
			}
			want := []string{"urn:li:sponsoredCampaign:" + first.PlatformCampaignID, "urn:li:sponsoredCampaign:" + second.PlatformCampaignID}
			if strings.Join(creatives, ",") != strings.Join(want, ",") {
				t.Errorf("creatives pushed onto %q, want one per slot onto its own campaign %q", creatives, want)
			}
		})
		t.Run("twitter-ads/"+slot2.name, func(t *testing.T) {
			w := &xWire{}
			first, err := w.dispatcher(t).Dispatch(slotCtx(model.FirstSlotVersion, &model.Campaign{}), testBrief(), model.ProviderTwitterAds, twitterMultiCfg)
			if err != nil {
				t.Fatalf("slot 1: %v", err)
			}
			second, err := w.dispatcher(t).Dispatch(slotCtx(2, slot2.row), testBrief(), model.ProviderTwitterAds, twitterMultiCfg)
			if err != nil {
				t.Fatalf("slot 2: %v", err)
			}
			if second.PlatformCampaignID == "" || second.PlatformCampaignID == first.PlatformCampaignID {
				t.Fatalf("slot 2 bound upstream campaign %q, slot 1 is %q: want a campaign of its own", second.PlatformCampaignID, first.PlatformCampaignID)
			}
			if second.Status != campaignStatusCreated {
				t.Errorf("slot 2 status = %q, want a clean %q (a reused campaign is created_degraded)", second.Status, campaignStatusCreated)
			}
			creates, lineFor, promoted, _ := w.snapshot()
			if len(creates) != 2 || creates[0] == creates[1] {
				t.Errorf("campaign creates = %q, want two creates under different names", creates)
			}
			if want := []string{first.PlatformCampaignID, second.PlatformCampaignID}; strings.Join(lineFor, ",") != strings.Join(want, ",") {
				t.Errorf("line items created under %q, want one per slot under its own campaign %q", lineFor, want)
			}
			if strings.Join(promoted, ",") != "li_1,li_2" {
				t.Errorf("promoted tweets attached to %q, want li_1 then li_2 (each slot's own line item)", promoted)
			}
		})
	}
}

// A labelled or additional campaign whose composed name is over the platform's 255-character
// limit is refused before any request, as a pre-create failure that releases the claim. The
// same event, unlabelled, composes a name at the limit and still dispatches: the labels alone
// push it over, so the refusal is the label's and no create that worked before cs-4 is refused.
func TestLinkedInX_OverlongLabelledNameIsRefusedBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct {
		provider model.Provider
		// event is the event-name length that puts the unlabelled name exactly at 255.
		event    int
		dispatch func(t *testing.T, ctx context.Context, brief *model.CampaignBrief) (camp *model.Campaign, requests int, err error)
	}{
		{model.ProviderLinkedInAds, 255 - len("Events |  | LinkedIn | Traffic | Prospecting | Static | cncf | MoFU"),
			func(t *testing.T, ctx context.Context, brief *model.CampaignBrief) (*model.Campaign, int, error) {
				w := &liWire{}
				camp, err := w.dispatcher(t).Dispatch(ctx, brief, model.ProviderLinkedInAds, linkedInMultiCfg)
				_, _, n := w.snapshot()
				return camp, n, err
			}},
		{model.ProviderTwitterAds, 255 - len("Events |  | Global | Awareness | Prospecting | Promoted Post | cncf | MoFU"),
			func(t *testing.T, ctx context.Context, brief *model.CampaignBrief) (*model.Campaign, int, error) {
				w := &xWire{}
				camp, err := w.dispatcher(t).Dispatch(ctx, brief, model.ProviderTwitterAds, twitterMultiCfg)
				_, _, _, n := w.snapshot()
				return camp, n, err
			}},
	} {
		brief := testBrief()
		brief.EventDetails = json.RawMessage(`{"eventName":"` + strings.Repeat("E", tc.event) + `","registrationUrl":"https://events.example/kc"}`)

		t.Run(string(tc.provider)+"/control: unlabelled slot 1 at the limit still dispatches", func(t *testing.T) {
			camp, n, err := tc.dispatch(t, slotCtx(model.FirstSlotVersion, &model.Campaign{}), brief)
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if got := len([]rune(camp.CampaignName)); got != 255 || n == 0 {
				t.Fatalf("control name is %d characters with %d request(s); want 255 and a dispatch that reached the platform", got, n)
			}
		})
		t.Run(string(tc.provider)+"/labelled slot 2 is refused before any request", func(t *testing.T) {
			row := stagedRow(stagePtr(model.FunnelStageMid), tacticPtr(model.TacticRetargeting))
			camp, n, err := tc.dispatch(t, slotCtx(2, row), brief)
			if err == nil || camp != nil {
				t.Fatalf("Dispatch = %+v, %v; want a nil campaign and a refusal", camp, err)
			}
			if !strings.Contains(err.Error(), "255") {
				t.Errorf("err = %v, want the 255-character name refusal", err)
			}
			var nuc interface{ NoUpstreamCreate() bool }
			if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("the refusal must be NoUpstreamCreate (claim released), got %T: %v", err, err)
			}
			if n != 0 {
				t.Errorf("%d request(s) before the refusal; want 0", n)
			}
		})
	}
}
