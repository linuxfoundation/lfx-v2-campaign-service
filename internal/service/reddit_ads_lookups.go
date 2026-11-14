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

// RedditCustomAudienceReader is an OPTIONAL dispatcher capability: list the custom audiences on
// the Reddit ad account a create for the project uses (#549). Type-asserted like AudienceReader;
// a dispatcher without it yields ErrTargetingLookupUnsupported (400).
//
// A successful call MUST return a non-nil result with a non-nil Audiences slice. called reports
// whether the platform was actually contacted: false for the deployment gate and a local refusal
// before the request (no connection, an unusable one), so only real upstream calls are recorded.
type RedditCustomAudienceReader interface {
	ListRedditCustomAudiences(ctx context.Context, projectID string, platform model.Provider) (res *model.RedditCustomAudiences, called bool, err error)
}

const opListRedditCustomAudiences = "list_custom_audiences"

// redditAdsCustomAudienceLookup reuses classifyDiscoveryError's connection-state arms under its
// own operation noun, as the Google lookups do.
var redditAdsCustomAudienceLookup = accountDiscovery{
	provider:    model.ProviderRedditAds,
	displayName: "reddit ads",
	notUsableRemedy: "check that it is active, that the stored credential is valid json with " +
		"client_id, client_secret and refresh_token set, and that an account_id is selected on the connection",
	operation: "custom audience lookup",
}

// ReadRedditCustomAudiences lists the custom audiences on the Reddit ad account a create for the
// project uses, through the platform's dispatcher. A pure read; nothing is persisted.
//
// Like ReadAudiences, it is NOT refused on a shared-tenancy platform: the answer holds every
// foundation's audiences on the shared LF Reddit ad account, so the gateway admits only
// global_marketing_ops of the project to this route (the marketing-ops-api rule).
func (o *Orchestrator) ReadRedditCustomAudiences(ctx context.Context, projectID string, platform model.Provider) (*model.RedditCustomAudiences, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	r, ok := d.(RedditCustomAudienceReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	res, called, err := r.ListRedditCustomAudiences(callCtx, projectID, platform)
	if called {
		o.recordUpstream(ctx, platform, opListRedditCustomAudiences, start, err)
	}
	if err != nil {
		return nil, err
	}
	if res == nil || res.Audiences == nil {
		return nil, fmt.Errorf("%s custom audience reader returned a nil result with no error", platform)
	}
	return res, nil
}

// ListRedditAdsCustomAudiences lists the custom audiences on the Reddit ad account a create for
// this project uses, for the customAudienceIds / excludedCustomAudienceIds picker (#549).
func (s *ConnectionService) ListRedditAdsCustomAudiences(ctx context.Context, p *conn.ListRedditAdsCustomAudiencesPayload) (*conn.RedditAdsCustomAudiences, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(redditAdsCustomAudienceLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadRedditCustomAudiences(ctx, p.ProjectID, model.ProviderRedditAds)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, redditAdsCustomAudienceLookup, rerr)
	}
	rows := append([]model.RedditCustomAudience(nil), res.Audiences...)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := strings.ToLower(rows[i].Name), strings.ToLower(rows[j].Name)
		if a != b {
			return a < b
		}
		return rows[i].ID < rows[j].ID
	})
	out := make([]*conn.RedditAdsCustomAudience, 0, len(rows))
	for _, a := range rows {
		out = append(out, &conn.RedditAdsCustomAudience{
			ID: a.ID, Name: a.Name, Type: a.Type, Status: a.Status, SizeLower: a.SizeLower, SizeUpper: a.SizeUpper,
		})
	}
	return &conn.RedditAdsCustomAudiences{
		AccountID:     res.AccountID,
		Audiences:     out,
		AudienceCount: len(out),
		Truncated:     res.Truncated,
	}, nil
}

// RedditPixelReader is an OPTIONAL dispatcher capability: list the conversion pixels on the
// Reddit ad account a create for the project uses (#549 gap 10). Type-asserted like
// RedditCustomAudienceReader; a dispatcher without it yields ErrTargetingLookupUnsupported (400).
//
// A successful call MUST return a non-nil result with a non-nil Pixels slice. called reports
// whether the platform was actually contacted, so only real upstream calls are recorded.
type RedditPixelReader interface {
	ListRedditPixels(ctx context.Context, projectID string, platform model.Provider) (res *model.RedditPixels, called bool, err error)
}

const opListRedditPixels = "list_pixels"

// redditAdsPixelLookup reuses classifyDiscoveryError's connection-state arms under its own
// operation noun, like redditAdsCustomAudienceLookup.
var redditAdsPixelLookup = accountDiscovery{
	provider:        model.ProviderRedditAds,
	displayName:     "reddit ads",
	notUsableRemedy: redditAdsCustomAudienceLookup.notUsableRemedy,
	operation:       "pixel lookup",
}

// ReadRedditPixels lists the pixels on the Reddit ad account a create for the project uses,
// through the platform's dispatcher. A pure read; nothing is persisted. Like
// ReadRedditCustomAudiences it is not refused on the shared account: the gateway admits only
// global_marketing_ops of the project to this route (the marketing-ops-api rule).
func (o *Orchestrator) ReadRedditPixels(ctx context.Context, projectID string, platform model.Provider) (*model.RedditPixels, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	r, ok := d.(RedditPixelReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	res, called, err := r.ListRedditPixels(callCtx, projectID, platform)
	if called {
		o.recordUpstream(ctx, platform, opListRedditPixels, start, err)
	}
	if err != nil {
		return nil, err
	}
	if res == nil || res.Pixels == nil {
		return nil, fmt.Errorf("%s pixel reader returned a nil result with no error", platform)
	}
	return res, nil
}

// ListRedditAdsPixels lists the conversion pixels on the Reddit ad account a create for this
// project uses, for the conversionPixelId picker (#549 gap 10).
func (s *ConnectionService) ListRedditAdsPixels(ctx context.Context, p *conn.ListRedditAdsPixelsPayload) (*conn.RedditAdsPixels, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(redditAdsPixelLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadRedditPixels(ctx, p.ProjectID, model.ProviderRedditAds)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, redditAdsPixelLookup, rerr)
	}
	rows := append([]model.RedditPixel(nil), res.Pixels...)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := strings.ToLower(rows[i].Name), strings.ToLower(rows[j].Name)
		if a != b {
			return a < b
		}
		return rows[i].ID < rows[j].ID
	})
	out := make([]*conn.RedditAdsPixel, 0, len(rows))
	for _, px := range rows {
		row := &conn.RedditAdsPixel{ID: px.ID, Name: px.Name}
		if px.BusinessID != "" {
			b := px.BusinessID
			row.BusinessID = &b
		}
		out = append(out, row)
	}
	return &conn.RedditAdsPixels{
		AccountID:  res.AccountID,
		Pixels:     out,
		PixelCount: len(out),
		Truncated:  res.Truncated,
	}, nil
}
