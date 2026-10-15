// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// This file pins resolver THREADING (issue #174): for every non-Google dispatcher and every
// entry point, WHICH credential entry point the operation authenticates through.
//
// The resolvers' behaviour is pinned elsewhere (creds_test.go, creds_force_system_*_test.go)
// by calling credsSource methods directly. What nothing pinned was the caller's choice:
// hardcoding d.creds.resolve into ToggleStatus — the shape a merge or refactor produces —
// compiles and kept the whole suite green, and it makes every pre-cutover campaign
// unpausable while it keeps spending.
//
// The resolvers are credsSource methods, not injectable values, so the fake here sits one
// layer down: a recording connReader (scopedConnReader.gets) whose call sequence is a
// fingerprint unique to each resolver. Two fixtures produce it, both with the forced-system
// flag ON and every row present but credential-less, so resolution fails before any client
// is built and no test here can reach the network:
//
//	fixture A: the project and the system scope both have a row
//	fixture B: only the system scope has a row
//
//	resolver                          A (scopes asked)      B (scopes asked)
//	resolve (creation, forced)        [system]              [system]
//	existingResolver(<recorded acct>) [project, system…]    [project, system…]
//	existingResolver("") / unforced   [project]             [project, system…]
//	resolveOwned / ...ForAdoption     [project]             [project]
//
// classifyResolver maps that pair back to the resolver's identity, so each table row states
// the identity it expects and a swap in either direction fails with the identity it got.
// resolveOwned and resolveOwnedForAdoption share a fingerprint: the latter only translates
// resolveOwned's ErrNotFound, and adoption's own tests pin that translation.

// Resolver identities, as classifyResolver names them.
const (
	// idCreation is credsSource.resolve with the forced-system flag honoured: creation and
	// account discovery.
	idCreation = "resolve (creation)"
	// idExisting is existingResolver / resolveExisting bound to the account the campaign
	// RECORDS having been created under: every operation on an existing campaign.
	idExisting = "resolveExisting (recorded creation account)"
	// idFallback is project-then-system resolution with nothing recorded to steer it:
	// existingResolver("") for a paid provider, and resolve for HubSpot, which the
	// forced-system flag never governs.
	idFallback = "project-then-system fallback (no recorded account)"
	// idOwned is resolveOwned / resolveOwnedForAdoption: the project's own connection only.
	idOwned = "resolveOwned (project only)"
	// idUnresolved means the entry point returned before resolving anything — a fixture that
	// failed a precondition, not a resolver choice.
	idUnresolved = "never resolved"
)

// resolverIdentityProject is the project every case runs as. The rows it reads are keyed by it.
const resolverIdentityProject = "cncf"

// classifyResolver names the resolver an entry point used from the scopes it asked for under
// fixtures A and B (see the table at the top of this file).
func classifyResolver(a, b []string) string {
	switch {
	case len(a) == 0:
		return idUnresolved
	case a[0] == model.SystemProjectID:
		return idCreation
	case slices.Contains(a, model.SystemProjectID):
		return idExisting
	case slices.Contains(b, model.SystemProjectID):
		return idFallback
	default:
		return idOwned
	}
}

// resolverEntryPoint is one row of a dispatcher's sweep: an entry point, driven against the
// given repo, and the resolver it must use.
type resolverEntryPoint struct {
	name string
	want string
	call func(repo connReader, campaign *model.Campaign)
}

