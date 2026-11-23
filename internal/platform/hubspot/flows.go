// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
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
	// EnrollmentListIDs are the lists the new workflow enrolls, or none when the source's own
	// enrollment was kept. Reported because they decide WHO receives the series, and an operator
	// reading a success message has no other way to tell which of the two happened.
	EnrollmentListIDs []string `json:"enrollment_list_ids"`
	// SuppressionListIDs is the new workflow's whole suppression set: the source's own lists
	// plus the ones the caller added.
	SuppressionListIDs []string `json:"suppression_list_ids"`
}

// FlowEditURL is where a human reviews and enables a workflow.
func (c *Client) FlowEditURL(flowID string) string {
	return FlowEditURLFor(c.account.PortalID, flowID)
}

// FlowEditURLFor is a workflow's editor link in a named portal. A caller that has VERIFIED the
// portal (the send-workflow path proves it before creating anything) builds the link from that,
// because a connection with no configured portal id leaves the client's own copy empty and the
// link would read /workflows//platform/...
func FlowEditURLFor(portalID, flowID string) string {
	return fmt.Sprintf("https://app.hubspot.com/workflows/%s/platform/flow/%s/edit", portalID, flowID)
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
	// EnrollmentListIDs replace WHO the workflow enrolls: the campaign's own audience, rather
	// than whichever list the source workflow was built around. A contact in ANY of them is
	// enrolled, matching an email sent to several lists. Empty keeps the source's enrollment
	// criteria untouched.
	//
	// This is the difference between a clone that is a template and a clone that is a campaign.
	// Left alone, the new workflow would send this campaign's emails to the SOURCE campaign's
	// audience -- a mistake that is invisible in the editor, because the enrollment step looks
	// correctly filled in.
	EnrollmentListIDs []string
	// SuppressionListIDs are ADDED to the source workflow's own suppression lists, never put in
	// their place. A workflow's send step ignores the suppression lists recorded on the draft,
	// so the audience's suppressions have to be carried here or the series would send to
	// contacts the staged drafts exclude. The source's lists are kept because they are the
	// portal's own policy.
	SuppressionListIDs []string
	// BeforeCreate, when set, is called with the new workflow's whole suppression set (the
	// source's own lists plus SuppressionListIDs) after every rewrite and before the create
	// call. A non-nil error stops the clone with nothing created. It is how a caller runs its
	// send-time checks against the suppressions the workflow will ACTUALLY apply, which include
	// lists only the source definition names.
	BeforeCreate func(suppressionListIDs []string) error
}

// ErrFlowStepCountMismatch is returned, wrapped, when the source workflow's send or
// wait-until-date steps do not pair one-to-one with the emails or dates given. Nothing is
// created. The message names both counts.
var ErrFlowStepCountMismatch = errors.New("hubspot: workflow step count does not match")

