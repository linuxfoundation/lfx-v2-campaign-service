// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"

	"goa.design/goa/v3/security"
)

// BriefService implements the generated briefs service interface, delegating to
// the brief/campaign repositories and the async orchestrator.
//
// The collaborators are guarded by mu so the container can LATE-BIND them after a
// cold-start DB retry succeeds (SetBackend), just like ConnectionService: the
// routes are mounted at boot against this instance, so the retry must mutate it in
// place rather than swap the instance. Handlers snapshot the collaborators under the
// lock (deps) and never dereference the fields directly.
type BriefService struct {
	mu        sync.RWMutex
	briefs    domain.BriefRepository
	campaigns domain.CampaignRepository
	jobs      domain.JobRepository
	orch      *Orchestrator
	// indexer publishes resource snapshots for the Query Service. Never nil after
	// construction (a Noop stands in when NATS is unconfigured), so call sites publish
	// unconditionally rather than nil-checking at each of them.
	indexer indexer.Publisher
	// indexingDisabled is a CONFIGURATION fact (NATS_URL empty), not an observation of the
	// publisher: a Noop also appears when the broker is merely unreachable. See DisableIndexing.
	indexingDisabled bool
	// eventFetcher and eventParser back FetchEventURL only. Nil in every construction that
	// does not call SetEventURL, which is why that handler checks them rather than ready().
	eventFetcher EventFetcher
	eventParser  EventParser
}

var (
	_ briefs.Service = (*BriefService)(nil)
	_ briefs.Auther  = (*BriefService)(nil)
)

// SetIndexer injects the Query Service index publisher. Separate from the constructor so the
// ~40 existing NewBriefService call sites (mostly tests) are unaffected and default to Noop.
func (s *BriefService) SetIndexer(p indexer.Publisher) {
	if p == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.indexer = p
}

// DisableIndexing marks indexing as DELIBERATELY off, so writes skip the outbox entirely.
//
// This is a CONFIGURATION fact (NATS_URL is empty), not an observation of the current publisher.
// The distinction is load-bearing: NewNATSPublisher also returns a Noop when the broker is merely
// UNREACHABLE, and treating that as "disabled" would drop outbox rows for the entire life of a
// pod that happened to start during a broker restart — permanently, since pending rows are never
// pruned and there is no reindex path. A transient outage must still enqueue; the relay delivers
// once the connection recovers (the publisher is built with RetryOnFailedConnect).
//
// Set once at wiring, before any request is served, so it cannot change between two writes.
func (s *BriefService) DisableIndexing() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.indexingDisabled = true
}

// indexingIsDisabled reports the configuration fact set by DisableIndexing.
func (s *BriefService) indexingIsDisabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.indexingDisabled
}

// IndexerIsNoop reports whether this service would publish nothing. Exported so the
// container's wiring tests can assert that EVERY startup path injected a real publisher:
// SetIndexer is opt-in, so a path that forgets it still compiles, boots and serves — it
// just silently indexes nothing. That failure is invisible without this accessor.
func (s *BriefService) IndexerIsNoop() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, isNoop := s.indexer.(indexer.Noop)
	return isNoop
}

// briefDoc maps a brief response to the INDEXED shape. The generated briefs.Brief has no json
// tags, so publishing it directly emits PascalCase keys that no API-shaped consumer matches.
func briefDoc(b *briefs.Brief) indexer.BriefDoc {
	return indexer.BriefDoc{
		ID:          b.ID,
		ProjectID:   b.ProjectID,
		ProgramType: b.ProgramType,
		EventSlug:   b.EventSlug,
		URL:         derefStr(b.URL),
		Status:      b.Status,
		Version:     b.Version,

		// The revisable content: without it a copy-only edit indexes a new version showing
		// nothing changed, so revision history cannot answer "what was revised?".
		Platforms:    b.Platforms,
		EventDetails: b.EventDetails,
		Copy:         b.Copy,
		Keywords:     b.Keywords,
		Targeting:    b.Targeting,
	}
}

