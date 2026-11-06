// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// cs-2 of the multi-campaign-per-event plan: a second Google campaign on one slot, labelled by
// funnel stage and tactic. Every Google name — budget, campaign and ad group — is composed from
// the CLAIMED ROW (slot version and stored labels, carried on the dispatch context), so slot 1
// and every unlabelled row keep their exact legacy names, and slot 2 never composes a name slot 1
// holds. That last property is what keeps every name-keyed path (DUPLICATE_CAMPAIGN_NAME, the
// budget reuse after DUPLICATE_NAME, adoptExisting) from binding slot 2 to slot 1.

// googleWire is a fake Google Ads API that records every name the dispatcher sends and every
// GAQL search it runs. Unless overridden, budgets, campaigns, ad groups and ads all succeed
// and searches find nothing.
type googleWire struct {
	mu            sync.Mutex
	budgetNames   []string
	campaignNames []string
	adGroupNames  []string
	searches      []string
	finalURLs     []string
	campaignSeq   int

	// search answers one GAQL query body; nil means "nothing found".
	search func(query string) string
	// budget and campaign, when set, answer those mutates instead of the default success.
	budget   http.HandlerFunc
	campaign http.HandlerFunc
}

func createNameFrom(body []byte) string {
	var req struct {
		Operations []struct {
			Create struct {
				Name string `json:"name"`
			} `json:"create"`
		} `json:"operations"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Operations) == 0 {
		return ""
	}
	return req.Operations[0].Create.Name
}

func finalURLsFrom(body []byte) []string {
	var req struct {
		Operations []struct {
			Create struct {
				Ad struct {
					FinalUrls []string `json:"finalUrls"`
				} `json:"ad"`
			} `json:"create"`
		} `json:"operations"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	var out []string
	for _, op := range req.Operations {
		out = append(out, op.Create.Ad.FinalUrls...)
	}
	return out
}

func (g *googleWire) dispatcher(t *testing.T) *GoogleAdsDispatcher {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			g.mu.Lock()
			g.searches = append(g.searches, string(body))
			answer := g.search
			g.mu.Unlock()
			if answer != nil {
				_, _ = io.WriteString(w, answer(string(body)))
				return
			}
			_, _ = io.WriteString(w, `{"results":[]}`)
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			g.mu.Lock()
			g.budgetNames = append(g.budgetNames, createNameFrom(body))
			h := g.budget
			g.mu.Unlock()
			if h != nil {
				h(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			g.mu.Lock()
			g.campaignNames = append(g.campaignNames, createNameFrom(body))
			g.campaignSeq++
			id := 2000 + g.campaignSeq
			h := g.campaign
			g.mu.Unlock()
			if h != nil {
				h(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/`+strconv.Itoa(id)+`"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			g.mu.Lock()
			g.adGroupNames = append(g.adGroupNames, createNameFrom(body))
			g.mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			g.mu.Lock()
			g.finalURLs = append(g.finalURLs, finalURLsFrom(body)...)
			g.mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/333~444"}]}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL))
}

func (g *googleWire) names() (budgets, campaigns, adGroups []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.budgetNames...), append([]string(nil), g.campaignNames...), append([]string(nil), g.adGroupNames...)
}

// googleSlotCtx is the dispatch context the orchestrator builds for a claimed row: its slot
// version and, when row is non-nil, its stored labels.
func googleSlotCtx(slot int, row *model.Campaign) context.Context {
	ctx := model.WithDispatchSlotVersion(context.Background(), slot)
	if row != nil {
		ctx = model.WithDispatchLabels(ctx, row)
	}
	return ctx
}

func labelledRow(stage model.FunnelStage, tactic *model.Tactic) *model.Campaign {
	return &model.Campaign{FunnelStage: &stage, Tactic: tactic}
}

// AC 11 and AC 12 (Google): literal golden budget, campaign and ad group names per claimed row.
//   - An unlabelled (legacy) row on slot 1 — or a context with no labels at all — composes the
//     names every Google campaign had before this change, byte for byte.
//   - A labelled row puts its stage token and tactic in their own segments before the suffix:
//     the exact 7-segment `LFX | <kind> | <project> | <event> | <stage> | <tactic> | <suffix>`.
//   - Slot 2+ extends the suffix to `<brief>-<n>` on all three names.
//   - Each row is dispatched twice: a retry of the same row composes the same names.
func TestGoogleAds_NamesComeFromTheClaimedRow(t *testing.T) {
	retargeting := model.TacticRetargeting
	prospecting := model.TacticProspecting
	contentPromotion := model.TacticContentPromotion
	unlisted := model.Tactic("retarget | BoFU")
	const head = "cncf | KubeCon NA 2026"
	for _, tc := range []struct {
		name                            string
		slot                            int
		row                             *model.Campaign
		wantBudget, wantCamp, wantAdGrp string
		wantSegments                    int
	}{
		{"legacy row, slot 1: the pre-label names, byte for byte", model.FirstSlotVersion, &model.Campaign{},
			"LFX | Budget | cncf | KubeCon NA 2026 | brief-1",
			"LFX | Search Campaign | cncf | KubeCon NA 2026 | brief-1",
			"LFX | Ad Group | cncf | KubeCon NA 2026 | brief-1", 5},
		{"no labels on the context at all: the same legacy names", model.FirstSlotVersion, nil,
			"LFX | Budget | cncf | KubeCon NA 2026 | brief-1",
			"LFX | Search Campaign | cncf | KubeCon NA 2026 | brief-1",
			"LFX | Ad Group | cncf | KubeCon NA 2026 | brief-1", 5},
		{"legacy row, slot 2: only the slot suffix changes", 2, &model.Campaign{},
			"LFX | Budget | cncf | KubeCon NA 2026 | brief-1-2",
			"LFX | Search Campaign | cncf | KubeCon NA 2026 | brief-1-2",
			"LFX | Ad Group | cncf | KubeCon NA 2026 | brief-1-2", 5},
		{"labelled row, slot 1: stage and tactic, bare brief id", model.FirstSlotVersion, labelledRow(model.FunnelStageTop, &prospecting),
			"LFX | Budget | " + head + " | ToFU | prospecting | brief-1",
			"LFX | Search Campaign | " + head + " | ToFU | prospecting | brief-1",
			"LFX | Ad Group | " + head + " | ToFU | prospecting | brief-1", 7},
		{"labelled row, slot 2: stage, tactic and the slot suffix", 2, labelledRow(model.FunnelStageMid, &retargeting),
			"LFX | Budget | cncf | KubeCon NA 2026 | MoFU | retargeting | brief-1-2",
			"LFX | Search Campaign | cncf | KubeCon NA 2026 | MoFU | retargeting | brief-1-2",
			"LFX | Ad Group | cncf | KubeCon NA 2026 | MoFU | retargeting | brief-1-2", 7},
		{"stage without a tactic, slot 3: no empty tactic segment", 3, labelledRow(model.FunnelStageLower, nil),
			"LFX | Budget | cncf | KubeCon NA 2026 | BoFU | brief-1-3",
			"LFX | Search Campaign | cncf | KubeCon NA 2026 | BoFU | brief-1-3",
			"LFX | Ad Group | cncf | KubeCon NA 2026 | BoFU | brief-1-3", 6},
		{"content-promotion is written content-promo (keeps the pipeline's Motion parser off it)", 2, labelledRow(model.FunnelStageTop, &contentPromotion),
			"LFX | Budget | cncf | KubeCon NA 2026 | ToFU | content-promo | brief-1-2",
			"LFX | Search Campaign | cncf | KubeCon NA 2026 | ToFU | content-promo | brief-1-2",
			"LFX | Ad Group | cncf | KubeCon NA 2026 | ToFU | content-promo | brief-1-2", 7},
		{"an unlisted stored tactic never reaches the name", 2, labelledRow(model.FunnelStageTop, &unlisted),
			"LFX | Budget | cncf | KubeCon NA 2026 | ToFU | brief-1-2",
			"LFX | Search Campaign | cncf | KubeCon NA 2026 | ToFU | brief-1-2",
			"LFX | Ad Group | cncf | KubeCon NA 2026 | ToFU | brief-1-2", 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := &googleWire{}
			d := wire.dispatcher(t)
			for attempt := 1; attempt <= 2; attempt++ {
				camp, err := d.Dispatch(googleSlotCtx(tc.slot, tc.row), testBrief(), model.ProviderGoogleAds, json.RawMessage(`{"googleAdsConfig":{"budget":50}}`))
				if err != nil {
					t.Fatalf("attempt %d: Dispatch: %v", attempt, err)
				}
				if camp.CampaignName != tc.wantCamp {
					t.Errorf("attempt %d: row campaign name %q, want %q", attempt, camp.CampaignName, tc.wantCamp)
				}
			}
			budgets, camps, adGroups := wire.names()
			for _, got := range [][]string{budgets, camps, adGroups} {
				if len(got) != 2 {
					t.Fatalf("names sent = %v / %v / %v, want each sent once per attempt", budgets, camps, adGroups)
				}
			}
			for i := 0; i < 2; i++ {
				if budgets[i] != tc.wantBudget {
					t.Errorf("attempt %d: budget name %q, want %q", i+1, budgets[i], tc.wantBudget)
				}
				if camps[i] != tc.wantCamp {
					t.Errorf("attempt %d: campaign name %q, want %q", i+1, camps[i], tc.wantCamp)
				}
				if adGroups[i] != tc.wantAdGrp {
					t.Errorf("attempt %d: ad group name %q, want %q", i+1, adGroups[i], tc.wantAdGrp)
				}
			}
			if n := len(strings.Split(camps[0], " | ")); n != tc.wantSegments {
				t.Errorf("campaign name %q has %d segments, want %d", camps[0], n, tc.wantSegments)
			}
		})
	}
}

// Search, Demand Gen and the other channels stay separate slots (variants); a slot-2 Demand Gen
// campaign carries the slot suffix and labels on its own channel's budget and campaign names.
func TestGoogleAds_Slot2DemandGenNamesCarryTheSlotAndLabels(t *testing.T) {
	wire := &googleWire{}
	d := wire.dispatcher(t)
	keyword := model.TacticKeyword
	_, err := d.Dispatch(googleSlotCtx(2, labelledRow(model.FunnelStageTop, &keyword)), testBrief(), model.ProviderGoogleAds,
		json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen"}}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	budgets, camps, _ := wire.names()
	if len(budgets) != 1 || budgets[0] != "LFX | DemandGen Budget | cncf | KubeCon NA 2026 | ToFU | keyword | brief-1-2" {
		t.Errorf("budget names = %q", budgets)
	}
	if len(camps) != 1 || camps[0] != "LFX | DemandGen Campaign | cncf | KubeCon NA 2026 | ToFU | keyword | brief-1-2" {
		t.Errorf("campaign names = %q", camps)
	}
}

const slot1CampaignName = "LFX | Search Campaign | cncf | KubeCon NA 2026 | brief-1"

// gaqlNames reports whether a GAQL search asks for exactly this name literal, quoted as the
// client quotes it, so a test can tell `... | brief-1'` (slot 1) from `... | brief-1-2'` (slot 2),
// whose name extends it.
func gaqlNames(query, name string) bool {
	return strings.Contains(query, "'"+name+"'")
}

// AC 10 (Google): slot 2 never binds to slot 1 by name — for an UNLABELLED slot 2, whose name
// differs from slot 1's only by the slot suffix, as much as for a labelled one.
//   - adoptExisting looks up SLOT 2's name, so slot 1's live campaign (found only under its own
//     name) is not adopted as slot 2; the control dispatch on slot 1 does adopt it.
//   - a DUPLICATE_CAMPAIGN_NAME on slot 2 means a prior SLOT 2 attempt: the partial it returns
//     names slot 2's campaign and carries no upstream id — never slot 1's 999.
func TestGoogleAds_Slot2NeverBindsSlot1ByName(t *testing.T) {
	slot1Row := `{"results":[{"campaign":{"id":"999","name":"` + slot1CampaignName + `","status":"ENABLED","resourceName":"customers/1234567890/campaigns/999"}}]}`
	answer := func(q string) string {
		if gaqlNames(q, slot1CampaignName) {
			return slot1Row
		}
		return `{"results":[]}`
	}
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"adoptExisting":true}}`)

	t.Run("control: slot 1 adopts the campaign that carries its name", func(t *testing.T) {
		wire := &googleWire{search: answer}
		camp, err := wire.dispatcher(t).Dispatch(googleSlotCtx(model.FirstSlotVersion, &model.Campaign{}), testBrief(), model.ProviderGoogleAds, cfg)
		if err != nil || camp == nil || camp.PlatformCampaignID != "999" {
			t.Fatalf("slot 1 = %+v, %v; want slot 1's campaign 999 adopted", camp, err)
		}
	})

	retargeting := model.TacticRetargeting
	for _, row := range []struct {
		name      string
		row       *model.Campaign
		slot2Name string
	}{
		{"unlabelled slot 2", &model.Campaign{}, slot1CampaignName + "-2"},
		{"labelled slot 2", labelledRow(model.FunnelStageMid, &retargeting),
			"LFX | Search Campaign | cncf | KubeCon NA 2026 | MoFU | retargeting | brief-1-2"},
	} {
		t.Run(row.name+": adoptExisting creates instead of adopting slot 1", func(t *testing.T) {
			wire := &googleWire{search: answer}
			camp, err := wire.dispatcher(t).Dispatch(googleSlotCtx(2, row.row), testBrief(), model.ProviderGoogleAds, cfg)
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if camp.PlatformCampaignID == "999" {
				t.Fatal("slot 2 adopted slot 1's campaign 999")
			}
			if camp.CampaignName != row.slot2Name {
				t.Errorf("campaign name %q, want %q", camp.CampaignName, row.slot2Name)
			}
			wire.mu.Lock()
			defer wire.mu.Unlock()
			if len(wire.searches) == 0 || !gaqlNames(wire.searches[0], row.slot2Name) {
				t.Errorf("the adoption lookup searched %q, want slot 2's name %q", wire.searches, row.slot2Name)
			}
		})

		t.Run(row.name+": DUPLICATE_CAMPAIGN_NAME reports slot 2, not slot 1", func(t *testing.T) {
			wire := &googleWire{search: answer, campaign: googleadsError("campaignError", "DUPLICATE_CAMPAIGN_NAME")}
			camp, err := wire.dispatcher(t).Dispatch(googleSlotCtx(2, row.row), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil || !strings.Contains(err.Error(), "DUPLICATE_CAMPAIGN_NAME") {
				t.Fatalf("err = %v, want the duplicate reported", err)
			}
			if camp == nil {
				t.Fatal("a duplicate name may exist upstream: the partial must be returned so the claim is retained")
			}
			if camp.PlatformCampaignID != "" {
				t.Errorf("partial bound upstream id %q; a slot 2 duplicate must not bind any campaign, least of all slot 1's", camp.PlatformCampaignID)
			}
			if camp.CampaignName != row.slot2Name {
				t.Errorf("partial names %q, want slot 2's %q", camp.CampaignName, row.slot2Name)
			}
			if !strings.Contains(err.Error(), `"`+row.slot2Name+`"`) || strings.Contains(err.Error(), `"`+slot1CampaignName+`"`) {
				t.Errorf("the duplicate error must name slot 2's campaign only: %v", err)
			}
		})
	}
}

// googleadsError answers a mutate with a GoogleAdsFailure carrying one error code.
func googleadsError(category, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT","details":[`+
			`{"@type":"type.googleapis.com/google.ads.googleads.v23.errors.GoogleAdsFailure",`+
			`"errors":[{"errorCode":{"`+category+`":"`+code+`"},"message":"boom"}]}]}}`)
	}
}

