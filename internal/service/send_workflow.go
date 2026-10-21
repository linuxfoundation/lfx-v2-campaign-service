// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
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
	if configured := strings.TrimSpace(os.Getenv(constants.EnvSurveySequenceWorkflowID)); configured != "" {
		flowID = configured
	}
	if named := strings.TrimSpace(derefString(p.Input.SourceWorkflowURL)); named != "" {
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

	sendDates, derr := parseSendDates(p.Input.SendDates)
	if derr != nil {
		return nil, derr
	}

	emailIDs, eerr := s.stagedEmailIDs(ctx, briefRepo, campaignRepo, p.ProjectID, p.Input.BriefIds)
	if eerr != nil {
		return nil, eerr
	}

	enrollmentListID, aerr := s.seriesAudienceListID(ctx, p.ProjectID, p.Input.BriefIds)
	if aerr != nil {
		return nil, aerr
	}

	client, _, rerr := resolver.ResolveHubSpotClient(ctx, p.ProjectID)
	if rerr != nil {
		if errors.Is(rerr, domain.ErrNotFound) {
			return nil, &briefs.ConflictError{Code: "409", Message: "this project has no usable HubSpot connection"}
		}
		return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the HubSpot connection could not be resolved"}
	}

	// The LF-wide fallback is NOT refused here, unlike the wizard's email search. The search
	// refuses it because HubSpot's email namespace is portal-wide and a match there can be
	// another project's email. This path names no stranger's asset: every email id came from
	// this project's own campaign rows, and the workflow it copies is one the operator pasted.
	ref, cerr := client.CloneFlowWithEmails(ctx, flowID, hubspot.CloneFlowOptions{
		Name:             name,
		EmailIDs:         emailIDs,
		SendDates:        sendDates,
		EnrollmentListID: enrollmentListID,
	})
	if cerr != nil {
		return nil, mapFlowCloneErr(cerr)
	}

	return &briefs.SendWorkflowResult{
		FlowID:         ref.FlowID,
		Name:           ref.Name,
		EditURL:        ref.EditURL,
		SourceFlowID:   ref.SourceFlowID,
		SourceFlowName: &ref.SourceFlowName,
		IsEnabled:      ref.IsEnabled,
		SendEmailSteps: ref.SendEmailSteps,
		DatedSteps:     ref.DatedSteps,
	}, nil
}

// stagedEmailIDs resolves each brief to the HubSpot draft its staging produced, IN THE ORDER
// the caller gave, which is the order the workflow will send them in.
//
// Resolved here rather than taken from the client because which draft a brief produced is this
// service's own record: the campaign row's platform id. A client that had to look that up could
// pair a brief with an email belonging to another campaign -- and a workflow is exactly where
// that would not be noticed, since every step looks filled in.
func (s *BriefService) stagedEmailIDs(
	ctx context.Context,
	briefRepo domain.BriefRepository,
	campaignRepo domain.CampaignRepository,
	projectID string,
	briefIDs []string,
) ([]string, error) {
	seen := make(map[string]struct{}, len(briefIDs))
	emailIDs := make([]string, 0, len(briefIDs))

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
		emailIDs = append(emailIDs, emailID)
	}
	return emailIDs, nil
}

