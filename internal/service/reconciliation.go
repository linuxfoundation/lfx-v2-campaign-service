// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	recon "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_reconciliation"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"

	"goa.design/goa/v3/security"
)

// reconcileReportMinAge is the minimum age a non-settled row must have before it is
// REPORTED. It is far below ClaimReleaseFloor on purpose: seeing a stuck state early is
// useful and harmless, whereas acting on one early is not. It sits just above the
// orchestrator's providerCallTimeout (2m) plus its detached persist window, so a healthy
// in-flight dispatch is never listed as needing attention.
const reconcileReportMinAge = 3 * time.Minute

// reconcileItemLimit bounds the items returned per kind so a pathological project
// cannot produce an unbounded response. The report carries the true total alongside,
// so truncation is visible rather than silent.
const reconcileItemLimit = 200

// ReconciliationService implements the generated reconciliation service.
//
// Like the other services it guards its collaborators with mu so the container can
// late-bind them after a cold-start DB retry (SetBackend).
type ReconciliationService struct {
	mu   sync.RWMutex
	repo domain.ReconciliationRepository
}

var (
	_ recon.Service = (*ReconciliationService)(nil)
	_ recon.Auther  = (*ReconciliationService)(nil)
)

// NewReconciliationService constructs a ReconciliationService.
func NewReconciliationService(r domain.ReconciliationRepository) *ReconciliationService {
	return &ReconciliationService{repo: r}
}

// SetBackend late-binds the repository after a cold-start DB retry opens the pool, so
// the reconciliation routes go live without a pod restart.
func (s *ReconciliationService) SetBackend(r domain.ReconciliationRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo = r
}

// ready snapshots the repository under the read lock and returns the typed 503 when the
// service has no database wired, matching the brief/connection services.
func (s *ReconciliationService) ready() (domain.ReconciliationRepository, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.repo == nil {
		return nil, &recon.ConnServiceUnavailableError{
			Code:    "503",
			Message: "the service is temporarily unable to handle the request",
		}
	}
	return s.repo, nil
}

// JWTAuth validates the bearer token is present. Authorization on campaign_manager is
// enforced at the gateway, matching every other service here.
func (s *ReconciliationService) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	if token == "" {
		return ctx, &recon.BadRequestError{Code: "400", Message: "missing bearer token"}
	}
	return ctx, nil
}

// GetReconciliation returns everything in the project needing operator attention.
func (s *ReconciliationService) GetReconciliation(ctx context.Context, p *recon.GetReconciliationPayload) (*recon.ReconciliationReport, error) {
	repo, err := s.ready()
	if err != nil {
		return nil, err
	}
	items, total, lerr := repo.ListReconciliationItems(ctx, p.ProjectID, reconcileReportMinAge, reconcileItemLimit)
	if lerr != nil {
		return nil, mapReconErr(lerr)
	}
	out := make([]*recon.ReconciliationItem, 0, len(items))
	for i := range items {
		out = append(out, reconItemResult(&items[i]))
	}
	return &recon.ReconciliationReport{
		ProjectID: p.ProjectID,
		Items:     out,
		Total:     total,
		Truncated: total > int64(len(out)),
	}, nil
}

// ReleaseDispatchClaim releases a stranded bare claim.
//
// The handler enforces the operator's explicit assertion BEFORE touching the database.
// verified_absent is not a formality: the service genuinely cannot tell whether a paid
// campaign exists upstream, so the only thing standing between a release and a possible
// duplicate is a human who checked. Refusing without it keeps that decision explicit
// rather than letting a default carry it.
func (s *ReconciliationService) ReleaseDispatchClaim(ctx context.Context, p *recon.ReleaseDispatchClaimPayload) (*recon.ReconciliationItem, error) {
	repo, err := s.ready()
	if err != nil {
		return nil, err
	}
	if !p.VerifiedAbsent {
		return nil, &recon.BadRequestError{
			Code:    "400",
			Message: "verified_absent must be true: confirm on the ad platform that no campaign exists for this brief before releasing the claim",
		}
	}
	version, verr := parseReconIfMatch(p.IfMatch)
	if verr != nil {
		return nil, verr
	}
	item, rerr := repo.ReleaseDispatchClaimByID(ctx, p.ProjectID, p.BriefID, p.CampaignID, version, model.ClaimReleaseFloor)
	if rerr != nil {
		return nil, mapReconErr(rerr)
	}
	return reconItemResult(item), nil
}

