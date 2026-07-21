// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"

	audiences "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"

	"goa.design/goa/v3/security"
)

// AudienceService implements the generated audiences service interface, delegating to
// the audience repository. Built audiences are the "B2" resource: a pointer +
// provenance to a platform-side audience (a HubSpot master list), never its contents.
type AudienceService struct {
	mu   sync.RWMutex
	repo domain.AudienceRepository
}

var (
	_ audiences.Service = (*AudienceService)(nil)
	_ audiences.Auther  = (*AudienceService)(nil)
)

// NewAudienceService constructs an AudienceService. A nil repo mounts the routes in
// the typed-503 (unavailable) mode, matching the brief/connection services.
func NewAudienceService(repo domain.AudienceRepository) *AudienceService {
	return &AudienceService{repo: repo}
}

// SetBackend late-binds the repo after a cold-start DB retry (guarded by the RWMutex;
// handlers snapshot via ready() so a mid-request swap can't race). Mirrors the brief
// service's late-binding.
func (s *AudienceService) SetBackend(repo domain.AudienceRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo = repo
}

// ready returns the repo or a typed 503 when the database is not wired yet.
func (s *AudienceService) ready() (domain.AudienceRepository, error) {
	s.mu.RLock()
	repo := s.repo
	s.mu.RUnlock()
	if repo == nil {
		return nil, &audiences.ConnServiceUnavailableError{Code: "503", Message: "audience storage is unavailable"}
	}
	return repo, nil
}

// JWTAuth records the authenticated actor (validated by Heimdall at the gateway) into
// the context for attribution, mirroring the brief service.
func (s *AudienceService) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	if token == "" {
		return ctx, &audiences.BadRequestError{Code: "400", Message: "missing bearer token"}
	}
	if a := actorFromToken(token); a != nil {
		ctx = context.WithValue(ctx, actorCtxKey{}, a)
	}
	return ctx, nil
}

// ─── Handlers ───

func (s *AudienceService) CreateAudience(ctx context.Context, p *audiences.CreateAudiencePayload) (*audiences.Audience, error) {
	repo, err := s.ready()
	if err != nil {
		return nil, err
	}
	a := audienceFromInput(p.ProjectID, p.BriefID, p.Audience)
	a.CreatedBy = marshalActor(actorFromCtx(ctx))
	// Reject an inconsistent built audience (status=built with no master-list id)
	// before hitting the DB — AudienceBuilt means the platform list exists.
	if verr := a.Validate(); verr != nil {
		return nil, audienceValidationErr(verr)
	}
	created, cerr := repo.CreateAudience(ctx, a)
	if cerr != nil {
		return nil, mapAudienceErr(cerr)
	}
	return audienceResult(created), nil
}

func (s *AudienceService) GetAudience(ctx context.Context, p *audiences.GetAudiencePayload) (*audiences.Audience, error) {
	repo, err := s.ready()
	if err != nil {
		return nil, err
	}
	a, gerr := repo.GetAudience(ctx, p.ProjectID, p.BriefID, p.AudienceID)
	if gerr != nil {
		return nil, mapAudienceErr(gerr)
	}
	return audienceResult(a), nil
}

func (s *AudienceService) ListAudiences(ctx context.Context, p *audiences.ListAudiencesPayload) (*audiences.ListAudiencesResult, error) {
	repo, err := s.ready()
	if err != nil {
		return nil, err
	}
	list, lerr := repo.ListAudiences(ctx, p.ProjectID, p.BriefID)
	if lerr != nil {
		return nil, mapAudienceErr(lerr)
	}
	out := make([]*audiences.Audience, 0, len(list))
	for _, a := range list {
		out = append(out, audienceResult(a))
	}
	return &audiences.ListAudiencesResult{Audiences: out}, nil
}

