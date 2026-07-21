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

func TestSearchEmails_FollowsCursorPagination(t *testing.T) {
	// Page 1 returns paging.next.after; page 2 omits it. The walker must forward the
	// cursor, aggregate both pages, and terminate — a match on page 2 must not be lost.
	var afters []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after")
		afters = append(afters, after)
		if after == "" {
			_, _ = io.WriteString(w, `{"results":[{"id":"1","name":"KubeCon A","subject":"x"}],"paging":{"next":{"after":"CURSOR2"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"id":"2","name":"KubeCon B","subject":"y"}]}`)
	})
	got, err := c.SearchEmails(context.Background(), "kubecon")
	if err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("both pages must aggregate, got %+v", got)
	}
	if len(afters) != 2 || afters[0] != "" || afters[1] != "CURSOR2" {
		t.Errorf("cursor not forwarded across pages: %v", afters)
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

// Recipients are set ONLY via contactIlsLists (legacy contactLists was removed by
// HubSpot's ILS migration after 2024-10-31), and the PATCH targets the /draft route.
func TestSetSendList_ILSOnlyOnDraftRoute(t *testing.T) {
	var body map[string]any
	var gotPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"id":"999"}`)
	})
	_, err := c.SetSendList(context.Background(), "999", "26991", []string{"111", " 222 ", ""})
	if err != nil {
		t.Fatalf("SetSendList: %v", err)
	}
	if gotPath != "/marketing/v3/emails/999/draft" {
		t.Errorf("SetSendList must PATCH the draft route, got %q", gotPath)
	}
	to, _ := body["to"].(map[string]any)
	if to == nil {
		t.Fatalf("no `to` in body: %v", body)
	}
	// The removed legacy field must never be emitted.
	if _, hasLegacy := to["contactLists"]; hasLegacy {
		t.Error("SetSendList must NOT emit the removed legacy contactLists field")
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

func TestSetSendList_TrimsILSSendListID(t *testing.T) {
	// A whitespace-padded ILS send-list id must be trimmed — a padded id sent raw
	// could be rejected by HubSpot, leaving the email with no recipients.
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"id":"999"}`)
	})
	if _, err := c.SetSendList(context.Background(), "999", "  ils-123  ", nil); err != nil {
		t.Fatalf("SetSendList: %v", err)
	}
	ils := body["to"].(map[string]any)["contactIlsLists"].(map[string]any)
	inc, _ := ils["include"].([]any)
	if len(inc) != 1 || inc[0] != "ils-123" {
		t.Errorf("ILS include must be the trimmed id, got %v", inc)
	}
}

func TestSetSendList_RejectsEmptyIDs(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request expected on invalid input")
	})
	if _, err := c.SetSendList(context.Background(), "", "1", nil); err == nil {
		t.Error("empty email id should be rejected")
	}
	if _, err := c.SetSendList(context.Background(), "1", "  ", nil); err == nil {
		t.Error("empty/whitespace ILS send-list id should be rejected")
	}
}
