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
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
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

// TestMonitorScopeForAccount_RefusesUnknownProvenance pins the strictness Google Ads, Reddit and X
// share on the monitor: a row that records no creating account cannot be proven to name a campaign
// on the monitored account, alone or beside a matching one, and beside a mismatching one it is the
// mixed fault that names both remedies. Meta keeps the permissive read.
func TestMonitorScopeForAccount_RefusesUnknownProvenance(t *testing.T) {
	cases := []struct {
		name    string
		scoper  service.AccountMonitorScoper
		key     string
		account string
		other   string
		ids     [2]string
	}{
		{"google ads", &GoogleAdsDispatcher{}, "customerId", "1234567890", "9876543210", [2]string{"111", "222"}},
		{"reddit", &RedditDispatcher{}, "accountId", "a2_owner", "a2_other", [2]string{"camp1", "camp2"}},
		{"x", &TwitterDispatcher{}, "AccountID", "18ce54d4x5t", "18ce54aaaaa", [2]string{"hkqkb", "hkqkc"}},
	}
	for _, tc := range cases {
		unrecorded := model.ProjectCampaignScope{PlatformCampaignID: tc.ids[1]}
		t.Run(tc.name+"/unrecorded entry fails the scope", func(t *testing.T) {
			for _, scope := range [][]model.ProjectCampaignScope{
				{unrecorded},
				{monitorScopeEntry(tc.ids[0], tc.key, tc.account), unrecorded},
			} {
				got, err := tc.scoper.MonitorScopeForAccount(scope, tc.account)
				if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) || !errors.Is(err, domain.ErrCampaignAccountMismatch) {
					t.Fatalf("err = %v, want ErrCampaignProvenanceUnknown joined with ErrCampaignAccountMismatch", err)
				}
				if errors.Is(err, domain.ErrCampaignProvenanceMixed) {
					t.Fatalf("err = %v: no entry mismatches, so this is not the mixed fault", err)
				}
				if got != nil {
					t.Fatalf("ids = %v on a refusal, want nil", got)
				}
			}
		})
		t.Run(tc.name+"/unrecorded beside mismatched is the mixed fault", func(t *testing.T) {
			scope := []model.ProjectCampaignScope{monitorScopeEntry(tc.ids[0], tc.key, tc.other), unrecorded}
			_, err := tc.scoper.MonitorScopeForAccount(scope, tc.account)
			for _, want := range []error{domain.ErrCampaignProvenanceMixed, domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch} {
				if !errors.Is(err, want) {
					t.Fatalf("err = %v, want it to wrap %v", err, want)
				}
			}
		})
	}

	t.Run("meta keeps an unrecorded entry", func(t *testing.T) {
		got, err := (&MetaDispatcher{}).MonitorScopeForAccount([]model.ProjectCampaignScope{{PlatformCampaignID: "120000001"}}, "act_1001")
		if err != nil || len(got) != 1 {
			t.Fatalf("ids, err = %v, %v; want the one id", got, err)
		}
	})
}

// TestMonitorScopePrecheck pins the credential-free refusals the orchestrator runs before the
// project's scope: each platform's own account id shape check, and X's TWITTER_METRICS_ENABLED
// gate, so a disabled X monitor is a 400 whatever the project has recorded.
func TestMonitorScopePrecheck(t *testing.T) {
	t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
	cases := []struct {
		name      string
		scoper    service.AccountMonitorScoper
		valid     string
		malformed string
	}{
		{"google ads", &GoogleAdsDispatcher{}, "1234567890", "123-456-7890"},
		{"meta", &MetaDispatcher{}, "act_1001", "1001"},
		{"reddit", &RedditDispatcher{}, "a2_owner", "a2-owner"},
		{"x", &TwitterDispatcher{}, "18ce54d4x5t", "18ce54d4x5t!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.scoper.MonitorScopePrecheck(tc.valid); err != nil {
				t.Fatalf("precheck(%q) = %v, want nil", tc.valid, err)
			}
			if err := tc.scoper.MonitorScopePrecheck(tc.malformed); !errors.Is(err, domain.ErrAccountIDMalformed) {
				t.Fatalf("precheck(%q) = %v, want ErrAccountIDMalformed", tc.malformed, err)
			}
		})
	}
	t.Run("x disabled", func(t *testing.T) {
		t.Setenv(constants.EnvTwitterMetricsEnabled, "")
		if err := (&TwitterDispatcher{}).MonitorScopePrecheck("18ce54d4x5t"); !errors.Is(err, domain.ErrAccountMetricsUnsupported) {
			t.Fatalf("precheck = %v, want ErrAccountMetricsUnsupported while the flag is off", err)
		}
	})
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