// sweepResolverIdentity runs every entry point under both fixtures and asserts the identity.
// sysAccount is the account id the SYSTEM row carries and blob is the campaign's persisted
// result recording that same account as the creating one — so an existing-campaign resolver
// bound to the recorded account is steered to the system scope, and one that is not is not.
func sweepResolverIdentity(t *testing.T, provider model.Provider, sysAccount, blob string, cases []resolverEntryPoint) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ON, so creation's forced resolution is distinguishable from the project-first
			// resolution every other entry point does. This is also the deployment state in
			// which the wrong choice strands a live campaign.
			t.Setenv(constants.EnvForceSystemAdsAccount, "true")
			// Feature gates that would otherwise refuse before resolving anything.
			t.Setenv(constants.EnvRedditMetricsEnabled, "true")
			t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
			t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
			t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")

			run := func(withProjectRow bool) []string {
				// Credential-less rows: resolveConn refuses them before decrypting, so
				// resolution fails at the scope it chose and nothing downstream runs.
				rows := map[string]*model.Connection{model.SystemProjectID: credlessConn(provider, sysAccount)}
				if withProjectRow {
					rows[resolverIdentityProject] = credlessConn(provider, "project-account")
				}
				repo := &scopedConnReader{rows: rows}
				campaign := &model.Campaign{
					ID:                 "camp-174",
					PlatformCampaignID: "1001",
					Platform:           provider,
					Status:             campaignStatusCreated,
					Result:             json.RawMessage(blob),
				}
				tc.call(repo, campaign)
				return repo.gets
			}
			a, b := run(true), run(false)
			if got := classifyResolver(a, b); got != tc.want {
				t.Errorf("%s %s authenticates through %q, want %q (scopes asked: with a project row %v, without %v).\n"+
					"The resolver decides which account the operation authenticates as. An existing-campaign operation "+
					"on the creation resolver cannot address a campaign created before the forced-system cutover, so it "+
					"cannot be paused while it keeps spending; a monitor, probe or adoption read that may fall back "+
					"answers from an account the project does not own.",
					provider, tc.name, got, tc.want, a, b)
			}
		})
	}
}

// credlessConn is a present, active row with no stored credentials: resolveConn refuses it
// before the cache or the decryptor is consulted.
func credlessConn(provider model.Provider, accountID string) *model.Connection {
	c := usableConn("", accountID)
	c.ID = "conn-174-" + string(provider) + "-" + accountID
	c.Provider = provider
	return c
}

// Shared inputs, each valid enough to pass the entry points' checks that run before resolution.
var (
	riCtx   = context.Background()
	riScope = func(blob string) []model.ProjectCampaignScope {
		return []model.ProjectCampaignScope{{PlatformCampaignID: "1001", Result: json.RawMessage(blob)}}
	}
	riBid    = model.BidChange{Amount: 1.5, Type: model.BidTypeCPC}
	riBudget = model.BudgetChange{Amount: 50, Type: model.BudgetDaily}
	riNegKws = []model.NegativeKeyword{{Text: "free", MatchType: "Exact"}}
)

func TestResolverIdentity_Meta(t *testing.T) {
	const acct = "act_999"
	raw, err := json.Marshal(meta.CampaignResult{CampaignID: "1001", AdSetID: "2002", AccountID: acct})
	if err != nil {
		t.Fatalf("marshal meta result: %v", err)
	}
	blob := string(raw)
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"]}}`)
	p := model.ProviderMetaAds
	d := func(r connReader) *MetaDispatcher { return NewMetaDispatcher(r, identityEncryptor{}) }

	sweepResolverIdentity(t, p, acct, blob, []resolverEntryPoint{
		{"Dispatch", idCreation, func(r connReader, _ *model.Campaign) { _, _ = d(r).Dispatch(riCtx, testBrief(), p, cfg) }},
		{"ListAccounts", idCreation, func(r connReader, _ *model.Campaign) { _, _ = d(r).ListAccounts(riCtx, resolverIdentityProject, p) }},
		{"ToggleStatus", idExisting, func(r connReader, c *model.Campaign) {
			_ = d(r).ToggleStatus(riCtx, resolverIdentityProject, p, c, model.CampaignRunPaused)
		}},
		{"ReadMetrics", idExisting, func(r connReader, c *model.Campaign) {
			_, _ = d(r).ReadMetrics(riCtx, resolverIdentityProject, p, c, model.MetricsWindowLast7Days)
		}},
		{"WriteBudget", idExisting, func(r connReader, c *model.Campaign) {
			_ = d(r).WriteBudget(riCtx, resolverIdentityProject, p, c, riBudget)
		}},
		{"WriteBid", idExisting, func(r connReader, c *model.Campaign) { _ = d(r).WriteBid(riCtx, resolverIdentityProject, p, c, riBid) }},
		{"ReadSettings", idExisting, func(r connReader, c *model.Campaign) { _, _ = d(r).ReadSettings(riCtx, resolverIdentityProject, p, c) }},
		{"ReadMetaAdSets", idExisting, func(r connReader, c *model.Campaign) {
			_, _ = d(r).ReadMetaAdSets(riCtx, resolverIdentityProject, p, c, model.MetricsWindowLast7Days)
		}},
		{"ToggleMetaAdSetStatus", idExisting, func(r connReader, c *model.Campaign) {
			_, _ = d(r).ToggleMetaAdSetStatus(riCtx, resolverIdentityProject, p, c, "2002", "PAUSED", nil)
		}},
		// Not campaign-scoped: it reads across the project's campaigns, so there is no single
		// recorded account to bind, and it deliberately resolves with none.
		{"ReadMetaAudienceInsights", idFallback, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ReadMetaAudienceInsights(riCtx, resolverIdentityProject, p, model.MetricsWindowLast7Days, riScope(blob))
		}},
		{"LookupCampaign (adopt)", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).LookupCampaign(riCtx, resolverIdentityProject, p, "1001")
		}},
		{"ProbeConnection", idOwned, func(r connReader, _ *model.Campaign) { _ = d(r).ProbeConnection(riCtx, resolverIdentityProject, p) }},
		{"ReadAccountMonitor", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ReadAccountMonitor(riCtx, resolverIdentityProject, p, acct, 7)
		}},
		{"ListAccountCampaignMetrics", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ListAccountCampaignMetrics(riCtx, resolverIdentityProject, p, acct, 7)
		}},
	})
}

