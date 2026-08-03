// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	audiences "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// AudienceBuilder performs the platform-side half of a build: resolving an event's past
// editions and creating the HubSpot lists. Declared as an interface here (rather than taking
// the concrete snowflake/hubspot clients) so the orchestration is testable without a warehouse
// or a live portal — and so a deployment with neither configured degrades to a typed error
// instead of a nil dereference.
type AudienceBuilder interface {
	// ResolvePastEditions returns the VERBATIM names of this event's past editions. The names
	// are used as exact HubSpot filter values, so an implementation must not guess or
	// normalise them. Returning an empty slice is valid: a first-time event has none.
	ResolvePastEditions(ctx context.Context, eventTerm, locationTerm, currentYear string) ([]string, error)
	// CreateList creates one DYNAMIC contact list and returns its platform id.
	// CreateList creates one DYNAMIC contact list in the PROJECT's portal and returns its
	// platform id. projectID is explicit rather than context-carried: HubSpot credentials are
	// stored per project, so building in the wrong portal is a silent, damaging failure.
	CreateList(ctx context.Context, projectID, name string, filter json.RawMessage) (string, error)
}

// errUnconfirmedCreate marks a create whose outcome is genuinely UNKNOWN — a 2xx carrying no
// list id. It is a SENTINEL rather than a message convention because the failed-vs-building
// decision depends on it: hubspot.IsUnconfirmed cannot classify this case (the client returned
// no typed error at all), so without it a list that MAY exist upstream would be recorded as
// "nothing was created" and an operator would skip reconciling it.
var errUnconfirmedCreate = errors.New("the create returned no list id")

// audienceUnavailableErr is the typed 503 returned when the pieces BuildAudience needs (the
// brief repository, the Snowflake/HubSpot builder) are not wired. Mirrors the CRUD routes'
// unavailable mode: the route stays mounted and returns the contract's 503 rather than a bare
// 404 or a nil-dereference panic.
func audienceUnavailableErr() error {
	return &audiences.ConnServiceUnavailableError{
		Code:    "503",
		Message: "audience building is not configured (requires the brief repository and the HubSpot/Snowflake clients)",
	}
}

// audienceBuildErr maps a platform-side build failure to the typed InternalServerError.
//
// The Code is "500", matching the status Goa actually encodes for this type. An earlier version
// set "502" to signal an upstream failure, but Goa maps InternalServerError to
// StatusInternalServerError regardless of the string — so clients got a 500 carrying a body
// claiming 502, which is worse than either alone. The upstream nature of the failure is carried
// in the MESSAGE, where it does not contradict the status line.
//
// Upstream error text can carry request ids but not credentials — the clients redact those.
func audienceBuildErr(err error) error {
	return &audiences.InternalServerError{
		Code:    "500",
		Message: "the audience build failed upstream: " + err.Error(),
	}
}

// SetBuilder injects the platform-side builder. Opt-in like the other late-bound dependencies
// so the ~existing NewAudienceService call sites are unaffected.
func (s *AudienceService) SetBuilder(b AudienceBuilder) {
	if b == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.builder = b
}

// BuilderIsSet reports whether the platform-side builder was injected. Exported ONLY so the
// container's wiring tests can assert injection directly: BuildAudience returns the same typed
// 503 whether the repo or the builder is missing, so an error-based assertion cannot tell a
// wired service from an unwired one.
func (s *AudienceService) BuilderIsSet() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.builder != nil
}

// briefEventDetails is the slice of a brief's opaque EventDetails blob this build needs.
// EventDetails is deliberately schemaless (see internal/dispatch/reddit.go), so this decodes
// opportunistically and validates what it found rather than assuming a shape.
type briefEventDetails struct {
	EventName string `json:"eventName"`
	Country   string `json:"country"`
	Location  string `json:"location"`
	Year      string `json:"year"`
}