// reconItemResult converts a domain item to the generated result type.
func reconItemResult(it *model.ReconciliationItem) *recon.ReconciliationItem {
	ageSeconds := int64(it.Age.Seconds())
	out := &recon.ReconciliationItem{
		Kind:       string(it.Kind),
		BriefID:    it.BriefID,
		Status:     it.Status,
		AgeSeconds: ageSeconds,
		Resolvable: it.Resolvable,
		Detail:     it.Detail,
	}
	if p := string(it.Platform); p != "" {
		out.Platform = &p
	}
	if it.CampaignID != "" {
		id := it.CampaignID
		out.CampaignID = &id
	}
	if it.AudienceID != "" {
		id := it.AudienceID
		out.AudienceID = &id
	}
	if it.PlatformCampaignID != "" {
		pid := it.PlatformCampaignID
		out.PlatformCampaignID = &pid
	}
	v := it.Version
	out.Version = &v
	etag := strconv.FormatInt(it.Version, 10)
	out.Etag = &etag
	return out
}

// parseReconIfMatch mirrors parseBriefIfMatch's RFC 7232 handling, returning
// reconciliation-typed errors. The rules are identical deliberately: an operator should
// not have to remember that one endpoint accepts a weak validator when another does not.
func parseReconIfMatch(ifMatch *string) (int64, error) {
	if ifMatch == nil || *ifMatch == "" {
		return 0, &recon.PreconditionRequiredError{Code: "428", Message: "an If-Match header is required"}
	}
	raw := strings.TrimSpace(*ifMatch)
	// RFC 7232 §3.1 requires If-Match to use the strong comparison function, so a weak
	// validator must not authorize a write.
	if strings.HasPrefix(raw, "W/") || strings.HasPrefix(raw, "w/") {
		return 0, &recon.BadRequestError{Code: "400", Message: "If-Match must be a strong validator; weak tags (W/\"…\") are not accepted"}
	}
	hasOpen := strings.HasPrefix(raw, `"`)
	hasClose := strings.HasSuffix(raw, `"`)
	switch {
	case hasOpen && hasClose && len(raw) >= 2:
		raw = raw[1 : len(raw)-1]
	case hasOpen || hasClose:
		return 0, &recon.BadRequestError{Code: "400", Message: "If-Match has an unbalanced quote"}
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, &recon.BadRequestError{Code: "400", Message: "If-Match must be an integer version"}
	}
	return v, nil
}

// mapReconErr maps domain sentinels to the generated error types.
//
// ErrConflict deliberately carries a SPECIFIC message rather than the generic "already
// exists": for this endpoint a 409 always means "this is not a releasable bare claim",
// which is the single most important thing to tell an operator who is one call away
// from unblocking a pair that should stay blocked.
func mapReconErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrNotFound):
		return &recon.NotFoundError{Code: "404", Message: "the campaign was not found in this project"}
	case errors.Is(err, domain.ErrConflict):
		return &recon.ConflictError{
			Code:    "409",
			Message: "not a releasable claim: it carries evidence of an upstream campaign, is no longer pending, or is too recent to be considered stranded",
		}
	case errors.Is(err, domain.ErrPreconditionFailed):
		return &recon.PreconditionFailedError{Code: "412", Message: "the supplied ETag does not match the current version; re-read the reconciliation report"}
	}
	var (
		unavail   *recon.ConnServiceUnavailableError
		badReq    *recon.BadRequestError
		notFound  *recon.NotFoundError
		conflict  *recon.ConflictError
		preFailed *recon.PreconditionFailedError
		preReq    *recon.PreconditionRequiredError
	)
	switch {
	case errors.As(err, &unavail), errors.As(err, &badReq), errors.As(err, &notFound),
		errors.As(err, &conflict), errors.As(err, &preFailed), errors.As(err, &preReq):
		return err
	default:
		return &recon.InternalServerError{Code: "500", Message: "an internal server error occurred"}
	}
}
