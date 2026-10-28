// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseFlowID(t *testing.T) {
	cases := map[string]string{
		"https://app.hubspot.com/workflows/8112310/platform/flow/1841232183/edit": "1841232183",
		"https://app.hubspot.com/workflows/8112310/platform/flow/42/edit/actions": "42",
		"1841232183":     "1841232183",
		"  1841232183  ": "1841232183",
		"https://app.hubspot.com/email/8112310/edit/224196159396/content": "",
		"not a url": "",
		"":          "",
	}
	for in, want := range cases {
		if got := ParseFlowID(in); got != want {
			t.Errorf("ParseFlowID(%q) = %q, want %q", in, got, want)
		}
	}
}

// sourceFlow is a workflow shaped like one an operator would point at: three sends, each
// gated by its own wait-until-date, plus a step this service knows nothing about.
//
// The canvas order (startActionId, then each step's connection) is deliberately NOT the
// action-id order: step 20 was inserted at the top after the rest were built, so it has the
// highest id while running first. An id sort would put it last and map every email and date to
// the wrong step while still producing a complete-looking workflow.
func sourceFlow() map[string]any {
	action := func(id, typeID, next string, fields map[string]any) map[string]any {
		a := map[string]any{"actionId": id, "actionTypeId": typeID, "fields": fields}
		if next != "" {
			a["connection"] = map[string]any{"edgeType": "STANDARD", "nextActionId": next}
		}
		return a
	}
	dateField := func(ms string) map[string]any {
		return map[string]any{
			"date":        map[string]any{"staticValue": ms},
			"time_of_day": map[string]any{"hour": 9, "minute": 30},
		}
	}
	return map[string]any{
		"id":            "1841232183",
		"name":          "26Q2 - LF Research - Survey Promo",
		"flowType":      "WORKFLOW",
		"isEnabled":     true,
		"revisionId":    "7",
		"startActionId": "20",
		"enrollmentCriteria": map[string]any{
			"type":                               "LIST_BASED",
			"shouldReEnroll":                     true,
			"reEnrollmentTriggersFilterBranches": []any{map[string]any{"listId": "555"}},
			"listFilterBranch":                   map[string]any{"filters": []any{map[string]any{"listId": "555"}}},
		},
		"suppressionListIds": []any{"900"},
		"actions": []any{
			action("2", sendEmailActionTypeID, "9", map[string]any{"content_id": "old-invite"}),
			action("20", waitUntilDateActionTypeID, "2", dateField("1000")),
			action("10", sendEmailActionTypeID, "15", map[string]any{"content_id": "old-deadline"}),
			action("9", waitUntilDateActionTypeID, "8", dateField("3000")),
			action("8", sendEmailActionTypeID, "13", map[string]any{"content_id": "old-reminder"}),
			action("13", waitUntilDateActionTypeID, "10", dateField("2000")),
			action("15", "0-1", "", map[string]any{"unknown": "preserved"}),
		},
	}
}

// threeDates is one date per dated step in sourceFlow.
func threeDates() []time.Time {
	return []time.Time{
		time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC),
	}
}

// capturedFlow is what flowServer's handler saw. httptest runs each handler in its own
// goroutine, so every read goes through the mutex.
type capturedFlow struct {
	mu      sync.Mutex
	posted  map[string]any
	methods []string
}

func (c *capturedFlow) Posted() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.posted
}

func (c *capturedFlow) Methods() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.methods...)
}

// flowServer serves source (sourceFlow when nil) and captures what a create POSTs.
func flowServer(t *testing.T, created map[string]any, source ...map[string]any) (*Client, *capturedFlow) {
	t.Helper()
	got := &capturedFlow{}
	flow := sourceFlow()
	if len(source) > 0 {
		flow = source[0]
	}

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got.mu.Lock()
		defer got.mu.Unlock()
		got.methods = append(got.methods, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, flowsPath+"/"):
			_ = json.NewEncoder(w).Encode(flow)
		case r.Method == http.MethodPost && r.URL.Path == flowsPath:
			_ = json.NewDecoder(r.Body).Decode(&got.posted)
			_ = json.NewEncoder(w).Encode(created)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return c, got
}

