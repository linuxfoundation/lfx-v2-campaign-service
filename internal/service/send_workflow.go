// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// sendDateLayout is the calendar-date form the API takes. Date-only on purpose: a workflow
// step's SEND HOUR belongs to the source workflow, which is where an operator set their
// sending-hours policy, and accepting a time here would quietly override it.
const sendDateLayout = "2006-01-02"

// defaultSurveySequenceWorkflowID is the workflow a survey series is cloned from when the
// deployment names no other: "26Q2 - LF Research - FINOS 2026: Workflow (Survey Promo)", which is
// the workflow the standalone survey tool this replaces cloned for every sequence it built.
//
// Carried as a DEFAULT rather than a hard requirement on the request, because an operator should
// not have to find a workflow URL in HubSpot to do the ordinary thing. A request may still name a
// different workflow, and a deployment on another portal must, since this id exists in one portal
// only.
const defaultSurveySequenceWorkflowID = "1841232183"

// maxSendWorkflowBriefs bounds one workflow's sends, matching the design's MaxLength.
// Restated here because the design's bound is the published contract and this is the one
// the service enforces; a request that slipped past a regenerated client still stops here.
const maxSendWorkflowBriefs = 10

// CreateSendWorkflow produces ONE HubSpot workflow that sends an already-staged series.
//
// WHY A WORKFLOW AT ALL. A single email is staged, scheduled and sent on its own; a series is
// not, because the sends have to happen in order, days apart, to one enrolled audience. That
// schedule is a HubSpot concept, so the series is handed to HubSpot as a workflow rather than
// modelled as three independent scheduled sends this service would then have to keep in step.
//
// WHY A COPY OF AN EXISTING WORKFLOW. Enrollment criteria, suppression, sending windows and
// blocked dates are a marketing-operations policy that already exists in the portal, and
// rebuilding it from scratch here would mean re-deciding all of it in code. The operator points
// at a workflow that already embodies the policy; this swaps in the emails and the dates.
//
// The source workflow is only ever READ, and the new one is always created DISABLED.
func (s *BriefService) CreateSendWorkflow(ctx context.Context, p *briefs.CreateSendWorkflowPayload) (*briefs.SendWorkflowResult, error) {
	briefRepo, campaignRepo, _, _, err := s.ready()
	if err != nil {
		return nil, err
	}
	_, resolver, _ := s.wizardDeps()
	if resolver == nil {
		// Availability-neutral, like ready(): on a cold start HubSpot IS configured and has
		// simply not bound yet, so "not configured" would send an operator to change config
		// during a window that clears on its own.
		return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the HubSpot connection is unavailable"}
	}
	// Slug-only, matching create-campaigns: project_id is the exact-match key for the
	// connection lookup, and a UUID-scoped request would find no connection at all.
	if verr := validateProjectSlug(p.ProjectID); verr != nil {
		return nil, verr
	}
	if p.Input == nil {
		return nil, &briefs.BadRequestError{Code: "400", Message: "input is required"}
	}

	// An unnamed source is the ordinary case: the deployment's configured sequence workflow is
	// what a survey series is normally built from. Only a request that NAMES something unusable
	// is refused, so a blank field falls back rather than failing.
	flowID := defaultSurveySequenceWorkflowID
	sourceNamed := false
	if configured := strings.TrimSpace(os.Getenv(constants.EnvSurveySequenceWorkflowID)); configured != "" {
		flowID = configured
	}
	if named := strings.TrimSpace(strVal(p.Input.SourceWorkflowURL)); named != "" {
		sourceNamed = true
		flowID = hubspot.ParseFlowID(named)
		if flowID == "" {
			return nil, &briefs.BadRequestError{
				Code:    "400",
				Message: "source_workflow_url names no workflow; paste the workflow's URL from HubSpot, which ends /flow/{id}/edit",
			}
		}
	}

	name := strings.TrimSpace(p.Input.Name)
	if name == "" {
		return nil, &briefs.BadRequestError{Code: "400", Message: "name is required"}
	}

	// Two is the floor, and it is a PRODUCT rule rather than a technical one: a single email
	// has nothing to sequence, and wrapping it in a workflow would add an enrollment step
	// between the draft and its send for no gain. The prototype this replaces had the same
	// rule -- it skipped the workflow for a one-email campaign even when given a workflow.
	if len(p.Input.BriefIds) < 2 {
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: "a workflow needs at least two briefs; a single email is scheduled and sent on its own",
		}
	}
	if len(p.Input.BriefIds) > maxSendWorkflowBriefs {
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: fmt.Sprintf("a workflow may send at most %d emails", maxSendWorkflowBriefs),
		}
	}

	sendDates, derr := parseSendDates(p.Input.SendDates, s.now())
	if derr != nil {
		return nil, derr
	}

	drafts, eerr := s.stagedDrafts(ctx, briefRepo, campaignRepo, p.ProjectID, p.Input.BriefIds)
	if eerr != nil {
		return nil, eerr
	}

	series, aerr := s.seriesAudience(ctx, p.ProjectID, p.Input.BriefIds)
	if aerr != nil {
		return nil, aerr
	}
	// The drafts and the audience must live in one portal: a brief staged against the LF portal
	// whose audience was later rebuilt in the project's own would pass the audience's portal
	// check below while its send step pointed at an email the new portal cannot see.
	emailIDs := make([]string, 0, len(drafts))
	for i, d := range drafts {
		if d.portalID == "" || d.portalID != series.portalID {
			return nil, &briefs.ConflictError{
				Code: "409",
				Message: fmt.Sprintf(
					"send %d (brief %s)'s draft does not record being staged in HubSpot portal %s, where the series' audience was built; re-stage it",
					i+1, p.Input.BriefIds[i], series.portalID),
			}
		}
		emailIDs = append(emailIDs, d.emailID)
	}

	client, _, rerr := resolver.ResolveHubSpotClient(ctx, p.ProjectID)
	if rerr != nil {
		if errors.Is(rerr, domain.ErrNotFound) {
			return nil, &briefs.ConflictError{Code: "409", Message: "this project has no usable HubSpot connection"}
		}
		return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the HubSpot connection could not be resolved"}
	}

	// The same send-time checks staging and the wizard run before SetSendList, because enrolling
	// a list in a workflow IS the send for this path: the drafts' own recipient settings are not
	// what a workflow's send step uses. The audience's lists must live in the portal this
	// credential reaches; then, against the suppressions the workflow will ACTUALLY apply (the
	// source workflow's own plus the audience's, known only once the source is read), no send
	// list may also be suppressed and the project's standing suppression list must not have grown
	// to cover what the series sends to (#396). All read-only, and all before anything exists.
	if perr := assertWizardPortal(ctx, client, series.portalID); perr != nil {
		return nil, perr
	}
	beforeCreate := func(suppression []string) error {
		for _, id := range suppression {
			if slices.Contains(series.sendIDs, id) {
				return &briefs.ConflictError{
					Code:    "409",
					Message: fmt.Sprintf("list %s is one the series sends to, but the source workflow suppresses it, so the workflow would reach nobody on it; choose another source workflow", id),
				}
			}
		}
		return wizardStandingCoversSendList(ctx, client, p.ProjectID, series.sendIDs, suppression)
	}

	// The LF-wide fallback is NOT refused here, unlike the wizard's email search. The search
	// refuses it because HubSpot's email namespace is portal-wide and a match there can be
	// another project's email. Here every email id and list id came from this project's own
	// rows and is checked against the portal above. The SOURCE workflow is the one trust gap:
	// it is the deployment's configured workflow or one a campaign manager pasted, and on the
	// shared LF portal that can be any workflow in it. Only its schedule, time windows and
	// suppression lists are inherited -- its emails, dates and enrollment are replaced, and a
	// source still referring to its own emails anywhere else is refused.
	ref, cerr := client.CloneFlowWithEmails(ctx, flowID, hubspot.CloneFlowOptions{
		Name:               name,
		EmailIDs:           emailIDs,
		SendDates:          sendDates,
		EnrollmentListIDs:  series.sendIDs,
		SuppressionListIDs: series.suppression,
		BeforeCreate:       beforeCreate,
	})
	if cerr != nil {
		return nil, mapFlowCloneErr(cerr, flowID, sourceNamed)
	}

	return &briefs.SendWorkflowResult{
		FlowID:             ref.FlowID,
		Name:               ref.Name,
		EditURL:            ref.EditURL,
		SourceFlowID:       ref.SourceFlowID,
		SourceFlowName:     &ref.SourceFlowName,
		IsEnabled:          ref.IsEnabled,
		SendEmailSteps:     ref.SendEmailSteps,
		DatedSteps:         ref.DatedSteps,
		EnrollmentListIds:  ref.EnrollmentListIDs,
		SuppressionListIds: ref.SuppressionListIDs,
	}, nil
}

