// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// This file answers the three live-verification gates research.md §7 opens for the
// Demand Gen single-image creative (O1, O2, O4). It talks to the REAL Google Ads
// API — never a fake server — using validateOnly:true on every mutate, so it never
// creates, updates, or spends anything on the account. O3 (asset dedupe) is
// deliberately out of scope: it needs two NON-validateOnly creates of identical
// bytes, which this harness does not attempt.
//
// Mirrors internal/infrastructure/postgres/dbtest/dbtest.go's opt-in convention:
// unset, this test calls t.Skip so `go test ./...` stays green with no Google Ads
// access configured — the exact situation research.md §7 says blocked O1/O2/O4
// until now. Once GOOGLE_ADS_LIVE_CUSTOMER_ID is set, every failure is a hard
// t.Fatal, never a skip: a probe that silently skips when it should run is
// indistinguishable from a probe that never existed, and would let a stale "not
// live-verified" comment stand unchallenged.
//
// Env vars are named distinctly from pkg/constants (GOOGLE_ADS_LIVE_*, not the
// production credential columns) so nothing here suggests the service itself
// reads them — Credentials/AccountConfig are injected in production exactly as
// client.go documents, never from the environment.
const (
	envLiveCustomerID     = "GOOGLE_ADS_LIVE_CUSTOMER_ID"
	envLiveClientID       = "GOOGLE_ADS_LIVE_CLIENT_ID"
	envLiveClientSecret   = "GOOGLE_ADS_LIVE_CLIENT_SECRET"
	envLiveDeveloperToken = "GOOGLE_ADS_LIVE_DEVELOPER_TOKEN"
	envLiveRefreshToken   = "GOOGLE_ADS_LIVE_REFRESH_TOKEN"
)

// liveClient builds a client against the REAL Google Ads endpoints (no
// WithBaseURL/WithTokenURL override) from env-injected credentials, or calls
// t.Skip if the gate is unset. LoginCustomerID is deliberately left empty — the
// 2026-08-19 lesson that marketingops_lfx is a direct user on 8666746580, not an
// MCC member, and a login-customer-id header there is a 403.
func liveClient(t *testing.T) (*Client, string) {
	t.Helper()
	customerID := strings.TrimSpace(os.Getenv(envLiveCustomerID))
	if customerID == "" {
		t.Skip("GOOGLE_ADS_LIVE_CUSTOMER_ID unset; skipping live Google Ads verification (see research.md §7 O1/O2/O4)")
	}
	// Once a customer id was named, every other credential is REQUIRED — a
	// half-configured gate must fail loudly, not skip, or a broken CI secret
	// would silently stop verifying anything while still reporting green.
	creds := Credentials{
		ClientID:       strings.TrimSpace(os.Getenv(envLiveClientID)),
		ClientSecret:   strings.TrimSpace(os.Getenv(envLiveClientSecret)),
		DeveloperToken: strings.TrimSpace(os.Getenv(envLiveDeveloperToken)),
		RefreshToken:   strings.TrimSpace(os.Getenv(envLiveRefreshToken)),
	}
	if creds.ClientID == "" || creds.ClientSecret == "" || creds.DeveloperToken == "" || creds.RefreshToken == "" {
		t.Fatalf("%s is set but one or more of %s/%s/%s/%s is empty — a live account was named, so an incomplete credential set must fail, not skip",
			envLiveCustomerID, envLiveClientID, envLiveClientSecret, envLiveDeveloperToken, envLiveRefreshToken)
	}
	account := AccountConfig{CustomerID: customerID, Label: "live-verification-probe"}
	return NewClient(creds, account), customerID
}

// liveCtx bounds every probe call so a stalled real network call fails the test
// instead of hanging `go test`.
func liveCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// --- discovery (read-only; also the before/after snapshot for the no-writes check) ---

type liveInventory struct {
	demandGenAdGroups []string // ad_group.resource_name for advertising_channel_type = DEMAND_GEN
	searchAdGroups    []string // ad_group.resource_name for advertising_channel_type = SEARCH (Tier B fallback)
	imageAssets       []string // asset.resource_name where asset.type = IMAGE
	budgets           []string // campaign_budget.resource_name (for O4)
}

