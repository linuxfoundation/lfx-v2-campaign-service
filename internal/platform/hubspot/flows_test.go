// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

// sourceFlow is a workflow shaped like one an operator would point at: three sends,
// each gated by its own wait-until-date, plus a step this service knows nothing about.
//
// The action ids are deliberately NOT in ascending textual order and cross ten, which
// is what catches a string sort: "10" sorts before "2", so a lexical ordering would map
// the second email to the third send while still producing a complete-looking workflow.
func sourceFlow() map[string]any {
	action := func(id, typeID string, fields map[string]any) map[string]any {
		return map[string]any{"actionId": id, "actionTypeId": typeID, "fields": fields}
	}
	dateField := func(ms string) map[string]any {
		return map[string]any{
			"date":        map[string]any{"staticValue": ms},
			"time_of_day": map[string]any{"hour": 9, "minute": 30},
		}
	}
	return map[string]any{
		"id":                 "1841232183",
		"name":               "26Q2 - LF Research - Survey Promo",
		"flowType":           "WORKFLOW",
		"isEnabled":          true,
		"revisionId":         "7",
		"enrollmentCriteria": map[string]any{"shouldReEnroll": false},
		"suppressionListIds": []any{"900"},
		"actions": []any{
			action("2", sendEmailActionTypeID, map[string]any{"content_id": "old-invite"}),
			action("4", waitUntilDateActionTypeID, dateField("1000")),
			action("10", sendEmailActionTypeID, map[string]any{"content_id": "old-deadline"}),
			action("9", waitUntilDateActionTypeID, dateField("3000")),
			action("8", sendEmailActionTypeID, map[string]any{"content_id": "old-reminder"}),
			action("13", waitUntilDateActionTypeID, dateField("2000")),
			action("15", "0-1", map[string]any{"unknown": "preserved"}),
		},
	}
}

// flowServer serves the source flow and captures what a create POSTs.
func flowServer(t *testing.T, created map[string]any) (*Client, *map[string]any, *[]string) {
	t.Helper()
	var posted map[string]any
	var methods []string

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, flowsPath+"/"):
			_ = json.NewEncoder(w).Encode(sourceFlow())
		case r.Method == http.MethodPost && r.URL.Path == flowsPath:
			_ = json.NewDecoder(r.Body).Decode(&posted)
			_ = json.NewEncoder(w).Encode(created)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return c, &posted, &methods
}

func TestCloneFlowWithEmails_MapsEmailsAndDatesInFlowOrder(t *testing.T) {
	c, posted, methods := flowServer(t, map[string]any{"id": "99", "name": "Survey 2026", "isEnabled": false})

	ref, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:     "Survey 2026",
		EmailIDs: []string{"e-invite", "e-reminder", "e-deadline"},
		SendDates: []time.Time{
			time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 10, 19, 23, 45, 0, 0, time.UTC),
			time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC),
		},
	})
	if err != nil {
		t.Fatalf("CloneFlowWithEmails: %v", err)
	}

	// Send steps 2, 8, 10 take the emails in that order. A lexical sort would put 10
	// second and hand the reminder's email to the closing send.
	wantContent := map[string]string{"2": "e-invite", "8": "e-reminder", "10": "e-deadline"}
	// Dated steps 4, 9, 13 take the dates in that order, as midnight-UTC epoch millis.
	// Step 9's input carries a time of day, which must NOT reach the stored date.
	wantDates := map[string]string{
		"4":  "1791504000000",
		"9":  "1792368000000",
		"13": "1792713600000",
	}

	for _, raw := range (*posted)["actions"].([]any) {
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
	for _, m := range *methods {
		if m != "GET "+flowsPath+"/1841232183" && m != "POST "+flowsPath {
			t.Errorf("unexpected request %q: the source workflow must only be read", m)
		}
	}
}

func TestCloneFlowWithEmails_EnrollsThisCampaignsAudience(t *testing.T) {
	// Left alone, the clone would send this campaign's emails to the SOURCE campaign's audience.
	// That is invisible in the editor, where the enrollment step looks correctly filled in.
	c, posted, _ := flowServer(t, map[string]any{"id": "99"})

	ref, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:             "Survey 2026",
		EmailIDs:         []string{"a", "b", "c"},
		EnrollmentListID: "list-77",
	})
	if err != nil {
		t.Fatalf("CloneFlowWithEmails: %v", err)
	}

	enrollment := (*posted)["enrollmentCriteria"].(map[string]any)
	branch := enrollment["listFilterBranch"].(map[string]any)
	inner := branch["filterBranches"].([]any)
	if len(inner) != 1 {
		t.Fatalf("expected exactly one enrollment branch, got %d -- the source's own list must be replaced, not joined", len(inner))
	}
	filters := inner[0].(map[string]any)["filters"].([]any)
	if len(filters) != 1 {
		t.Fatalf("expected one membership filter, got %d", len(filters))
	}
	filter := filters[0].(map[string]any)
	if filter["listId"] != "list-77" || filter["operator"] != "IN_LIST" || filter["filterType"] != "IN_LIST" {
		t.Errorf("enrollment filter = %v, want an IN_LIST on list-77", filter)
	}
	if ref.EnrollmentListID != "list-77" {
		t.Errorf("EnrollmentListID = %q, want it reported so the operator can see who is enrolled", ref.EnrollmentListID)
	}
}