// campaignDoc maps a campaign response to the INDEXED shape (same reasoning as briefDoc).
func campaignDoc(c *briefs.Campaign) indexer.CampaignDoc {
	return indexer.CampaignDoc{
		ID:                 c.ID,
		ProjectID:          c.ProjectID,
		BriefID:            c.BriefID,
		Platform:           c.Platform,
		PlatformCampaignID: derefStr(c.PlatformCampaignID),
		CampaignName:       c.CampaignName,
		Status:             c.Status,
		Version:            c.Version,
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// NewBriefService constructs a BriefService. The index publisher is NOT a parameter: it is
// injected via SetIndexer so the many existing call sites (mostly tests) are unaffected and
// default to a Noop publisher.
func NewBriefService(b domain.BriefRepository, c domain.CampaignRepository, j domain.JobRepository, orch *Orchestrator) *BriefService {
	return &BriefService{briefs: b, campaigns: c, jobs: j, orch: orch,
		indexer: indexer.Noop{},
	}
}

// SetBackend late-binds the brief/campaign/job repositories and the orchestrator
// after a cold-start DB retry opens the pool, so the brief and job routes go live
// without a pod restart (mirrors ConnectionService.SetBackend). Guarded by mu against
// concurrent handler reads.
func (s *BriefService) SetBackend(b domain.BriefRepository, c domain.CampaignRepository, j domain.JobRepository, orch *Orchestrator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.briefs, s.campaigns, s.jobs, s.orch = b, c, j, orch
}

// deps snapshots the collaborators under the read lock so a handler works against a
// consistent set even if SetBackend fires mid-request.
func (s *BriefService) deps() (domain.BriefRepository, domain.CampaignRepository, domain.JobRepository, *Orchestrator) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.briefs, s.campaigns, s.jobs, s.orch
}

// ready snapshots the collaborators under the read lock and returns the typed 503
// ServiceUnavailable error when the service has no database wired (DATABASE_URL
// unset, or a cold start that hasn't finished retrying). Handlers call this once and
// use the returned locals, so they work against a consistent set even if SetBackend
// fires mid-request and never dereference the fields directly. The brief routes are
// still mounted in the unavailable mode so runtime matches the published OpenAPI
// contract, consistent with the connection service.
func (s *BriefService) ready() (domain.BriefRepository, domain.CampaignRepository, domain.JobRepository, *Orchestrator, error) {
	// Check every collaborator the service methods dereference, not just briefs:
	// in the no-database (and cold-start) mode they are all nil together, but
	// guarding only briefs would nil-panic if the service were ever partially wired.
	b, c, j, orch := s.deps()
	if b == nil || c == nil || j == nil || orch == nil {
		// Availability-neutral wording (matches the connection service): in
		// cold-start mode the database IS configured but the backend hasn't
		// bound yet, so "not configured" would wrongly tell operators to change
		// config during a transient startup window.
		return nil, nil, nil, nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "brief storage is unavailable"}
	}
	return b, c, j, orch, nil
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

// projectSlugRe matches a canonical LFX project slug: one or more lowercase
// alphanumeric segments joined by SINGLE internal hyphens (an alphanumeric on each
// side of every hyphen), no leading/trailing hyphen and no consecutive hyphens
// (`foo--bar` is rejected). Old `[a-z0-9-]*` in the middle wrongly allowed `--`.
var projectSlugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// projectUUIDRe matches a canonical UUID (the shape the project path also accepts on
// read routes). A UUID in a campaign-naming path breaks the slug-based attribution
// join, so it is rejected explicitly. The generated HTTP decoder also validates the
// slug Pattern/MaxLength for the create routes; this app-level guard duplicates that
// for direct/non-HTTP callers (e.g. service tests) — belt-and-suspenders, not the sole
// enforcement.
var projectUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// projectSlugProblem returns a human-readable reason if projectID is NOT a canonical
// slug, or "" if it is valid. It guards the CREATE paths only (brief-create,
// campaign-create): those store project_id as the campaign-name attribution key AND
// the exact-match key for the connection lookup at dispatch, so a brief-slug and a
// connection-UUID would never join. Read/update/delete stay UUID-or-slug (migration
// 000003 preserved historical UUID rows). The generated HTTP decoder validates the
// same Pattern/MaxLength on the create routes; this guard duplicates it for
// direct/non-HTTP callers.
func projectSlugProblem(projectID string) string {
	if projectUUIDRe.MatchString(projectID) {
		return "project_id must be the canonical project slug, not a UUID"
	}
	if len(projectID) > 35 || !projectSlugRe.MatchString(projectID) {
		return "project_id must be a canonical lowercase project slug (e.g. 'cncf', 'tlf')"
	}
	return ""
}

// validateProjectSlug wraps projectSlugProblem as a briefs BadRequestError for the
// brief/campaign create endpoints.
func validateProjectSlug(projectID string) error {
	if msg := projectSlugProblem(projectID); msg != "" {
		return &briefs.BadRequestError{Code: "400", Message: msg}
	}
	return nil
}

// ─── Briefs ───

func (s *BriefService) CreateBrief(ctx context.Context, p *briefs.CreateBriefPayload) (*briefs.Brief, error) {
	briefRepo, _, _, _, err := s.ready()
	if err != nil {
		return nil, err
	}
	if err := validateProjectSlug(p.ProjectID); err != nil {
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
		// A nil actor — no bearer token, or claims this service could not decode — is
		// stored as NULL rather than rejected: losing the attribution is bad, refusing
		// the write because of it is worse. attributedActor logs it so the loss is at
		// least visible. See model.CampaignBrief.CreatedBy.
		CreatedBy: attributedActor(ctx, "create brief"),
	}
	// The index message co-commits with the row (see briefIndexPayload); the relay delivers it.
	created, err := briefRepo.CreateBrief(ctx, b, s.briefIndexPayload(indexer.ActionCreated))
	if err != nil {
		return nil, mapBriefErr(err)
	}
	return briefResult(created), nil
}

// briefIndexPayload builds the outbox payload for a brief write. It is passed INTO the repo so
// the message is enqueued in the same transaction as the row, giving every brief mutation ONE
// ordered sequence per resource.
//
// This is what makes archival safe. When some writes published directly after commit and only
// the archive went through the outbox, the two paths could not be ordered against each other: a
// replace could commit, stall before its publish, and land its update AFTER the archive had been
// replayed and retired — resurrecting a deleted brief in the index. The publisher's per-object
// lock could not prevent it, being process-local and only ordering calls as they arrive.
//
// NO bearer token is serialized. The outbox is JSONB retained for audit with no pruning, so
// storing the caller's JWT would persist a live credential indefinitely; the relay stamps a
// service credential at publish time instead.
// campaignIndexPayload mirrors briefIndexPayload for campaign writes made on the REQUEST path
// (update, status toggle). The orchestrator has its own copy for its async dispatch writes.
func (s *BriefService) campaignIndexPayload(action string) domain.CampaignIndexPayloadFunc {
	if s.indexingIsDisabled() {
		return nil
	}
	return func(c *model.Campaign) ([]byte, error) {
		return json.Marshal(indexer.NewTransaction(
			action, indexer.ObjectTypeCampaign,
			c.ID, c.ProjectID, "",
			campaignDoc(campaignResult(c)), c.CampaignName,
		))
	}
}

func (s *BriefService) briefIndexPayload(action string) domain.IndexPayloadFunc {
	// Indexing DELIBERATELY disabled (NATS_URL="") enqueues nothing: the row could never be
	// delivered and is never pruned, so writing one is unbounded growth with no upside.
	//
	// Gated on the CONFIG flag, never on IndexerIsNoop(): a Noop publisher also appears when the
	// broker is temporarily UNREACHABLE, and skipping the enqueue then would permanently lose
	// every write made by a pod that started during a broker restart. A missing CREDENTIAL is
	// likewise not covered — both are transient states whose rows are real work.
	if s.indexingIsDisabled() {
		return nil
	}
	return func(b *model.CampaignBrief) ([]byte, error) {
		return json.Marshal(indexer.NewTransaction(
			action, indexer.ObjectTypeBrief,
			b.ID, b.ProjectID, "",
			briefDoc(briefResult(b)), b.EventSlug,
		))
	}
}

// FindBrief returns the saved brief for an event slug, or 404 when none exists.
//
// This is the "have I already generated a brief for this event?" lookup. The UI derives the
// slug from a pasted event URL and calls this BEFORE generating: a 200 returns the stored
// brief (with its AI-generated copy/keywords/targeting, plus any edits made since), and a
// 404 means this event has no brief yet, so one should be generated.
//
// A 404 is an ORDINARY outcome, not a failure — first-time generation is the common case.
// This endpoint never generates or mutates anything; regenerating is an explicit
// update-brief call, so a marketer's edits to the AI output are never silently clobbered.
func (s *BriefService) FindBrief(ctx context.Context, p *briefs.FindBriefPayload) (*briefs.Brief, error) {
	briefRepo, _, _, _, err := s.ready()
	if err != nil {
		return nil, err
	}
	b, err := briefRepo.FindBriefByEventSlug(ctx, p.ProjectID, p.EventSlug)
	if err != nil {
		return nil, mapBriefErr(err)
	}
	return briefResult(b), nil
}

func (s *BriefService) GetBrief(ctx context.Context, p *briefs.GetBriefPayload) (*briefs.Brief, error) {
	briefRepo, _, _, _, err := s.ready()
	if err != nil {
		return nil, err
	}
	b, err := briefRepo.GetBrief(ctx, p.ProjectID, p.BriefID)
	if err != nil {
		return nil, mapBriefErr(err)
	}
	return briefResult(b), nil
}

func (s *BriefService) UpdateBrief(ctx context.Context, p *briefs.UpdateBriefPayload) (*briefs.Brief, error) {
	briefRepo, _, _, _, err := s.ready()
	if err != nil {
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
		// Only updated_by moves on an edit; created_by is untouched by the UPDATE and
		// keeps naming the original author.
		UpdatedBy: attributedActor(ctx, "update brief"),
	}
	updated, uerr := briefRepo.ReplaceBrief(ctx, b, version, s.briefIndexPayload(indexer.ActionUpdated))
	if uerr != nil {
		return nil, mapBriefErr(uerr)
	}
	return briefResult(updated), nil
}

func (s *BriefService) ApproveBrief(ctx context.Context, p *briefs.ApproveBriefPayload) (*briefs.Brief, error) {
	briefRepo, _, _, _, err := s.ready()
	if err != nil {
		return nil, err
	}
	version, err := parseBriefIfMatch(p.IfMatch)
	if err != nil {
		return nil, err
	}
	b, aerr := briefRepo.Approve(ctx, p.ProjectID, p.BriefID, attributedActor(ctx, "approve brief"), version, s.briefIndexPayload(indexer.ActionUpdated))
	if aerr != nil {
		return nil, mapBriefErr(aerr)
	}
	return briefResult(b), nil
}

func (s *BriefService) DeleteBrief(ctx context.Context, p *briefs.DeleteBriefPayload) error {
	briefRepo, _, _, _, err := s.ready()
	if err != nil {
		return err
	}
	// ArchiveBrief RETURNS the archived row, so the published document is exactly what was
	// committed. Reading separately would race a concurrent ReplaceBrief/Approve landing
	// between the read and the archive: the archive would apply to the newer row while the
	// index received the older snapshot, with a hand-incremented version that never existed.
	// The index message is built INSIDE the archive transaction and co-committed to the
	// outbox, so a dropped publish is recoverable by the relay. Archiving is terminal: without
	// this, one lost message leaves the brief searchable forever.
	_, aerr := briefRepo.ArchiveBrief(ctx, p.ProjectID, p.BriefID, attributedActor(ctx, "archive brief"), s.briefIndexPayload(indexer.ActionDeleted))
	if aerr != nil {
		return mapBriefErr(aerr)
	}
	return nil
}

// ─── Campaigns ───

func (s *BriefService) CreateCampaigns(ctx context.Context, p *briefs.CreateCampaignsPayload) (*briefs.JobCreateResponse, error) {
	briefRepo, _, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	// Campaign creation stamps project_id into the campaign name (the attribution join
	// key) AND uses it as the exact-match key for the connection lookup at dispatch, so
	// a UUID-scoped request would break the slug-based attribution join and never match
	// a slug-keyed connection. Reject a non-slug scope up front; every dispatcher then
	// receives a guaranteed-canonical slug.
	if err := validateProjectSlug(p.ProjectID); err != nil {
		return nil, err
	}
	brief, err := briefRepo.GetBrief(ctx, p.ProjectID, p.BriefID)
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
	jobID, err := orch.Start(ctx, brief, brief.Version, platforms, marshalAny(p.Input.Config))
	if err != nil {
		return nil, mapBriefErr(err)
	}
	queued := "queued"
	return &briefs.JobCreateResponse{JobID: jobID, Status: queued, Platforms: p.Input.Platforms}, nil
}

func (s *BriefService) GetCampaign(ctx context.Context, p *briefs.GetCampaignPayload) (*briefs.Campaign, error) {
	_, campaignRepo, _, _, err := s.ready()
	if err != nil {
		return nil, err
	}
	c, err := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if err != nil {
		return nil, mapBriefErr(err)
	}
	return campaignResult(c), nil
}

// GetCampaignMetrics reads live performance metrics for a campaign directly from its ad
// platform. Unlike GetCampaign, this is a pure read: nothing is persisted, so there is no
// If-Match/version to check.
func (s *BriefService) GetCampaignMetrics(ctx context.Context, p *briefs.GetCampaignMetricsPayload) (*briefs.CampaignMetrics, error) {
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	// Default when no window is specified by the caller is platform-aware, not a single
	// global constant: X Ads' stats endpoint caps queryable date ranges at 7 days per
	// request (internal/platform/twitter, internal/dispatch/twitter.go), so last_30_days
	// — otherwise the documented default — is unreachable for that platform and would
	// turn every omitted-window request into a guaranteed 400. See
	// defaultMetricsWindowFor and docs/knowledge/code/internal-service.md.
	window := defaultMetricsWindowFor(existing.Platform)
	if p.Window != nil {
		window = model.MetricsWindow(*p.Window)
		if !model.IsValidMetricsWindow(window) {
			return nil, &briefs.BadRequestError{Code: "400", Message: "window must be one of: today, yesterday, last_7_days, last_14_days, last_30_days, this_month, last_month"}
		}
	}
	m, merr := orch.ReadCampaignMetrics(ctx, p.ProjectID, existing.Platform, existing, window)
	if merr != nil {
		switch {
		case errors.Is(merr, ErrMetricsUnsupported):
			return nil, &briefs.BadRequestError{Code: "400", Message: "metrics reads are not supported for this campaign's platform"}
		case errors.Is(merr, ErrMetricsWindowUnsupported):
			// merr's wrapped detail (from the adapter) is logged server-side, not concatenated
			// into the client-facing message: an adapter error can carry internal detail (a
			// platform API's own error text, an allow-list of internal literals) that isn't
			// meant for an API client.
			slog.WarnContext(ctx, "campaign metrics window unsupported by platform",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "window", window, "error", safeErrSummary(merr))
			// Provide platform-specific guidance on window support (e.g., X Ads' 7-day limit)
			msg := "this window is not supported for the campaign's platform"
			if existing.Platform == model.ProviderTwitterAds {
				msg = "X Ads supports only today, yesterday, and last_7_days windows (API cap: 7-day queryable range)"
			}
			return nil, &briefs.BadRequestError{Code: "400", Message: msg}
		case errors.Is(merr, ErrCampaignNotProvisioned):
			return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned — it has no platform campaign id yet"}
		case errors.Is(merr, domain.ErrNoMetricsInWindow):
			// A successful read of nothing. Kept out of the 503 default deliberately: the
			// email channel stages a DRAFT, so this is what every read before the human
			// presses send looks like, and calling that an ad-platform outage would send an
			// operator to investigate a healthy integration. The message enumerates the
			// three indistinguishable causes rather than picking one, because the upstream
			// response genuinely cannot separate them.
			slog.InfoContext(ctx, "campaign metrics read returned no data for the window",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "window", string(window))
			return nil, &briefs.ConflictError{Code: "409", Message: "the platform reported no data for this campaign in the requested window — it may not have run inside the window, may not have been sent or started yet, or may no longer exist upstream"}
		case errors.Is(merr, domain.ErrCampaignProvenanceUnknown):
			// Split out from the general mismatch arm below, and placed ABOVE it: this row
			// does not name a tenant to be mismatched against, so "reconnect the original
			// account" tells the operator to point the connection back at a tenant that was
			// never recorded — an instruction they cannot follow, because there is nothing
			// to reconnect to. The only way to give the row a provenance is to re-dispatch
			// it, which is the state every campaign written before provenance tracking
			// existed is in.
			slog.WarnContext(ctx, "campaign metrics read blocked: campaign does not record which platform tenant it was created under",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "error", safeErrSummary(merr))
			return nil, &briefs.ConflictError{Code: "409", Message: "this campaign does not record which platform account it was created under, so its metrics cannot be resolved safely — it must be re-dispatched before it can be read"}
		case errors.Is(merr, ErrCampaignAccountMismatch):
			// The two customer ids stay server-side: which ad account a project is connected
			// to is connection configuration, not something a metrics reader needs told.
			//
			// The LOG is scrubbed too, for a separate reason. merr embeds
			// client.CustomerID(), which comes from the connection's account_id — a design
			// attribute with no Pattern, MaxLength, or charset constraint (unlike Meta's
			// act_<digits> or X's alphanumeric ids). This guard also runs BEFORE any request,
			// so the client's own validateAccountIDs has not executed for this instance yet.
			// The value reaching this line is therefore arbitrary operator-supplied text, and
			// safeErrSummary is what keeps it from being written verbatim into a log record.
			//
			// Worded "account" rather than "ad account" because this arm serves the EMAIL
			// channel as well, where the mismatch is a HubSpot portal and the operator has no
			// ad account to reconnect. Naming a thing they do not have turns a correct refusal
			// into an instruction they cannot follow. The toggle arm below keeps "ad account":
			// no email dispatcher implements a status toggle, so it genuinely is ads-only.
			slog.WarnContext(ctx, "campaign metrics read blocked: campaign belongs to a different platform account than the current connection",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "error", safeErrSummary(merr))
			return nil, &briefs.ConflictError{Code: "409", Message: "the campaign belongs to a different account than this project's current connection — reconnect the original account to read its metrics"}
		case errors.Is(merr, domain.ErrSystemConnectionNotUsable):
			// The project has no connection of its own and the LF system row it fell back to
			// is unusable. This arm must sit ABOVE both arms below, because systemScoped
			// WRAPS rather than replaces: errors.Is still reports ErrConnectionNotUsable (and
			// ErrAccountNotSelected where that applies), so a broad match would win and hand
			// back a 409 telling this caller to repair "this project's connection" — which
			// they do not have, and which names a scope they cannot address. That misdirection
			// is the entire reason the sentinel exists, so failing to inspect it here makes
			// the tag decorative.
			//
			// Nobody but an operator can act, so page one and tell the caller nothing
			// specific. The reason token is safe to log; the error itself is not, for the
			// reason spelled out at unusableConnectionReason.
			slog.ErrorContext(ctx, "the LF system connection is not usable; campaign metrics reads are failing for every project without its own connection",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "reason", unusableConnectionReason(merr))
			return nil, &briefs.InternalServerError{Code: "500", Message: "campaign metrics could not be read"}
		case errors.Is(merr, domain.ErrAccountNotSelected):
			// Split out from the general unusable-connection arm below, and placed ABOVE it,
			// because ErrAccountNotSelected is always wrapped alongside ErrConnectionNotUsable
			// — a broad match would swallow it and the caller would be told "select an account
			// or repair the credentials" for a connection whose credentials are fine.
			//
			// The distinction is carried in the MESSAGE, not a separate field: ConflictError is
			// a shared Goa type with exactly code and message (design/brief.go), so there is no
			// machine-readable reason to populate without changing a type every 409 in this
			// service returns. The reason token still reaches operators through the log.
			slog.WarnContext(ctx, "campaign metrics read blocked: no ad account selected on the project's connection",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "reason", unusableConnectionReason(merr))
			return nil, &briefs.ConflictError{Code: "409", Message: "this project's ad-platform connection has no ad account selected — choose one from the connection's accounts endpoint and save it before reading metrics"}
		case errors.Is(merr, domain.ErrConnectionNotUsable):
			// Everything else that makes the connection unusable: inactive, credentials
			// absent/incomplete/malformed, provider config invalid. The platform was never
			// contacted and never will be until a human edits the connection, so the 503 below
			// would be a false promise — it tells the caller to retry a request that cannot
			// succeed with time alone.
			//
			// "channel", not "ad platform": HubSpot's resolveHubSpotClient tags the same three
			// reasons (inactive, credentials undecodable, credentials incomplete) with this
			// sentinel, so an email connection reaches this arm too — naming an ad platform
			// would send that caller to check a system they never connected.
			//
			// Logged with the fixed reason token rather than the error, for the reason
			// spelled out at unusableConnectionReason: one of the conditions behind this
			// sentinel is detected by decoding the DECRYPTED credential blob.
			slog.WarnContext(ctx, "campaign metrics read blocked: the project's connection is not usable",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "reason", unusableConnectionReason(merr))
			return nil, &briefs.ConflictError{Code: "409", Message: "this project's channel connection is not ready — its stored credentials or provider settings need attention; repair the connection before reading metrics"}
		default:
			// "channel", not "ad platform": HubSpot reaches this arm too, and a message
			// naming an ad platform on an email read tells the caller to check a system
			// they never connected.
			slog.WarnContext(ctx, "campaign metrics read failed on the channel",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID, "error", safeErrSummary(merr))
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "campaign metrics could not be read from the campaign's channel"}
		}
	}
	return &briefs.CampaignMetrics{
		CampaignID:         existing.ID,
		PlatformCampaignID: existing.PlatformCampaignID,
		// The validated request window, not m.Window: adapters are not required to echo it
		// back, and trusting them would emit "" for one that doesn't, violating the response
		// enum and causing generated clients to reject an otherwise successful 200.
		Window:      string(window),
		Impressions: m.Impressions,
		Clicks:      m.Clicks,
		CostMicros:  m.CostMicros,
		Ctr:         m.Ctr,
		Email:       emailMetricsResult(m.Email),
	}, nil
}

