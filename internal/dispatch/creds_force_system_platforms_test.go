// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestEveryPlatformReadsItsRecordedCreationAccount pins, for ALL FIVE paid platforms, that the
// account a campaign RECORDS having been created under is the value the existing-campaign
// credential resolver keys on.
//
// The Meta pause tests prove the mechanism end to end, but only for Meta. The other four
// adapters each wire their OWN *CreationAccountID reader into resolveExisting, and nothing in a
// Meta test can tell whether those four are wired at all: neutering any one of them to a
// constant "" left the entire dispatch suite green. A campaign created on the system account
// would then resolve the project's account and become unpausable — the exact bug this change
// fixes — on four of the five platforms, silently.
//
// It asserts through each platform's real reader against its real persisted blob shape, so a
// reader that stops matching what the create path writes fails here rather than in production.
// The blob shapes deliberately differ (tagged vs untagged, act_ prefix, per-platform key
// names) because that divergence is what makes a single shared reader impossible.
func TestEveryPlatformReadsItsRecordedCreationAccount(t *testing.T) {
	metaBlob, err := json.Marshal(meta.CampaignResult{CampaignID: "c1", AccountID: "act_999"})
	if err != nil {
		t.Fatalf("marshal meta result: %v", err)
	}

	cases := []struct {
		platform string
		blob     string
		read     func(*model.Campaign) string
		want     string
	}{
		{"meta", string(metaBlob), metaCreationAccountID, "act_999"},
		{"googleads", `{"customerId":"9999999999"}`, googleAdsCreationCustomerID, "9999999999"},
		{"microsoft", `{"accountId":"7654321"}`, microsoftCreationAccountID, "7654321"},
		{"linkedin", `{"accountId":"987654321"}`, linkedInCreationAccountID, "987654321"},
		// X/Twitter persists an UNTAGGED marshal, so the key is the Go field name.
		{"twitter", `{"AccountID":"t_sys"}`, twitterCreationAccountID, "t_sys"},
		{"reddit", `{"accountId":"t2_sys"}`, redditCreationAccountID, "t2_sys"},
	}

	for _, tc := range cases {
		t.Run(tc.platform, func(t *testing.T) {
			campaign := &model.Campaign{PlatformCampaignID: "c1", Result: json.RawMessage(tc.blob)}
			got := tc.read(campaign)
			if got != tc.want {
				t.Fatalf("%s creation-account reader = %q, want %q: a campaign created on this "+
					"account cannot be resolved back to it, so a system-created campaign becomes "+
					"unpausable", tc.platform, got, tc.want)
			}
			// The recorded account must be the one resolution keys on. matchesAccount is the
			// single predicate resolveExisting uses to decide whether a resolved connection is
			// the creating one, so asserting through it pins the reader to the DECISION rather
			// than to a string that nothing consumes.
			if !matchesAccount(got, tc.want) {
				t.Errorf("%s: matchesAccount(%q, %q) = false; the creating account would not be "+
					"recognised and resolution would fall to the wrong connection", tc.platform, got, tc.want)
			}
			if matchesAccount("some-other-account", tc.want) {
				t.Errorf("%s: matchesAccount treated a DIFFERENT account as the creating one; the "+
					"provenance steering would be inert", tc.platform)
			}
		})
	}
}

