// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A membership row with no recordId must FAIL the read, not be skipped.
//
// Skipping it left `out` short while `truncated` stayed false, so PreviewCount's caller
// took the "sweep completed" path and labelled an incomplete walk EXACT — an undercount
// presented as a counted total. Understating how many people an email reaches is the one
// direction with no recovery after the send, so a row the API could not identify is a
// malformed response rather than an empty membership.
func TestListMembershipIDsRejectsARowWithNoRecordID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The middle row OMITS recordId, which decodes to an empty json.Number. (A literal
		// `""` is not valid for json.Number and is rejected by the decoder before this
		// code runs, so the omitted field is the reachable shape.) Previously this
		// returned 2 ids with truncated=false, reading as a complete membership of two.
		_, _ = fmt.Fprint(w, `{"results":[{"recordId":1},{"other":"x"},{"recordId":3}]}`)
	}))
	defer server.Close()

	c := NewClient(
		Credentials{PrivateAppToken: "t"}, AccountConfig{PortalID: "8112310"},
		WithBaseURL(server.URL),
	)

	ids, truncated, err := c.ListMembershipIDs(context.Background(), "123")
	if err == nil {
		t.Fatalf("a row with no recordId was accepted: got %d ids, truncated=%v — an incomplete walk would be labelled exact", len(ids), truncated)
	}
	if !strings.Contains(err.Error(), "recordId") {
		t.Errorf("the error must name the malformed field so the cause is diagnosable; got %v", err)
	}
}

// The ordinary path must still work, or the guard above would break every preview.
func TestListMembershipIDsReadsAWellFormedPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"results":[{"recordId":1},{"recordId":3}]}`)
	}))
	defer server.Close()

	c := NewClient(
		Credentials{PrivateAppToken: "t"}, AccountConfig{PortalID: "8112310"},
		WithBaseURL(server.URL),
	)

	ids, truncated, err := c.ListMembershipIDs(context.Background(), "123")
	if err != nil {
		t.Fatalf("a well-formed page must read cleanly: %v", err)
	}
	if truncated {
		t.Error("a single complete page is not truncated")
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 ids, got %d (%v)", len(ids), ids)
	}
}

// A 2xx with no `to` object must FAIL, not return an email that targeted nobody.
//
// `to` decoded as a value struct, so a truncated response like `{"id":"123"}` produced a
// successful EmailSendLists with empty include and exclude — the exact outcome this
// function's doc comment guards `includedProperties` against, arriving by another route.
// The caller uses those ids to decide what a past send reached; an empty answer that is
// really "the response was malformed" misreports the send's audience as nobody.
func TestGetEmailSendListsRejectsAResponseWithNoToObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"123","publishDate":"2026-01-01T00:00:00Z"}`)
	}))
	defer server.Close()

	c := NewClient(Credentials{PrivateAppToken: "t"}, AccountConfig{PortalID: "8112310"}, WithBaseURL(server.URL))

	got, err := c.GetEmailSendLists(context.Background(), "123")
	if err == nil {
		t.Fatalf("a response with no `to` object was accepted: %+v — the send's audience is reported as nobody", got)
	}
	if !strings.Contains(err.Error(), "`to`") {
		t.Errorf("the error must name the missing field so the cause is diagnosable; got %v", err)
	}
}

// An empty-but-PRESENT `to` is a real answer and must still be accepted.
func TestGetEmailSendListsAcceptsAnEmptyToObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"123","to":{}}`)
	}))
	defer server.Close()

	c := NewClient(Credentials{PrivateAppToken: "t"}, AccountConfig{PortalID: "8112310"}, WithBaseURL(server.URL))

	got, err := c.GetEmailSendLists(context.Background(), "123")
	if err != nil {
		t.Fatalf("an email that genuinely selected nothing must still read: %v", err)
	}
	if len(got.Include) != 0 || len(got.Exclude) != 0 {
		t.Errorf("want an empty selection, got include=%v exclude=%v", got.Include, got.Exclude)
	}
}
