// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"

	"goa.design/goa/v3/security"
)

// BriefService implements the generated briefs service interface, delegating to
// the brief/campaign repositories and the async orchestrator.
type BriefService struct {
	briefs    domain.BriefRepository
	campaigns domain.CampaignRepository
	jobs      domain.JobRepository
	orch      *Orchestrator
}

var (
	_ briefs.Service = (*BriefService)(nil)
	_ briefs.Auther  = (*BriefService)(nil)
)

// NewBriefService constructs a BriefService.
func NewBriefService(b domain.BriefRepository, c domain.CampaignRepository, j domain.JobRepository, orch *Orchestrator) *BriefService {
	return &BriefService{briefs: b, campaigns: c, jobs: j, orch: orch}
}

// ensureAvailable returns the typed 503 ServiceUnavailable error when the
// service has no database wired (DATABASE_URL unset). The brief routes are still
// mounted in that mode so runtime matches the published OpenAPI contract,
// consistent with the connection service.
func (s *BriefService) ensureAvailable() error {
	// Check every collaborator the service methods dereference, not just briefs:
	// in the no-database mode they are all nil together, but guarding only briefs
	// would nil-panic if the service were ever partially wired.
	if s.briefs == nil || s.campaigns == nil || s.jobs == nil || s.orch == nil {
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: "brief storage is not configured"}
	}
	return nil
}

// JWTAuth mirrors the connection service: it records the authenticated actor
// (validated by Heimdall at the gateway) into the context for attribution.
func (s *BriefService) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	if token == "" {
		return ctx, &briefs.BadRequestError{Code: "400", Message: "missing bearer token"}
	}
	if a := actorFromToken(token); a != nil {
		ctx = context.WithValue(ctx, actorCtxKey{}, a)
	}
	return ctx, nil
}

// ─── Briefs ───

func (s *BriefService) CreateBrief(ctx context.Context, p *briefs.CreateBriefPayload) (*briefs.Brief, error) {
	if err := s.ensureAvailable(); err != nil {
		return nil, err
	}
	in := p.Brief
	b := &model.CampaignBrief{
		ProjectID:    p.ProjectID,
		ProgramType:  model.ProgramType(in.ProgramType),
		EventSlug:    in.EventSlug,
		URL:          strVal(in.URL),
		Platforms:    marshalStrings(in.Platforms),
		EventDetails: marshalAny(in.EventDetails),
		Copy:         marshalAny(in.Copy),
		Keywords:     marshalAny(in.Keywords),
		Targeting:    marshalAny(in.Targeting),
	}
	created, err := s.briefs.CreateBrief(ctx, b)
	if err != nil {
		return nil, mapBriefErr(err)
	}
	return briefResult(created), nil
}

func (s *BriefService) GetBrief(ctx context.Context, p *briefs.GetBriefPayload) (*briefs.Brief, error) {
	if err := s.ensureAvailable(); err != nil {
		return nil, err
	}
	b, err := s.briefs.GetBrief(ctx, p.ProjectID, p.BriefID)
	if err != nil {
		return nil, mapBriefErr(err)
	}
	return briefResult(b), nil
}

func (s *BriefService) UpdateBrief(ctx context.Context, p *briefs.UpdateBriefPayload) (*briefs.Brief, error) {
	if err := s.ensureAvailable(); err != nil {
		return nil, err
	}
	version, err := parseBriefIfMatch(p.IfMatch)
	if err != nil {
		return nil, err
	}
	in := p.Brief
	b := &model.CampaignBrief{
		ID:           p.BriefID,
		ProjectID:    p.ProjectID,
		ProgramType:  model.ProgramType(in.ProgramType),
		EventSlug:    in.EventSlug,
		URL:          strVal(in.URL),
		Platforms:    marshalStrings(in.Platforms),
		EventDetails: marshalAny(in.EventDetails),
		Copy:         marshalAny(in.Copy),
		Keywords:     marshalAny(in.Keywords),
		Targeting:    marshalAny(in.Targeting),
	}
	updated, uerr := s.briefs.ReplaceBrief(ctx, b, version)
	if uerr != nil {
		return nil, mapBriefErr(uerr)
	}
	return briefResult(updated), nil
}

