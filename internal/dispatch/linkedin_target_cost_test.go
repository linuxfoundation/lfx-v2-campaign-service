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
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestLinkedIn_TargetAmountPreflight pins linkedInConfig.bidding.targetAmount's synchronous 400s
// (#674): refused unless LINKEDIN_ADS_TARGET_COST_ENABLED is exactly "true", with a manual bid,
// and for an amount that is not whole cents above zero; accepted with automatic bidding, explicit
// or omitted, for traffic and for conversions.
func TestLinkedIn_TargetAmountPreflight(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsConversionsEnabled, "true")
	const flagOff = `linkedInConfig.bidding: targetAmount is disabled on this deployment until an operator sets LINKEDIN_ADS_TARGET_COST_ENABLED to "true"; remove targetAmount to create the campaign with automatic bidding and no target`
	const manualRefusal = `linkedInConfig.bidding: targetAmount is only accepted with automatic bidding; a manual campaign's bid is bidAmount, so remove targetAmount or choose strategy "automatic"`
	auto := func(target string) string { return `"bidding":{"strategy":"automatic","targetAmount":` + target + `}` }
	for _, tc := range []struct {
		name, flag, extra, want string
	}{
		{"unset refuses", "", auto("17.88"), flagOff},
		{"false refuses", "false", auto("17.88"), flagOff},
		{"TRUE (wrong case) refuses", "TRUE", auto("17.88"), flagOff},
		{"flag off leaves automatic without a target alone", "", `"bidding":{"strategy":"automatic"}`, ""},
		{"flag off leaves manual alone", "", `"bidding":{"strategy":"manual","bidAmount":4.5}`, ""},
		{"traffic automatic with a target", "true", auto("17.88"), ""},
		{"omitted strategy with a target", "true", `"bidding":{"targetAmount":17.88}`, ""},
		{"explicit traffic with a target", "true", `"objective":"traffic",` + auto("17.88"), ""},
		{"conversions automatic with a target", "true", `"objective":"conversions","conversions":["104012"],` + auto("10"), ""},
		{"manual with a target", "true", `"bidding":{"strategy":"manual","bidAmount":4.5,"targetAmount":10}`, manualRefusal},
		{"manual conversions with a target", "true", `"objective":"conversions","bidding":{"strategy":"manual","bidAmount":4.5,"targetAmount":10}`, manualRefusal},
		{"zero", "true", auto("0"), "linkedInConfig.bidding: targetAmount: the target cost must be a finite number greater than zero"},
		{"negative", "true", auto("-1"), "linkedInConfig.bidding: targetAmount: the target cost must be a finite number greater than zero"},
		{"sub-cent", "true", auto("17.885"), "linkedInConfig.bidding: targetAmount: the target cost 17.885 is not a whole number of cents; LinkedIn target costs are set in the account currency to two decimal places"},
		{"over the ceiling", "true", auto("1000000.01"), "linkedInConfig.bidding: targetAmount: the target cost 1000000.01 exceeds the largest target cost this service sets on LinkedIn (1000000)"},
		{"null", "true", auto("null"), "targetAmount must be a number (for example 17.88), not null"},
		{"string", "true", auto(`"10"`), "targetAmount must be a number (for example 17.88), not null"},
		{"case variant key", "true", `"bidding":{"TargetAmount":10}`, `use "targetAmount" (exact spelling)`},
		// The flag is checked first: a deployment with it off names the gate, not the amount.
		{"flag off names the gate before the amount", "", auto("17.885"), flagOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsTargetCostEnabled, tc.flag)
			d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, linkedInObjectiveConfig(tc.extra))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// TestBidding_TargetAmountIsLinkedInOnly (#674): every other platform's bidding object refuses
// targetAmount as an unknown field, exactly as before #674, and Microsoft keeps its own targetCpa.
func TestBidding_TargetAmountIsLinkedInOnly(t *testing.T) {
	for _, key := range []string{"metaConfig", "redditConfig", "twitterConfig", "microsoftConfig"} {
		cfg := json.RawMessage(`{"` + key + `":{"bidding":{"strategy":"automatic","targetAmount":10}}}`)
		err := checkBiddingShape(cfg, key)
		if err == nil || !strings.Contains(err.Error(), `unknown bidding field "targetAmount"`) {
			t.Errorf("%s: checkBiddingShape = %v, want an unknown-field refusal", key, err)
		}
	}
	if err := checkBiddingShape(json.RawMessage(`{"linkedInConfig":{"bidding":{"targetCpa":10}}}`), "linkedInConfig"); err == nil ||
		!strings.Contains(err.Error(), `bidding accepts only "strategy", "bidAmount" and "targetAmount"`) {
		t.Errorf("linkedInConfig targetCpa: %v, want an unknown-field refusal naming targetAmount", err)
	}
	if err := checkBiddingShape(json.RawMessage(`{"microsoftConfig":{"bidding":{"strategy":"targetCpa","targetCpa":10}}}`), "microsoftConfig"); err != nil {
		t.Errorf("microsoftConfig targetCpa: %v, want accepted", err)
	}
	// A decoded targetAmount that reaches the shared resolver on another platform is refused there too.
	b := &biddingConfig{}
	if err := json.Unmarshal([]byte(`{"targetAmount":10}`), b); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveBidding("metaConfig", b); err == nil || !strings.Contains(err.Error(), "targetAmount is only accepted by linkedInConfig") {
		t.Errorf("resolveBidding(metaConfig, targetAmount) = %v, want refused", err)
	}
}