// TestEveryPlatformResolvesTheSystemAccountForASystemCreatedCampaign is the WIRING half, and
// the one that catches an adapter which reads its creation account and then does not pass it on.
//
// TestEveryPlatformReadsItsRecordedCreationAccount proves each reader returns the right value
// from the right blob shape, but a reader is inert if the toggle path ignores it. Replacing any
// one adapter's reader with a constant "" left BOTH that test and the whole dispatch suite green,
// because nothing asserted that the resolved SCOPE depends on the campaign. Four of the five
// platforms were unpinned that way.
//
// The observable seam is which project scope the connection repo is asked for. For a campaign
// recorded as created on the SYSTEM account, resolution must consult model.SystemProjectID —
// otherwise it authenticates as the project and the provenance guard 409s the pause. The
// assertion is therefore on scopedConnReader.gets, not on a returned account id, so it holds for
// every adapter regardless of how its client is built or which upstream calls it makes.
//
// Errors from the toggle are deliberately ignored: these adapters reach real HTTP once resolved,
// and the resolution decision under test happens strictly before that.
func TestEveryPlatformResolvesTheSystemAccountForASystemCreatedCampaign(t *testing.T) {
	metaBlob, err := json.Marshal(meta.CampaignResult{CampaignID: "c1", AdSetID: "s1", AccountID: "act_999"})
	if err != nil {
		t.Fatalf("marshal meta result: %v", err)
	}

	cases := []struct {
		platform string
		provider model.Provider
		// sysAccount is the account id the SYSTEM connection row carries, in the vocabulary
		// this platform's persisted blob records.
		sysAccount string
		blob       string
		toggle     func(t *testing.T, repo *scopedConnReader, campaign *model.Campaign)
	}{
		{
			platform: "meta", provider: model.ProviderMetaAds, sysAccount: "act_999",
			blob: string(metaBlob),
			toggle: func(t *testing.T, repo *scopedConnReader, c *model.Campaign) {
				d := NewMetaDispatcher(repo, identityEncryptor{})
				_ = d.ToggleStatus(context.Background(), "cncf", model.ProviderMetaAds, c, model.CampaignRunPaused)
			},
		},
		{
			platform: "googleads", provider: model.ProviderGoogleAds, sysAccount: "9999999999",
			blob: `{"customerId":"9999999999","adGroupId":"ag1","adId":"ad1"}`,
			toggle: func(t *testing.T, repo *scopedConnReader, c *model.Campaign) {
				d := NewGoogleAdsDispatcher(repo, identityEncryptor{})
				_ = d.ToggleStatus(context.Background(), "cncf", model.ProviderGoogleAds, c, model.CampaignRunPaused)
			},
		},
		{
			platform: "microsoft", provider: model.ProviderMicrosoftAds, sysAccount: "7654321",
			blob: `{"accountId":"7654321"}`,
			toggle: func(t *testing.T, repo *scopedConnReader, c *model.Campaign) {
				d := NewMicrosoftDispatcher(repo, identityEncryptor{})
				_ = d.ToggleStatus(context.Background(), "cncf", model.ProviderMicrosoftAds, c, model.CampaignRunPaused)
			},
		},
		{
			platform: "linkedin", provider: model.ProviderLinkedInAds, sysAccount: "987654321",
			blob: `{"accountId":"987654321"}`,
			toggle: func(t *testing.T, repo *scopedConnReader, c *model.Campaign) {
				d := NewLinkedInDispatcher(repo, identityEncryptor{})
				_ = d.ToggleStatus(context.Background(), "cncf", model.ProviderLinkedInAds, c, model.CampaignRunPaused)
			},
		},
		{
			platform: "twitter", provider: model.ProviderTwitterAds, sysAccount: "t_sys",
			blob: `{"AccountID":"t_sys"}`,
			toggle: func(t *testing.T, repo *scopedConnReader, c *model.Campaign) {
				d := NewTwitterDispatcher(repo, identityEncryptor{})
				_ = d.ToggleStatus(context.Background(), "cncf", model.ProviderTwitterAds, c, model.CampaignRunPaused)
			},
		},
		{
			platform: "reddit", provider: model.ProviderRedditAds, sysAccount: "t2_sys",
			blob: `{"accountId":"t2_sys"}`,
			toggle: func(t *testing.T, repo *scopedConnReader, c *model.Campaign) {
				d := NewRedditDispatcher(repo, identityEncryptor{})
				_ = d.ToggleStatus(context.Background(), "cncf", model.ProviderRedditAds, c, model.CampaignRunPaused)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.platform, func(t *testing.T) {
			// The flag is OFF deliberately: a campaign created during the cutover must stay
			// resolvable to its creating account once the flag is retired. With the flag on this
			// would also pass, so testing the off case pins the stronger property.
			t.Setenv(constants.EnvForceSystemAdsAccount, "false")

			projectConn := usableConn(`{"accessToken":"tok","AccessToken":"tok","refreshToken":"r","developerToken":"d","clientId":"i","clientSecret":"s"}`, "project-account")
			projectConn.Provider = tc.provider
			sysConn := usableConn(`{"accessToken":"tok","AccessToken":"tok","refreshToken":"r","developerToken":"d","clientId":"i","clientSecret":"s"}`, tc.sysAccount)
			sysConn.Provider = tc.provider
			repo := &scopedConnReader{rows: map[string]*model.Connection{
				"cncf":                projectConn,
				model.SystemProjectID: sysConn,
			}}

			campaign := &model.Campaign{
				PlatformCampaignID: "c1",
				Platform:           tc.provider,
				Status:             campaignStatusCreated,
				Result:             json.RawMessage(tc.blob),
			}
			tc.toggle(t, repo, campaign)

			if !slices.Contains(repo.gets, model.SystemProjectID) {
				t.Errorf("%s: pausing a campaign recorded as created on the SYSTEM account never "+
					"consulted the system scope (scopes asked: %v).\nIt authenticates as the project "+
					"instead, the provenance guard sees a different account, and the campaign cannot "+
					"be stopped.", tc.platform, repo.gets)
			}
		})
	}
}