func TestCloneFlowWithEmails_KeepsTheSourceEnrollmentWhenNoListIsGiven(t *testing.T) {
	// No list means "this service could not tell who to enrol". Keeping the source's criteria is
	// the honest outcome; writing an empty filter would enroll everyone or no one.
	c, posted, _ := flowServer(t, map[string]any{"id": "99"})

	ref, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:     "Survey 2026",
		EmailIDs: []string{"a", "b", "c"},
	})
	if err != nil {
		t.Fatalf("CloneFlowWithEmails: %v", err)
	}
	enrollment := (*posted)["enrollmentCriteria"].(map[string]any)
	if _, rewritten := enrollment["listFilterBranch"]; rewritten {
		t.Error("the source's enrollment was rewritten although no audience was given")
	}
	if enrollment["shouldReEnroll"] != false {
		t.Errorf("the source's own enrollment fields were not preserved: %v", enrollment)
	}
	if ref.EnrollmentListID != "" {
		t.Errorf("EnrollmentListID = %q, want empty to say the source's audience was kept", ref.EnrollmentListID)
	}
}

func TestCloneFlowWithEmails_IsAlwaysCreatedDisabled(t *testing.T) {
	// The source is enabled, and HubSpot is made to answer with isEnabled true, which is
	// the shape that would matter most: a clone that inherited "on" would start enrolling
	// contacts and sending live email from drafts nobody has reviewed.
	c, posted, _ := flowServer(t, map[string]any{"id": "99", "isEnabled": true})

	ref, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:     "Survey 2026",
		EmailIDs: []string{"a", "b", "c"},
	})
	if err != nil {
		t.Fatalf("CloneFlowWithEmails: %v", err)
	}
	if enabled, ok := (*posted)["isEnabled"].(bool); !ok || enabled {
		t.Errorf("posted isEnabled = %v, want false", (*posted)["isEnabled"])
	}
	// Reported, not assumed: the caller is told what HubSpot said, so a portal that
	// ignored the flag is visible rather than papered over.
	if !ref.IsEnabled {
		t.Error("IsEnabled should report the server's answer, not the request's")
	}
}

func TestCloneFlowWithEmails_RefusesACountMismatchBeforeCreating(t *testing.T) {
	cases := []struct {
		name string
		opts CloneFlowOptions
		want string
	}{
		{
			name: "fewer emails than send steps",
			opts: CloneFlowOptions{Name: "n", EmailIDs: []string{"a", "b"}},
			want: "3 send-email step(s) but 2 email(s)",
		},
		{
			name: "fewer dates than dated steps",
			opts: CloneFlowOptions{
				Name:      "n",
				EmailIDs:  []string{"a", "b", "c"},
				SendDates: []time.Time{time.Now()},
			},
			want: "3 wait-until-date step(s) but 1 send date(s)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, posted, methods := flowServer(t, map[string]any{"id": "99"})
			_, err := c.CloneFlowWithEmails(context.Background(), "1841232183", tc.opts)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name the mismatch (%q)", err, tc.want)
			}
			// Refused BEFORE the create: a partially-mapped workflow whose remaining send
			// still points at the source campaign's email is worse than no workflow, and
			// reads as a filled-in step rather than a missing one.
			if *posted != nil {
				t.Error("a workflow was created despite the mismatch")
			}
			for _, m := range *methods {
				if strings.HasPrefix(m, http.MethodPost) {
					t.Errorf("unexpected %q before the counts were checked", m)
				}
			}
		})
	}
}

func TestCloneFlowWithEmails_KeepsFieldsItDoesNotUnderstand(t *testing.T) {
	// A flow definition carries far more than this service models. Anything dropped here
	// is silently deleted from the operator's cloned workflow, so the fields that are
	// carried over are pinned, and the server-owned ones are pinned as NOT carried.
	c, posted, _ := flowServer(t, map[string]any{"id": "99"})

	if _, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:     "Survey 2026",
		EmailIDs: []string{"a", "b", "c"},
	}); err != nil {
		t.Fatalf("CloneFlowWithEmails: %v", err)
	}

	for _, key := range []string{"flowType", "enrollmentCriteria", "suppressionListIds"} {
		if _, ok := (*posted)[key]; !ok {
			t.Errorf("%q was dropped from the clone", key)
		}
	}
	// id and revisionId belong to the SOURCE. Posting them would at best be ignored and
	// at worst be read as a claim about the new flow.
	for _, key := range []string{"id", "revisionId"} {
		if _, ok := (*posted)[key]; ok {
			t.Errorf("%q was copied onto the new flow", key)
		}
	}
	// The step this service has no opinion about survives untouched.
	var found bool
	for _, raw := range (*posted)["actions"].([]any) {
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
	c, _, _ := flowServer(t, map[string]any{"name": "no id here"})

	_, err := c.CloneFlowWithEmails(context.Background(), "1841232183", CloneFlowOptions{
		Name:     "Survey 2026",
		EmailIDs: []string{"a", "b", "c"},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsUnconfirmed(err) {
		t.Errorf("error = %v, want it classified unconfirmed", err)
	}
}