// TestLinkedIn_TargetAmountRefusedByDispatchSendsNothing: a job reaching Dispatch without the
// pre-flight still refuses before any LinkedIn request, with PreflightCreate's pre-flight refusal.
// The wrong-typed rows (PR #677 review) pin that the bidding shape check runs before the strict
// decode, which would otherwise fail them with a plain error the job reports as a generic failure.
func TestLinkedIn_TargetAmountRefusedByDispatchSendsNothing(t *testing.T) {
	const wrongType = "targetAmount must be a number (for example 17.88), not null"
	for _, tc := range []struct{ name, flag, extra, want string }{
		{"flag off", "", `"bidding":{"targetAmount":17.88}`, "targetAmount is disabled on this deployment"},
		{"manual with a target", "true", `"bidding":{"strategy":"manual","bidAmount":4.5,"targetAmount":10}`, "targetAmount is only accepted with automatic bidding"},
		{"sub-cent target", "true", `"bidding":{"targetAmount":17.885}`, "is not a whole number of cents"},
		{"string target", "true", `"bidding":{"targetAmount":"10"}`, wrongType},
		{"null target", "true", `"bidding":{"targetAmount":null}`, wrongType},
		{"object target", "true", `"bidding":{"targetAmount":{"amount":10}}`, wrongType},
		{"string target with the flag off", "", `"bidding":{"targetAmount":"10"}`, wrongType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsTargetCostEnabled, tc.flag)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer srv.Close()
			d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInObjectiveConfig(tc.extra))
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, tc.want) {
				t.Fatalf("Dispatch = %v, want a pre-flight ErrCreateConfigInvalid containing %q", err, tc.want)
			}
			if !isNoUpstreamCreate(err) {
				t.Errorf("Dispatch = %v, want it marked as nothing created upstream", err)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%d LinkedIn requests for a refused create", n)
			}
		})
	}
}