func TestResolverIdentity_LinkedIn(t *testing.T) {
	const acct = "987654321"
	blob := `{"accountId":"` + acct + `","campaignId":"1001","campaignGroupId":"3003","creativeIds":["4004"]}`
	cfg := json.RawMessage(`{"linkedInConfig":{"adAccountId":"` + acct + `","targetingProfile":"x","targetingProfiles":[{"id":"x","label":"X"}],"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]}}`)
	p := model.ProviderLinkedInAds
	d := func(r connReader) *LinkedInDispatcher { return NewLinkedInDispatcher(r, identityEncryptor{}) }

	sweepResolverIdentity(t, p, acct, blob, []resolverEntryPoint{
		{"Dispatch", idCreation, func(r connReader, _ *model.Campaign) { _, _ = d(r).Dispatch(riCtx, testBrief(), p, cfg) }},
		{"ListAccounts", idCreation, func(r connReader, _ *model.Campaign) { _, _ = d(r).ListAccounts(riCtx, resolverIdentityProject, p) }},
		{"ToggleStatus", idExisting, func(r connReader, c *model.Campaign) {
			_ = d(r).ToggleStatus(riCtx, resolverIdentityProject, p, c, model.CampaignRunPaused)
		}},
		{"ReadMetrics", idExisting, func(r connReader, c *model.Campaign) {
			_, _ = d(r).ReadMetrics(riCtx, resolverIdentityProject, p, c, model.MetricsWindowLast7Days)
		}},
		{"WriteBudget", idExisting, func(r connReader, c *model.Campaign) {
			_ = d(r).WriteBudget(riCtx, resolverIdentityProject, p, c, riBudget)
		}},
		{"WriteBid", idExisting, func(r connReader, c *model.Campaign) { _ = d(r).WriteBid(riCtx, resolverIdentityProject, p, c, riBid) }},
		{"ReadSettings", idExisting, func(r connReader, c *model.Campaign) { _, _ = d(r).ReadSettings(riCtx, resolverIdentityProject, p, c) }},
		{"ReadLinkedInAudienceInsights", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ReadLinkedInAudienceInsights(riCtx, resolverIdentityProject, p, model.MetricsWindowLast7Days, riScope(blob))
		}},
		{"LookupCampaign (adopt)", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).LookupCampaign(riCtx, resolverIdentityProject, p, "1001")
		}},
		{"VerifyAccountOrg", idOwned, func(r connReader, _ *model.Campaign) { _ = d(r).VerifyAccountOrg(riCtx, resolverIdentityProject, p) }},
		{"ReadAccountMonitor", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ReadAccountMonitor(riCtx, resolverIdentityProject, p, acct, 7)
		}},
		{"ListAccountCampaignMetrics", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ListAccountCampaignMetrics(riCtx, resolverIdentityProject, p, acct, 7)
		}},
	})
}