// emailMetricsResult renders the email-channel counters, or nil for an ad platform. Kept a
// function rather than an inline conditional so the nil case is the one the type system
// enforces: the ad adapters never populate m.Email, and a nil dereference here would turn
// every ad-platform metrics read into a 500.
func emailMetricsResult(e *model.EmailMetrics) *briefs.EmailMetrics {
	if e == nil {
		return nil
	}
	return &briefs.EmailMetrics{
		Sent:         e.Sent,
		Delivered:    e.Delivered,
		Opens:        e.Opens,
		Clicks:       e.Clicks,
		Bounces:      e.Bounces,
		Unsubscribes: e.Unsubscribes,
	}
}

// defaultMetricsWindowFor returns the window GetCampaignMetrics uses when the caller omits
// the window parameter, for the given platform. last_30_days is the default for every
// platform except X Ads: its stats endpoint caps queryable date ranges at 7 days per
// request, so last_30_days always fails there (twitterMetricsWindow only maps yesterday,
// today, and last_7_days). Falling through to last_30_days for any platform not listed here is
// intentional — a future platform with its own similarly narrow window support should add
// a case rather than silently omit one and rediscover this the same way.
func defaultMetricsWindowFor(platform model.Provider) model.MetricsWindow {
	switch platform {
	case model.ProviderTwitterAds:
		return model.MetricsWindowLast7Days
	default:
		return model.MetricsWindowLast30Days
	}
}