// stagedDrafts resolves each brief to the HubSpot draft its staging produced, IN THE ORDER
// the caller gave, which is the order the workflow will send them in.
//
// Resolved here rather than taken from the client because which draft a brief produced is this
// service's own record: the campaign row's platform id. A client that had to look that up could
// pair a brief with an email belonging to another campaign -- and a workflow is exactly where
// that would not be noticed, since every step looks filled in.
func (s *BriefService) stagedDrafts(
	ctx context.Context,
	briefRepo domain.BriefRepository,
	campaignRepo domain.CampaignRepository,
	projectID string,
	briefIDs []string,
) ([]stagedDraft, error) {
	seen := make(map[string]struct{}, len(briefIDs))
	drafts := make([]stagedDraft, 0, len(briefIDs))

	for i, briefID := range briefIDs {
		position := i + 1
		if _, dup := seen[briefID]; dup {
			// The same brief twice would point two sends at ONE draft, so the series would
			// send identical copy on both dates -- and read as correct in the editor.
			return nil, &briefs.BadRequestError{
				Code:    "400",
				Message: fmt.Sprintf("brief %s appears twice; each send needs its own brief", briefID),
			}
		}
		seen[briefID] = struct{}{}

		brief, err := briefRepo.GetBrief(ctx, projectID, briefID)
		if err != nil {
			return nil, mapBriefErr(err)
		}
		// Paid briefs carry ad copy, not a subject and a body, and have no HubSpot draft to
		// send. Checked before the campaign lookup so the answer names the real problem rather
		// than reporting a missing draft that was never going to exist.
		if brief.DeliveryType == model.DeliveryPaidMarketing {
			return nil, &briefs.BadRequestError{
				Code:    "400",
				Message: fmt.Sprintf("brief %s is a paid-marketing brief; a send workflow carries email sends", briefID),
			}
		}

		campaign, cerr := campaignRepo.GetCampaignByPlatform(ctx, projectID, briefID, model.ProviderHubSpot, model.VariantDefault)
		if cerr != nil {
			if errors.Is(cerr, domain.ErrNotFound) {
				return nil, &briefs.BadRequestError{
					Code: "400",
					Message: fmt.Sprintf(
						"brief %s (send %d) has no staged HubSpot draft yet; stage every email before building the workflow",
						briefID, position),
				}
			}
			return nil, mapBriefErr(cerr)
		}
		// A claimed-but-not-dispatched row has no platform id yet. That is a draft still being
		// created, not a missing one, so it is reported the same way: stage it, then retry.
		emailID := strings.TrimSpace(campaign.PlatformCampaignID)
		if emailID == "" {
			return nil, &briefs.BadRequestError{
				Code: "400",
				Message: fmt.Sprintf(
					"brief %s (send %d) has no staged HubSpot draft yet; stage every email before building the workflow",
					briefID, position),
			}
		}
		// Only a FINISHED staging. A pending row can carry the draft's id while its content step
		// never ran -- it still holds the template email's copy -- and an unconfirmed or partial
		// one is the orchestrator's to reconcile; isReusableCampaign is the same rule the create
		// path uses to decide a row is safe to put in front of an audience.
		if !isReusableCampaign(campaign) {
			return nil, &briefs.ConflictError{
				Code: "409",
				Message: fmt.Sprintf(
					"brief %s (send %d)'s draft is %s, not fully staged; finish or re-stage it before building the workflow",
					briefID, position, campaign.Status),
			}
		}
		drafts = append(drafts, stagedDraft{emailID: emailID, portalID: draftPortalID(campaign.Result)})
	}
	return drafts, nil
}