// BuildAudience derives a brief's HubSpot audience and records it.
//
// The email channel cannot dispatch until an audience reaches "built" (the HubSpot dispatcher
// refuses a brief whose audience is unbuilt or carries no master list), so this is the step
// that takes an approved brief from "planned" to "sendable".
//
// Ordering is deliberate: the audience row is created as BUILDING *before* any HubSpot call,
// so a crash mid-build leaves a visible building row rather than an invisible gap plus orphan
// lists in the portal.
func (s *AudienceService) BuildAudience(ctx context.Context, p *audiences.BuildAudiencePayload) (*audiences.Audience, error) {
	repo, err := s.ready()
	if err != nil {
		return nil, err
	}
	briefs, builder, err := s.buildDeps()
	if err != nil {
		return nil, err
	}

	brief, berr := briefs.GetBrief(ctx, p.ProjectID, p.BriefID)
	if berr != nil {
		return nil, mapAudienceErr(berr)
	}

	// Same lifecycle guard the campaign-creation path applies (brief.go). Building creates
	// REAL HubSpot lists and makes the brief sendable, so a draft must not reach it — the
	// event details it would be built from are still being edited.
	if brief.Status != model.BriefApproved {
		return nil, audienceValidationErr(fmt.Errorf("brief must be approved before building its audience (it is %s)", brief.Status))
	}

	details, derr := decodeEventDetails(brief)
	if derr != nil {
		return nil, audienceValidationErr(derr)
	}

	// Resolve past editions BEFORE creating anything. A warehouse failure here must not leave
	// half-built lists in the portal, and the names it returns are the only acceptable source
	// for the group-5/7 filters.
	editions, rerr := builder.ResolvePastEditions(ctx, details.EventName, details.Location, details.Year)
	if rerr != nil {
		// Degrade rather than fail: group 4 needs no editions, so a warehouse outage still
		// yields a usable (narrower) audience. The gap is recorded in the plan's notes.
		slog.WarnContext(ctx, "could not resolve past editions; building a country-only audience",
			"brief_id", p.BriefID, "event", details.EventName, "error", rerr)
		editions = nil
	}

	plan, perr := audience.BuildPlan(audience.PlanInput{
		EventName:    details.EventName,
		Country:      details.Country,
		PastEditions: editions,
	})
	if perr != nil {
		return nil, audienceValidationErr(perr)
	}

	// Record the intent first (status defaults to building), so an interrupted build is
	// visible and reconcilable instead of silently absent.
	row := &model.CampaignAudience{
		ProjectID: p.ProjectID,
		BriefID:   p.BriefID,
		Platform:  model.ProviderHubSpot,
		Status:    model.AudienceBuilding,
		CreatedBy: marshalActor(actorFromCtx(ctx)),
	}
	created, cerr := repo.CreateAudience(ctx, row)
	if cerr != nil {
		return nil, mapAudienceErr(cerr)
	}

	master, ids, buildErr := createPlanLists(ctx, builder, p.ProjectID, plan)
	summary := plan.InclusionSummary()

	if buildErr != nil {
		// Two DIFFERENT outcomes, and telling an operator the wrong one costs real time:
		//
		//   - Nothing was created and the failure is DEFINITE (bad credentials, a plain 4xx):
		//     there is no upstream state, so mark it FAILED. Leaving it building would send
		//     someone hunting for portal orphans that do not exist.
		//   - Anything was created, or the outcome is UNCONFIRMED: keep it BUILDING, because a
		//     list may exist upstream and a blind retry would duplicate it.
		//
		// Either way the created ids are RECORDED. They are the only confirmed handles to the
		// lists that do exist; discarding them (as this originally did) makes the row
		// unreconcilable — the operator knows a build broke but not what it left behind.
		created.InclusionSummary = partialSummary(summary, ids, buildErr)
		if len(ids) == 0 && !hubspot.IsUnconfirmed(buildErr) && !errors.Is(buildErr, errUnconfirmedCreate) {
			created.Status = model.AudienceFailed
		}
		if _, uerr := repo.UpdateAudience(ctx, created, created.Version); uerr != nil {
			slog.ErrorContext(ctx, "failed to record a partial audience build",
				"audience_id", created.ID, "created_lists", strings.Join(ids, ","), "error", uerr)
		}
		return nil, audienceBuildErr(buildErr)
	}

	created.PlatformMasterListID = master
	created.SuppressionListIDs = nil
	created.InclusionSummary = summary
	created.Status = model.AudienceBuilt
	if verr := created.Validate(); verr != nil {
		return nil, audienceValidationErr(verr)
	}

	updated, uerr := repo.UpdateAudience(ctx, created, created.Version)
	if uerr != nil {
		// The lists EXIST upstream but the row does not reflect them. Log the ids so the
		// portal state can be reconciled by hand rather than orphaned silently.
		slog.ErrorContext(ctx, "audience lists created but recording them failed",
			"audience_id", created.ID, "master_list_id", master, "list_ids", strings.Join(ids, ","),
			"error", uerr)
		return nil, mapAudienceErr(uerr)
	}
	return audienceResult(updated), nil
}