func discoverLiveInventory(t *testing.T, c *Client) liveInventory {
	t.Helper()
	ctx := liveCtx(t)

	var inv liveInventory

	type adGroupRow struct {
		Campaign struct {
			AdvertisingChannelType string `json:"advertisingChannelType"`
		} `json:"campaign"`
		AdGroup struct {
			ResourceName string `json:"resourceName"`
		} `json:"adGroup"`
	}
	rows, err := c.gaqlSearch(ctx, "SELECT campaign.advertising_channel_type, ad_group.resource_name FROM ad_group")
	if err != nil {
		t.Fatalf("discovery: ad_group search failed: %v", err)
	}
	for _, raw := range rows {
		var row adGroupRow
		if jerr := json.Unmarshal(raw, &row); jerr != nil {
			t.Fatalf("discovery: decode ad_group row: %v", jerr)
		}
		switch row.Campaign.AdvertisingChannelType {
		case "DEMAND_GEN":
			inv.demandGenAdGroups = append(inv.demandGenAdGroups, row.AdGroup.ResourceName)
		case "SEARCH":
			inv.searchAdGroups = append(inv.searchAdGroups, row.AdGroup.ResourceName)
		}
	}

	type assetRow struct {
		Asset struct {
			ResourceName string `json:"resourceName"`
			Type         string `json:"type"`
		} `json:"asset"`
	}
	rows, err = c.gaqlSearch(ctx, "SELECT asset.resource_name, asset.type FROM asset WHERE asset.type = 'IMAGE'")
	if err != nil {
		t.Fatalf("discovery: asset search failed: %v", err)
	}
	for _, raw := range rows {
		var row assetRow
		if jerr := json.Unmarshal(raw, &row); jerr != nil {
			t.Fatalf("discovery: decode asset row: %v", jerr)
		}
		inv.imageAssets = append(inv.imageAssets, row.Asset.ResourceName)
	}

	type budgetRow struct {
		CampaignBudget struct {
			ResourceName string `json:"resourceName"`
		} `json:"campaignBudget"`
	}
	rows, err = c.gaqlSearch(ctx, "SELECT campaign_budget.resource_name FROM campaign_budget")
	if err != nil {
		t.Fatalf("discovery: campaign_budget search failed: %v", err)
	}
	for _, raw := range rows {
		var row budgetRow
		if jerr := json.Unmarshal(raw, &row); jerr != nil {
			t.Fatalf("discovery: decode campaign_budget row: %v", jerr)
		}
		inv.budgets = append(inv.budgets, row.CampaignBudget.ResourceName)
	}

	t.Logf("discovery: %d Demand Gen ad groups, %d Search ad groups, %d IMAGE assets, %d budgets",
		len(inv.demandGenAdGroups), len(inv.searchAdGroups), len(inv.imageAssets), len(inv.budgets))
	return inv
}

// probeResult is a single named result line — never logs a token/secret, only
// status code, Google's error codes, and the (already-redacted) response snapshot.
func logProbe(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Logf("%s: HTTP 200 (validateOnly accepted the payload)", name)
		return
	}
	var ae *apiError
	if as, ok := err.(*apiError); ok {
		ae = as
	}
	if ae != nil {
		t.Logf("%s: HTTP %d, errorCodes=%v, body=%s", name, ae.StatusCode, ae.ErrorCodes, ae.Body)
		return
	}
	t.Logf("%s: non-API error: %v", name, err)
}

// TestLive_DemandGen_ValidateOnlyGates runs O2, O4, and (Tier A or Tier B) O1
// against the real account named by GOOGLE_ADS_LIVE_CUSTOMER_ID. Every mutate
//
//	carries validateOnly:true. Run with: go test ./internal/platform/googleads/... \
//	  -run TestLive_DemandGen_ValidateOnlyGates -v
func TestLive_DemandGen_ValidateOnlyGates(t *testing.T) {
	c, customerID := liveClient(t)
	before := discoverLiveInventory(t, c)

	t.Run("O2_asset_create_shape", func(t *testing.T) {
		testO2(t, c)
	})

	t.Run("O4_targetSpend_still_200", func(t *testing.T) {
		testO4(t, c, before.budgets)
	})

	t.Run("O1_ad_shape_and_required_set", func(t *testing.T) {
		testO1(t, c, before)
	})

	// Confirm nothing was created: re-run discovery and diff counts against `before`.
	after := discoverLiveInventory(t, c)
	if len(after.demandGenAdGroups) != len(before.demandGenAdGroups) ||
		len(after.searchAdGroups) != len(before.searchAdGroups) ||
		len(after.imageAssets) != len(before.imageAssets) ||
		len(after.budgets) != len(before.budgets) {
		t.Fatalf("inventory changed during the probe run (before=%+v after=%+v) — a validateOnly probe must never create anything; treat this as a real incident on account %s",
			before, after, customerID)
	}
	t.Logf("post-run discovery matches pre-run discovery exactly — confirmed no object was created on account %s", customerID)
}