// ErrFlowShapeUnsupported is returned, wrapped, when the source workflow cannot be cloned
// safely: two of its sends (or two of its dated steps) sit on parallel branches so no order
// exists, a send or dated step is not reachable from its start action, it loops, it enrolls by
// something other than list membership, or it would keep pointing at the source campaign's
// emails. Nothing is created.
var ErrFlowShapeUnsupported = errors.New("hubspot: workflow shape is not supported for a send series")

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
		return nil, fmt.Errorf("%w: workflow %s has no actions to rewrite", ErrFlowShapeUnsupported, flowID)
	}
	path, perr := flowSeriesSteps(flow, actions)
	if perr != nil {
		return nil, fmt.Errorf("%w: workflow %s: %v", ErrFlowShapeUnsupported, flowID, perr)
	}

	sendSteps := actionsOfType(path, sendEmailActionTypeID)
	if len(sendSteps) != len(opts.EmailIDs) {
		return nil, fmt.Errorf(
			"%w: workflow %s has %d send-email step(s) but %d email(s) were built; "+
				"point at a workflow with one send step per email",
			ErrFlowStepCountMismatch, flowID, len(sendSteps), len(opts.EmailIDs))
	}
	// The source campaign's own email ids, read before they are overwritten, so the rewritten
	// definition can be checked for any reference to them that survived.
	sourceContentIDs := make([]string, 0, len(sendSteps))
	for i, step := range sendSteps {
		fields, ferr := actionFields(step)
		if ferr != nil {
			return nil, fmt.Errorf("%w: workflow %s send step %d: %v", ErrFlowShapeUnsupported, flowID, i+1, ferr)
		}
		if old := flowIDString(fields["content_id"]); old != "" {
			sourceContentIDs = append(sourceContentIDs, old)
		}
		fields["content_id"] = opts.EmailIDs[i]
	}

	// Every dated step needs a date from the caller. Keeping the source's would schedule this
	// series on ANOTHER campaign's calendar -- usually in the past, where HubSpot moves an
	// enrolled contact straight on, so the whole series would go out back to back on enable.
	datedSteps := actionsOfType(path, waitUntilDateActionTypeID)
	if len(datedSteps) != len(opts.SendDates) {
		return nil, fmt.Errorf(
			"%w: workflow %s has %d wait-until-date step(s) but %d send date(s) were given; give one date per step",
			ErrFlowStepCountMismatch, flowID, len(datedSteps), len(opts.SendDates))
	}
	for i, step := range datedSteps {
		if derr := setStepDate(step, opts.SendDates[i]); derr != nil {
			return nil, fmt.Errorf("%w: workflow %s dated step %d: %v", ErrFlowShapeUnsupported, flowID, i+1, derr)
		}
	}

	enrollIDs := uniqueTrimmed(opts.EnrollmentListIDs)
	if len(enrollIDs) > 0 {
		enrollment, ok := flow["enrollmentCriteria"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: workflow %s has no enrollment criteria to re-point", ErrFlowShapeUnsupported, flowID)
		}
		// Only a LIST_BASED workflow enrolls through listFilterBranch. On an event-based or
		// manual one, rewriting that branch would leave the source's own triggers in charge --
		// enrolling the source campaign's audience, the mistake this rewrite exists to prevent.
		if t := stringField(enrollment, "type"); t != "LIST_BASED" {
			return nil, fmt.Errorf("%w: workflow %s enrolls by %q, not by list membership (LIST_BASED)",
				ErrFlowShapeUnsupported, flowID, t)
		}
		// REPLACES the list branch rather than merging into it. The source's own membership
		// filters name the source campaign's audience, and keeping them alongside this one would
		// enroll both -- so the operator's list would be a widening of somebody else's, not the
		// audience they built.
		//
		// The shape is HubSpot's: an OR branch holding one AND branch per list, each holding one
		// IN_LIST filter, so a contact in any of the lists is enrolled. A flatter form is
		// accepted by the create call and then read back by the editor as no enrollment at all.
		branches := make([]any, 0, len(enrollIDs))
		for _, id := range enrollIDs {
			branches = append(branches, map[string]any{
				"filterBranches":       []any{},
				"filters":              []any{map[string]any{"listId": id, "operator": "IN_LIST", "filterType": "IN_LIST"}},
				"filterBranchType":     "AND",
				"filterBranchOperator": "AND",
			})
		}
		enrollment["listFilterBranch"] = map[string]any{
			"filterBranches":       branches,
			"filters":              []any{},
			"filterBranchType":     "OR",
			"filterBranchOperator": "OR",
		}
		// Re-enrollment triggers name the SOURCE's lists, and a series sends once per contact.
		delete(enrollment, "reEnrollmentTriggersFilterBranches")
		enrollment["shouldReEnroll"] = false
		flow["enrollmentCriteria"] = enrollment
	}

	suppression, serr := mergeSuppressionListIDs(flow["suppressionListIds"], opts.SuppressionListIDs)
	if serr != nil {
		return nil, fmt.Errorf("%w: workflow %s: %v", ErrFlowShapeUnsupported, flowID, serr)
	}
	if len(suppression) > 0 {
		flow["suppressionListIds"] = suppression
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

	// Anything still naming the source campaign's emails -- a goal, a custom property, an
	// action this service does not rewrite -- would decide who gets later sends by what they
	// did with ANOTHER campaign's email, and would look correctly filled in.
	if lerr := refuseLeftoverContentIDs(newFlow, sourceContentIDs, opts.EmailIDs); lerr != nil {
		return nil, fmt.Errorf("%w: workflow %s: %v", ErrFlowShapeUnsupported, flowID, lerr)
	}

	suppressionIDs := flowIDStrings(suppression)
	if opts.BeforeCreate != nil {
		if berr := opts.BeforeCreate(suppressionIDs); berr != nil {
			return nil, berr
		}
	}

	raw, perr2 := c.doRequest(ctx, http.MethodPost, flowsPath, newFlow, false)
	if perr2 != nil {
		return nil, fmt.Errorf("hubspot: create workflow %q: %w", name, perr2)
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
		FlowID:             created.ID,
		Name:               orDefaultString(created.Name, name),
		EditURL:            c.FlowEditURL(created.ID),
		SourceFlowID:       flowID,
		SourceFlowName:     stringField(flow, "name"),
		IsEnabled:          created.IsEnabled,
		SendEmailSteps:     len(sendSteps),
		DatedSteps:         len(opts.SendDates),
		EnrollmentListIDs:  enrollIDs,
		SuppressionListIDs: suppressionIDs,
	}, nil
}

