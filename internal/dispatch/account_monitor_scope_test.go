// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// monitorScopeEntry builds one scope row whose Result records the creating account under key.
func monitorScopeEntry(id, key, account string) model.ProjectCampaignScope {
	blob, _ := json.Marshal(map[string]string{key: account})
	return model.ProjectCampaignScope{PlatformCampaignID: id, Result: blob}
}

// TestMonitorScopeForAccount covers the four shared-tenancy dispatchers: a scope recorded under
// the monitored account comes back as its ids, and one entry recorded under another account fails
// the whole call with ErrCampaignAccountMismatch rather than being dropped.
func TestMonitorScopeForAccount(t *testing.T) {
	cases := []struct {
		name    string
		scoper  service.AccountMonitorScoper
		key     string
		account string
		other   string
		ids     [2]string
	}{
		{"google ads", &GoogleAdsDispatcher{}, "customerId", "1234567890", "9876543210", [2]string{"111", "222"}},
		{"meta", &MetaDispatcher{}, "AccountID", "act_1001", "act_2002", [2]string{"120000001", "120000002"}},
		{"reddit", &RedditDispatcher{}, "accountId", "a2_owner", "a2_other", [2]string{"camp1", "camp2"}},
		{"x", &TwitterDispatcher{}, "AccountID", "18ce54d4x5t", "18ce54aaaaa", [2]string{"hkqkb", "hkqkc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/matching scope returns its ids", func(t *testing.T) {
			scope := []model.ProjectCampaignScope{
				monitorScopeEntry(tc.ids[0], tc.key, tc.account),
				monitorScopeEntry(tc.ids[1], tc.key, tc.account),
			}
			got, err := tc.scoper.MonitorScopeForAccount(scope, tc.account)
			if err != nil {
				t.Fatalf("MonitorScopeForAccount: %v", err)
			}
			if len(got) != 2 || got[0] != tc.ids[0] || got[1] != tc.ids[1] {
				t.Fatalf("ids = %v, want %v", got, tc.ids)
			}
		})
		t.Run(tc.name+"/one entry under another account fails the whole scope", func(t *testing.T) {
			scope := []model.ProjectCampaignScope{
				monitorScopeEntry(tc.ids[0], tc.key, tc.account),
				monitorScopeEntry(tc.ids[1], tc.key, tc.other),
			}
			got, err := tc.scoper.MonitorScopeForAccount(scope, tc.account)
			if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Fatalf("err = %v, want ErrCampaignAccountMismatch", err)
			}
			if got != nil {
				t.Fatalf("ids = %v on a mismatch, want nil", got)
			}
		})
	}
}

// TestMonitorScopeForAccount_RedditRefusesUnknownProvenance pins the Reddit-only strictness: a row
// that records no creating account cannot be proven to name a campaign on the monitored account.
func TestMonitorScopeForAccount_RedditRefusesUnknownProvenance(t *testing.T) {
	scope := []model.ProjectCampaignScope{{PlatformCampaignID: "camp1"}}
	_, err := (&RedditDispatcher{}).MonitorScopeForAccount(scope, "a2_owner")
	if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) {
		t.Fatalf("err = %v, want ErrCampaignProvenanceUnknown", err)
	}
}

// TestMonitorScopeForAccount_LinkedInAndMicrosoftAreNotScoped pins that only the shared-tenancy
// platforms carry the capability: LinkedIn and Microsoft monitors keep their account-wide read.
func TestMonitorScopeForAccount_LinkedInAndMicrosoftAreNotScoped(t *testing.T) {
	for name, d := range map[string]any{
		"linkedin":  &LinkedInDispatcher{},
		"microsoft": &MicrosoftDispatcher{},
	} {
		if _, ok := d.(service.AccountMonitorScoper); ok {
			t.Errorf("%s dispatcher implements AccountMonitorScoper; its account is not recorded as shared", name)
		}
	}
}
