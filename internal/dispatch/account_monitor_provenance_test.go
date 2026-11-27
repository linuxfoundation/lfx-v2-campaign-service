// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The account-wide monitor's row labels (#672) through each platform's REAL classifier: a record
// naming no account ranked before one naming the monitored account must not win, one naming
// another account never labels, and an unknown one labels only when nothing matches.
func TestRecordedProjectsByID_RealClassifiers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		classifier service.AccountMonitorProvenanceClassifier
		account    string
		match      string
		foreign    string
	}{
		{"google ads", &GoogleAdsDispatcher{}, "8666746580", `{"customerId":"8666746580"}`, `{"customerId":"1111111111"}`},
		{"meta", &MetaDispatcher{}, "act_1001", `{"AccountID":"act_1001"}`, `{"AccountID":"act_2002"}`},
		{"reddit", &RedditDispatcher{}, "a2_owner", `{"accountId":"a2_owner"}`, `{"accountId":"a2_other"}`},
		{"x", &TwitterDispatcher{}, "18ce54d4x5t", `{"AccountID":"18ce54d4x5t"}`, `{"AccountID":"18ce00000aa"}`},
		{"linkedin", &LinkedInDispatcher{}, "512345678", `{"accountId":"512345678"}`, `{"accountId":"500000001"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := func(id, project, result string) model.RecordedCampaignOwner {
				var raw json.RawMessage
				if result != "" {
					raw = json.RawMessage(result)
				}
				return model.RecordedCampaignOwner{ProjectCampaignScope: model.ProjectCampaignScope{PlatformCampaignID: id, Result: raw}, ProjectID: project}
			}
			got := service.RecordedProjectsByID([]model.RecordedCampaignOwner{
				owner("123", "legacy", ""), owner("123", "p", tc.match), // unknown ranked first, a match after
				owner("456", "q", tc.foreign), owner("456", "legacy", ""), // foreign first, unknown after
				owner("789", "q", tc.foreign), // foreign only
			}, tc.classifier, tc.account)
			if got["123"] != "p" || got["456"] != "legacy" {
				t.Errorf("owners = %v, want 123:p (match beats an earlier unknown), 456:legacy (unknown beats foreign)", got)
			}
			if _, labeled := got["789"]; labeled {
				t.Errorf("owners = %v, want 789 unlabeled (only a foreign record)", got)
			}
		})
	}
}
