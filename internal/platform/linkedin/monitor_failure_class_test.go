// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// Every LinkedIn row failure — an unparseable budget amount or costInLocalCurrency — is classed decode;
// a healthy row carries no class.
func TestListAccountCampaigns_FailedRowIsDecode(t *testing.T) {
	srv, _ := adAccountsServer(t,
		`{"elements":[`+
			`{"id":111,"name":"Cost","status":"ACTIVE"},`+
			`{"id":222,"name":"Budget","status":"ACTIVE","dailyBudget":{"amount":"not-a-number"}},`+
			`{"id":333,"name":"Healthy","status":"ACTIVE"}`+
			`],"metadata":{}}`,
		`{"elements":[`+
			`{"pivotValues":["urn:li:sponsoredCampaign:111"],"impressions":1000,"clicks":50,"costInLocalCurrency":"not-a-number"},`+
			`{"pivotValues":["urn:li:sponsoredCampaign:222"],"impressions":10,"clicks":1,"costInLocalCurrency":"1.00"},`+
			`{"pivotValues":["urn:li:sponsoredCampaign:333"],"impressions":10,"clicks":1,"costInLocalCurrency":"1.00"}`+
			`]}`,
	)
	c := NewClient(Credentials{AccessToken: "tok-secret-abc"}, RuntimeConfig{}, WithBaseURL(srv.URL))
	rows, err := c.ListAccountCampaigns(context.Background(), "512345678", 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	want := map[string]model.MonitorFailureClass{"111": model.MonitorFailureDecode, "222": model.MonitorFailureDecode, "333": ""}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for _, r := range rows {
		w := want[r.CampaignID]
		if r.FailureClass != w || r.FetchFailed != (w != "") {
			t.Errorf("%s: FetchFailed=%v FailureClass=%q, want class %q", r.CampaignID, r.FetchFailed, r.FailureClass, w)
		}
	}
}
