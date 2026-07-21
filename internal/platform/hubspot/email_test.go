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

func TestSearchEmails_MatchesFieldsIndependently(t *testing.T) {
	// A query must match within name OR subject, not across their concatenation:
	// name "Sale" + subject "Invite" must NOT match "e i" (which spans the boundary).
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"id":"1","name":"Sale","subject":"Invite"}]}`)
	})
	got, err := c.SearchEmails(context.Background(), "e i")
	if err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a boundary-spanning query must not match, got %+v", got)
	}
	// A query fully inside the subject still matches.
	got, err = c.SearchEmails(context.Background(), "invit")
	if err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("a query within subject must match, got %+v", got)
	}
}

func TestClone_Post2xxUnconfirmedIsRecognizedByHelper(t *testing.T) {
	// A mutating 2xx with no id / undecodable body is labeled UNCONFIRMED in the text
	// AND must make IsUnconfirmed(err) true, so a caller using the helper alone won't
	// blind-retry into a duplicate clone.
	cNoID, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"name":"clone but no id"}`)
	})
	_, err := cNoID.CloneEmail(context.Background(), "src", "copy")
	if !IsUnconfirmed(err) {
		t.Errorf("a 2xx-no-id clone must be IsUnconfirmed, got %T: %v", err, err)
	}
	cBad, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{not json`)
	})
	_, err = cBad.CloneEmail(context.Background(), "src", "copy")
	if !IsUnconfirmed(err) {
		t.Errorf("an undecodable 2xx clone must be IsUnconfirmed, got %T: %v", err, err)
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
			// "A" is the more recently updated, so it must sort first after aggregation.
			_, _ = io.WriteString(w, `{"results":[{"id":"1","name":"KubeCon A","subject":"x","updatedAt":"2026-01-02T00:00:00Z"}],"paging":{"next":{"after":"CURSOR2"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"id":"2","name":"KubeCon B","subject":"y","updatedAt":"2026-01-01T00:00:00Z"}]}`)
	})
	got, err := c.SearchEmails(context.Background(), "kubecon")
	if err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("both pages must aggregate (most-recent A first), got %+v", got)
	}
	if len(afters) != 2 || afters[0] != "" || afters[1] != "CURSOR2" {
		t.Errorf("cursor not forwarded across pages: %v", afters)
	}
}

func TestSearchEmails_SortsMostRecentlyUpdatedFirst(t *testing.T) {
	// The `sort` query param isn't a documented field on GET /marketing/v3/emails, so
	// order is guaranteed CLIENT-SIDE: results come back most-recently-updated first
	// regardless of server order, and no `sort` param is sent.
	var sentSort bool
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["sort"]; ok {
			sentSort = true
		}
		// Intentionally returned oldest-first to prove the client re-orders.
		_, _ = io.WriteString(w, `{"results":[`+
			`{"id":"1","name":"Old","subject":"x","updatedAt":"2024-01-01T00:00:00Z"},`+
			`{"id":"2","name":"New","subject":"x","updatedAt":"2026-06-01T00:00:00Z"},`+
			`{"id":"3","name":"Mid","subject":"x","updatedAt":"2025-03-01T00:00:00Z"}`+
			`]}`)
	})
	got, err := c.SearchEmails(context.Background(), "")
	if err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if sentSort {
		t.Error("SearchEmails must NOT send an unsupported `sort` query param")
	}
	if len(got) != 3 || got[0].ID != "2" || got[1].ID != "3" || got[2].ID != "1" {
		t.Errorf("results must be most-recently-updated first (2,3,1), got %v", []string{got[0].ID, got[1].ID, got[2].ID})
	}
}

func TestSearchEmails_SortsByParsedInstantNotLexical(t *testing.T) {
	// A lexical sort of RFC3339 strings is WRONG: `2026-01-01T00:30:00+01:00`
	// (= 2025-12-31T23:30:00Z, OLDER) sorts lexically AFTER `2026-01-01T00:00:00Z`.
	// Parsing to instants puts the truly-newer Z email first.
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[`+
			`{"id":"older","name":"A","subject":"x","updatedAt":"2026-01-01T00:30:00+01:00"},`+
			`{"id":"newer","name":"A","subject":"x","updatedAt":"2026-01-01T00:00:00Z"}`+
			`]}`)
	})
	got, err := c.SearchEmails(context.Background(), "")
	if err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if len(got) != 2 || got[0].ID != "newer" || got[1].ID != "older" {
		t.Errorf("must sort by parsed instant (newer first), got %v", []string{got[0].ID, got[1].ID})
	}
}