// stagedDraft is one staged HubSpot draft and the portal its staging verified it in.
type stagedDraft struct {
	emailID  string
	portalID string
}

// draftPortalID is the portal HubSpot dispatch recorded in the campaign's result blob after
// proving the audience's lists live there. Empty when the row predates that record or the blob
// is unreadable, which the caller treats as unproven.
func draftPortalID(result json.RawMessage) string {
	var blob struct {
		PortalID string `json:"portalId"`
	}
	if len(result) == 0 || json.Unmarshal(result, &blob) != nil {
		return ""
	}
	return strings.TrimSpace(blob.PortalID)
}

// seriesAudienceSet is the one audience a workflow enrolls: the lists it sends to, the lists it
// suppresses, and the portal those ids belong to.
type seriesAudienceSet struct {
	sendIDs     []string
	suppression []string
	portalID    string
}

// seriesAudience is the audience the series shares, read from each brief's newest HubSpot
// audience exactly as staging and the wizard read it (resolveWizardAudience): the newest one
// decides, it must be BUILT, and it sends to every include list when it was attached from several.
//
// ONE audience, because a workflow enrolls contacts once and then sends every step to them. The
// sends are separate briefs with separate audiences in this service, so this is where that
// modelling meets HubSpot's -- and the two only agree when every send was built against the same
// lists, with the same suppressions, in the same portal.
//
// A disagreement is REFUSED rather than resolved by picking one. Either choice would send some
// of the emails to people the operator never selected for them, and nothing downstream would
// show it: the workflow's enrollment step looks correctly filled in whichever list it names.
//
// So is an absent or unreadable audience. Keeping the source workflow's own enrollment instead
// would send this campaign's emails to the SOURCE campaign's audience -- the mistake this
// rewrite exists to prevent -- and would skip the suppression checks the drafts were staged under.
func (s *BriefService) seriesAudience(ctx context.Context, projectID string, briefIDs []string) (*seriesAudienceSet, error) {
	_, _, audiences := s.wizardDeps()
	if audiences == nil {
		// Availability-neutral, like the resolver above: the audience repository binds with the
		// wizard's collaborators, so on a cold start it can simply not be bound yet.
		return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the audience store is unavailable; retry shortly"}
	}

	var series *seriesAudienceSet
	for i, briefID := range briefIDs {
		sendIDs, suppression, portalID, err := resolveWizardAudience(ctx, audiences, projectID, briefID)
		if err != nil {
			return nil, withSendPosition(err, i+1, briefID)
		}
		got := &seriesAudienceSet{
			sendIDs:     audience.UniqueIDs(sendIDs),
			suppression: audience.UniqueIDs(suppression),
			portalID:    portalID,
		}
		for _, id := range got.suppression {
			if slices.Contains(got.sendIDs, id) {
				// Dispatch's pre-flight, restated: a list both sent to and suppressed is excluded
				// whole, so the send would quietly reach nobody on it.
				return nil, &briefs.ConflictError{
					Code:    "409",
					Message: fmt.Sprintf("send %d's audience lists %s as both a recipient and a suppression list; rebuild the audience", i+1, id),
				}
			}
		}
		if series == nil {
			series = got
			continue
		}
		if !sameIDSet(series.sendIDs, got.sendIDs) || !sameIDSet(series.suppression, got.suppression) || series.portalID != got.portalID {
			return nil, &briefs.ConflictError{
				Code: "409",
				Message: fmt.Sprintf(
					"send %d is built against a different audience than the earlier sends; a workflow enrolls one audience for the whole series, so attach the same lists to every send",
					i+1),
			}
		}
	}
	return series, nil
}

