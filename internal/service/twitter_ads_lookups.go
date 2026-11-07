// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TwitterCustomAudienceReader is an OPTIONAL dispatcher capability: list the custom audiences on
// the X ad account a create for the project uses (#564 gap 3). Type-asserted like
// RedditCustomAudienceReader; a dispatcher without it yields ErrTargetingLookupUnsupported (400).
//
// A successful call MUST return a non-nil result with a non-nil Audiences slice. called reports
// whether the platform was actually contacted: false for the deployment gate and a local refusal
// before the request (no connection, an unusable one), so only real upstream calls are recorded.
type TwitterCustomAudienceReader interface {
	ListTwitterCustomAudiences(ctx context.Context, projectID string, platform model.Provider) (res *model.TwitterCustomAudiences, called bool, err error)
}

const opListTwitterCustomAudiences = "list_twitter_custom_audiences"

// twitterAdsCustomAudienceLookup reuses classifyDiscoveryError's connection-state arms under its
// own operation noun, as the Reddit lookup does.
var twitterAdsCustomAudienceLookup = accountDiscovery{
	provider:    model.ProviderTwitterAds,
	displayName: "twitter ads",
	notUsableRemedy: "check that it is active, that the stored credential is valid json with " +
		"consumerKey, consumerSecret, accessToken and accessTokenSecret set, and that an account_id is selected on the connection",
	operation: "custom audience lookup",
}

// ReadTwitterCustomAudiences lists the custom audiences on the X ad account a create for the
// project uses, through the platform's dispatcher. A pure read; nothing is persisted.
//
// Like ReadRedditCustomAudiences, it is NOT refused on a shared-tenancy platform: the answer
// holds every foundation's audiences on the shared LF X ad account, so the gateway admits only
// global_marketing_ops of the project to this route (the marketing-ops-api rule).
func (o *Orchestrator) ReadTwitterCustomAudiences(ctx context.Context, projectID string, platform model.Provider) (*model.TwitterCustomAudiences, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	r, ok := d.(TwitterCustomAudienceReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	res, called, err := r.ListTwitterCustomAudiences(callCtx, projectID, platform)
	if called {
		o.recordUpstream(ctx, platform, opListTwitterCustomAudiences, start, err)
	}
	if err != nil {
		return nil, err
	}
	if res == nil || res.Audiences == nil {
		return nil, fmt.Errorf("%s custom audience reader returned a nil result with no error", platform)
	}
	return res, nil
}

// ListTwitterAdsCustomAudiences lists the custom audiences on the X ad account a create for this
// project uses, for the customAudienceIds / excludedCustomAudienceIds picker (#564 gap 3).
func (s *ConnectionService) ListTwitterAdsCustomAudiences(ctx context.Context, p *conn.ListTwitterAdsCustomAudiencesPayload) (*conn.TwitterAdsCustomAudiences, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(twitterAdsCustomAudienceLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadTwitterCustomAudiences(ctx, p.ProjectID, model.ProviderTwitterAds)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, twitterAdsCustomAudienceLookup, rerr)
	}
	rows := append([]model.TwitterCustomAudience(nil), res.Audiences...)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := strings.ToLower(rows[i].Name), strings.ToLower(rows[j].Name)
		if a != b {
			return a < b
		}
		return rows[i].ID < rows[j].ID
	})
	out := make([]*conn.TwitterAdsCustomAudience, 0, len(rows))
	for _, a := range rows {
		reasons := append([]string{}, a.ReasonsNotTargetable...)
		out = append(out, &conn.TwitterAdsCustomAudience{
			ID: a.ID, Name: a.Name, AudienceType: a.AudienceType, Targetable: a.Targetable,
			AudienceSize: a.AudienceSize, ReasonsNotTargetable: reasons,
		})
	}
	return &conn.TwitterAdsCustomAudiences{
		AccountID:     res.AccountID,
		Audiences:     out,
		AudienceCount: len(out),
		Truncated:     res.Truncated,
	}, nil
}