// refuseLeftoverContentIDs fails when the rewritten definition still carries one of the source's
// email ids as a value anywhere, unless that id is also one of this series' own emails.
func refuseLeftoverContentIDs(flow map[string]any, sourceIDs, ownIDs []string) error {
	stale := make(map[string]struct{}, len(sourceIDs))
	for _, id := range sourceIDs {
		if !slices.Contains(ownIDs, id) {
			stale[id] = struct{}{}
		}
	}
	if len(stale) == 0 {
		return nil
	}
	var found string
	var walk func(v any)
	walk = func(v any) {
		if found != "" {
			return
		}
		switch t := v.(type) {
		case map[string]any:
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		case string, float64:
			if _, hit := stale[flowIDString(t)]; hit {
				found = flowIDString(t)
			}
		}
	}
	walk(flow)
	if found != "" {
		return fmt.Errorf("it still refers to the source campaign's email %s outside its send steps "+
			"(a branch, goal or other step); point at a workflow whose only email references are its send steps", found)
	}
	return nil
}

// uniqueTrimmed is in with blanks and repeats dropped, order kept.
func uniqueTrimmed(in []string) []string {
	var out []string
	seen := make(map[string]struct{}, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// mergeSuppressionListIDs is the source flow's suppressionListIds with add appended, each id
// once. The source's own entries are kept exactly as read. An added id takes the source's
// element type -- a number when the source lists numbers -- so the posted array is not a mix
// the create call may refuse; with no source entries it stays a string, which is how every
// other list id in a flow definition (the IN_LIST filter's listId) is written.
func mergeSuppressionListIDs(source any, add []string) ([]any, error) {
	var out []any
	if source != nil {
		existing, ok := source.([]any)
		if !ok {
			return nil, fmt.Errorf("suppressionListIds is not an array")
		}
		out = append(out, existing...)
	}
	numeric := false
	if len(out) > 0 {
		_, numeric = out[0].(float64)
	}
	have := make(map[string]struct{}, len(out)+len(add))
	for _, v := range out {
		have[flowIDString(v)] = struct{}{}
	}
	for _, id := range uniqueTrimmed(add) {
		if _, dup := have[id]; dup {
			continue
		}
		have[id] = struct{}{}
		if !numeric {
			out = append(out, id)
			continue
		}
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("suppression list id %q is not numeric, but the workflow lists its suppressions as numbers", id)
		}
		out = append(out, n)
	}
	return out, nil
}

// flowIDString renders one id from a flow definition, which decodes a number as float64.
func flowIDString(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

// flowIDStrings renders a flow id array for reporting.
func flowIDStrings(in []any) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, flowIDString(v))
	}
	return out
}