func TestResolverIdentity_Reddit(t *testing.T) {
	const acct = "t2_sys"
	blob := `{"accountId":"` + acct + `","campaignId":"1001","adGroupId":"2002","adId":"3003"}`
	cfg := json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"]}}`)
	p := model.ProviderRedditAds
	d := func(r connReader) *RedditDispatcher { return NewRedditDispatcher(r, identityEncryptor{}) }

	sweepResolverIdentity(t, p, acct, blob, []resolverEntryPoint{
		{"Dispatch", idCreation, func(r connReader, _ *model.Campaign) { _, _ = d(r).Dispatch(riCtx, testBrief(), p, cfg) }},
		{"ListAccounts", idCreation, func(r connReader, _ *model.Campaign) { _, _ = d(r).ListAccounts(riCtx, resolverIdentityProject, p) }},
		{"ToggleStatus", idExisting, func(r connReader, c *model.Campaign) {
			_ = d(r).ToggleStatus(riCtx, resolverIdentityProject, p, c, model.CampaignRunPaused)
		}},
		{"ReadMetrics", idExisting, func(r connReader, c *model.Campaign) {
			_, _ = d(r).ReadMetrics(riCtx, resolverIdentityProject, p, c, model.MetricsWindowLast7Days)
		}},
		{"WriteBudget", idExisting, func(r connReader, c *model.Campaign) {
			_ = d(r).WriteBudget(riCtx, resolverIdentityProject, p, c, riBudget)
		}},
		{"WriteBid", idExisting, func(r connReader, c *model.Campaign) { _ = d(r).WriteBid(riCtx, resolverIdentityProject, p, c, riBid) }},
		{"ReadSettings", idExisting, func(r connReader, c *model.Campaign) { _, _ = d(r).ReadSettings(riCtx, resolverIdentityProject, p, c) }},
		{"ReadKeywordTargeting", idExisting, func(r connReader, c *model.Campaign) {
			_, _ = d(r).ReadKeywordTargeting(riCtx, resolverIdentityProject, p, c)
		}},
		{"RemoveKeywordTargeting", idExisting, func(r connReader, c *model.Campaign) {
			// Reddit removes by keyword text against the revision get-keyword-targeting returned.
			removals := []model.KeywordTargetingRemoval{{Keyword: "kubernetes"}}
			_, _ = d(r).RemoveKeywordTargeting(riCtx, resolverIdentityProject, p, c, removals, "rev-1")
		}},
		{"AddNegativeKeywords", idExisting, func(r connReader, c *model.Campaign) {
			// Reddit takes broad-match negatives only, and requires the targeting revision.
			kws := []model.NegativeKeyword{{Text: "free", MatchType: negativeMatchBroad}}
			_, _ = d(r).AddNegativeKeywords(riCtx, resolverIdentityProject, p, c, kws, "rev-1")
		}},
		{"ReadRedditAudienceInsights", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ReadRedditAudienceInsights(riCtx, resolverIdentityProject, p, model.MetricsWindowLast7Days, riScope(blob))
		}},
		{"ReadRedditKeywordPerformance", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ReadRedditKeywordPerformance(riCtx, resolverIdentityProject, p, model.MetricsWindowLast7Days, riScope(blob))
		}},
		{"LookupCampaign (adopt)", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).LookupCampaign(riCtx, resolverIdentityProject, p, "1001")
		}},
		{"ProbeConnection", idOwned, func(r connReader, _ *model.Campaign) { _ = d(r).ProbeConnection(riCtx, resolverIdentityProject, p) }},
		{"ReadAccountMonitor", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ReadAccountMonitor(riCtx, resolverIdentityProject, p, acct, 7)
		}},
		{"ListAccountCampaignMetrics", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ListAccountCampaignMetrics(riCtx, resolverIdentityProject, p, acct, 7)
		}},
	})
}