// The budget name is unique per slot, so the reuse after a DUPLICATE_NAME looks up SLOT 2's
// budget only: an orphan budget found under slot 1's name is never attached to slot 2's campaign.
func TestGoogleAds_BudgetNameIsUniquePerSlot(t *testing.T) {
	const slot1Budget = "LFX | Budget | cncf | KubeCon NA 2026 | brief-1"
	const slot2Budget = "LFX | Budget | cncf | KubeCon NA 2026 | brief-1-2"
	orphan := `{"results":[{"campaignBudget":{"resourceName":"customers/1234567890/campaignBudgets/777","id":"777","name":"` +
		slot1Budget + `","amountMicros":"50000000","status":"ENABLED","explicitlyShared":false,"referenceCount":"0"}}]}`
	wire := &googleWire{
		budget: googleadsError("campaignBudgetError", "DUPLICATE_NAME"),
		search: func(q string) string {
			if gaqlNames(q, slot1Budget) {
				return orphan
			}
			return `{"results":[]}`
		},
	}
	camp, err := wire.dispatcher(t).Dispatch(googleSlotCtx(2, &model.Campaign{}), testBrief(), model.ProviderGoogleAds,
		json.RawMessage(`{"googleAdsConfig":{"budget":50}}`))
	if err == nil {
		t.Fatal("slot 2's budget name was refused and no budget of THAT name is visible: the dispatch must not succeed")
	}
	budgets, camps, _ := wire.names()
	if len(budgets) != 1 || budgets[0] != slot2Budget {
		t.Errorf("budget create names = %q, want slot 2's %q", budgets, slot2Budget)
	}
	if len(camps) != 0 {
		t.Errorf("a campaign was attempted (%q) on a budget slot 2 does not own", camps)
	}
	if camp != nil && camp.PlatformCampaignID != "" {
		t.Errorf("partial bound %q", camp.PlatformCampaignID)
	}
	wire.mu.Lock()
	defer wire.mu.Unlock()
	for _, q := range wire.searches {
		if gaqlNames(q, slot1Budget) {
			t.Errorf("slot 2 looked up slot 1's budget name: %s", q)
		}
	}
	// The first BUDGET search is the reuse lookup; the client's pre-write campaign-name check
	// (#540) reads FROM campaign before it.
	var budgetSearches []string
	for _, q := range wire.searches {
		if strings.Contains(q, "FROM campaign_budget") {
			budgetSearches = append(budgetSearches, q)
		}
	}
	if len(budgetSearches) == 0 || !gaqlNames(budgetSearches[0], slot2Budget) {
		t.Errorf("the budget reuse lookup = %q, want it to search slot 2's name %q", budgetSearches, slot2Budget)
	}
}

