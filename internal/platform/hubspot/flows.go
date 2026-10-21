// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Automation (v4 Flows) operations.
//
// A multi-send campaign -- a survey promoted by an opening email, a reminder and a
// closing push -- is ONE HubSpot workflow that owns the schedule and sends each email
// itself. This file is how such a workflow is produced.
//
// It is a READ-THEN-RECREATE clone, not a copy endpoint: the v4 Flows API has no clone
// operation, so the source definition is fetched, the parts that differ are swapped in
// memory, and the result is POSTed as a brand-new flow. The source workflow is only
// ever read -- nothing here PATCHes it.
// ---------------------------------------------------------------------------

const (
	flowsPath = "/automation/v4/flows"

	// HubSpot's action type ids, which are how a step is identified inside a flow
	// definition. Matching on these rather than on an action's POSITION or its id is what
	// lets this work against any workflow the operator points at: a hand-built workflow
	// numbers its actions however the editor happened to, and two workflows that do the
	// same thing rarely agree on those numbers.
	sendEmailActionTypeID     = "0-4"
	waitUntilDateActionTypeID = "0-35"
)

// flowIDPattern pulls the numeric flow id out of a HubSpot workflow URL, which is what
// an operator can actually copy -- the editor's address bar shows
// .../workflows/{portal}/platform/flow/{id}/edit and never the bare id.
var flowIDPattern = regexp.MustCompile(`/flow/(\d+)`)

// ParseFlowID returns the flow id in a HubSpot workflow URL, or "" if there is none.
//
// A bare numeric id is accepted too, so an operator who pasted just the id is not told
// their input is malformed for being more precise than required.
func ParseFlowID(workflowURL string) string {
	s := strings.TrimSpace(workflowURL)
	if s == "" {
		return ""
	}
	if m := flowIDPattern.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if _, err := strconv.ParseUint(s, 10, 64); err == nil {
		return s
	}
	return ""
}

// FlowRef identifies a created workflow for the operator who has to go and enable it.
type FlowRef struct {
	FlowID         string `json:"flow_id"`
	Name           string `json:"name"`
	EditURL        string `json:"edit_url"`
	SourceFlowID   string `json:"source_flow_id"`
	SourceFlowName string `json:"source_flow_name"`
	// IsEnabled is always false for a workflow this client creates. Reported rather than
	// assumed, so a caller shows the operator what HubSpot actually returned.
	IsEnabled bool `json:"is_enabled"`
	// SendEmailSteps and DatedSteps say how much of the source was rewritten, which is
	// what tells an operator whether the clone matched the shape they expected.
	SendEmailSteps int `json:"send_email_steps"`
	DatedSteps     int `json:"dated_steps"`
	// EnrollmentListID is the audience the new workflow enrolls, or "" when the source's own
	// enrollment was kept. Reported because it decides WHO receives the series, and an operator
	// reading a success message has no other way to tell which of the two happened.
	EnrollmentListID string `json:"enrollment_list_id"`
}

// FlowEditURL is where a human reviews and enables a workflow.
func (c *Client) FlowEditURL(flowID string) string {
	return fmt.Sprintf("https://app.hubspot.com/workflows/%s/platform/flow/%s/edit", c.account.PortalID, flowID)
}

// GetFlow fetches one workflow's full definition as opaque JSON.
//
// Deliberately a map rather than a typed struct: a flow definition carries dozens of
// fields this service has no opinion about (branch logic, goals, enrollment windows),
// and a struct would silently DROP every field it does not declare when the definition
// is posted back as a new flow. Only the few fields that get rewritten are read with
// any knowledge of their shape.
func (c *Client) GetFlow(ctx context.Context, flowID string) (map[string]any, error) {
	if flowID = strings.TrimSpace(flowID); flowID == "" {
		return nil, fmt.Errorf("hubspot: GetFlow requires a flow id")
	}
	raw, err := c.doRequest(ctx, http.MethodGet, flowsPath+"/"+flowID, nil, true)
	if err != nil {
		return nil, fmt.Errorf("hubspot: get flow %s: %w", flowID, err)
	}
	var flow map[string]any
	if uerr := json.Unmarshal(raw, &flow); uerr != nil {
		return nil, fmt.Errorf("hubspot: get flow %s: decode: %w", flowID, uerr)
	}
	return flow, nil
}