// testO2 answers: is imageAsset accepted WITHOUT an explicit type, and is sending
// an explicit type ignored or rejected? Uses a minimal valid 1x1 PNG so the
// payload is a real image, not just well-formed base64.
func testO2(t *testing.T, c *Client) {
	ctx := liveCtx(t)
	// The smallest valid PNG: 1x1, single red pixel.
	onePixelPNG := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x03, 0x01, 0x01, 0x00, 0x18, 0xdd, 0x8d, 0xb0, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
		0x44, 0xae, 0x42, 0x60, 0x82,
	}

	// Variant A: assetCreate as the code actually sends it (assets.go) — no `type`.
	reqA := mutateRequest{
		ValidateOnly: true,
		Operations: []mutateOperation{{Create: assetCreate{
			ImageAsset: imageAsset{Data: base64.StdEncoding.EncodeToString(onePixelPNG)},
		}}},
	}
	_, errA := c.doRequest(ctx, "POST", c.customerPath("assets:mutate"), reqA, false)
	logProbe(t, "O2 (no type field, matches assets.go)", errA)
	if errA != nil {
		if ae, ok := errA.(*apiError); ok && ae.StatusCode >= 400 && ae.StatusCode < 500 {
			t.Errorf("O2: assets.go's exact payload was rejected (HTTP %d) — assets.go is no longer correct as written: %v", ae.StatusCode, errA)
		}
	}

	// Variant B: same payload PLUS an explicit type, to learn ignored-vs-rejected
	// (the open question left in assets.go's doc comment).
	type imageAssetWithType struct {
		Data string `json:"data"`
		Type string `json:"type"`
	}
	type assetCreateWithType struct {
		ImageAsset imageAssetWithType `json:"imageAsset"`
	}
	reqB := mutateRequest{
		ValidateOnly: true,
		Operations: []mutateOperation{{Create: assetCreateWithType{
			ImageAsset: imageAssetWithType{Data: base64.StdEncoding.EncodeToString(onePixelPNG), Type: "IMAGE"},
		}}},
	}
	_, errB := c.doRequest(ctx, "POST", c.customerPath("assets:mutate"), reqB, false)
	logProbe(t, "O2 (explicit type=IMAGE)", errB)
}

// testO4 re-confirms the 2026-08-14 result: DEMAND_GEN + targetSpend:{} still
// validates at v23, and the maximizeConversions alternative is still rejected
// against a shared budget. Requires a real campaignBudget resource name; skips
// with a clear reason if the account has none (rather than fabricating one,
// which validateOnly would reject as NOT_FOUND — indistinguishable from a real
// shape failure).
func testO4(t *testing.T, c *Client, budgets []string) {
	if len(budgets) == 0 {
		t.Skip("O4: no existing campaign_budget on this account to validate against; cannot run without creating one (out of scope for a no-writes probe)")
	}
	ctx := liveCtx(t)
	budget := budgets[0]

	okReq := mutateRequest{
		ValidateOnly: true,
		Operations: []mutateOperation{{Create: demandGenCampaignCreate{
			Name:                           fmt.Sprintf("O4-probe-%d", time.Now().UnixNano()),
			Status:                         "PAUSED",
			AdvertisingChannelType:         advertisingChannelDemandGen,
			CampaignBudget:                 budget,
			ContainsEuPoliticalAdvertising: euPoliticalAdvertisingNo,
			TargetSpend:                    map[string]any{},
		}}},
	}
	_, err := c.doRequest(ctx, "POST", c.customerPath("campaigns:mutate"), okReq, false)
	logProbe(t, "O4 (targetSpend:{})", err)
	if err != nil {
		t.Errorf("O4: expected HTTP 200 for DEMAND_GEN + targetSpend (2026-08-14 precedent) but got an error — demandgen.go's committed shape is no longer valid: %v", err)
	}

	// The documented-as-rejected alternative, re-run so a 400 here confirms the
	// 200 above is meaningful (not just "the endpoint accepts anything").
	type demandGenCampaignCreateMaximize struct {
		Name                           string         `json:"name"`
		Status                         string         `json:"status"`
		AdvertisingChannelType         string         `json:"advertisingChannelType"`
		CampaignBudget                 string         `json:"campaignBudget"`
		ContainsEuPoliticalAdvertising string         `json:"containsEuPoliticalAdvertising"`
		MaximizeConversions            map[string]any `json:"maximizeConversions"`
	}
	badReq := mutateRequest{
		ValidateOnly: true,
		Operations: []mutateOperation{{Create: demandGenCampaignCreateMaximize{
			Name:                           fmt.Sprintf("O4-probe-neg-%d", time.Now().UnixNano()),
			Status:                         "PAUSED",
			AdvertisingChannelType:         advertisingChannelDemandGen,
			CampaignBudget:                 budget,
			ContainsEuPoliticalAdvertising: euPoliticalAdvertisingNo,
			MaximizeConversions:            map[string]any{},
		}}},
	}
	_, err = c.doRequest(ctx, "POST", c.customerPath("campaigns:mutate"), badReq, false)
	logProbe(t, "O4 (maximizeConversions, expected 400)", err)
	if err == nil {
		t.Logf("O4 note: maximizeConversions against a shared budget now validates — the 2026-08-14 rejection may no longer hold; this is informative, not a failure of demandgen.go")
	}
}