func (s *BriefService) UpdateCampaign(ctx context.Context, p *briefs.UpdateCampaignPayload) (*briefs.Campaign, error) {
	_, campaignRepo, _, _, err := s.ready()
	if err != nil {
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
	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	// Check the If-Match version against the LOADED row BEFORE the status-mismatch
	// check below. Without this, a stale If-Match is validated against a row the
	// client never saw: a concurrent ToggleCampaignStatus can flip existing.Status
	// between the client's read and this request, and the status-mismatch check
	// would then compare the client's (now-stale) status field against the NEW
	// existing.Status and return 400 ("use the status-toggle endpoint") for what is
	// actually a stale-ETag conflict — misreporting a 412 as a 400. Mirrors
	// UpdateAudience's early version check for the same reason.
	if existing.Version != version {
		return nil, &briefs.PreconditionFailedError{Code: "412", Message: "the supplied ETag does not match the current version"}
	}
	// This DB-only update MUST NOT be a back door for changing the RUN status
	// (active/paused): that would persist a run state WITHOUT contacting the ad platform,
	// recreating exactly the DB/platform divergence the ToggleCampaignStatus endpoint exists
	// to prevent. `status` stays in the payload so a client can round-trip the row it read
	// (a name/config edit re-sends the current status unchanged), but an ATTEMPT to flip the
	// run state here is rejected and routed to the platform toggle.
	//
	// This path NEVER writes status, so ANY mismatch must be refused, not just a run-state one:
	// returning 200 for a PUT whose required `status` field was not applied would tell the
	// caller a replacement succeeded when it silently did not. Run-state attempts get the
	// specific toggle-endpoint message; every other mismatch (a provisioning state, or an
	// unknown value) is rejected as unsupported on this path.
	//
	// VALIDATION MUST HAPPEN BEFORE CLAIMING. A rejected request has no business taking the
	// campaign's write lock: claiming acquires a dedicated pooled connection and blocks every
	// other writer for this campaign until it is released, so validating first keeps an
	// invalid request from queueing behind — or ahead of — legitimate writers. (The claim
	// itself does not bump the version; only ReplaceCampaign does. So the ETag a rejected
	// caller holds stays valid either way.)
	if p.Campaign.Status != existing.Status {
		if model.IsCampaignRunStatus(p.Campaign.Status) {
			return nil, &briefs.BadRequestError{Code: "400", Message: "run status (active/paused) cannot be changed via update-campaign; use the status-toggle endpoint so the change is applied on the ad platform first"}
		}
		return nil, &briefs.BadRequestError{Code: "400", Message: "status cannot be changed via update-campaign; re-send the campaign's current status (it is set by the create/dispatch flow and the status-toggle endpoint)"}
	}
	// Claim write ownership at the read version BEFORE building the edit, not just
	// at the final ReplaceCampaign. This path itself has no I/O in between, so a
	// bare ReplaceCampaign(version) would already be race-free against another
	// UpdateCampaign — but not against a concurrent ToggleCampaignStatus, which
	// claims its own version before calling the ad platform. Without this claim, a
	// name/config edit landing in that toggle's claim-to-persist window would win
	// the version the toggle already reserved, so the toggle's own post-platform
	// persist then loses — the platform and the row diverge even though this edit
	// and that toggle were each individually consistent. Claiming here makes every
	// campaign writer serialize through the same version-gated mutex.
	_, lockToken, cerr := campaignRepo.ClaimCampaignVersion(ctx, p.ProjectID, p.BriefID, p.CampaignID, version)
	if cerr != nil {
		return nil, mapBriefErr(cerr)
	}
	// Ensure lock is released even if a panic or context cancellation occurs.
	defer func() { _ = campaignRepo.ReleaseCampaignLock(ctx, lockToken) }()

	existing.CampaignName = p.Campaign.CampaignName
	// Only overwrite the stored config when the caller actually supplied one.
	// config is optional in CampaignUpdateInput, so an omitted value must leave
	// the existing ConfigSnapshot intact rather than wiping it to NULL on a
	// name/status-only edit (the GET response doesn't expose config, so a client
	// can't round-trip it back).
	if p.Campaign.Config != nil {
		existing.ConfigSnapshot = marshalAny(p.Campaign.Config)
	}
	existing.UpdatedBy = attributedActor(ctx, "update campaign")
	// Gate the final write on the original claimed version. The claim acquired
	// the lock but did NOT bump the version; ReplaceCampaign will bump it
	// (from version to version+1) inside the outbox transaction, preserving the
	// invariant that every campaign write co-commits its index event.
	updated, uerr := campaignRepo.ReplaceCampaign(ctx, existing, version, lockToken, s.campaignIndexPayload(indexer.ActionUpdated))
	if uerr != nil {
		return nil, mapBriefErr(uerr)
	}
	return campaignResult(updated), nil
}

// unconfirmedLockCooldown bounds how long ToggleCampaignStatus holds the claim lock after an
// UNCONFIRMED platform outcome before letting another writer claim the same (still-unbumped)
// version. It gives an operator or the caller's own retry a window to verify the platform's
// actual state before a second call can race a possibly-already-applied change.
const unconfirmedLockCooldown = 30 * time.Second

// ToggleCampaignStatus pauses/resumes a campaign ON THE AD PLATFORM, then persists the new
// status. Unlike UpdateCampaign (DB-only), the platform call happens FIRST — the row is
// updated only after the platform confirms, so a persisted "paused" always reflects reality.
func (s *BriefService) ToggleCampaignStatus(ctx context.Context, p *briefs.ToggleCampaignStatusPayload) (*briefs.Campaign, error) {
	_, campaignRepo, _, orch, err := s.ready()
	if err != nil {
		return nil, err
	}
	version, err := parseBriefIfMatch(p.IfMatch)
	if err != nil {
		return nil, err
	}
	// The design enum restricts status to active/paused, but validate defensively so a
	// direct (non-generated) caller can't push an unsupported value to a platform.
	if p.Status != model.CampaignRunActive && p.Status != model.CampaignRunPaused {
		return nil, &briefs.BadRequestError{Code: "400", Message: "status must be 'active' or 'paused'"}
	}

	existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}
	// Check the If-Match version against the LOADED row BEFORE any state checks or platform
	// calls. Without this, a stale If-Match is validated against a row the client never saw:
	// a concurrent UpdateCampaign can change existing.Status or other fields between the
	// client's read and this request, and the status-mismatch check below would then compare
	// the client's (now-stale) status field against the NEW existing.Status and return 400
	// ("use the status-toggle endpoint") for what is actually a stale-ETag conflict —
	// misreporting a 412 as a 400.
	if existing.Version != version {
		return nil, &briefs.PreconditionFailedError{Code: "412", Message: "the supplied ETag does not match the current version"}
	}

	// Only a fully-created campaign (or one already in a run state) may be ACTIVATED. A
	// "pending" ambiguous orphan or a "created_degraded" campaign (a sub-step still needs
	// reconciliation) must not be: doing so would put an incomplete campaign in front of an
	// audience. A non-empty PlatformCampaignID alone is not enough — a degraded/partial
	// campaign can carry an upstream id. Reject with 409 (the state must be reconciled first).
	//
	// PAUSE is the exception, and only for created_degraded. That status means the campaign
	// definitely EXISTS upstream and this service does not know its full wiring — and an
	// ADOPTED campaign (LFXV2-3042) can already be ENABLED and spending, because the adoption
	// lookup treats ENABLED and PAUSED alike as live. Refusing to pause those made the one
	// campaign most likely to need stopping the one campaign this service could not stop,
	// while the dispatchers explicitly support pausing a campaign with no child ids (see
	// GoogleAdsDispatcher.ToggleStatus). A pause costs nothing this guard was protecting: it
	// cannot activate anything, and the reconciliation marker is preserved below rather than
	// overwritten with the run state.
	//
	// The other markers stay refused in BOTH directions: 'pending' and the partial-orphan
	// statuses may have no upstream campaign at all, so there is nothing for a pause to act on.
	pauseDegraded := existing.Status == model.CampaignStatusCreatedDegraded && p.Status == model.CampaignRunPaused
	if !model.CampaignStatusToggleable(existing.Status) && !pauseDegraded {
		msg := "campaign is not in a toggleable state (it is still provisioning or needs reconciliation); resolve its status before toggling"
		if existing.Status == model.CampaignStatusCreatedDegraded {
			msg = "campaign still needs reconciliation, so it cannot be activated; it can be PAUSED to stop any spend, but resolve its status before resuming it"
		}
		return nil, &briefs.ConflictError{Code: "409", Message: msg}
	}

	// VALIDATION MUST HAPPEN BEFORE CLAIMING, for the same reason as UpdateCampaign above:
	// claiming takes the campaign's write lock on a dedicated pooled connection and blocks
	// every other writer for this campaign until release, so a request that is going to be
	// rejected anyway must never take it. (The claim leaves the version unchanged — only
	// ReplaceCampaign bumps it — so a rejected caller's ETag survives either way.) Check
	// platform-independent errors here; platform-specific errors (like UNCONFIRMED from
	// transport) must still go through the platform call.
	if existing.Platform.Kind() == model.ChannelEmail {
		return nil, &briefs.BadRequestError{Code: "400", Message: "status toggle does not apply to the email channel: it stages a draft for a human to send, so there is no running campaign to pause or resume"}
	}
	if existing.PlatformCampaignID == "" {
		return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned for activation — it has no platform campaign id yet, or it lacks the child entities needed to serve (e.g. its ad group/ad, ad set, or creatives); finish or recreate the campaign before toggling its status"}
	}

	// Claim write ownership at the read version BEFORE the (side-effecting, paid) platform
	// call, atomically via the repository rather than by comparing existing.Version in memory.
	// An in-memory comparison only rejects THIS caller if it's already stale; it does nothing
	// to stop a second concurrent caller (another toggle, or UpdateCampaign) that read the
	// same version from also passing its own check and also mutating the platform — both would
	// then race the final persist, and only one wins, leaving the platform and the row
	// diverged with no compensating rollback. The claim closes that: a second caller is turned
	// away HERE, before either the platform is called or a claim is granted for anyone else to
	// race against.
	//
	// "Turned away", not "queued". The claim is pg_try_advisory_lock, so the loser gets
	// ErrCampaignWriteInProgress immediately — mapped to a retryable 409 — and does NOT park
	// until the holder releases. A client that wants the write retries as a fresh request,
	// which is deliberate: parking would pin a pool connection for the length of someone
	// else's platform call. See ClaimCampaignVersion's POOL COST note.
	//
	// And "turned away", not "can never win". The claim is a contention guard whose exclusion
	// lasts only as long as the lock's session — a failover or a severed connection releases it
	// server-side while this call is still inside its platform call, and a successor can then
	// claim the same still-unbumped version (see ClaimCampaignVersion's durability boundary).
	// What makes that safe is not this claim but the compare-and-swap in the ReplaceCampaign
	// below: whichever writer commits first bumps the version and the other is rejected, so a
	// lost lock costs at most one duplicated declarative platform call, never two persisted
	// writes at the same version.
	_, lockToken, cerr := campaignRepo.ClaimCampaignVersion(ctx, p.ProjectID, p.BriefID, p.CampaignID, version)
	if cerr != nil {
		return nil, mapBriefErr(cerr)
	}
	// Defer lock release: it MUST NOT be released until after persistence, and it MUST use
	// a detached context because persistence will run on context.WithoutCancel after the
	// platform call succeeds (to persist even if the request is cancelled). ReleaseCampaignLock
	// internally uses context.WithoutCancel to guarantee the unlock runs even if the caller's
	// context is cancelled.
	//
	// releaseNow is turned off on the UNCONFIRMED path below, which schedules its own delayed
	// release instead — releasing inline there would let the next request to arrive claim the
	// same still-unbumped version and call the platform again while this call's outcome is
	// unknown. During the cooldown those requests are refused with 409, not queued.
	releaseNow := true
	defer func() {
		if releaseNow {
			_ = campaignRepo.ReleaseCampaignLock(ctx, lockToken)
		}
	}()

	// Platform-side toggle FIRST. On failure the row is left untouched (no optimistic
	// lie that the campaign is paused when the platform still has it running).
	if terr := orch.ToggleCampaignStatus(ctx, p.ProjectID, existing.Platform, existing, p.Status); terr != nil {
		var unconfirmed interface{ Unconfirmed() bool }
		switch {
		case errors.Is(terr, ErrToggleUnsupported):
			// The platform (or its dispatcher) doesn't support toggling — a client error,
			// the platform was never called. This is a platform-specific error that should
			// have been caught earlier if the dispatcher wiring was complete; it indicates
			// the toggle capability is not wired yet for this provider.
			return nil, &briefs.BadRequestError{Code: "400", Message: "status toggle is not supported for this campaign's platform"}
		case errors.Is(terr, ErrCampaignNotProvisioned):
			// The campaign is not fully provisioned for this toggle — either it has no upstream
			// platform id yet (still creating / ambiguous create), or it lacks the child entities
			// needed to serve (on ACTIVATE) or to reach for status changes (on PAUSE). The specific
			// missing entity is platform-dependent (Reddit: an ad group/ad; Meta: an ad set;
			// LinkedIn: creatives; Microsoft: ad group/ad for activate, only ad-group for orphan ad
			// on pause), so the message stays platform-NEUTRAL rather than naming one provider's
			// shape for all. A client/state error, NOT a platform rejection: a retry now would fail
			// the same way, so this is a 409, not a 503. It avoids "wait" (a campaign missing a
			// child never gains one by waiting) and points at the actual remedy.
			return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned — it has no platform campaign id yet, or it lacks the child entities needed to change its status (e.g. its ad group/ad, ad set, or creatives); finish or recreate the campaign before toggling its status"}
		case errors.Is(terr, ErrCampaignAccountMismatch):
			// The campaign belongs to a different ad account than the project's current
			// connection, so the mutation was refused BEFORE the platform was contacted —
			// nothing changed upstream, and a retry would be refused identically, so this is a
			// non-retryable 409 rather than a 503 or an UNCONFIRMED outcome. The two customer
			// ids stay server-side (connection configuration, not client business), and the
			// log goes through safeErrSummary for the same unvalidated-account_id reason
			// spelled out on the metrics branch above.
			slog.WarnContext(ctx, "campaign status toggle blocked: campaign belongs to a different ad account than the current connection",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "status", p.Status, "error", safeErrSummary(terr))
			return nil, &briefs.ConflictError{Code: "409", Message: "the campaign belongs to a different ad account than this project's current connection — reconnect the original account to change its status"}
		case errors.Is(terr, domain.ErrSystemConnectionNotUsable):
			// Same placement and same reason as the metrics branch: systemScoped WRAPS the
			// usability sentinels rather than replacing them, so either arm below would match
			// first and tell a caller with no connection of their own to repair "this
			// project's connection". Only an operator can act on the LF system row.
			//
			// Note this is reached BEFORE the unconfirmed check on purpose. Credential
			// resolution refused the connection before the platform was contacted, so there is
			// no ambiguous mutation to protect — nothing was sent, and the campaign's stored
			// status is still correct.
			slog.ErrorContext(ctx, "the LF system connection is not usable; campaign status toggles are failing for every project without its own connection",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "status", p.Status, "reason", unusableConnectionReason(terr))
			return nil, &briefs.InternalServerError{Code: "500", Message: "the campaign status could not be changed"}
		case errors.Is(terr, domain.ErrAccountNotSelected):
			// Above the general arm for the reason given on the metrics branch: the sentinel is
			// always wrapped alongside ErrConnectionNotUsable, so a broad match would swallow
			// it and hand back the ambiguous "or its credentials need attention" message for a
			// connection whose credentials are fine. The distinction rides in the message
			// because ConflictError carries only code and message.
			slog.WarnContext(ctx, "campaign status toggle blocked: no ad account selected on the project's connection",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "status", p.Status, "reason", unusableConnectionReason(terr))
			return nil, &briefs.ConflictError{Code: "409", Message: "this project's ad-platform connection has no ad account selected — choose one from the connection's accounts endpoint and save it before changing campaign status"}
		case errors.Is(terr, domain.ErrConnectionNotUsable):
			// Credential resolution refused the connection BEFORE the platform was contacted,
			// so — like the branches above — nothing changed upstream and this is decidable
			// without asking the platform.
			//
			// This must sit ABOVE the unconfirmed check as well as the default: nothing on this
			// path is ambiguous, and the default's 503 would tell the caller to retry a request
			// that cannot succeed until a human edits the connection.
			//
			// Logged with the fixed reason token, not the error, for the reason at
			// unusableConnectionReason: one condition behind this sentinel is detected by
			// decoding the DECRYPTED credential blob.
			slog.WarnContext(ctx, "campaign status toggle blocked: the project's connection is not usable",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "status", p.Status, "reason", unusableConnectionReason(terr))
			return nil, &briefs.ConflictError{Code: "409", Message: "this project's ad-platform connection is not ready — its stored credentials or provider settings need attention; repair the connection before changing campaign status"}
		case errors.As(terr, &unconfirmed) && unconfirmed.Unconfirmed():
			// UNCONFIRMED: a transport/5xx/redirect error means the PATCH MAY already have
			// applied on the platform. Do NOT say "not modified" (it might be) and do NOT
			// blindly write the DB (it might not be) — surface it as UNCONFIRMED so the
			// caller verifies before retrying (mirrors the creation path's contract), and log
			// it as a reconcile signal. The row is left at its prior status.
			slog.WarnContext(ctx, "campaign status toggle outcome is UNCONFIRMED (the platform may or may not reflect the change)",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
				"requested_status", p.Status, "error", terr)
			// The row is deliberately left untouched here (it might already be right, or not
			// — see the test asserting this). But releasing the claim lock immediately, as the
			// deferred release above would, lets a second caller already waiting on it claim
			// the SAME still-unbumped expectedVersion right away and call the platform again
			// while THIS call's outcome is still unknown — doubling up on an already-ambiguous
			// change. Hold the lock for a bounded cooldown instead of releasing it inline: skip
			// the deferred release for this path and let the repo release it asynchronously
			// after unconfirmedLockCooldown. This gives an operator/retry a window to reconcile
			// before any other writer can proceed, without persisting anything (a crash before
			// the cooldown elapses still releases the lock immediately — it's a Postgres session
			// lock, so it drops the moment the holding connection closes). The repo's cooldown
			// release also cuts short at process shutdown instead of holding its pooled
			// connection for the full 30s: see ReleaseCampaignLockAfterCooldown.
			releaseNow = false
			campaignRepo.ReleaseCampaignLockAfterCooldown(lockToken, unconfirmedLockCooldown)
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the campaign status change is unconfirmed — it may or may not have been applied on the ad platform; verify in the platform before retrying"}
		default:
			// A DEFINITE platform-call failure (4xx) or the dispatcher's cred resolution
			// failing: the ad platform was not updated. Log the underlying error (the client
			// gets only a sanitized message) so an operator has a diagnostic record, then 503.
			slog.WarnContext(ctx, "campaign status toggle failed on the ad platform",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
				"requested_status", p.Status, "error", terr)
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the campaign status could not be changed on the ad platform; the campaign was not modified"}
		}
	}

	// The platform change ALREADY committed. The DB row MUST catch up even if the request
	// context is now cancelled (client disconnect / shutdown) — otherwise the platform is
	// paused while the row still says active, a silent divergence with no compensating
	// rollback (the ad platform is the source of truth here). Persist on a cancel-detached
	// context so the write completes; the read/guard above already ran on the live ctx. The
	// detached write is BOUNDED by persistResultTimeout (mirrors the orchestrator's
	// post-provider persists) so a stuck DB can't hang shutdown grace indefinitely.
	if pauseDegraded {
		// The pause committed upstream, and the row is deliberately NOT rewritten. Overwriting
		// 'created_degraded' with 'paused' would erase the only record that this campaign's
		// wiring is unverified, and pausing reconciles nothing — it stops spend. So the status
		// this endpoint would normally persist is exactly the one that must not be persisted
		// here; the campaign comes back at its unchanged status and version because that is
		// what the row now says. The platform call is declarative, so a repeat pause is a
		// no-op upstream and this stays idempotent without a version to compare.
		//
		// DURABILITY: before returning success with the platform changed, verify that the
		// row version has not changed since we claimed it. If the claimed connection died
		// and a successor modified the row, surface the platform/DB divergence instead of
		// returning stale data. Use the live context (not persistCtx) for the verification:
		// the claim was also acquired on the live context, so the two share a consistent
		// view and this check can tell if the row was modified during this request.
		verified, verifyErr := campaignRepo.VerifyClaimedVersion(
			ctx, p.ProjectID, p.BriefID, p.CampaignID, version, lockToken)
		if verifyErr != nil {
			if errors.Is(verifyErr, domain.ErrPreconditionFailed) {
				// The row's version changed since we claimed it. The platform was updated
				// but the local row was modified by someone else — a divergence that should
				// not happen under normal operation, but must be surfaced (it is NOT a 409
				// retry; the caller got the platform change but has a stale row).
				slog.ErrorContext(ctx, "campaign status changed on the platform but the DB row was modified by another writer (platform/DB diverged)",
					"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
					"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
					"requested_status", p.Status, "expected_version", version)
				return nil, &briefs.ConflictError{Code: "409", Message: "this campaign was modified by another request while its status was being changed on the ad platform; verify the campaign status before retrying"}
			}
			if errors.Is(verifyErr, domain.ErrNotFound) {
				// The row was deleted between claim and now. The platform was changed but
				// the local row is gone — another kind of divergence.
				slog.ErrorContext(ctx, "campaign status changed on the platform but the DB row was deleted (platform/DB diverged)",
					"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
					"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
					"requested_status", p.Status, "error", verifyErr)
				return nil, &briefs.ConflictError{Code: "409", Message: "this campaign was deleted while its status was being changed on the ad platform; verify the campaign status before retrying"}
			}
			// A read error (transient DB failure, etc). Surface it so the caller retries
			// after backoff, not immediately.
			slog.ErrorContext(ctx, "failed to verify row version after platform pause (platform/DB divergence detection failed)",
				"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
				"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
				"error", verifyErr)
			return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the campaign status was changed on the ad platform, but verifying the local row failed; verify in the platform and retry"}
		}
		slog.InfoContext(ctx, "paused a campaign that still needs reconciliation; the reconciliation marker is preserved",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
			"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
			"status", existing.Status)
		return campaignResult(verified), nil
	}
	existing.Status = p.Status
	// Resolve the actor from the LIVE ctx, before persistCtx replaces it below. A
	// context.WithoutCancel derivative keeps the values, so this would work either way —
	// it is done here so the ordering does not become load-bearing if the detached
	// context is ever built some other way.
	existing.UpdatedBy = attributedActor(ctx, "toggle campaign status")
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistResultTimeout)
	defer cancel()
	// Gate the final write on the original claimed version. The claim acquired the lock but
	// did NOT bump the version; ReplaceCampaign will bump it (from version to version+1)
	// inside the outbox transaction, preserving the invariant that every campaign write
	// co-commits its index event.
	updated, uerr := campaignRepo.ReplaceCampaign(persistCtx, existing, version, lockToken, s.campaignIndexPayload(indexer.ActionUpdated))
	if uerr != nil {
		// The platform WAS changed but the row write failed → platform and DB now diverge.
		// Log it loudly as an operational reconcile signal (the run state on the platform is
		// authoritative; a monitor/human reconciles the stale row) before surfacing the error.
		slog.ErrorContext(ctx, "campaign status changed on the platform but the DB row write failed (platform/DB diverged)",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
			"platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
			"new_status", p.Status, "error", uerr)
		return nil, mapBriefErr(uerr)
	}
	// The index message co-committed with the row on persistCtx, so a cancelled request still
	// records BOTH the platform change and its index event — previously the row could be
	// written while the publish was dropped, leaving the status right in the database and
	// stale in search for exactly the requests most likely to need reconciling.
	return campaignResult(updated), nil
}