// CloneFlowOptions is what a clone changes about the source workflow.
type CloneFlowOptions struct {
	// Name of the new workflow.
	Name string
	// EmailIDs are the marketing email ids to put in the flow's send steps, in flow
	// order. Exactly one per send step in the source.
	EmailIDs []string
	// SendDates optionally overwrites each wait-until-date step's target date, in flow
	// order. Empty leaves the source's dates untouched; otherwise exactly one per dated
	// step. Only the DATE is replaced -- the step's time of day stays the source's,
	// because that is a sending-hours decision its author made and a date carries no time.
	SendDates []time.Time
	// EnrollmentListID replaces WHO the workflow enrolls: the campaign's own audience, rather
	// than whichever list the source workflow was built around. Empty keeps the source's
	// enrollment criteria untouched.
	//
	// This is the difference between a clone that is a template and a clone that is a campaign.
	// Left alone, the new workflow would send this campaign's emails to the SOURCE campaign's
	// audience -- a mistake that is invisible in the editor, because the enrollment step looks
	// correctly filled in.
	EnrollmentListID string
}

// CloneFlowWithEmails recreates a workflow with this campaign's emails in its send
// steps, and returns the new, DISABLED workflow.
//
// Always created disabled. A workflow that began enrolling contacts the moment it was
// created would send live email from drafts nobody has read, so enabling it -- the one
// irreversible step on this path -- is left to the human who reviews it.
//
// Counts must match exactly, and a mismatch is refused BEFORE anything is created: a
// source workflow with three send steps and two built emails would otherwise produce a
// workflow whose third send still points at the SOURCE campaign's email. That is both
// wrong and very hard to notice in the HubSpot editor, where it looks like a filled-in
// step rather than a missing one.
func (c *Client) CloneFlowWithEmails(ctx context.Context, flowID string, opts CloneFlowOptions) (*FlowRef, error) {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return nil, fmt.Errorf("hubspot: CloneFlowWithEmails requires a name")
	}
	if len(opts.EmailIDs) == 0 {
		return nil, fmt.Errorf("hubspot: CloneFlowWithEmails requires at least one email id")
	}

	flow, err := c.GetFlow(ctx, flowID)
	if err != nil {
		return nil, err
	}

	actions, ok := flow["actions"].([]any)
	if !ok {
		return nil, fmt.Errorf("hubspot: workflow %s has no actions to rewrite", flowID)
	}

	sendSteps := actionsOfType(actions, sendEmailActionTypeID)
	if len(sendSteps) != len(opts.EmailIDs) {
		return nil, fmt.Errorf(
			"hubspot: workflow %s has %d send-email step(s) but %d email(s) were built; "+
				"point at a workflow with one send step per email",
			flowID, len(sendSteps), len(opts.EmailIDs))
	}
	for i, step := range sendSteps {
		fields, ferr := actionFields(step)
		if ferr != nil {
			return nil, fmt.Errorf("hubspot: workflow %s send step %d: %w", flowID, i+1, ferr)
		}
		fields["content_id"] = opts.EmailIDs[i]
	}

	datedSteps := actionsOfType(actions, waitUntilDateActionTypeID)
	if len(opts.SendDates) > 0 {
		if len(datedSteps) != len(opts.SendDates) {
			return nil, fmt.Errorf(
				"hubspot: workflow %s has %d wait-until-date step(s) but %d send date(s) were given",
				flowID, len(datedSteps), len(opts.SendDates))
		}
		for i, step := range datedSteps {
			if derr := setStepDate(step, opts.SendDates[i]); derr != nil {
				return nil, fmt.Errorf("hubspot: workflow %s dated step %d: %w", flowID, i+1, derr)
			}
		}
	}

	if strings.TrimSpace(opts.EnrollmentListID) != "" {
		enrollment, ok := flow["enrollmentCriteria"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("hubspot: workflow %s has no enrollment criteria to re-point", flowID)
		}
		// REPLACES the list branch rather than merging into it. The source's own membership
		// filters name the source campaign's audience, and keeping them alongside this one would
		// enroll both -- so the operator's list would be a widening of somebody else's, not the
		// audience they built.
		//
		// The shape is HubSpot's: an OR branch holding one AND branch holding one IN_LIST filter.
		// A flatter form is accepted by the create call and then read back by the editor as no
		// enrollment at all.
		enrollment["listFilterBranch"] = map[string]any{
			"filterBranches": []any{
				map[string]any{
					"filterBranches":       []any{},
					"filters":              []any{map[string]any{"listId": opts.EnrollmentListID, "operator": "IN_LIST", "filterType": "IN_LIST"}},
					"filterBranchType":     "AND",
					"filterBranchOperator": "AND",
				},
			},
			"filters":              []any{},
			"filterBranchType":     "OR",
			"filterBranchOperator": "OR",
		}
		flow["enrollmentCriteria"] = enrollment
	}

	// Rebuilt field by field rather than posted back wholesale: the GET response carries
	// server-owned fields (id, revision ids, timestamps, counters) that the create
	// endpoint either rejects or reinterprets as a claim about the NEW flow. A field
	// absent from the source is left out entirely, since an explicit null is not the
	// same as "unset" to this API.
	newFlow := map[string]any{
		"name":      name,
		"isEnabled": false,
		"actions":   actions,
	}
	for _, key := range []string{
		"flowType", "startActionId", "nextAvailableActionId", "enrollmentCriteria",
		"timeWindows", "blockedDates", "customProperties", "dataSources",
		"suppressionListIds", "goalFilterBranch", "canEnrollFromSalesforce", "type", "objectTypeId",
	} {
		if v, present := flow[key]; present && v != nil {
			newFlow[key] = v
		}
	}

	raw, perr := c.doRequest(ctx, http.MethodPost, flowsPath, newFlow, false)
	if perr != nil {
		return nil, fmt.Errorf("hubspot: create workflow %q: %w", name, perr)
	}
	var created struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		IsEnabled bool   `json:"isEnabled"`
	}
	if uerr := json.Unmarshal(raw, &created); uerr != nil || strings.TrimSpace(created.ID) == "" {
		// A 2xx create whose id could not be read is UNCONFIRMED: the workflow may exist,
		// and a blind retry would create a second one enrolling the same contacts.
		return nil, unconfirmed(
			fmt.Sprintf("hubspot: create workflow %q UNCONFIRMED (a workflow may have been created; look for it before retrying)", name),
			uerr)
	}

	return &FlowRef{
		FlowID:           created.ID,
		Name:             orDefaultString(created.Name, name),
		EditURL:          c.FlowEditURL(created.ID),
		SourceFlowID:     flowID,
		SourceFlowName:   stringField(flow, "name"),
		IsEnabled:        created.IsEnabled,
		SendEmailSteps:   len(sendSteps),
		DatedSteps:       len(opts.SendDates),
		EnrollmentListID: strings.TrimSpace(opts.EnrollmentListID),
	}, nil
}