func TestResolverIdentity_Twitter(t *testing.T) {
	const acct = "18ce54d4x5t"
	// X/Twitter persists an UNTAGGED marshal, so the keys are the Go field names.
	blob := `{"AccountID":"` + acct + `","CampaignID":"1001","LineItemID":"2002","PromotedTweetID":"3003"}`
	cfg := json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890"}}`)
	p := model.ProviderTwitterAds
	d := func(r connReader) *TwitterDispatcher { return NewTwitterDispatcher(r, identityEncryptor{}) }

	sweepResolverIdentity(t, p, acct, blob, []resolverEntryPoint{
		{"Dispatch", idCreation, func(r connReader, _ *model.Campaign) { _, _ = d(r).Dispatch(riCtx, testBrief(), p, cfg) }},
		{"ListAccounts", idCreation, func(r connReader, _ *model.Campaign) { _, _ = d(r).ListAccounts(riCtx, resolverIdentityProject, p) }},
		{"ToggleStatus", idExisting, func(r connReader, c *model.Campaign) {
			_ = d(r).ToggleStatus(riCtx, resolverIdentityProject, p, c, model.CampaignRunPaused)
		}},
		{"ReadMetrics", idExisting, func(r connReader, c *model.Campaign) {
			_, _ = d(r).ReadMetrics(riCtx, resolverIdentityProject, p, c, model.MetricsWindowLast7Days)
		}},
		{"WriteBudget", idExisting, func(r connReader, c *model.Campaign) {
			_ = d(r).WriteBudget(riCtx, resolverIdentityProject, p, c, riBudget)
		}},
		{"WriteBid", idExisting, func(r connReader, c *model.Campaign) { _ = d(r).WriteBid(riCtx, resolverIdentityProject, p, c, riBid) }},
		{"ReadSettings", idExisting, func(r connReader, c *model.Campaign) { _, _ = d(r).ReadSettings(riCtx, resolverIdentityProject, p, c) }},
		{"ReadKeywordTargeting", idExisting, func(r connReader, c *model.Campaign) {
			_, _ = d(r).ReadKeywordTargeting(riCtx, resolverIdentityProject, p, c)
		}},
		{"RemoveKeywordTargeting", idExisting, func(r connReader, c *model.Campaign) {
			// X removes by criterion id, and has no revision.
			removals := []model.KeywordTargetingRemoval{{CriterionID: "k1"}}
			_, _ = d(r).RemoveKeywordTargeting(riCtx, resolverIdentityProject, p, c, removals, "")
		}},
		{"AddNegativeKeywords", idExisting, func(r connReader, c *model.Campaign) {
			_, _ = d(r).AddNegativeKeywords(riCtx, resolverIdentityProject, p, c, riNegKws, "")
		}},
		// Not campaign-scoped (reads across the project's campaigns), like Meta's.
		{"ReadTwitterAudienceInsights", idFallback, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ReadTwitterAudienceInsights(riCtx, resolverIdentityProject, p, model.MetricsWindowLast7Days, riScope(blob))
		}},
		{"LookupCampaign (adopt)", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).LookupCampaign(riCtx, resolverIdentityProject, p, "1001")
		}},
		{"ProbeConnection", idOwned, func(r connReader, _ *model.Campaign) { _ = d(r).ProbeConnection(riCtx, resolverIdentityProject, p) }},
		{"ListAccountCampaigns", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ListAccountCampaigns(riCtx, resolverIdentityProject, p, acct)
		}},
		{"ReadAccountCurrency", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ReadAccountCurrency(riCtx, resolverIdentityProject, p, acct)
		}},
		{"SubmitAccountReport", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).SubmitAccountReport(riCtx, resolverIdentityProject, p, acct, 7)
		}},
		{"CheckAccountReport", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).CheckAccountReport(riCtx, resolverIdentityProject, p, acct, "job-1")
		}},
	})
}