func TestCloneFlowWithEmails_MapsEmailsAndDatesInCanvasOrder(t *testing.T) {
	c, got := flowServer(t, map[string]any{"id": "99", "name": "Survey 2026", "isEnabled": false})

	dates := threeDates()
	dates[1] = time.Date(2026, 11, 19, 23, 45, 0, 0, time.UTC)
	ref, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:      "Survey 2026",
		EmailIDs:  []string{"e-invite", "e-reminder", "e-deadline"},
		SendDates: dates,
	})
	if err != nil {
		t.Fatalf("CloneFlowWithEmails: %v", err)
	}

	// The canvas runs 20 -> 2 -> 9 -> 8 -> 13 -> 10 -> 15. Send steps 2, 8, 10 take the emails in
	// that order and dated steps 20, 9, 13 the dates, as midnight-UTC epoch millis. Step 9's input
	// carries a time of day, which must NOT reach the stored date.
	wantContent := map[string]string{"2": "e-invite", "8": "e-reminder", "10": "e-deadline"}
	wantDates := map[string]string{
		"20": "1794182400000",
		"9":  "1795046400000",
		"13": "1795392000000",
	}

	for _, raw := range got.Posted()["actions"].([]any) {
		a := raw.(map[string]any)
		id := a["actionId"].(string)
		fields := a["fields"].(map[string]any)
		switch a["actionTypeId"] {
		case sendEmailActionTypeID:
			if got := fields["content_id"]; got != wantContent[id] {
				t.Errorf("send step %s: content_id = %v, want %v", id, got, wantContent[id])
			}
		case waitUntilDateActionTypeID:
			got := fields["date"].(map[string]any)["staticValue"]
			if got != wantDates[id] {
				t.Errorf("dated step %s: staticValue = %v, want %v", id, got, wantDates[id])
			}
			if tod := fields["time_of_day"].(map[string]any); tod["hour"] != float64(9) {
				t.Errorf("dated step %s: time_of_day was rewritten (%v); only the date may change", id, tod)
			}
		}
	}

	if ref.IsEnabled {
		t.Error("the clone must be reported disabled")
	}
	if ref.SendEmailSteps != 3 || ref.DatedSteps != 3 {
		t.Errorf("rewritten counts = %d sends / %d dates, want 3/3", ref.SendEmailSteps, ref.DatedSteps)
	}
	if ref.SourceFlowName != "26Q2 - LF Research - Survey Promo" {
		t.Errorf("source name = %q", ref.SourceFlowName)
	}

	// The source is only ever READ. Any write to /flows/{id} would mean this edited the
	// operator's live workflow instead of creating a new one.
	for _, m := range got.Methods() {
		if m != "GET "+flowsPath+"/1841232183" && m != "POST "+flowsPath {
			t.Errorf("unexpected request %q: the source workflow must only be read", m)
		}
	}
}

func TestCloneFlowWithEmails_EnrollsThisCampaignsAudience(t *testing.T) {
	// Left alone, the clone would send this campaign's emails to the SOURCE campaign's audience.
	// That is invisible in the editor, where the enrollment step looks correctly filled in.
	c, got := flowServer(t, map[string]any{"id": "99"})

	ref, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:              "Survey 2026",
		EmailIDs:          []string{"a", "b", "c"},
		SendDates:         threeDates(),
		EnrollmentListIDs: []string{"list-77", "list-78", "list-77"},
	})
	if err != nil {
		t.Fatalf("CloneFlowWithEmails: %v", err)
	}

	enrollment := got.Posted()["enrollmentCriteria"].(map[string]any)
	branch := enrollment["listFilterBranch"].(map[string]any)
	if branch["filterBranchType"] != "OR" {
		t.Errorf("outer branch = %v, want OR so a contact in any list is enrolled", branch["filterBranchType"])
	}
	inner := branch["filterBranches"].([]any)
	// One AND branch per DISTINCT list: the source's own list is replaced, not joined, and a
	// repeated id is not a second branch.
	if len(inner) != 2 {
		t.Fatalf("expected one enrollment branch per list, got %d", len(inner))
	}
	for i, want := range []string{"list-77", "list-78"} {
		filters := inner[i].(map[string]any)["filters"].([]any)
		if len(filters) != 1 {
			t.Fatalf("branch %d: expected one membership filter, got %d", i, len(filters))
		}
		filter := filters[0].(map[string]any)
		if filter["listId"] != want || filter["operator"] != "IN_LIST" || filter["filterType"] != "IN_LIST" {
			t.Errorf("branch %d filter = %v, want an IN_LIST on %s", i, filter, want)
		}
	}
	// Re-enrollment triggers name the SOURCE's lists; a series sends once per contact.
	if _, kept := enrollment["reEnrollmentTriggersFilterBranches"]; kept {
		t.Error("the source's re-enrollment triggers were kept")
	}
	if enrollment["shouldReEnroll"] != false {
		t.Errorf("shouldReEnroll = %v, want false", enrollment["shouldReEnroll"])
	}
	if got := strings.Join(ref.EnrollmentListIDs, ","); got != "list-77,list-78" {
		t.Errorf("EnrollmentListIDs = %q, want them reported so the operator can see who is enrolled", got)
	}
}