// partialSummary records what a failed build actually left upstream. The plan summary alone
// describes what was INTENDED, which is misleading after a failure — an operator needs the ids
// of the lists that exist in order to reconcile them before retrying.
func partialSummary(planSummary string, ids []string, buildErr error) string {
	var b strings.Builder
	b.WriteString(planSummary)
	b.WriteString("\nBuild incomplete: ")
	b.WriteString(buildErr.Error())
	if len(ids) == 0 {
		b.WriteString("\nNo HubSpot lists were created.")
		return b.String()
	}
	b.WriteString("\nHubSpot lists ALREADY CREATED (reconcile these before retrying): ")
	b.WriteString(strings.Join(ids, ", "))
	return b.String()
}

// createPlanLists creates every planned inclusion list, then creates the MASTER list as their
// union and returns its id.
//
// The master is what the email dispatcher sends to — it reads only platform_master_list_id — so
// it MUST be a union. Recording one inclusion list as the master would create the others in the
// portal and never email them: a build that reports success while reaching a fraction of the
// intended people.
//
// It stops at the FIRST failure and returns the ids created so far, so a partial build records
// what exists upstream instead of creating more state it cannot recover.
func createPlanLists(ctx context.Context, b AudienceBuilder, projectID string, plan *audience.Plan) (master string, ids []string, err error) {
	for _, l := range plan.Lists {
		id, cerr := b.CreateList(ctx, projectID, l.Name, l.Filter)
		if cerr != nil {
			return "", ids, fmt.Errorf("create list %q: %w", l.Name, cerr)
		}
		if strings.TrimSpace(id) == "" {
			// A 2xx with no id is UNCONFIRMED, not success: the list may exist. Fail here so a
			// retry verifies by name rather than blind-creating a duplicate.
			return "", ids, fmt.Errorf("create list %q %w (UNCONFIRMED: verify before retrying)", l.Name, errUnconfirmedCreate)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return "", nil, errors.New("the plan produced no lists")
	}

	masterFilter, ferr := audience.MasterListFilter(ids)
	if ferr != nil {
		return "", ids, ferr
	}
	masterName := fmt.Sprintf("%s — Master", plan.EventName)
	masterID, merr := b.CreateList(ctx, projectID, masterName, masterFilter)
	if merr != nil {
		return "", ids, fmt.Errorf("create master list %q: %w", masterName, merr)
	}
	if strings.TrimSpace(masterID) == "" {
		return "", ids, fmt.Errorf("create master list %q %w (UNCONFIRMED: verify before retrying)", masterName, errUnconfirmedCreate)
	}
	return masterID, append(ids, masterID), nil
}

// decodeEventDetails pulls the fields the build needs out of the brief's opaque blobs. It
// mirrors internal/dispatch's decodeBriefFields: EventDetails is the primary source, Copy is a
// fallback, and a blob that isn't this shape is skipped rather than failing the request.
func decodeEventDetails(b *model.CampaignBrief) (briefEventDetails, error) {
	var out briefEventDetails
	for _, blob := range []json.RawMessage{b.EventDetails, b.Copy} {
		if len(blob) == 0 {
			continue
		}
		var partial briefEventDetails
		if err := json.Unmarshal(blob, &partial); err != nil {
			continue
		}
		if out.EventName == "" {
			out.EventName = strings.TrimSpace(partial.EventName)
		}
		if out.Country == "" {
			out.Country = strings.TrimSpace(partial.Country)
		}
		if out.Location == "" {
			out.Location = strings.TrimSpace(partial.Location)
		}
		if out.Year == "" {
			out.Year = strings.TrimSpace(partial.Year)
		}
	}
	if out.EventName == "" {
		return out, fmt.Errorf("brief %s has no eventName in its details; an audience cannot be named or scoped without it", b.ID)
	}
	if out.Country == "" {
		return out, fmt.Errorf("brief %s has no country in its details; every inclusion list is country-scoped", b.ID)
	}
	return out, nil
}

// buildDeps returns the brief repo and builder, or a typed error when either is missing.
func (s *AudienceService) buildDeps() (domain.BriefRepository, AudienceBuilder, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.briefs == nil {
		return nil, nil, audienceUnavailableErr()
	}
	if s.builder == nil {
		return nil, nil, audienceUnavailableErr()
	}
	return s.briefs, s.builder, nil
}