func (s *BriefService) ApproveBrief(ctx context.Context, p *briefs.ApproveBriefPayload) (*briefs.Brief, error) {
	if err := s.ensureAvailable(); err != nil {
		return nil, err
	}
	version, err := parseBriefIfMatch(p.IfMatch)
	if err != nil {
		return nil, err
	}
	b, aerr := s.briefs.Approve(ctx, p.ProjectID, p.BriefID, actorFromCtx(ctx), version)
	if aerr != nil {
		return nil, mapBriefErr(aerr)
	}
	return briefResult(b), nil
}

func (s *BriefService) DeleteBrief(ctx context.Context, p *briefs.DeleteBriefPayload) error {
	if err := s.ensureAvailable(); err != nil {
		return err
	}
	return mapBriefErr(s.briefs.ArchiveBrief(ctx, p.ProjectID, p.BriefID))
}

// ─── Campaigns ───

func (s *BriefService) CreateCampaigns(ctx context.Context, p *briefs.CreateCampaignsPayload) (*briefs.JobCreateResponse, error) {
	if err := s.ensureAvailable(); err != nil {
		return nil, err
	}
	brief, err := s.briefs.GetBrief(ctx, p.ProjectID, p.BriefID)
	if err != nil {
		return nil, mapBriefErr(err)
	}
	if brief.Status != model.BriefApproved {
		return nil, &briefs.BadRequestError{Code: "400", Message: "brief must be approved before creating campaigns"}
	}
	if len(p.Input.Platforms) == 0 {
		// Reject an empty platform set: it would create a job with zero dispatches
		// that instantly aggregates to "succeeded" — a meaningless no-op job.
		return nil, &briefs.BadRequestError{Code: "400", Message: "at least one platform is required"}
	}
	platforms := make([]model.Provider, 0, len(p.Input.Platforms))
	seen := make(map[model.Provider]struct{}, len(p.Input.Platforms))
	for _, pl := range p.Input.Platforms {
		prov := model.Provider(pl)
		if !prov.Valid() {
			return nil, &briefs.BadRequestError{Code: "400", Message: "unknown platform: " + pl}
		}
		if _, dup := seen[prov]; dup {
			// Reject duplicates outright: dispatching the same platform twice would
			// create two paid upstream campaigns concurrently, only one of which the
			// (brief_id, platform)-unique persistence can record.
			return nil, &briefs.BadRequestError{Code: "400", Message: "duplicate platform: " + pl}
		}
		seen[prov] = struct{}{}
		platforms = append(platforms, prov)
	}
	// Pass the version we just observed as 'approved'. Start gates job creation on
	// the brief still being approved at this exact version, so a concurrent replace
	// (which resets it to draft, bumping version) or archive committing between this
	// read and job creation makes Start fail (domain.ErrStaleApproval → 409) rather
	// than launching paid campaigns from a stale "approved" snapshot.
	jobID, err := s.orch.Start(ctx, brief, brief.Version, platforms, marshalAny(p.Input.Config))
	if err != nil {
		return nil, mapBriefErr(err)
	}
	queued := "queued"
	return &briefs.JobCreateResponse{JobID: jobID, Status: queued, Platforms: p.Input.Platforms}, nil
}

func (s *BriefService) GetCampaign(ctx context.Context, p *briefs.GetCampaignPayload) (*briefs.Campaign, error) {
	if err := s.ensureAvailable(); err != nil {
		return nil, err
	}
	c, err := s.campaigns.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if err != nil {
		return nil, mapBriefErr(err)
	}
	return campaignResult(c), nil
}

func (s *BriefService) UpdateCampaign(ctx context.Context, p *briefs.UpdateCampaignPayload) (*briefs.Campaign, error) {
	if err := s.ensureAvailable(); err != nil {
		return nil, err
	}
	version, err := parseBriefIfMatch(p.IfMatch)
	if err != nil {
		return nil, err
	}
	// Load the existing campaign and overlay only the client-editable fields
	// (name, status, config). ReplaceCampaign writes every column, so budget,
	// dates, platform, and result must be carried over from the stored row or a
	// config-only edit would zero them out.
	existing, gerr := s.campaigns.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	existing.CampaignName = p.Campaign.CampaignName
	existing.Status = p.Campaign.Status
	// Only overwrite the stored config when the caller actually supplied one.
	// config is optional in CampaignUpdateInput, so an omitted value must leave
	// the existing ConfigSnapshot intact rather than wiping it to NULL on a
	// name/status-only edit (the GET response doesn't expose config, so a client
	// can't round-trip it back).
	if p.Campaign.Config != nil {
		existing.ConfigSnapshot = marshalAny(p.Campaign.Config)
	}
	updated, uerr := s.campaigns.ReplaceCampaign(ctx, existing, version)
	if uerr != nil {
		return nil, mapBriefErr(uerr)
	}
	return campaignResult(updated), nil
}