func TestCloneFlowWithEmails_AddsTheAudiencesSuppressionsToTheSources(t *testing.T) {
	// A workflow's send step does not apply the suppression lists recorded on the draft, so the
	// audience's must reach the workflow -- beside the source's own, which are portal policy. The
	// BeforeCreate hook sees exactly that merged set, before anything is created.
	c, got := flowServer(t, map[string]any{"id": "99"})

	var seen []string
	ref, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:               "Survey 2026",
		EmailIDs:           []string{"a", "b", "c"},
		SendDates:          threeDates(),
		SuppressionListIDs: []string{"9002", "900", " "},
		BeforeCreate: func(ids []string) error {
			seen = ids
			return nil
		},
	})
	if err != nil {
		t.Fatalf("CloneFlowWithEmails: %v", err)
	}
	posted := got.Posted()["suppressionListIds"].([]any)
	if len(posted) != 2 || posted[0] != "900" || posted[1] != "9002" {
		t.Errorf("suppressionListIds = %v, want the source's 900 kept and 9002 added once", posted)
	}
	if strings.Join(ref.SuppressionListIDs, ",") != "900,9002" || strings.Join(seen, ",") != "900,9002" {
		t.Errorf("reported %v / hook saw %v, want the whole set [900 9002]", ref.SuppressionListIDs, seen)
	}
}

func TestCloneFlowWithEmails_BeforeCreateRefusalCreatesNothing(t *testing.T) {
	c, got := flowServer(t, map[string]any{"id": "99"})
	refusal := errors.New("the standing list covers a send list")

	_, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:         "Survey 2026",
		EmailIDs:     []string{"a", "b", "c"},
		SendDates:    threeDates(),
		BeforeCreate: func([]string) error { return refusal },
	})
	if !errors.Is(err, refusal) {
		t.Fatalf("error = %v, want the hook's refusal passed through", err)
	}
	if got.Posted() != nil {
		t.Error("a workflow was created although the caller's check refused it")
	}
}

func TestMergeSuppressionListIDs_FollowsTheSourcesElementType(t *testing.T) {
	// A flow that lists its suppressions as numbers gets numbers added, so the posted array is
	// not a mix of the two.
	got, err := mergeSuppressionListIDs([]any{float64(900)}, []string{"9002", "900"})
	if err != nil {
		t.Fatalf("mergeSuppressionListIDs: %v", err)
	}
	if len(got) != 2 || got[0] != float64(900) || got[1] != int64(9002) {
		t.Errorf("merged = %#v, want [900 9002] as numbers", got)
	}
	if _, err := mergeSuppressionListIDs([]any{float64(900)}, []string{"not-a-number"}); err == nil {
		t.Error("a non-numeric id was added to a numeric suppression array")
	}
	if _, err := mergeSuppressionListIDs("900", nil); err == nil {
		t.Error("a suppressionListIds that is not an array was accepted")
	}
}

