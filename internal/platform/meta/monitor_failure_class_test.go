// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// Every Meta row failure is a value Meta returned that would not parse, so a failed row is
// always classed decode, and a healthy one carries no class.
func TestListAccountCampaigns_FailedRowIsDecode(t *testing.T) {
	insights := `{"data":[` +
		`{"campaign_id":"111","impressions":"not-a-number","clicks":"50","spend":"12.50"},` +
		`{"campaign_id":"222","impressions":"10","clicks":"1","spend":"1.00"}` +
		`],"paging":{}}`
	srv, _ := monitorPageResponses(t,
		[]string{`{"data":[` +
			// Campaign-level (CBO) budgets: a campaign with none would send the read to the
			// ad-set budget fallback, which this fixture does not serve.
			`{"id":"111","name":"One","status":"ACTIVE","daily_budget":"1000"},` +
			`{"id":"222","name":"Two","status":"ACTIVE","daily_budget":"1000"}` +
			`],"paging":{}}`},
		[]string{insights},
	)
	rows, err := newMonitorClient(srv).ListAccountCampaigns(context.Background(), "act_123", 30)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	for _, r := range rows {
		want := model.MonitorFailureClass("")
		if r.CampaignID == "111" {
			want = model.MonitorFailureDecode
		}
		if r.FailureClass != want || r.FetchFailed != (want != "") {
			t.Errorf("%s: FetchFailed=%v FailureClass=%q, want class %q", r.CampaignID, r.FetchFailed, r.FailureClass, want)
		}
	}
}