// AC 18 / N1: utm_campaign does not move with the slot. It is event-level attribution: the slug
// when set, else the event name, on slot 1 and slot 2 alike. (The last-resort fallback, for an
// input with neither, is unreachable from the dispatcher — a brief without an event name is
// refused — so the client test TestUTMCampaignFallbackIsTheBareBriefID pins it.)
func TestGoogleAds_UTMCampaignDoesNotMoveWithTheSlot(t *testing.T) {
	noSlug := testBrief()
	noSlug.EventSlug = ""
	for _, tc := range []struct {
		name  string
		brief *model.CampaignBrief
		slot  int
		want  string
	}{
		{"no slug, slot 1: the event name", noSlug, model.FirstSlotVersion, "KubeCon NA 2026"},
		{"no slug, slot 2: the event name", noSlug, 2, "KubeCon NA 2026"},
		{"slug set, slot 1", testBrief(), model.FirstSlotVersion, "kubecon-na-2026"},
		{"slug set, slot 2", testBrief(), 2, "kubecon-na-2026"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := &googleWire{}
			mid := model.TacticKeyword
			if _, err := wire.dispatcher(t).Dispatch(googleSlotCtx(tc.slot, labelledRow(model.FunnelStageMid, &mid)), tc.brief, model.ProviderGoogleAds,
				json.RawMessage(`{"googleAdsConfig":{"budget":50}}`)); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			wire.mu.Lock()
			defer wire.mu.Unlock()
			if len(wire.finalURLs) == 0 {
				t.Fatal("no ad final URL was sent")
			}
			for _, raw := range wire.finalURLs {
				u, err := url.Parse(raw)
				if err != nil {
					t.Fatalf("final URL %q: %v", raw, err)
				}
				if got := u.Query().Get("utm_campaign"); got != tc.want {
					t.Errorf("utm_campaign = %q, want %q (%s)", got, tc.want, raw)
				}
			}
		})
	}
}