// flowSeriesSteps returns the workflow's send-email and wait-until-date steps, each kind in the
// order a contact meets them: the sends first, then the dated steps.
//
// The workflow is walked from startActionId along EVERY edge out of each action -- the plain
// connection and each arm of a branch -- and a step's place is read off what it can reach. Not off
// its action id: a step inserted later gets the next free id, so an id sort puts it last while it
// sits earlier on the canvas, and the emails would be mapped to the wrong sends in an order that
// still looks plausible.
//
// A branch is walked THROUGH, not refused. A survey sequence gates a later send on one (skip
// whoever already responded), so refusing at the first fork refused the very workflow this clone
// exists to serve. What a series needs is an unambiguous ORDER, and that is what is checked: each
// kind of step must be totally ordered by reachability, every step able to reach the one after it.
// Two steps on parallel arms are incomparable, no order exists, and the clone is refused -- mapping
// email 2 onto one of them would be a guess, and a plausible-looking one.
//
// A step the walk cannot reach is refused as well, because left in place it would keep the SOURCE
// campaign's email or date. That also makes an unrecognised branch spelling fail closed: an arm
// the walk does not read is an arm it does not follow, so what lies behind it is unreachable.
func flowSeriesSteps(flow map[string]any, actions []any) ([]map[string]any, error) {
	byID := make(map[string]map[string]any, len(actions))
	for _, a := range actions {
		action, ok := a.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("an action is not an object")
		}
		byID[flowIDString(action["actionId"])] = action
	}
	start := flowIDString(flow["startActionId"])
	if start == "" {
		return nil, fmt.Errorf("it names no startActionId")
	}
	reach, err := flowReach(byID, start)
	if err != nil {
		return nil, err
	}

	var ordered []map[string]any
	for _, typeID := range []string{sendEmailActionTypeID, waitUntilDateActionTypeID} {
		var steps []map[string]any
		total := 0
		for _, a := range actions {
			action, _ := a.(map[string]any)
			if stringField(action, "actionTypeId") != typeID {
				continue
			}
			total++
			if _, ok := reach[flowIDString(action["actionId"])]; ok {
				steps = append(steps, action)
			}
		}
		if len(steps) != total {
			return nil, fmt.Errorf("%d of its %d %s step(s) are not reachable from its start action", total-len(steps), total, typeID)
		}
		sorted, serr := flowOrderByReach(steps, reach)
		if serr != nil {
			return nil, serr
		}
		ordered = append(ordered, sorted...)
	}
	return ordered, nil
}

// flowReach maps every action reachable from start to the set of actions reachable FROM it. It
// fails on a reference to an action the workflow does not define and on a loop: a loop makes two
// steps each reachable from the other, which satisfies "comparable" while being no order at all.
func flowReach(byID map[string]map[string]any, start string) (map[string]map[string]struct{}, error) {
	reach := make(map[string]map[string]struct{}, len(byID))
	onPath := make(map[string]struct{}, len(byID))
	var visit func(id string) error
	visit = func(id string) error {
		if _, loop := onPath[id]; loop {
			return fmt.Errorf("action %s is reached twice", id)
		}
		if _, done := reach[id]; done {
			return nil
		}
		action, ok := byID[id]
		if !ok {
			return fmt.Errorf("action %s is referenced but not defined", id)
		}
		nexts, err := flowSuccessors(action, id)
		if err != nil {
			return err
		}
		onPath[id] = struct{}{}
		below := make(map[string]struct{})
		for _, n := range nexts {
			if err := visit(n); err != nil {
				return err
			}
			below[n] = struct{}{}
			for d := range reach[n] {
				below[d] = struct{}{}
			}
		}
		delete(onPath, id)
		reach[id] = below
		return nil
	}
	if err := visit(start); err != nil {
		return nil, err
	}
	return reach, nil
}

