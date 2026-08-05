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
	b, aerr := briefRepo.Approve(ctx, p.ProjectID, p.BriefID, actorFromCtx(ctx), version, s.briefIndexPayload(indexer.ActionUpdated))
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
	_, aerr := briefRepo.ArchiveBrief(ctx, p.ProjectID, p.BriefID, s.briefIndexPayload(indexer.ActionDeleted))
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
	// VALIDATION MUST HAPPEN BEFORE CLAIMING. If validation fails and returns 400, we must not
	// bump the version; otherwise a rejected request unexpectedly invalidates the caller's ETag.
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
	claimed, cerr := campaignRepo.ClaimCampaignVersion(ctx, p.ProjectID, p.BriefID, p.CampaignID, version)
	if cerr != nil {
		return nil, mapBriefErr(cerr)
	}
	// Ensure lock is released even if a panic or context cancellation occurs.
	defer campaignRepo.ReleaseCampaignLock(ctx, p.CampaignID) //nolint:errcheck
	existing.Version = claimed.Version
	existing.CampaignName = p.Campaign.CampaignName
	// Only overwrite the stored config when the caller actually supplied one.
	// config is optional in CampaignUpdateInput, so an omitted value must leave
	// the existing ConfigSnapshot intact rather than wiping it to NULL on a
	// name/status-only edit (the GET response doesn't expose config, so a client
	// can't round-trip it back).
	if p.Campaign.Config != nil {
		existing.ConfigSnapshot = marshalAny(p.Campaign.Config)
	}
	updated, uerr := campaignRepo.ReplaceCampaign(ctx, existing, version, s.campaignIndexPayload(indexer.ActionUpdated))
	if uerr != nil {
		return nil, mapBriefErr(uerr)
	}
	return campaignResult(updated), nil
}

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
	// Only a fully-created campaign (or one already in a run state) may be toggled. A
	// "pending" ambiguous orphan or a "created_degraded" campaign (a sub-step still needs
	// reconciliation) must NOT be toggled: doing so would activate an incomplete campaign
	// and/or OVERWRITE the reconciliation status with the run state, erasing the signal. A
	// non-empty PlatformCampaignID alone is not enough — a degraded/partial campaign can
	// carry an upstream id. Reject with 409 (the state must be reconciled first).
	if !model.CampaignStatusToggleable(existing.Status) {
		return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not in a toggleable state (it is still provisioning or needs reconciliation); resolve its status before toggling"}
	}

	// VALIDATION MUST HAPPEN BEFORE CLAIMING. If validation fails and returns 400/409, we
	// must not bump the version; otherwise a rejected request unexpectedly invalidates the
	// caller's ETag. A stale If-Match on retry would then get 412 instead of the original
	// validation error. Check platform-independent errors here; platform-specific errors
	// (like UNCONFIRMED from transport) must still go through the platform call.
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
	// diverged with no compensating rollback. The atomic claim closes that: only one caller can
	// ever win a given expectedVersion, so the loser is rejected here, before either the
	// platform is called or a claim is granted for anyone else to race against.
	claimed, cerr := campaignRepo.ClaimCampaignVersion(ctx, p.ProjectID, p.BriefID, p.CampaignID, version)
	if cerr != nil {
		return nil, mapBriefErr(cerr)
	}
	// Ensure lock is released even if a panic or context cancellation occurs. The lock
	// guards the platform-call window, so it must not outlive this function.
	defer campaignRepo.ReleaseCampaignLock(ctx, p.CampaignID) //nolint:errcheck
	existing.Version = claimed.Version

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
			// The campaign lacks child entities needed to serve (e.g., ad group/ad, ad set,
			// creatives) even though it has a platform campaign id. This is a platform-specific
			// validation error that goes deeper than just the platform_campaign_id check we did
			// above. The specific missing entity is platform-dependent, so the message stays
			// platform-NEUTRAL rather than naming one provider's shape for all. A client/state
			// error, NOT a platform rejection: a retry now would fail the same way, so this is a
			// 409, not a 503. It avoids "wait" (a campaign missing a child never gains one by
			// waiting) and points at the actual remedy.
			return nil, &briefs.ConflictError{Code: "409", Message: "campaign is not fully provisioned for activation — it lacks the child entities needed to serve (e.g. its ad group/ad, ad set, or creatives); finish or recreate the campaign before toggling its status"}
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
	existing.Status = p.Status
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistResultTimeout)
	defer cancel()
	updated, uerr := campaignRepo.ReplaceCampaign(persistCtx, existing, version, s.campaignIndexPayload(indexer.ActionUpdated))
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
