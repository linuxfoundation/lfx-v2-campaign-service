// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// MetaTargetingSearcher is an OPTIONAL dispatcher capability: search Meta's detailed-targeting
// taxonomy by kind and name (#550). Type-asserted like MetaCustomAudienceReader; a dispatcher
// without it yields ErrTargetingLookupUnsupported (400) and Meta is never contacted.
//
// A successful call MUST return a NON-NIL slice. called reports whether Meta was actually
// contacted: false for a cache hit and for a local refusal before any request.
type MetaTargetingSearcher interface {
	SearchMetaTargeting(ctx context.Context, projectID string, platform model.Provider, q model.MetaTargetingQuery) (rows []model.MetaTargetingOption, called bool, err error)
}

const opSearchMetaTargeting = "search_meta_targeting"

// metaTargetingSearchCallTimeout bounds one search. Short: the read is one request a picker makes
// while the operator types, so a slow Meta is better answered 503 now than after they typed on.
const metaTargetingSearchCallTimeout = 10 * time.Second

// ReadMetaTargeting runs a Meta targeting search through the platform's dispatcher. A pure read;
// nothing is persisted.
func (o *Orchestrator) ReadMetaTargeting(ctx context.Context, projectID string, platform model.Provider, q model.MetaTargetingQuery) ([]model.MetaTargetingOption, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	s, ok := d.(MetaTargetingSearcher)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, metaTargetingSearchCallTimeout)
	defer cancel()
	start := time.Now()
	rows, called, err := s.SearchMetaTargeting(callCtx, projectID, platform, q)
	// Recorded only when Meta was reached: a cache hit or a local refusal sent nothing.
	if called {
		o.recordUpstream(ctx, platform, opSearchMetaTargeting, start, err)
	}
	if err != nil {
		return nil, err
	}
	if rows == nil {
		return nil, fmt.Errorf("%s targeting searcher returned a nil result with no error", platform)
	}
	return rows, nil
}

var metaAdsTargetingSearch = accountDiscovery{
	provider:    model.ProviderMetaAds,
	displayName: "meta ads",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with access_token set",
	operation: "targeting search",
}

// SearchMetaTargetingOptions searches Meta's detailed-targeting taxonomy by name for the
// metaConfig.targeting interests, behaviors and workPositions pickers (#550).
//
// Gated on campaign_manager (the project-api gateway rule), NOT global_marketing_ops like the
// custom-audience picker: that one lists resources ON the shared LF Meta ad account, so it names
// other foundations' audiences. This read names only Meta's own taxonomy, identical for every token,
// so a foundation's executive director learns nothing about any other foundation from it, and the
// resolution may fall back to the LF system credential, as SuggestGoogleAdsGeoTargets does. The
// reserved LF scope stays unaddressable.
//
// Off unless META_TARGETING_SEARCH_ENABLED is "true" (400, before any credential is resolved). The
// query is the operator's free text and is never logged or echoed.
func (s *ConnectionService) SearchMetaTargetingOptions(ctx context.Context, p *conn.SearchMetaTargetingOptionsPayload) (*conn.MetaTargetingOptions, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	if os.Getenv(constants.EnvMetaTargetingSearchEnabled) != "true" {
		return nil, &conn.BadRequestError{Code: "400", Message: "the meta targeting search is disabled on this deployment (" +
			constants.EnvMetaTargetingSearchEnabled + " is not \"true\"); ask an operator to enable it"}
	}
	// Re-applies the design constraints for a caller that bypasses the generated decoder.
	if !slices.Contains(meta.TargetingSearchKinds(), p.Type) {
		return nil, &conn.BadRequestError{Code: "400", Message: "type must be one of " + strings.Join(meta.TargetingSearchKinds(), ", ")}
	}
	query, qerr := meta.NormalizeTargetingQuery(p.Query)
	if qerr != nil {
		return nil, &conn.BadRequestError{Code: "400", Message: "query must be " + strconv.Itoa(meta.MinTargetingQueryLen) + "-" +
			strconv.Itoa(meta.MaxTargetingQueryLen) + " characters after trimming, with no control characters"}
	}
	_, _, orch, err := s.resolveBackendWithOrch(metaAdsTargetingSearch.label())
	if err != nil {
		return nil, err
	}
	rows, rerr := orch.ReadMetaTargeting(ctx, p.ProjectID, model.ProviderMetaAds, model.MetaTargetingQuery{Kind: p.Type, Query: query})
	if rerr != nil {
		if errors.Is(rerr, domain.ErrMetaTargetingSearchInvalid) {
			return nil, &conn.BadRequestError{Code: "400", Message: "the targeting search is not valid: check type and query"}
		}
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, metaAdsTargetingSearch, rerr)
	}
	out := make([]*conn.MetaTargetingOption, 0, len(rows))
	for _, r := range rows {
		o := &conn.MetaTargetingOption{
			ID: r.ID, Name: r.Name, Description: optionalString(r.Description),
			AudienceSizeLowerBound: r.AudienceLower, AudienceSizeUpperBound: r.AudienceUpper, Coverage: r.Coverage,
		}
		if len(r.Path) > 0 {
			o.Path = slices.Clone(r.Path)
		}
		out = append(out, o)
	}
	return &conn.MetaTargetingOptions{Type: p.Type, Options: out, OptionCount: len(out)}, nil
}