// withSendPosition prefixes an audience refusal with the send it is about, so the operator knows
// which of the series' briefs to fix. resolveWizardAudience's own wording names the wizard's
// send_list_id override, which this endpoint does not take, so that remedy is dropped here.
func withSendPosition(err error, position int, briefID string) error {
	prefix := fmt.Sprintf("send %d (brief %s): ", position, briefID)
	clean := func(msg string) string {
		return prefix + strings.ReplaceAll(msg, " or supply send_list_id", "")
	}
	var conflict *briefs.ConflictError
	if errors.As(err, &conflict) {
		return &briefs.ConflictError{Code: conflict.Code, Message: clean(conflict.Message)}
	}
	var bad *briefs.BadRequestError
	if errors.As(err, &bad) {
		// A brief with no audience at all is a state of the series, not a malformed request.
		return &briefs.ConflictError{Code: "409", Message: clean(bad.Message)}
	}
	return err
}

// sameIDSet reports whether a and b hold the same ids, whatever their order.
func sameIDSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// parseSendDates turns the request's calendar dates into times, preserving order.
//
// Rejects anything that is not exactly YYYY-MM-DD (time.Parse already refuses an impossible
// date such as 2026-02-30; the round trip also refuses a non-padded one such as 2026-2-3). Each
// date must be after today (UTC) and after the one before it: a wait-until-date step whose date
// has passed moves an enrolled contact straight on, so a past or out-of-order date would send
// two emails back to back, or the closing push before the reminder.
func parseSendDates(in []string, now time.Time) ([]time.Time, error) {
	if len(in) == 0 {
		return nil, nil
	}
	today := now.UTC().Format(sendDateLayout)
	out := make([]time.Time, 0, len(in))
	for i, raw := range in {
		s := strings.TrimSpace(raw)
		t, err := time.Parse(sendDateLayout, s)
		if err != nil || t.Format(sendDateLayout) != s {
			return nil, &briefs.BadRequestError{
				Code:    "400",
				Message: fmt.Sprintf("send_dates[%d] (%q) is not a calendar date in YYYY-MM-DD form", i, raw),
			}
		}
		if s <= today {
			return nil, &briefs.BadRequestError{
				Code:    "400",
				Message: fmt.Sprintf("send_dates[%d] (%s) is not after today (%s, UTC)", i, s, today),
			}
		}
		if i > 0 && !t.After(out[i-1]) {
			return nil, &briefs.BadRequestError{
				Code:    "400",
				Message: fmt.Sprintf("send_dates[%d] (%s) is not after send_dates[%d]; dates are in send order", i, s, i-1),
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// mapFlowCloneErr turns a clone failure into the status that describes it.
//
// A refusal from the caller's own BeforeCreate checks is already a contract error and passes
// through. A step-count mismatch or an unsupported source shape is a 409 rather than a 400: the
// request is well-formed and the briefs are staged, and what conflicts is the workflow the
// operator chose -- a different workflow, or a different set of briefs, resolves it. Those
// messages already name the counts or the shape, so they are passed through.
func mapFlowCloneErr(err error, flowID string, sourceNamed bool) error {
	var conflict *briefs.ConflictError
	var unavailable *briefs.ConnServiceUnavailableError
	if errors.As(err, &conflict) || errors.As(err, &unavailable) {
		return err
	}
	msg := strings.TrimPrefix(err.Error(), "hubspot: ")
	switch {
	case errors.Is(err, hubspot.ErrFlowStepCountMismatch), errors.Is(err, hubspot.ErrFlowShapeUnsupported):
		return &briefs.ConflictError{Code: "409", Message: msg}
	case hubspot.IsUnconfirmed(err):
		// The workflow MAY exist. Reported as a server error with that said plainly, because a
		// client that retried would enroll the same contacts in a second workflow.
		return &briefs.InternalServerError{Code: "500", Message: msg}
	case hubspot.IsPermissionRejection(err):
		return &briefs.ConflictError{
			Code:    "409",
			Message: "the HubSpot credential may not read or create workflows; it needs the automation scope",
		}
	case hubspot.IsNotFound(err) && !sourceNamed:
		// The request named nothing; the deployment chose a workflow this portal does not have.
		return &briefs.ConflictError{
			Code: "409",
			Message: fmt.Sprintf("the default survey workflow %s does not exist in this project's HubSpot portal; pass source_workflow_url, or set %s for this deployment",
				flowID, constants.EnvSurveySequenceWorkflowID),
		}
	case hubspot.IsDefiniteRejection(err):
		return &briefs.BadRequestError{Code: "400", Message: msg}
	default:
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: "the workflow could not be created in HubSpot"}
	}
}