func TestCloneFlowWithEmails_KeepsTheSourceEnrollmentWhenNoListIsGiven(t *testing.T) {
	// No list means the caller asked for the source's own criteria. Writing an empty filter
	// instead would enroll everyone or no one.
	c, got := flowServer(t, map[string]any{"id": "99"})

	ref, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:      "Survey 2026",
		EmailIDs:  []string{"a", "b", "c"},
		SendDates: threeDates(),
	})
	if err != nil {
		t.Fatalf("CloneFlowWithEmails: %v", err)
	}
	enrollment := got.Posted()["enrollmentCriteria"].(map[string]any)
	if lfb := enrollment["listFilterBranch"].(map[string]any); lfb["filters"].([]any)[0].(map[string]any)["listId"] != "555" {
		t.Errorf("the source's enrollment was rewritten although no audience was given: %v", lfb)
	}
	if len(ref.EnrollmentListIDs) != 0 {
		t.Errorf("EnrollmentListIDs = %v, want none to say the source's audience was kept", ref.EnrollmentListIDs)
	}
}

func TestCloneFlowWithEmails_IsAlwaysCreatedDisabled(t *testing.T) {
	// The source is enabled, and HubSpot is made to answer with isEnabled true, which is
	// the shape that would matter most: a clone that inherited "on" would start enrolling
	// contacts and sending live email from drafts nobody has reviewed.
	c, got := flowServer(t, map[string]any{"id": "99", "isEnabled": true})

	ref, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:      "Survey 2026",
		EmailIDs:  []string{"a", "b", "c"},
		SendDates: threeDates(),
	})
	if err != nil {
		t.Fatalf("CloneFlowWithEmails: %v", err)
	}
	if enabled, ok := got.Posted()["isEnabled"].(bool); !ok || enabled {
		t.Errorf("posted isEnabled = %v, want false", got.Posted()["isEnabled"])
	}
	// Reported, not assumed: the caller is told what HubSpot said, so a portal that
	// ignored the flag is visible rather than papered over.
	if !ref.IsEnabled {
		t.Error("IsEnabled should report the server's answer, not the request's")
	}
}

func TestCloneFlowWithEmails_RefusesBeforeCreating(t *testing.T) {
	branching := sourceFlow()
	branching["actions"].([]any)[6].(map[string]any)["staticBranches"] = []any{}
	offPath := sourceFlow()
	offPath["actions"] = append(offPath["actions"].([]any),
		map[string]any{"actionId": "30", "actionTypeId": sendEmailActionTypeID, "fields": map[string]any{"content_id": "old-orphan"}})
	eventBased := sourceFlow()
	eventBased["enrollmentCriteria"].(map[string]any)["type"] = "EVENT_BASED"
	goalOnSource := sourceFlow()
	goalOnSource["goalFilterBranch"] = map[string]any{"filters": []any{map[string]any{"emailId": "old-reminder"}}}
	noStart := sourceFlow()
	delete(noStart, "startActionId")

	cases := []struct {
		name   string
		source map[string]any
		opts   CloneFlowOptions
		is     error
		want   string
	}{
		{
			name: "fewer emails than send steps",
			opts: CloneFlowOptions{Name: "n", EmailIDs: []string{"a", "b"}, SendDates: threeDates()},
			is:   ErrFlowStepCountMismatch, want: "3 send-email step(s) but 2 email(s)",
		},
		{
			name: "fewer dates than dated steps",
			opts: CloneFlowOptions{Name: "n", EmailIDs: []string{"a", "b", "c"}, SendDates: threeDates()[:1]},
			is:   ErrFlowStepCountMismatch, want: "3 wait-until-date step(s) but 1 send date(s)",
		},
		{
			// The source's own dates belong to another campaign and are usually past, where the
			// whole series would go out back to back the moment it is enabled.
			name: "no dates for a dated workflow",
			opts: CloneFlowOptions{Name: "n", EmailIDs: []string{"a", "b", "c"}},
			is:   ErrFlowStepCountMismatch, want: "3 wait-until-date step(s) but 0 send date(s)",
		},
		{
			name: "a branching workflow", source: branching,
			opts: CloneFlowOptions{Name: "n", EmailIDs: []string{"a", "b", "c"}, SendDates: threeDates()},
			is:   ErrFlowShapeUnsupported, want: "branches",
		},
		{
			name: "a send step off the main path", source: offPath,
			opts: CloneFlowOptions{Name: "n", EmailIDs: []string{"a", "b", "c"}, SendDates: threeDates()},
			is:   ErrFlowShapeUnsupported, want: "not on its main path",
		},
		{
			name: "no start action", source: noStart,
			opts: CloneFlowOptions{Name: "n", EmailIDs: []string{"a", "b", "c"}, SendDates: threeDates()},
			is:   ErrFlowShapeUnsupported, want: "startActionId",
		},
		{
			name: "an event-based enrollment", source: eventBased,
			opts: CloneFlowOptions{Name: "n", EmailIDs: []string{"a", "b", "c"}, SendDates: threeDates(), EnrollmentListIDs: []string{"77"}},
			is:   ErrFlowShapeUnsupported, want: `"EVENT_BASED"`,
		},
		{
			name: "a goal naming a source email", source: goalOnSource,
			opts: CloneFlowOptions{Name: "n", EmailIDs: []string{"a", "b", "c"}, SendDates: threeDates()},
			is:   ErrFlowShapeUnsupported, want: "old-reminder",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var source []map[string]any
			if tc.source != nil {
				source = append(source, tc.source)
			}
			c, got := flowServer(t, map[string]any{"id": "99"}, source...)
			_, err := c.CloneFlowWithEmails(context.Background(), "1841232183", tc.opts)
			if !errors.Is(err, tc.is) {
				t.Fatalf("error = %v, want %v", err, tc.is)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name the problem (%q)", err, tc.want)
			}
			// Refused BEFORE the create: a partially-mapped workflow whose remaining send
			// still points at the source campaign's email is worse than no workflow, and
			// reads as a filled-in step rather than a missing one.
			if got.Posted() != nil {
				t.Error("a workflow was created despite the refusal")
			}
			for _, m := range got.Methods() {
				if strings.HasPrefix(m, http.MethodPost) {
					t.Errorf("unexpected %q before the shape was checked", m)
				}
			}
		})
	}
}