// DeleteCampaign soft-deletes a campaign LOCALLY, freeing its (brief, platform) slot
// so the brief can be re-dispatched to that platform.
//
// It deliberately does NOT touch the ad platform. Removing the local row while a real
// paid campaign keeps spending is the worst available outcome, so the alternative —
// deleting upstream — would need a verified delete/remove API on every provider
// adapter, and none of the platform clients in internal/platform implement one. Rather
// than invent an unverified upstream call, this endpoint is explicitly local-only and
// says so in its API description: a campaign already created upstream keeps running
// until it is stopped there (the status-toggle endpoint pauses it).
//
// That is also why the delete is SOFT: the row carries platform_campaign_id, the only
// local pointer to whatever may still exist upstream. Hard-deleting would free the slot
// but destroy the sole record needed to find and stop the campaign that is still
// spending — the audit trail matters most in exactly the case that motivates deleting.
func (s *BriefService) DeleteCampaign(ctx context.Context, p *briefs.DeleteCampaignPayload) error {
	_, campaignRepo, _, _, err := s.ready()
	if err != nil {
		return err
	}
	version, err := parseBriefIfMatch(p.IfMatch)
	if err != nil {
		return err
	}
	derr := campaignRepo.DeleteCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID, version,
		attributedActor(ctx, "delete campaign"), s.campaignIndexPayload(indexer.ActionDeleted))
	if errors.Is(derr, domain.ErrConflict) {
		// The repo returns ErrConflict only when the campaign's status is an unresolved
		// reconciliation marker — a mid-dispatch 'pending' claim, or a
		// 'group_created'/'unconfirmed' partial orphan. mapBriefErr would render that as
		// "the resource already exists", which describes a uniqueness violation and tells
		// the caller nothing actionable. Name the real cause and both remedies instead: an
		// in-flight dispatch clears on its own, an orphan needs reconciling.
		return &briefs.ConflictError{Code: "409", Message: "the campaign cannot be deleted while its dispatch is unresolved; if a dispatch is in flight, wait for it to finish and retry, otherwise the campaign is a partially-created orphan that must be reconciled first"}
	}
	return mapBriefErr(derr)
}