// testO1 answers the ad-shape questions: field name recognition, the true
// required set among the three image roles, and the businessName/headline caps.
// Runs at full fidelity (Tier A) if the account has a Demand Gen ad group and
// real IMAGE assets; otherwise degrades to Tier B against a Search ad group and
// records exactly which sub-questions that leaves unanswered.
func testO1(t *testing.T, c *Client, inv liveInventory) {
	ctx := liveCtx(t)

	var adGroup string
	tierA := len(inv.demandGenAdGroups) > 0 && len(inv.imageAssets) >= 3
	switch {
	case tierA:
		adGroup = inv.demandGenAdGroups[0]
		t.Logf("O1: Tier A — real Demand Gen ad group + %d real IMAGE assets available", len(inv.imageAssets))
	case len(inv.searchAdGroups) > 0:
		adGroup = inv.searchAdGroups[0]
		t.Logf("O1: Tier B — no Demand Gen ad group/asset triple available; probing against a Search ad group %s. "+
			"This can only answer whether the field name is recognized and separate a channel-mismatch error from a "+
			"field-shape error; it CANNOT confirm the true required-role set, char limits, or portraitMarketingImages "+
			"against a live Demand Gen ad group. Recorded as a partial result, not green.", adGroup)
	default:
		t.Skip("O1: account has neither a Demand Gen ad group nor a Search ad group to probe against; cannot run under a no-writes constraint (would require creating a throwaway ad group first)")
	}

	// Fabricated asset resource names are fine here: validateOnly still runs
	// field-shape/business-rule validation before it would resolve the referenced
	// asset, and a NOT_FOUND on the asset reference is itself informative (proves
	// the field was reached), not a shape failure.
	fakeAsset := func(role string) adImageAsset {
		return adImageAsset{Asset: c.customerPath("assets/999999999") + "-" + role}
	}

	full := demandGenMultiAssetResponsiveDisplayAd{
		BusinessName:          "Linux Foundation",
		Headlines:             textAssets([]string{"H1", "H2", "H3"}),
		Descriptions:          textAssets([]string{"D1", "D2"}),
		MarketingImages:       []adImageAsset{fakeAsset("marketing")},
		SquareMarketingImages: []adImageAsset{fakeAsset("square")},
		LogoImages:            []adImageAsset{fakeAsset("logo")},
	}
	if tierA {
		full.MarketingImages = []adImageAsset{{Asset: inv.imageAssets[0]}}
		full.SquareMarketingImages = []adImageAsset{{Asset: inv.imageAssets[min1(len(inv.imageAssets)-1, 1)]}}
		full.LogoImages = []adImageAsset{{Asset: inv.imageAssets[min1(len(inv.imageAssets)-1, 2)]}}
	}

	fire := func(name string, ad demandGenMultiAssetResponsiveDisplayAd) error {
		req := mutateRequest{
			ValidateOnly: true,
			Operations: []mutateOperation{{Create: adGroupAdCreate{
				AdGroup: adGroup,
				Status:  StatusPaused,
				Ad: adCreate{
					FinalUrls:                              []string{"https://www.linuxfoundation.org/"},
					DemandGenMultiAssetResponsiveDisplayAd: &ad,
				},
			}}},
		}
		_, err := c.doRequest(ctx, "POST", c.customerPath("adGroupAds:mutate"), req, false)
		logProbe(t, name, err)
		return err
	}

	baseErr := fire("O1 (full ad, all three roles)", full)
	if tierA && baseErr != nil {
		t.Errorf("O1: the exact demandGenMultiAssetResponsiveDisplayAd shape from demandgen_ad.go was rejected against a real Demand Gen ad group: %v", baseErr)
	}
	if !tierA {
		// Tier B: distinguish "field unrecognized" from "field recognized, wrong channel".
		unrecognized := baseErr != nil && strings.Contains(errBody(baseErr), "Cannot find field")
		t.Logf("O1 Tier B verdict: field name %q unrecognized-by-parser=%v (false means Google parsed the field and rejected it for a business reason, e.g. channel mismatch — which still CONFIRMS the field name is correct)",
			"demandGenMultiAssetResponsiveDisplayAd", unrecognized)
		if unrecognized {
			t.Errorf("O1: demandGenMultiAssetResponsiveDisplayAd is not a recognized field at v23 — demandgen_ad.go's core assumption is wrong")
		}
	}

	// Required-role probes: drop one role at a time. Only trustworthy against a
	// real Demand Gen ad group (Tier A); still logged under Tier B for visibility
	// but not asserted on, since a channel-mismatch error there would mask the
	// real answer.
	dropped := full
	dropped.MarketingImages = nil
	errNoMarketing := fire("O1 (marketingImages omitted)", dropped)

	dropped = full
	dropped.SquareMarketingImages = nil
	errNoSquare := fire("O1 (squareMarketingImages omitted)", dropped)

	dropped = full
	dropped.LogoImages = nil
	errNoLogo := fire("O1 (logoImages omitted)", dropped)

	if tierA {
		for role, err := range map[string]error{"marketingImages": errNoMarketing, "squareMarketingImages": errNoSquare, "logoImages": errNoLogo} {
			if err == nil {
				t.Logf("O1 result: %s is NOT required (omitting it still validated) — demandgen_ad.go:98-109 currently hard-requires it; reconcile if this holds", role)
			} else {
				t.Logf("O1 result: %s IS required (omitting it was rejected), matching demandgen_ad.go:98-109", role)
			}
		}
	}

	// businessName over the documented 25-rune cap.
	over := full
	over.BusinessName = strings.Repeat("A", maxBusinessNameRunes+1)
	errLongName := fire(fmt.Sprintf("O1 (businessName %d runes, cap is %d)", maxBusinessNameRunes+1, maxBusinessNameRunes), over)
	if tierA {
		if errLongName == nil {
			t.Errorf("O1: businessName of %d runes validated — maxBusinessNameRunes=%d in demandgen_ad.go is too permissive", maxBusinessNameRunes+1, maxBusinessNameRunes)
		} else {
			t.Logf("O1 result: businessName cap of %d runes holds (over-length rejected)", maxBusinessNameRunes)
		}
	}

	// A 6th headline, over the documented Demand Gen cap of 5.
	over = full
	over.Headlines = textAssets([]string{"H1", "H2", "H3", "H4", "H5", "H6"})
	errSixHeadlines := fire(fmt.Sprintf("O1 (%d headlines, cap is %d)", maxDemandGenHeadlines+1, maxDemandGenHeadlines), over)
	if tierA {
		if errSixHeadlines == nil {
			t.Errorf("O1: %d headlines validated — maxDemandGenHeadlines=%d in demandgen_ad.go is too permissive", maxDemandGenHeadlines+1, maxDemandGenHeadlines)
		} else {
			t.Logf("O1 result: headline cap of %d holds (a 6th headline was rejected)", maxDemandGenHeadlines)
		}
	}

	// A headline at RSA's 40-char field limit rather than the 30-char WEIGHT cap
	// this code borrows from RSA (ad_copy.go maxHeadlineWeight) — informative only,
	// answers whether Demand Gen's real limit is wider than what composeAdCopy
	// currently enforces.
	over = full
	over.Headlines = textAssets([]string{strings.Repeat("H", 40), "H2", "H3"})
	errWideHeadline := fire("O1 (40-char headline, current cap borrowed from RSA is 30)", over)
	logProbe(t, "O1 (40-char headline note)", errWideHeadline)
}

func errBody(err error) string {
	if ae, ok := err.(*apiError); ok {
		return ae.Body
	}
	return err.Error()
}

func min1(a, b int) int {
	if a < b {
		return a
	}
	return b
}