func TestResolverIdentity_Microsoft(t *testing.T) {
	const acct = "7654321"
	blob := `{"accountId":"` + acct + `","campaignId":"1001","adGroupId":"2002","adId":"3003"}`
	cfg := json.RawMessage(`{"microsoftConfig":{"budget":50,"geoTargets":["US"]}}`)
	p := model.ProviderMicrosoftAds
	d := func(r connReader) *MicrosoftDispatcher { return NewMicrosoftDispatcher(r, identityEncryptor{}) }
	kwActions := []model.KeywordAction{{AdGroupID: "2002", CriterionID: "5005", Action: model.KeywordActionPause}}

	sweepResolverIdentity(t, p, acct, blob, []resolverEntryPoint{
		{"Dispatch", idCreation, func(r connReader, _ *model.Campaign) { _, _ = d(r).Dispatch(riCtx, testBrief(), p, cfg) }},
		{"ListAccounts", idCreation, func(r connReader, _ *model.Campaign) { _, _ = d(r).ListAccounts(riCtx, resolverIdentityProject, p) }},
		{"ToggleStatus", idExisting, func(r connReader, c *model.Campaign) {
			_ = d(r).ToggleStatus(riCtx, resolverIdentityProject, p, c, model.CampaignRunPaused)
		}},
		{"ReadMetrics", idExisting, func(r connReader, c *model.Campaign) {
			_, _ = d(r).ReadMetrics(riCtx, resolverIdentityProject, p, c, model.MetricsWindowLast7Days)
		}},
		{"WriteBudget", idExisting, func(r connReader, c *model.Campaign) {
			_ = d(r).WriteBudget(riCtx, resolverIdentityProject, p, c, riBudget)
		}},
		{"WriteBid", idExisting, func(r connReader, c *model.Campaign) { _ = d(r).WriteBid(riCtx, resolverIdentityProject, p, c, riBid) }},
		{"ReadSettings", idExisting, func(r connReader, c *model.Campaign) { _, _ = d(r).ReadSettings(riCtx, resolverIdentityProject, p, c) }},
		{"ApplyKeywordActions", idExisting, func(r connReader, c *model.Campaign) {
			_, _ = d(r).ApplyKeywordActions(riCtx, resolverIdentityProject, p, c, kwActions)
		}},
		{"AddNegativeKeywords", idExisting, func(r connReader, c *model.Campaign) {
			_, _ = d(r).AddNegativeKeywords(riCtx, resolverIdentityProject, p, c, riNegKws, "")
		}},
		{"LookupCampaign (adopt)", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).LookupCampaign(riCtx, resolverIdentityProject, p, "1001")
		}},
		{"ProbeConnection", idOwned, func(r connReader, _ *model.Campaign) { _ = d(r).ProbeConnection(riCtx, resolverIdentityProject, p) }},
		{"ListAccountCampaigns", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ListAccountCampaigns(riCtx, resolverIdentityProject, p, acct)
		}},
		{"ReadAccountCurrency", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ReadAccountCurrency(riCtx, resolverIdentityProject, p, acct)
		}},
		{"SubmitAccountReport", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).SubmitAccountReport(riCtx, resolverIdentityProject, p, acct, 7)
		}},
		{"CheckAccountReport", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).CheckAccountReport(riCtx, resolverIdentityProject, p, acct, "rep-1")
		}},
		{"AudienceReportAccount", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).AudienceReportAccount(riCtx, resolverIdentityProject, p, model.MetricsWindowLast7Days, riScope(blob))
		}},
		{"SubmitAudienceReport", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).SubmitAudienceReport(riCtx, resolverIdentityProject, p, acct, model.MetricsWindowLast7Days, riScope(blob))
		}},
		{"CheckAudienceReport", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).CheckAudienceReport(riCtx, resolverIdentityProject, p, acct, "rep-1")
		}},
		{"KeywordReportAccount", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).KeywordReportAccount(riCtx, resolverIdentityProject, p, model.MetricsWindowLast7Days, riScope(blob))
		}},
		{"SubmitKeywordReport", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).SubmitKeywordReport(riCtx, resolverIdentityProject, p, acct, model.MetricsWindowLast7Days, riScope(blob))
		}},
		{"CheckKeywordReport", idOwned, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).CheckKeywordReport(riCtx, resolverIdentityProject, p, acct, "rep-1")
		}},
	})
}