// seriesAudienceListID is the HubSpot list the workflow enrolls: the audience the series shares.
//
// ONE list, because a workflow enrolls contacts once and then sends every step to them. The sends
// are separate briefs with separate audiences in this service, so this is where that modelling
// meets HubSpot's -- and the two only agree when the sends were built against the same audience.
//
// A disagreement is REFUSED rather than resolved by picking one. Either choice would send two of
// the three emails to people the operator never selected for them, and nothing downstream would
// show it: the workflow's enrollment step looks correctly filled in whichever list it names.
//
// An absent audience is not refused here. Staging already requires one, so a brief with a staged
// draft has an audience; a brief without a draft is refused earlier, with a message that says to
// stage it. Falling back to the source workflow's own enrollment keeps this from inventing a
// second reason to fail for a state the earlier check already covers.
func (s *BriefService) seriesAudienceListID(ctx context.Context, projectID string, briefIDs []string) (string, error) {
	_, _, audiences := s.wizardDeps()
	if audiences == nil {
		// The audience repository is an OPTIONAL capability (it binds with the wizard's
		// collaborators). Without it the workflow still builds, enrolling the source's own
		// audience, which the result reports -- better than refusing a build for a read that
		// only narrows who it enrolls.
		return "", nil
	}

	listID := ""
	for i, briefID := range briefIDs {
		built, err := latestBuiltAudience(ctx, audiences, projectID, briefID)
		if err != nil || built == nil {
			// Unreadable is treated as absent, deliberately: the workflow still builds, enrolling
			// the source's own audience, and the result says which list it used. Failing the whole
			// build on a read this path can do without would be worse than saying what happened.
			continue
		}
		masterListID := strings.TrimSpace(built.PlatformMasterListID)
		if masterListID == "" {
			continue
		}
		if listID == "" {
			listID = masterListID
			continue
		}
		if masterListID != listID {
			return "", &briefs.ConflictError{
				Code: "409",
				Message: fmt.Sprintf(
					"send %d is built against a different audience than the earlier sends; a workflow enrolls one audience for the whole series",
					i+1),
			}
		}
	}
	return listID, nil
}

// latestBuiltAudience is the newest BUILT HubSpot audience under a brief, or nil.
//
// Newest, because composing again mints a second list and the latest one is what staging attached
// to the draft -- enrolling an earlier one would send to an audience the drafts themselves do not
// use. Only `built` counts: a building or failed audience has no usable list behind it.
func latestBuiltAudience(ctx context.Context, audiences domain.AudienceRepository, projectID, briefID string) (*model.CampaignAudience, error) {
	rows, err := audiences.ListAudiences(ctx, projectID, briefID)
	if err != nil {
		return nil, err
	}
	var newest *model.CampaignAudience
	for _, row := range rows {
		if row == nil || row.Platform != model.ProviderHubSpot || row.Status != model.AudienceBuilt {
			continue
		}
		if newest == nil || row.CreatedAt.After(newest.CreatedAt) {
			newest = row
		}
	}
	return newest, nil
}

// derefString reads an optional request attribute, which Goa models as a pointer.
func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// parseSendDates turns the request's calendar dates into times, preserving order.
//
// Rejects anything that is not exactly YYYY-MM-DD, including a date Go would otherwise
// normalise: "2026-02-30" parses as 2 March, and silently moving an operator's send by two
// days is worse than refusing the request.
func parseSendDates(in []string) ([]time.Time, error) {
	if len(in) == 0 {
		return nil, nil
	}
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
		out = append(out, t)
	}
	return out, nil
}

// mapFlowCloneErr turns a clone failure into the status that describes it.
//
// A count mismatch is a 409 rather than a 400: the request is well-formed and the briefs are
// staged, and what conflicts is the SHAPE of the workflow the operator chose -- a different
// workflow, or a different set of briefs, resolves it. The message already names both counts,
// so it is passed through rather than replaced with a generic one.
func mapFlowCloneErr(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "send-email step(s) but"), strings.Contains(msg, "wait-until-date step(s) but"):
		return &briefs.ConflictError{Code: "409", Message: strings.TrimPrefix(msg, "hubspot: ")}
	case hubspot.IsUnconfirmed(err):
		// The workflow MAY exist. Reported as a server error with that said plainly, because a
		// client that retried would enroll the same contacts in a second workflow.
		return &briefs.InternalServerError{Code: "500", Message: strings.TrimPrefix(msg, "hubspot: ")}
	case hubspot.IsPermissionRejection(err):
		return &briefs.ConflictError{
			Code:    "409",
			Message: "the HubSpot credential may not read or create workflows; it needs the automation scope",
		}
	case hubspot.IsDefiniteRejection(err):
		return &briefs.BadRequestError{Code: "400", Message: strings.TrimPrefix(msg, "hubspot: ")}
	default:
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: "the workflow could not be created in HubSpot"}
	}
}