// flowOrderByReach sorts steps into the order a contact meets them, or reports that none exists.
//
// A step's rank is how many OTHER steps it can reach. In a total order those ranks are exactly
// len-1 down to 0 -- the first step reaches every other, the last reaches none -- so two steps
// sharing a rank is the proof that they sit on parallel arms. Distinct ranks are sufficient too: in
// an acyclic graph, n distinct ranks in [0, n-1] force a chain, because the step ranked n-1 reaches
// all the rest and the same argument then repeats on what remains.
func flowOrderByReach(steps []map[string]any, reach map[string]map[string]struct{}) ([]map[string]any, error) {
	rank := make(map[string]int, len(steps))
	owner := make(map[int]string, len(steps))
	for _, step := range steps {
		id := flowIDString(step["actionId"])
		n := 0
		for _, other := range steps {
			otherID := flowIDString(other["actionId"])
			if otherID == id {
				continue
			}
			if _, ok := reach[id][otherID]; ok {
				n++
			}
		}
		if twin, dup := owner[n]; dup {
			return nil, fmt.Errorf("actions %s and %s are on parallel branches, so neither comes first; a send series needs one order", twin, id)
		}
		owner[n] = id
		rank[id] = n
	}
	ordered := slices.Clone(steps)
	slices.SortFunc(ordered, func(a, b map[string]any) int {
		return rank[flowIDString(b["actionId"])] - rank[flowIDString(a["actionId"])]
	})
	return ordered, nil
}

// flowSuccessors returns every action a contact can move to from this one: the plain connection,
// each arm of a static or list branch, and the default arm a contact matching no branch takes.
//
// Only the PLAIN connection's edge type is checked. A branch arm's edge is a branch edge by
// construction, so demanding a type there would refuse every branching workflow again under
// another name.
func flowSuccessors(action map[string]any, id string) ([]string, error) {
	var out []string
	follow := func(conn map[string]any) {
		if n := flowIDString(conn["nextActionId"]); n != "" {
			out = append(out, n)
		}
	}
	if conn, _ := action["connection"].(map[string]any); conn != nil {
		// STANDARD is the next step in line. GOTO is a jump to an action defined elsewhere in the
		// workflow, e.g. a branch rejoining the main line; workflow 1841232183 has one. Both are
		// followed as the reachability they are -- a GOTO that jumps BACK is a loop, which flowReach
		// refuses. Any other type is one this walk has not been shown, and following it blind
		// could map a send onto an order nobody checked.
		if et := stringField(conn, "edgeType"); et != "" && et != "STANDARD" && et != "GOTO" {
			return nil, fmt.Errorf("action %s continues by a %s edge, which this walk does not read", id, et)
		}
		follow(conn)
	}
	for _, key := range []string{"staticBranches", "listBranches"} {
		arms, _ := action[key].([]any)
		for _, a := range arms {
			arm, _ := a.(map[string]any)
			if arm == nil {
				continue
			}
			// An arm names its target through a nested connection on the shapes seen, and carries
			// the id itself on others; reading only one would silently drop the arm.
			if conn, ok := arm["connection"].(map[string]any); ok {
				follow(conn)
			} else {
				follow(arm)
			}
		}
	}
	switch db := action["defaultBranch"].(type) {
	case map[string]any:
		if conn, ok := db["connection"].(map[string]any); ok {
			follow(conn)
		} else {
			follow(db)
		}
	case string:
		if n := strings.TrimSpace(db); n != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

// actionsOfType returns the actions of one type, in the order given.
func actionsOfType(actions []map[string]any, actionTypeID string) []map[string]any {
	var out []map[string]any
	for _, action := range actions {
		if stringField(action, "actionTypeId") == actionTypeID {
			out = append(out, action)
		}
	}
	return out
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
