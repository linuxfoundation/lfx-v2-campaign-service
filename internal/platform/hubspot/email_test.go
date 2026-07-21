// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// decodeBody reads a request JSON body into a map.
func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return m
}

func strptr(s string) *string { return &s }

func TestSearchEmails_FiltersAndBuildsAppURL(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/marketing/v3/emails") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"results":[
			{"id":"1","name":"KubeCon Invite","subject":"Join us"},
			{"id":"2","name":"Newsletter","subject":"Monthly"}
		]}`)
	})
	got, err := c.SearchEmails(context.Background(), "kubecon")
	if err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("filter failed, got %+v", got)
	}
	if got[0].AppURL == "" || !strings.Contains(got[0].AppURL, "/edit/1/") {
		t.Errorf("AppURL not built: %q", got[0].AppURL)
	}
}

func TestGetEmail_2xxNoIDIsUnconfirmed(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"name":"no id here"}`)
	})
	_, err := c.GetEmail(context.Background(), "42")
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a 2xx with no id must be UNCONFIRMED, got: %v", err)
	}
}

func TestCloneEmail_SendsIDAndCloneName(t *testing.T) {
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/marketing/v3/emails/clone" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"id":"999","name":"KubeCon Invite (clone)","state":"DRAFT"}`)
	})
	e, err := c.CloneEmail(context.Background(), "123", "KubeCon Invite")
	if err != nil {
		t.Fatalf("CloneEmail: %v", err)
	}
	if e.ID != "999" {
		t.Errorf("clone id = %q, want 999", e.ID)
	}
	if body["id"] != "123" || body["cloneName"] != "KubeCon Invite" || body["language"] != "en" {
		t.Errorf("clone body = %v", body)
	}
}

func TestCloneEmail_2xxNoIDIsUnconfirmed(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"name":"draft with no id"}`)
	})
	_, err := c.CloneEmail(context.Background(), "123", "X")
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a clone 2xx with no id must be UNCONFIRMED (a draft may exist), got: %v", err)
	}
}

func TestCloneEmail_Mutating429IsNotRetried(t *testing.T) {
	var calls int
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.CloneEmail(context.Background(), "123", "X")
	if err == nil {
		t.Fatal("expected an error on clone 429")
	}
	if calls != 1 {
		t.Errorf("a mutating clone 429 must NOT be retried, got %d calls", calls)
	}
}

func TestPatchEmailSettings_OnlySetsProvidedFields(t *testing.T) {
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("want PATCH, got %s", r.Method)
		}
		body = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"id":"999"}`)
	})
	_, err := c.PatchEmailSettings(context.Background(), "999", EmailSettings{
		Subject: strptr("New subject"),
	})
	if err != nil {
		t.Fatalf("PatchEmailSettings: %v", err)
	}
	if body["subject"] != "New subject" {
		t.Errorf("patch body = %v", body)
	}
	if _, ok := body["from"]; ok {
		t.Errorf("from must be omitted when no from-name/email set: %v", body)
	}
}