func TestSearchEmails_DecodesEncodedCursor(t *testing.T) {
	// HubSpot returns paging.next.after already percent-encoded (e.g. "MjA%3D").
	// The next request must send the DECODED token ("MjA="), not a double-encoded
	// "MjA%253D" — url.Values re-encodes it once on the way out.
	var afters []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		afters = append(afters, r.URL.Query().Get("after")) // already url-decoded by net/http
		if r.URL.Query().Get("after") == "" {
			_, _ = io.WriteString(w, `{"results":[{"id":"1","name":"A","subject":"x"}],"paging":{"next":{"after":"MjA%3D"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"id":"2","name":"B","subject":"y"}]}`)
	})
	if _, err := c.SearchEmails(context.Background(), ""); err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if len(afters) != 2 || afters[1] != "MjA=" {
		t.Errorf("page-2 after must be the decoded cursor \"MjA=\", got %q (afters=%v)", afters[len(afters)-1], afters)
	}
}

func TestSearchEmails_PreservesPlusInCursor(t *testing.T) {
	// Base64 cursors legitimately contain literal '+'. PathUnescape must preserve it
	// (QueryUnescape would corrupt "A+B/C=" into "A B/C="), so page-2 sends the token
	// unchanged.
	var afters []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		afters = append(afters, r.URL.Query().Get("after"))
		if r.URL.Query().Get("after") == "" {
			_, _ = io.WriteString(w, `{"results":[{"id":"1","name":"A","subject":"x"}],"paging":{"next":{"after":"A+B/C="}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"id":"2","name":"B","subject":"y"}]}`)
	})
	if _, err := c.SearchEmails(context.Background(), ""); err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if len(afters) != 2 || afters[1] != "A+B/C=" {
		t.Errorf("page-2 after must preserve the literal '+', got %q", afters[len(afters)-1])
	}
}

func TestSearchEmails_StuckCursorErrors(t *testing.T) {
	// A server that echoes the same `after` token must not loop forever duplicating
	// the page — the walker errors on a non-advancing cursor.
	var calls int
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"results":[{"id":"1","name":"A","subject":"x"}],"paging":{"next":{"after":"SAME"}}}`)
	})
	if _, err := c.SearchEmails(context.Background(), ""); err == nil {
		t.Fatal("a non-advancing cursor must error, not loop to the page cap")
	}
	if calls > 3 {
		t.Errorf("expected the stuck-cursor guard to stop after 2 calls, got %d", calls)
	}
}

func TestGetEmail_2xxNoIDIsPlainErrorNotUnconfirmed(t *testing.T) {
	// GetEmail is an idempotent GET, so a malformed 2xx is a plain error, NOT
	// UNCONFIRMED (a read can't leave a mutation in doubt) — so it's safely retryable.
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"name":"no id here"}`)
	})
	_, err := c.GetEmail(context.Background(), "42")
	if err == nil || !strings.Contains(err.Error(), "malformed response") {
		t.Errorf("a 2xx with no id must be a malformed-response error, got: %v", err)
	}
	if IsUnconfirmed(err) {
		t.Error("a read (GetEmail) must NOT be UNCONFIRMED — it can't leave a mutation in doubt")
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
	if body["id"] != "123" || body["cloneName"] != "KubeCon Invite" {
		t.Errorf("clone body = %v", body)
	}
	// language is omitted so HubSpot preserves the source draft's locale.
	if _, ok := body["language"]; ok {
		t.Errorf("clone body must omit language (preserve source locale), got %v", body["language"])
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

func TestSetSendList_TrimsEmailID(t *testing.T) {
	// A whitespace-padded email id must be trimmed before it reaches the draft URL —
	// a padded id sent raw yields "/emails/%20999%20/draft", a 404 that silently
	// fails the send-list staging.
	var gotPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"id":"999"}`)
	})
	if _, err := c.SetSendList(context.Background(), "  999  ", "ils-123", nil); err != nil {
		t.Fatalf("SetSendList: %v", err)
	}
	if gotPath != "/marketing/v3/emails/999/draft" {
		t.Errorf("padded email id must be trimmed in the draft path, got %q", gotPath)
	}
}

func TestSearchEmails_TrimsQuery(t *testing.T) {
	// A padded query must still match — " kubecon " should find "KubeCon Invite".
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"id":"1","name":"KubeCon Invite","subject":"x"}]}`)
	})
	got, err := c.SearchEmails(context.Background(), "  kubecon  ")
	if err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if len(got) != 1 || got[0].ID != "1" {
		t.Errorf("padded query must match, got %+v", got)
	}
}

func TestCloneEmail_RejectsEmptyName(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request expected on invalid input")
	})
	if _, err := c.CloneEmail(context.Background(), "123", "   "); err == nil {
		t.Error("a whitespace-only clone name must be rejected")
	}
}

func TestCloneEmail_TrimsSourceID(t *testing.T) {
	// A whitespace-padded source id must be trimmed before it is posted in the clone
	// body — a padded id could be rejected by HubSpot, causing a silent clone failure.
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"id":"clone-1"}`)
	})
	if _, err := c.CloneEmail(context.Background(), "  src-42  ", "My Clone"); err != nil {
		t.Fatalf("CloneEmail: %v", err)
	}
	if body["id"] != "src-42" {
		t.Errorf("clone body id must be the trimmed source id, got %v", body["id"])
	}
}