// TestLinkedIn_TargetAmountReachesTheWire pins the dispatcher wiring end to end against an
// httptest LinkedIn (#674): a target is sent as unitCost beside MAX_CLICK or MAX_CONVERSION with
// costType CPM, the row records automatic bidding and no max CPC bid, the result blob records the
// target, the snapshot keeps targetAmount (and replays through the validator), and the settings
// readback's recorded objective is unchanged. Without a target nothing differs from before #674.
func TestLinkedIn_TargetAmountReachesTheWire(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsConversionsEnabled, "true")
	t.Setenv(constants.EnvLinkedInAdsTargetCostEnabled, "true")
	for _, tc := range []struct {
		name, extra                         string
		objective, target, wire, recordedOb string
	}{
		{"traffic target", `"bidding":{"strategy":"automatic","targetAmount":17.88}`, "WEBSITE_VISIT", "MAX_CLICK", "17.88", "WEBSITE_VISIT"},
		{"conversions target", `"objective":"conversions","conversions":["104012"],"bidding":{"targetAmount":10}`, "WEBSITE_CONVERSION", "MAX_CONVERSION", "10.00", "WEBSITE_CONVERSION"},
		{"traffic without a target", `"bidding":{"strategy":"automatic"}`, "WEBSITE_VISIT", "MAX_CLICK", "", "WEBSITE_VISIT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var campaignBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				switch {
				case strings.Contains(r.URL.Path, "campaignConversions"):
					w.WriteHeader(http.StatusCreated)
				case strings.Contains(r.URL.Path, "adCampaignGroups"):
					_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
				case strings.Contains(r.URL.Path, "adCampaigns"):
					raw, _ := io.ReadAll(r.Body)
					_ = json.Unmarshal(raw, &campaignBody)
					w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
					_, _ = io.WriteString(w, `{}`)
				case strings.Contains(r.URL.Path, "posts"):
					_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
				case strings.Contains(r.URL.Path, "creatives"):
					_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
				default:
					http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
				}
			}))
			defer srv.Close()
			clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
			d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
				linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock))

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInObjectiveConfig(tc.extra))
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			mu.Lock()
			b := campaignBody
			mu.Unlock()
			if b["objectiveType"] != tc.objective || b["optimizationTargetType"] != tc.target || b["costType"] != "CPM" {
				t.Fatalf("campaign POST objectiveType=%v optimizationTargetType=%v costType=%v, want %s/%s/CPM",
					b["objectiveType"], b["optimizationTargetType"], b["costType"], tc.objective, tc.target)
			}
			unit, has := b["unitCost"].(map[string]any)
			if tc.wire == "" {
				if _, present := b["unitCost"]; present {
					t.Fatalf("unitCost = %v, want none without a target", b["unitCost"])
				}
			} else if !has || unit["amount"] != tc.wire || unit["currencyCode"] != "USD" {
				t.Fatalf("unitCost = %v, want {amount %s, currencyCode USD}", b["unitCost"], tc.wire)
			}
			if camp.BiddingStrategy == nil || *camp.BiddingStrategy != model.BiddingAutomatic || camp.MaxCPCBid != nil {
				t.Fatalf("recorded strategy = %v, max CPC bid = %v; want automatic and none", camp.BiddingStrategy, camp.MaxCPCBid)
			}
			var blob struct {
				TargetCost string `json:"targetCost"`
			}
			if err := json.Unmarshal(camp.Result, &blob); err != nil || blob.TargetCost != tc.wire {
				t.Errorf("result targetCost = %q (%v), want %q; result %s", blob.TargetCost, err, tc.wire, camp.Result)
			}
			if got := linkedinRecordedObjective(context.Background(), camp); got == nil || *got != tc.recordedOb {
				t.Errorf("settings readback's recorded objective = %v, want %s", got, tc.recordedOb)
			}
			var snap linkedinConfig
			if err := json.Unmarshal(camp.ConfigSnapshot, &snap); err != nil {
				t.Fatalf("snapshot %s: %v", camp.ConfigSnapshot, err)
			}
			if tc.wire != "" && (snap.Bidding == nil || snap.Bidding.TargetAmount == nil) {
				t.Errorf("snapshot %s lost bidding.targetAmount", camp.ConfigSnapshot)
			}
			if tc.wire != "" {
				replayed, err := linkedinBidPlan(snap)
				if err != nil || replayed.targetCost == 0 {
					t.Errorf("replaying the snapshot's bidding = %+v, %v; want the same target", replayed, err)
				}
			}
		})
	}
}

// TestLinkedIn_ReadSettings_TargetCostCampaign (#674): a campaign created with a target reads back
// as the automatic campaign it is — bidding_strategy_type MAX_CLICK, cost_type CPM and the target
// as unit_cost (upstream-only, as every bid field is) — and its objective is compared as before.
func TestLinkedIn_ReadSettings_TargetCostCampaign(t *testing.T) {
	automatic := model.BiddingAutomatic
	body := liCampaignJSON(
		replace(`"objectiveType":"WEBSITE_CONVERSION","optimizationTargetType":"NONE"`, `"objectiveType":"WEBSITE_VISIT","optimizationTargetType":"MAX_CLICK"`),
		replace(`"costType":"CPC","unitCost":{"amount":"5.25"`, `"costType":"CPM","unitCost":{"amount":"17.88"`),
	)
	d, _ := liLeverDispatcher(t, map[string]settingsRoute{liCampaignPath: {status: 200, body: body}})
	row := liRow()
	row.BiddingStrategy = &automatic
	row.Result = json.RawMessage(resultWith(`"objectiveType":"WEBSITE_VISIT","optimizationTarget":"MAX_CLICK","targetCost":"17.88"`))
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderLinkedInAds, row)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	for field, want := range map[string]fieldWant{
		settingsFieldBiddingStrategy:  {"", "MAX_CLICK", model.SettingsUnknown},
		settingsFieldCostType:         {"", "CPM", model.SettingsUnknown},
		settingsFieldUnitCost:         {"", "17.88", model.SettingsUnknown},
		settingsFieldUnitCostCurrency: {"", "USD", model.SettingsUnknown},
		settingsFieldObjectiveType:    {"WEBSITE_VISIT", "WEBSITE_VISIT", model.SettingsMatch},
	} {
		f := settingsField(t, rb, field)
		if derefOr(f.Recorded) != want.recorded || derefOr(f.Upstream) != want.upstream || f.Comparison != want.verdict {
			t.Errorf("%s = %q / %q / %s, want %+v", field, derefOr(f.Recorded), derefOr(f.Upstream), f.Comparison, want)
		}
	}
}