func (s *BriefService) GetJob(ctx context.Context, p *briefs.GetJobPayload) (*briefs.JobPollResponse, error) {
	if err := s.ensureAvailable(); err != nil {
		return nil, err
	}
	j, err := s.jobs.GetJob(ctx, p.ProjectID, p.JobID)
	if err != nil {
		return nil, mapBriefErr(err)
	}
	resp := &briefs.JobPollResponse{JobID: j.ID, Status: string(j.Status)}
	if len(j.Result) > 0 {
		// The stored result is the orchestrator's per-platform outcome array; decode
		// it into the typed response shape so the OpenAPI contract is honored.
		var stored []struct {
			Platform   string `json:"platform"`
			OK         bool   `json:"ok"`
			CampaignID string `json:"campaign_id"`
			Error      string `json:"error"`
		}
		if err := json.Unmarshal(j.Result, &stored); err != nil {
			// A persisted result that won't decode is corruption, not a valid empty
			// poll response. Silently dropping it would hand back a terminal
			// succeeded/partial job with NO per-platform results — an inaccurate
			// response that masks the corruption as success. Surface it as a 500.
			slog.ErrorContext(ctx, "failed to decode persisted job result", "job_id", j.ID, "error", err)
			return nil, &briefs.InternalServerError{Code: "500", Message: "an internal server error occurred"}
		}
		resp.Result = make([]*briefs.PlatformResult, 0, len(stored))
		for _, r := range stored {
			pr := &briefs.PlatformResult{Platform: r.Platform, OK: r.OK}
			if r.CampaignID != "" {
				id := r.CampaignID
				pr.CampaignID = &id
			}
			if r.Error != "" {
				e := r.Error
				pr.Error = &e
			}
			resp.Result = append(resp.Result, pr)
		}
	} else if j.Status == model.JobSucceeded || j.Status == model.JobPartial {
		// A succeeded/partial job is an AGGREGATE over per-platform outcomes, so it
		// must carry those results. An empty/absent result on such a terminal status
		// means the stored row is corrupt (results lost); returning a 200 with no
		// results would misrepresent corruption as a successful dispatch. A 'failed'
		// job legitimately can carry only an error (e.g. a result-marshal failure),
		// so it is not held to this invariant.
		slog.ErrorContext(ctx, "terminal job has no per-platform results", "job_id", j.ID, "status", j.Status)
		return nil, &briefs.InternalServerError{Code: "500", Message: "an internal server error occurred"}
	}
	if j.Error != "" {
		errMsg := j.Error // copy: don't hand out a pointer aliasing the source struct field
		resp.Error = &errMsg
	}
	return resp, nil
}

// ─── mapping helpers ───

func briefResult(b *model.CampaignBrief) *briefs.Brief {
	return &briefs.Brief{
		ID:           b.ID,
		ProjectID:    b.ProjectID,
		ProgramType:  string(b.ProgramType),
		EventSlug:    b.EventSlug,
		URL:          optStr(b.URL),
		Platforms:    unmarshalStrings(b.Platforms),
		EventDetails: unmarshalAny(b.EventDetails),
		Copy:         unmarshalAny(b.Copy),
		Keywords:     unmarshalAny(b.Keywords),
		Targeting:    unmarshalAny(b.Targeting),
		Status:       string(b.Status),
		Version:      b.Version,
		Etag:         optStr(briefETag(b.Version)),
	}
}

// briefETag renders the version as a quoted HTTP entity-tag (RFC 7232), e.g.
// `"3"`. parseBriefIfMatch accepts both this quoted form and a bare integer, so
// a client can round-trip the returned validator.
func briefETag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

func campaignResult(c *model.Campaign) *briefs.Campaign {
	return &briefs.Campaign{
		ID:                 c.ID,
		ProjectID:          c.ProjectID,
		BriefID:            c.BriefID,
		Platform:           string(c.Platform),
		PlatformCampaignID: optStr(c.PlatformCampaignID),
		CampaignName:       c.CampaignName,
		Status:             c.Status,
		Version:            c.Version,
		Etag:               optStr(briefETag(c.Version)),
	}
}