// HubSpot is not a paid-ads provider, so the forced-system flag never redirects it: every
// entry point but the probe resolves project-then-LF-portal (resolve, which for HubSpot IS the
// fallback). The rows pin that it stays that way with the flag ON, and that the probe — which
// must report on the project's own connection — never falls back.
func TestResolverIdentity_HubSpot(t *testing.T) {
	const portal = "4455"
	blob := `{"portalId":"` + portal + `"}`
	cfg := json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`)
	p := model.ProviderHubSpot
	d := func(r connReader) *HubSpotDispatcher { return NewHubSpotDispatcher(r, identityEncryptor{}, nil) }

	sweepResolverIdentity(t, p, portal, blob, []resolverEntryPoint{
		{"Dispatch", idFallback, func(r connReader, _ *model.Campaign) { _, _ = d(r).Dispatch(riCtx, testBrief(), p, cfg) }},
		{"ReadMetrics", idFallback, func(r connReader, c *model.Campaign) {
			_, _ = d(r).ReadMetrics(riCtx, resolverIdentityProject, p, c, model.MetricsWindowLast7Days)
		}},
		{"PreflightCreate", idFallback, func(r connReader, _ *model.Campaign) {
			_ = d(r).PreflightCreate(riCtx, resolverIdentityProject, p, json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555","abTestEnabled":true}}`))
		}},
		{"SearchEmails", idFallback, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).SearchEmails(riCtx, resolverIdentityProject, p, "kubecon")
		}},
		{"SearchCampaigns", idFallback, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).SearchCampaigns(riCtx, resolverIdentityProject, p, "kubecon")
		}},
		{"CreateCampaign", idFallback, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).CreateCampaign(riCtx, resolverIdentityProject, p, "KubeCon 2099")
		}},
		{"ResolveEmailClient", idFallback, func(r connReader, _ *model.Campaign) {
			_, _ = d(r).ResolveEmailClient(riCtx, resolverIdentityProject)
		}},
		{"ReadEmailMonitor", idFallback, func(r connReader, c *model.Campaign) {
			_, _ = d(r).ReadEmailMonitor(riCtx, resolverIdentityProject, p, []*model.Campaign{c}, 7)
		}},
		{"ProbeConnection", idOwned, func(r connReader, _ *model.Campaign) { _ = d(r).ProbeConnection(riCtx, resolverIdentityProject, p) }},
	})
}

// TestClassifyResolver_FingerprintsAreDistinct guards the oracle itself: each resolver, driven
// directly under the two fixtures, must classify as its own identity. If a change to
// credsSource made two of them produce the same fingerprint, every sweep above would go blind
// to a swap between them while still passing — this fails instead.
func TestClassifyResolver_FingerprintsAreDistinct(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	const acct = "act_999"
	p := model.ProviderMetaAds

	for _, tc := range []struct {
		name string
		want string
		pick func(s *credsSource) credsResolver
	}{
		{"resolve", idCreation, func(s *credsSource) credsResolver { return s.resolve }},
		{"existingResolver(recorded)", idExisting, func(s *credsSource) credsResolver { return s.existingResolver(acct) }},
		{"existingResolver(\"\")", idFallback, func(s *credsSource) credsResolver { return s.existingResolver("") }},
		{"resolveOwned", idOwned, func(s *credsSource) credsResolver { return s.resolveOwned }},
		{"resolveOwnedForAdoption", idOwned, func(s *credsSource) credsResolver { return s.resolveOwnedForAdoption }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(withProjectRow bool) []string {
				rows := map[string]*model.Connection{model.SystemProjectID: credlessConn(p, acct)}
				if withProjectRow {
					rows[resolverIdentityProject] = credlessConn(p, "project-account")
				}
				repo := &scopedConnReader{rows: rows}
				if _, err := tc.pick(newCredsSource(repo, identityEncryptor{}))(riCtx, resolverIdentityProject, p); err == nil {
					t.Fatal("a credential-less row resolved; the fixture no longer stops resolution before a client is built")
				}
				return repo.gets
			}
			a, b := run(true), run(false)
			if got := classifyResolver(a, b); got != tc.want {
				t.Errorf("%s classifies as %q, want %q (scopes: %v / %v)", tc.name, got, tc.want,
					strings.Join(a, ","), strings.Join(b, ","))
			}
		})
	}
}