func (s *AudienceService) UpdateAudience(ctx context.Context, p *audiences.UpdateAudiencePayload) (*audiences.Audience, error) {
	repo, err := s.ready()
	if err != nil {
		return nil, err
	}
	version, verr := parseAudienceIfMatch(p.IfMatch)
	if verr != nil {
		return nil, verr
	}
	// Reject an empty patch. Because every AudienceUpdateInput field is optional,
	// `{"audience":{}}` passes the generated validator — but applying it changes
	// nothing while still bumping version/updated_at, which would invalidate other
	// clients' ETags and cause spurious 412s. A PATCH must supply at least one mutable
	// field.
	if !hasAudiencePatch(p.Audience) {
		return nil, &audiences.BadRequestError{Code: "400", Message: "update must supply at least one field to change"}
	}
	// Load the stored row and MERGE only the provided (non-nil) fields onto it —
	// otherwise an update that omits an optional field (platform_master_list_id,
	// suppression_list_ids, inclusion_summary, status) would write empty/null and
	// clear data set as the build progressed. The If-Match version guards this
	// read-modify-write: repo.UpdateAudience still fails with ErrPreconditionFailed
	// if the row changed between this load and the write.
	cur, gerr := repo.GetAudience(ctx, p.ProjectID, p.BriefID, p.AudienceID)
	if gerr != nil {
		return nil, mapAudienceErr(gerr)
	}
	applyAudiencePatch(cur, p.Audience)
	// Re-validate the MERGED row: a patch that sets status=built on a row with no
	// master-list id, or clears the id on an already-built row, would leave "built"
	// meaning nothing. Reject as a 400 before persisting.
	//
	// Precedence is deliberate: this content-400 runs BEFORE the repo's optimistic-
	// concurrency check (412 on a stale If-Match). A built-with-no-id patch is malformed
	// at ANY version, so failing fast with 400 is correct — returning 412 would only
	// send the client to refetch and retry a request that is still inherently invalid.
	if verr := cur.Validate(); verr != nil {
		return nil, audienceValidationErr(verr)
	}
	updated, uerr := repo.UpdateAudience(ctx, cur, version)
	if uerr != nil {
		return nil, mapAudienceErr(uerr)
	}
	return audienceResult(updated), nil
}

// hasAudiencePatch reports whether the patch carries at least one field to change.
// A field counts as supplied when its pointer is non-nil, a non-empty suppression list
// is supplied, or the explicit clear_suppression_lists flag is set. An all-omitted patch
// is a no-op and rejected. (A supplied EMPTY suppression_list_ids does not count on its
// own — the generated client drops it on the wire via omitempty, so it never reaches
// here; clearing must use clear_suppression_lists.)
func hasAudiencePatch(in *audiences.AudienceUpdateInput) bool {
	if in == nil {
		return false
	}
	return in.PlatformMasterListID != nil ||
		len(in.SuppressionListIds) > 0 ||
		(in.ClearSuppressionLists != nil && *in.ClearSuppressionLists) ||
		in.InclusionSummary != nil ||
		in.Status != nil
}

// applyAudiencePatch merges the provided fields of in onto cur (PATCH semantics).
// A nil pointer leaves a field unchanged. Suppression lists have two operations:
// clear_suppression_lists=true removes all (takes precedence), otherwise a non-empty
// suppression_list_ids replaces the set. A bare empty suppression_list_ids is a no-op
// because the generated client omits it on the wire (omitempty) — that is exactly why
// clearing has its own boolean flag. platform is immutable and ignored on update.
func applyAudiencePatch(cur *model.CampaignAudience, in *audiences.AudienceUpdateInput) {
	if in == nil {
		return
	}
	if in.PlatformMasterListID != nil {
		cur.PlatformMasterListID = *in.PlatformMasterListID
	}
	if in.ClearSuppressionLists != nil && *in.ClearSuppressionLists {
		cur.SuppressionListIDs = marshalStrings([]string{})
	} else if len(in.SuppressionListIds) > 0 {
		cur.SuppressionListIDs = marshalStrings(in.SuppressionListIds)
	}
	if in.InclusionSummary != nil {
		cur.InclusionSummary = *in.InclusionSummary
	}
	if in.Status != nil {
		cur.Status = model.AudienceStatus(*in.Status)
	}
}

// ─── Mapping helpers ───

// marshalActor renders the created-by actor to JSONB, or SQL NULL when there is no
// actor. It takes the CONCRETE *model.Actor (not `any`) so a nil pointer is detected
// directly: passing a typed-nil `*model.Actor` through marshalAny(any) would slip past
// its `v == nil` guard (a typed nil boxed in an interface is not == nil) and persist the
// JSONB literal `null` instead of SQL NULL, diverging from an unattributed row's intent.
func marshalActor(actor *model.Actor) json.RawMessage {
	if actor == nil {
		return nil
	}
	return marshalAny(actor)
}