// parseBriefIfMatch converts the If-Match header to a version (428 if missing,
// 400 if non-numeric), returning briefs-package errors.
func parseBriefIfMatch(ifMatch *string) (int64, error) {
	if ifMatch == nil || *ifMatch == "" {
		return 0, &briefs.PreconditionRequiredError{Code: "428", Message: "an If-Match header is required"}
	}
	// Accept the bare version we emit and a standards-compliant STRONG quoted
	// entity-tag (RFC 7232 `If-Match: "3"`). Reject a weak validator (`W/"3"`):
	// RFC 7232 §3.1 requires If-Match to use the strong comparison function, so a
	// weak tag must NOT authorize a write.
	raw := strings.TrimSpace(*ifMatch)
	if strings.HasPrefix(raw, "W/") || strings.HasPrefix(raw, "w/") {
		return 0, &briefs.BadRequestError{Code: "400", Message: "If-Match must be a strong validator; weak tags (W/\"…\") are not accepted"}
	}
	// Strip exactly one balanced pair of surrounding quotes; reject an unbalanced
	// quote (e.g. `"3` or `3"`) rather than silently accepting it as version 3.
	hasOpen := strings.HasPrefix(raw, `"`)
	hasClose := strings.HasSuffix(raw, `"`)
	switch {
	case hasOpen && hasClose && len(raw) >= 2:
		raw = raw[1 : len(raw)-1]
	case hasOpen || hasClose:
		return 0, &briefs.BadRequestError{Code: "400", Message: "If-Match has an unbalanced quote"}
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, &briefs.BadRequestError{Code: "400", Message: "If-Match must be an integer version"}
	}
	return v, nil
}

func mapBriefErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrNotFound):
		return &briefs.NotFoundError{Code: "404", Message: "the resource was not found"}
	case errors.Is(err, domain.ErrStaleApproval):
		// The approve→dispatch guard fired: the brief lost approval (or its version
		// changed) between the approval read and the guarded job insert. This is a
		// state conflict, not a uniqueness one — tell the client to refresh and
		// re-approve, which "already exists" would misdescribe.
		return &briefs.ConflictError{Code: "409", Message: "brief is no longer approved at the expected version; refresh and re-approve, then retry"}
	case errors.Is(err, domain.ErrConflict):
		return &briefs.ConflictError{Code: "409", Message: "the resource already exists"}
	case errors.Is(err, domain.ErrPreconditionFailed):
		return &briefs.PreconditionFailedError{Code: "412", Message: "the supplied ETag does not match the current version"}
	}
	// Preserve errors that are already typed briefs API errors (e.g. the typed
	// 503 the orchestrator returns during graceful shutdown, or a 400/428/412
	// constructed upstream) so their advertised status isn't flattened to 500.
	var (
		unavail   *briefs.ConnServiceUnavailableError
		badReq    *briefs.BadRequestError
		notFound  *briefs.NotFoundError
		conflict  *briefs.ConflictError
		preFailed *briefs.PreconditionFailedError
		preReq    *briefs.PreconditionRequiredError
	)
	switch {
	case errors.As(err, &unavail), errors.As(err, &badReq), errors.As(err, &notFound),
		errors.As(err, &conflict), errors.As(err, &preFailed), errors.As(err, &preReq):
		return err
	default:
		return &briefs.InternalServerError{Code: "500", Message: "an internal server error occurred"}
	}
}

func marshalStrings(ss []string) json.RawMessage {
	if len(ss) == 0 {
		return nil
	}
	b, _ := json.Marshal(ss)
	return b
}

func unmarshalStrings(j json.RawMessage) []string {
	if len(j) == 0 {
		return nil
	}
	var ss []string
	_ = json.Unmarshal(j, &ss)
	return ss
}

func marshalAny(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// unmarshalAny decodes a stored JSONB column back into an arbitrary value for
// the response. Returns nil for empty/undecodable input so the response omits
// the field rather than surfacing malformed data.
func unmarshalAny(j json.RawMessage) any {
	if len(j) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(j, &v); err != nil {
		return nil
	}
	return v
}