func TestPatchEmailSettings_FromUsesV3FieldNames(t *testing.T) {
	// The v3 `from` object uses fromName + replyTo, NOT name/email (which HubSpot
	// silently ignores). Verified against HubSpot's PublicEmailFromDetails schema.
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"id":"999"}`)
	})
	if _, err := c.PatchEmailSettings(context.Background(), "999", EmailSettings{
		FromName:  strptr("CNCF Events"),
		FromEmail: strptr("events@cncf.io"),
	}); err != nil {
		t.Fatalf("PatchEmailSettings: %v", err)
	}
	from, ok := body["from"].(map[string]any)
	if !ok {
		t.Fatalf("from object missing: %v", body)
	}
	if from["fromName"] != "CNCF Events" || from["replyTo"] != "events@cncf.io" {
		t.Errorf("from must use fromName/replyTo, got %v", from)
	}
	if _, bad := from["name"]; bad {
		t.Errorf("from must NOT use the ignored `name` field: %v", from)
	}
	if _, bad := from["email"]; bad {
		t.Errorf("from must NOT use the ignored `email` field: %v", from)
	}
}

func TestPatchEmailSettings_EmptyIsRejected(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request expected when nothing to set")
		_, _ = io.WriteString(w, `{}`)
	})
	if _, err := c.PatchEmailSettings(context.Background(), "1", EmailSettings{}); err == nil {
		t.Error("PatchEmailSettings with nothing to set should error before any request")
	}
}

// The load-bearing routing gotcha: an ILS list MUST go in contactIlsLists (never
// contactLists), and the opposite namespace must NOT appear in the same PATCH.
func TestSetSendList_ILSRoutesToIlsListsOnly(t *testing.T) {
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"id":"999"}`)
	})
	_, err := c.SetSendList(context.Background(), "999", "26991", []string{"111", " 222 ", ""}, true)
	if err != nil {
		t.Fatalf("SetSendList: %v", err)
	}
	to, _ := body["to"].(map[string]any)
	if to == nil {
		t.Fatalf("no `to` in body: %v", body)
	}
	if _, hasLegacy := to["contactLists"]; hasLegacy {
		t.Error("ILS send list must NOT put contactLists in the same PATCH (HubSpot rejects the whole `to`)")
	}
	ils, _ := to["contactIlsLists"].(map[string]any)
	if ils == nil {
		t.Fatalf("contactIlsLists missing: %v", to)
	}
	inc, _ := ils["include"].([]any)
	if len(inc) != 1 || inc[0] != "26991" {
		t.Errorf("ils include = %v, want [26991]", inc)
	}
	exc, _ := ils["exclude"].([]any)
	if len(exc) != 2 { // "111","222" — empty dropped
		t.Errorf("suppressions = %v, want 2 (empty trimmed)", exc)
	}
	// contactIds must be cleared so no stale clone-source recipients remain.
	if _, ok := to["contactIds"]; !ok {
		t.Error("contactIds must be cleared in the complete `to` object")
	}
}

func TestSetSendList_LegacyRoutesToContactListsNumeric(t *testing.T) {
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"id":"999"}`)
	})
	if _, err := c.SetSendList(context.Background(), "999", "12345", nil, false); err != nil {
		t.Fatalf("SetSendList: %v", err)
	}
	to, _ := body["to"].(map[string]any)
	if _, hasILS := to["contactIlsLists"]; hasILS {
		t.Error("legacy send list must NOT put contactIlsLists in the same PATCH")
	}
	cl, _ := to["contactLists"].(map[string]any)
	inc, _ := cl["include"].([]any)
	if len(inc) != 1 || inc[0].(float64) != 12345 {
		t.Errorf("legacy include = %v, want numeric [12345]", inc)
	}
}

func TestSetSendList_RejectsEmptyIDs(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request expected on invalid input")
	})
	if _, err := c.SetSendList(context.Background(), "", "1", nil, true); err == nil {
		t.Error("empty email id should be rejected")
	}
	if _, err := c.SetSendList(context.Background(), "1", "", nil, true); err == nil {
		t.Error("empty send-list id should be rejected")
	}
}

func TestSetSendList_RejectsNonNumericLegacyID(t *testing.T) {
	// A non-numeric legacy (non-ILS) send-list id must be rejected BEFORE the PATCH:
	// silently dropping it would send an empty include and HubSpot would clear all
	// recipients while returning success (a silent no-recipient send).
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no PATCH expected when the legacy send-list id is non-numeric")
	})
	if _, err := c.SetSendList(context.Background(), "999", "not-a-number", nil, false); err == nil {
		t.Error("a non-numeric legacy send-list id must be rejected, not turned into an empty-recipient PATCH")
	}
	// A whitespace-padded numeric id must be accepted (Atoi doesn't trim; we do).
	var patched bool
	c2, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		patched = true
		_, _ = io.WriteString(w, `{"id":"999"}`)
	})
	if _, err := c2.SetSendList(context.Background(), "999", " 12345 ", nil, false); err != nil {
		t.Errorf("a padded numeric legacy id should be accepted: %v", err)
	}
	if !patched {
		t.Error("expected a PATCH for a valid padded numeric id")
	}
}
