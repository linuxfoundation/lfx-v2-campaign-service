// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// #473: the Search-parity config fields must reach the outbound requests. The client's
// own tests pin each payload FROM a CampaignInput; these pin that the wire config is
// mapped onto that input at all.

type parityOp struct {
	Create struct {
		AdGroup     string `json:"adGroup"`
		Status      string `json:"status"`
		Negative    bool   `json:"negative"`
		Keyword     *struct{ Text, MatchType string }
		IncomeRange *struct {
			Type string `json:"type"`
		} `json:"incomeRange"`
		ParentalStatus *struct {
			Type string `json:"type"`
		} `json:"parentalStatus"`
		Ad struct {
			ResponsiveSearchAd struct {
				Path1 string `json:"path1"`
				Path2 string `json:"path2"`
			} `json:"responsiveSearchAd"`
		} `json:"ad"`
	} `json:"create"`
}

func parityOps(t *testing.T, body []byte) []parityOp {
	t.Helper()
	var req struct {
		Operations []parityOp `json:"operations"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode mutate body: %v (%s)", err, body)
	}
	return req.Operations
}

func TestGoogleAds_SearchParityConfigReachesTheCascade(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{
		"budget":50,
		"path1":"kubecon","path2":"na",
		"excludedIncomeRanges":["INCOME_RANGE_0_50"],
		"adGroups":[{
			"name":"Training",
			"negativeKeywords":[{"text":"free","matchType":"EXACT"}],
			"ads":[{"headlines":["Train with us","Hands-on labs","Certify"]},
			       {"headlines":["Level up","Experts","Live"],"path1":"training"}]
		}]
	}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cap.mu.Lock()
	ads, criteria := cap.adGroupAds, cap.adGroupCriteria
	cap.mu.Unlock()

	if len(ads) != 1 {
		t.Fatalf("got %d adGroupAds mutates, want 1", len(ads))
	}
	adOps := parityOps(t, ads[0])
	if p := adOps[0].Create.Ad.ResponsiveSearchAd; p.Path1 != "kubecon" || p.Path2 != "na" {
		t.Errorf("first ad paths = %q/%q, want the campaign-level kubecon/na", p.Path1, p.Path2)
	}
	if p := adOps[1].Create.Ad.ResponsiveSearchAd; p.Path1 != "training" || p.Path2 != "" {
		t.Errorf("second ad paths = %q/%q, want its own training and no path2", p.Path1, p.Path2)
	}

	if len(criteria) != 1 {
		t.Fatalf("got %d adGroupCriteria mutates, want 1", len(criteria))
	}
	ops := parityOps(t, criteria[0])
	if len(ops) != 2 {
		t.Fatalf("got %d criteria operations, want the negative then the income exclusion", len(ops))
	}
	if kw := ops[0].Create.Keyword; !ops[0].Create.Negative || kw == nil || kw.Text != "free" || kw.MatchType != "EXACT" {
		t.Errorf("negative keyword op = %+v", ops[0].Create)
	}
	if ir := ops[1].Create.IncomeRange; !ops[1].Create.Negative || ir == nil || ir.Type != "INCOME_RANGE_0_50" {
		t.Errorf("income exclusion op = %+v", ops[1].Create)
	}
}

// Display takes the parental-status exclusion, on its ad group.
func TestGoogleAds_ParentalStatusReachesTheDisplayAdGroup(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"display","excludedParentalStatuses":["NOT_A_PARENT"]}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cap.mu.Lock()
	criteria := cap.adGroupCriteria
	cap.mu.Unlock()
	if len(criteria) != 1 {
		t.Fatalf("got %d adGroupCriteria mutates, want 1", len(criteria))
	}
	ops := parityOps(t, criteria[0])
	if len(ops) != 1 || ops[0].Create.ParentalStatus == nil || ops[0].Create.ParentalStatus.Type != "NOT_A_PARENT" || !ops[0].Create.Negative {
		t.Errorf("parental exclusion ops = %+v", ops)
	}
}

// Each new field is refused before the budget on a channel that does not take it.
func TestGoogleAds_SearchParityFieldsAreRefusedOffChannelBeforeAnyCreate(t *testing.T) {
	cases := []struct {
		name, channel, field, want string
	}{
		{"path1 on demand gen", "demand-gen", `"path1":"kubecon"`, "display paths are not supported"},
		{"path2 on performance max", "performance-max", `"path2":"na"`, "display paths are not supported"},
		{"path1 on display", "display", `"path1":"kubecon"`, "display paths are not supported"},
		{"ad group negatives on demand gen", "demand-gen", `"adGroups":[{"name":"T","negativeKeywords":[{"text":"free","matchType":"EXACT"}]}]`, "AdGroups is the SEARCH ad-group list"},
		{"income on demand gen", "demand-gen", `"excludedIncomeRanges":["INCOME_RANGE_0_50"]`, "income-range exclusions are not supported"},
		{"income on performance max", "performance-max", `"excludedIncomeRanges":["INCOME_RANGE_0_50"]`, "income-range exclusions are not supported"},
		{"parental on search", "search", `"excludedParentalStatuses":["PARENT"]`, "OPERATION_NOT_PERMITTED_FOR_CONTEXT"},
		{"parental on demand gen", "demand-gen", `"excludedParentalStatuses":["PARENT"]`, "parental-status exclusions are not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"` + tc.channel + `",` + tc.field + `}}`)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal containing %q", err, tc.want)
			}
			if camp != nil {
				t.Errorf("nothing may be created for a refused input, got %+v", camp)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if cap.sawBudget {
				t.Error("the refusal must happen BEFORE the budget mutate")
			}
		})
	}
}