func (s *BriefService) GetJob(ctx context.Context, p *briefs.GetJobPayload) (*briefs.JobPollResponse, error) {
	_, _, jobRepo, _, err := s.ready()
	if err != nil {
		return nil, err
	}
	j, err := jobRepo.GetJob(ctx, p.ProjectID, p.JobID)
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
			Skipped    bool   `json:"skipped"`
		}
		if err := json.Unmarshal(j.Result, &stored); err != nil {
			// A persisted result that won't decode is corruption, not a valid empty
			// poll response. Silently dropping it would hand back a terminal
			// succeeded/partial job with NO per-platform results — an inaccurate
			// response that masks the corruption as success. Surface it as a 500.
			slog.ErrorContext(ctx, "failed to decode persisted job result", "job_id", j.ID, "error", err)
			return nil, &briefs.InternalServerError{Code: "500", Message: "an internal server error occurred"}
		}
		if len(stored) == 0 && (j.Status == model.JobSucceeded || j.Status == model.JobPartial) {
			// null/[] decode to an empty slice with len(j.Result) > 0, so they slip
			// past the outer length guard. A succeeded/partial job is an aggregate
			// over per-platform outcomes and MUST carry them; an empty decoded slice
			// on such a terminal status means the stored row is corrupt.
			slog.ErrorContext(ctx, "terminal job has empty per-platform results", "job_id", j.ID, "status", j.Status)
			return nil, &briefs.InternalServerError{Code: "500", Message: "an internal server error occurred"}
		}
		resp.Result = make([]*briefs.PlatformResult, 0, len(stored))
		for _, r := range stored {
			pr := &briefs.PlatformResult{Platform: r.Platform, OK: r.OK}
			if r.CampaignID != "" {
				id := r.CampaignID
				pr.CampaignID = &id
			}
			switch {
			case r.Skipped && !r.OK:
				// A SKIPPED platform is OK=false but is NOT a failure — a concurrent
				// dispatch already owns the (brief, platform) claim and is creating it.
				// The orchestrator persists a skip with BOTH Skipped=true AND a raw
				// internal Error string, so this case MUST be checked before the Error
				// case or the friendly message below is unreachable and polling leaks
				// the internal string. The generated PlatformResult has no dedicated
				// "skipped" field (a Goa design change / regen, tracked in LFXV2-2665),
				// so surface the deferral through Error with an explicit non-failure
				// message rather than leaving an unexplained ok=false that reads as a
				// silent failure.
				msg := "skipped: a concurrent request already owns this platform's campaign creation (not a failure)"
				pr.Error = &msg
			case r.Error != "":
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
	case errors.Is(err, domain.ErrCampaignWriteInProgress):
		// The claim is a try-lock, not a wait (see domain.ErrCampaignWriteInProgress). The
		// caller's ETag may be perfectly current, so 412 would send them off to refetch and
		// rebuild a request that was already correct — the right advice is simply to retry.
		return &briefs.ConflictError{Code: "409", Message: "another write to this campaign is already in progress; retry shortly"}
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

// errSummaryMaxRunes caps how much of an ad-platform error reaches structured logs.
// 200 runes is enough to identify the failure class (Graph's "(#100) Invalid parameter",
// an HTTP status line, a transport error) without letting an unbounded upstream body
// dominate a log record.
const errSummaryMaxRunes = 200

// safeErrSummary renders err for a log field after stripping the two properties that
// make a platform error unsafe to log verbatim: non-printable characters and unbounded
// length.
//
// The metrics failure path is platform-agnostic — every ReadMetrics implementation
// funnels through it — so it cannot assume each platform client has already scrubbed
// its response text. Meta's *meta.APIError.Error() in particular renders the Graph
// API's Message field verbatim, and the non-Graph fallback populates that field from
// the RAW response body (internal/platform/meta/client.go:589-612). Unbounded upstream
// text bloats log storage, and control characters make a record unreadable — or, in a
// LINE-ORIENTED sink, can split one record into several. That last effect is
// handler-dependent, not universal: slog's TextHandler and JSONHandler both quote and
// escape string values, so neither forges a line on its own. The guard is here because
// the sink is not this package's to choose, and the cost of normalising is a single
// pass over at most 200 runes.
//
// Newlines, tabs, carriage returns and every other non-graphic rune are replaced with
// U+FFFD rather than dropped, so the substitution is visible in the record instead of
// silently changing the text. Truncation is marked with a trailing ellipsis.
//
// This is deliberately scoped to the log call rather than to Meta's Error() itself:
// that method's raw-body behavior is pre-existing, tested, and relied on elsewhere for
// campaign-creation diagnostics.
func safeErrSummary(err error) string {
	if err == nil {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, r := range err.Error() {
		if n == errSummaryMaxRunes {
			b.WriteString("…")
			break
		}
		if unicode.IsGraphic(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(unicode.ReplacementChar)
		}
		n++
	}
	return b.String()
}