// audienceFromInput builds the domain model from a create/update payload. StatusOrDefault
// on the model handles an omitted status (defaults to "building").
func audienceFromInput(projectID, briefID string, in *audiences.AudienceInput) *model.CampaignAudience {
	a := &model.CampaignAudience{
		ProjectID:            projectID,
		BriefID:              briefID,
		Platform:             model.Provider(in.Platform),
		PlatformMasterListID: strVal(in.PlatformMasterListID),
		SuppressionListIDs:   marshalStrings(in.SuppressionListIds),
		InclusionSummary:     strVal(in.InclusionSummary),
	}
	// Preserve an explicit status; only default when omitted. (StatusOrDefault() is a
	// no-op when Status is already set, but the explicit if/else makes that self-evident
	// — a prior review misread the earlier unconditional call as an overwrite.)
	if in.Status != nil {
		a.Status = model.AudienceStatus(*in.Status)
	} else {
		a.Status = a.StatusOrDefault()
	}
	return a
}

// audienceResult maps the domain model to the API response view (ETag mirrors version).
// audienceETag renders the version as a quoted HTTP entity-tag (RFC 7232), matching
// the brief service (a bare integer is not a valid entity-tag).
func audienceETag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

func audienceResult(a *model.CampaignAudience) *audiences.Audience {
	etag := audienceETag(a.Version)
	res := &audiences.Audience{
		ID:                 a.ID,
		ProjectID:          a.ProjectID,
		BriefID:            a.BriefID,
		Platform:           string(a.Platform),
		SuppressionListIds: unmarshalStrings(a.SuppressionListIDs),
		Status:             string(a.Status),
		Version:            a.Version,
		Etag:               &etag,
	}
	if a.PlatformMasterListID != "" {
		res.PlatformMasterListID = &a.PlatformMasterListID
	}
	if a.InclusionSummary != "" {
		res.InclusionSummary = &a.InclusionSummary
	}
	return res
}

// parseAudienceIfMatch parses the If-Match header into a version. Reuses the same
// strong-validator rules as the brief service (RFC 7232): rejects a weak tag and an
// unbalanced quote, accepts a bare version or a strong quoted entity-tag.
func parseAudienceIfMatch(ifMatch *string) (int64, error) {
	if ifMatch == nil || *ifMatch == "" {
		return 0, &audiences.PreconditionRequiredError{Code: "428", Message: "an If-Match header is required"}
	}
	raw := strings.TrimSpace(*ifMatch)
	if strings.HasPrefix(raw, "W/") || strings.HasPrefix(raw, "w/") {
		return 0, &audiences.BadRequestError{Code: "400", Message: "If-Match must be a strong validator; weak tags (W/\"…\") are not accepted"}
	}
	hasOpen := strings.HasPrefix(raw, `"`)
	hasClose := strings.HasSuffix(raw, `"`)
	switch {
	case hasOpen && hasClose && len(raw) >= 2:
		raw = raw[1 : len(raw)-1]
	case hasOpen || hasClose:
		return 0, &audiences.BadRequestError{Code: "400", Message: "If-Match has an unbalanced quote"}
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, &audiences.BadRequestError{Code: "400", Message: "If-Match must be an integer version"}
	}
	return v, nil
}

// audienceValidationErr maps a domain model-validation failure to a typed 400. The
// message is the model error's own text (safe, human-readable, no internal detail —
// the offending field name it names is the public API attribute).
func audienceValidationErr(err error) error {
	return &audiences.BadRequestError{Code: "400", Message: err.Error()}
}

// mapAudienceErr maps domain errors to the generated audiences API error types,
// preserving already-typed audiences errors.
func mapAudienceErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrNotFound):
		// Resource-neutral: this mapping is shared across create/get/list/update, and
		// on create/list the ErrNotFound comes from a missing/cross-project/archived
		// PARENT BRIEF, not a missing audience — so don't claim "audience not found".
		return &audiences.NotFoundError{Code: "404", Message: "the audience or its parent brief was not found"}
	case errors.Is(err, domain.ErrConflict):
		return &audiences.ConflictError{Code: "409", Message: "the resource already exists"}
	case errors.Is(err, domain.ErrPreconditionFailed):
		return &audiences.PreconditionFailedError{Code: "412", Message: "the supplied ETag does not match the current version"}
	}
	var (
		unavail   *audiences.ConnServiceUnavailableError
		badReq    *audiences.BadRequestError
		notFound  *audiences.NotFoundError
		conflict  *audiences.ConflictError
		preFailed *audiences.PreconditionFailedError
		preReq    *audiences.PreconditionRequiredError
	)
	switch {
	case errors.As(err, &unavail), errors.As(err, &badReq), errors.As(err, &notFound),
		errors.As(err, &conflict), errors.As(err, &preFailed), errors.As(err, &preReq):
		return err
	default:
		return &audiences.InternalServerError{Code: "500", Message: "an internal server error occurred"}
	}
}