// actionsOfType returns the actions of one type in FLOW ORDER.
//
// Ordered by numeric action id, which is the order the editor creates steps in and so
// the order they read down the canvas. Sorted NUMERICALLY rather than as strings, or
// action 10 would sort between 1 and 2 and a workflow with ten or more steps would map
// its emails to the wrong sends -- in an order that still looks plausible.
func actionsOfType(actions []any, actionTypeID string) []map[string]any {
	var out []map[string]any
	for _, a := range actions {
		action, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if stringField(action, "actionTypeId") != actionTypeID {
			continue
		}
		out = append(out, action)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return actionIDOrder(out[i]) < actionIDOrder(out[j])
	})
	return out
}

// actionIDOrder is an action's id as a number, for ordering.
//
// An id that is not numeric sorts last rather than failing the clone: ordering is a
// property of the source workflow, and refusing an entire build over one odd id would
// be worse than putting that step at the end, where a reviewer sees it.
func actionIDOrder(action map[string]any) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(stringField(action, "actionId")), 10, 64)
	if err != nil {
		return 1<<62 - 1
	}
	return n
}

// actionFields returns an action's mutable fields map.
func actionFields(action map[string]any) (map[string]any, error) {
	fields, ok := action["fields"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("step has no fields object")
	}
	return fields, nil
}

// setStepDate overwrites a wait-until-date step's target date, keeping its time of day.
//
// HubSpot stores the calendar date as a midnight-UTC epoch-milliseconds STRING, in a
// sibling of the step's own time_of_day, which is where the send hour lives. Writing
// the date as anything else -- an ISO string, a number -- is accepted by the create
// call and then read back by the editor as a missing date.
func setStepDate(action map[string]any, when time.Time) error {
	fields, err := actionFields(action)
	if err != nil {
		return err
	}
	date, ok := fields["date"].(map[string]any)
	if !ok {
		return fmt.Errorf("step has no date object")
	}
	utc := time.Date(when.Year(), when.Month(), when.Day(), 0, 0, 0, 0, time.UTC)
	date["staticValue"] = strconv.FormatInt(utc.UnixMilli(), 10)
	return nil
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func orDefaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