func TestCloneFlowWithEmails_KeepsFieldsItDoesNotUnderstand(t *testing.T) {
	// A flow definition carries far more than this service models. Anything dropped here
	// is silently deleted from the operator's cloned workflow, so the fields that are
	// carried over are pinned, and the server-owned ones are pinned as NOT carried.
	c, got := flowServer(t, map[string]any{"id": "99"})

	if _, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:      "Survey 2026",
		EmailIDs:  []string{"a", "b", "c"},
		SendDates: threeDates(),
	}); err != nil {
		t.Fatalf("CloneFlowWithEmails: %v", err)
	}
	posted := got.Posted()
	for _, key := range []string{"flowType", "enrollmentCriteria", "suppressionListIds", "startActionId"} {
		if _, ok := posted[key]; !ok {
			t.Errorf("%q was dropped from the clone", key)
		}
	}
	// id and revisionId belong to the SOURCE. Posting them would at best be ignored and
	// at worst be read as a claim about the new flow.
	for _, key := range []string{"id", "revisionId"} {
		if _, ok := posted[key]; ok {
			t.Errorf("%q was copied onto the new flow", key)
		}
	}
	// The step this service has no opinion about survives untouched.
	var found bool
	for _, raw := range posted["actions"].([]any) {
		if a := raw.(map[string]any); a["actionId"] == "15" {
			found = true
			if a["fields"].(map[string]any)["unknown"] != "preserved" {
				t.Error("an unmodelled step's fields were rewritten")
			}
		}
	}
	if !found {
		t.Error("an unmodelled step was dropped from the clone")
	}
}

func TestCloneFlowWithEmails_UnreadableCreateIsUnconfirmed(t *testing.T) {
	// A 2xx whose id cannot be read means the workflow MAY exist. Reported as
	// unconfirmed so a caller does not blind-retry and enroll the same contacts twice.
	c, _ := flowServer(t, map[string]any{"name": "no id here"})

	_, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:      "Survey 2026",
		EmailIDs:  []string{"a", "b", "c"},
		SendDates: threeDates(),
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsUnconfirmed(err) {
		t.Errorf("error = %v, want it classified unconfirmed", err)
	}
}
