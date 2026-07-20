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

func TestSearchLists_ReturnsAndBuildsURL(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/crm/v3/lists/search" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"lists":[{"listId":"26991","name":"CNCF Master","size":1200}]}`)
	})
	got, err := c.SearchLists(context.Background(), "CNCF")
	if err != nil {
		t.Fatalf("SearchLists: %v", err)
	}
	if len(got) != 1 || got[0].ListID != "26991" {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got[0].AppURL, "/lists/26991") {
		t.Errorf("AppURL = %q", got[0].AppURL)
	}
}

func TestGetList_UnwrapsWrapperAndReturnsFilterBranch(t *testing.T) {
	var gotQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("includeFilters")
		_, _ = io.WriteString(w, `{"list":{"listId":"26991","name":"M","processingType":"DYNAMIC","filterBranch":{"filterBranchType":"OR"}}}`)
	})
	l, err := c.GetList(context.Background(), "26991")
	if err != nil {
		t.Fatalf("GetList: %v", err)
	}
	if gotQuery != "true" {
		t.Errorf("includeFilters query = %q, want true", gotQuery)
	}
	if l.ProcessingType != "DYNAMIC" {
		t.Errorf("processingType = %q", l.ProcessingType)
	}
	if !strings.Contains(string(l.FilterBranch), "filterBranchType") {
		t.Errorf("filterBranch not returned: %s", l.FilterBranch)
	}
}

func TestGetList_TopLevelShapeAlsoDecodes(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// No `list` wrapper — fields at the top level.
		_, _ = io.WriteString(w, `{"listId":"555","name":"Top"}`)
	})
	l, err := c.GetList(context.Background(), "555")
	if err != nil {
		t.Fatalf("GetList: %v", err)
	}
	if l.ListID != "555" {
		t.Errorf("listId = %q", l.ListID)
	}
}

func TestCreateList_SendsDynamicContactBodyAndFilterBranch(t *testing.T) {
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/crm/v3/lists/" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"list":{"listId":"77","name":"New"}}`)
	})
	fb := json.RawMessage(`{"filterBranchType":"OR","filterBranches":[]}`)
	l, err := c.CreateList(context.Background(), "New", fb)
	if err != nil {
		t.Fatalf("CreateList: %v", err)
	}
	if l.ListID != "77" {
		t.Errorf("listId = %q, want 77", l.ListID)
	}
	if body["objectTypeId"] != "0-1" || body["processingType"] != "DYNAMIC" || body["name"] != "New" {
		t.Errorf("create body = %v", body)
	}
	fbSent, _ := body["filterBranch"].(map[string]any)
	if fbSent["filterBranchType"] != "OR" {
		t.Errorf("filterBranch not passed through: %v", body["filterBranch"])
	}
}

func TestCreateList_2xxNoListIDIsUnconfirmed(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"list":{"name":"no id"}}`)
	})
	_, err := c.CreateList(context.Background(), "X", json.RawMessage(`{"a":1}`))
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a create 2xx with no listId must be UNCONFIRMED, got: %v", err)
	}
}

func TestCreateList_Mutating429NotRetried(t *testing.T) {
	var calls int
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.CreateList(context.Background(), "X", json.RawMessage(`{"a":1}`))
	if err == nil {
		t.Fatal("expected an error on create 429")
	}
	if calls != 1 {
		t.Errorf("a mutating create 429 must NOT be retried, got %d calls", calls)
	}
}

func TestCreateList_RejectsEmptyInput(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request expected on invalid input")
	})
	if _, err := c.CreateList(context.Background(), "", json.RawMessage(`{"a":1}`)); err == nil {
		t.Error("empty name should be rejected")
	}
	if _, err := c.CreateList(context.Background(), "N", nil); err == nil {
		t.Error("empty filterBranch should be rejected")
	}
}

func TestUpdateListFilters_PutsFilterBranch(t *testing.T) {
	var body map[string]any
	var gotPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("want PUT, got %s", r.Method)
		}
		gotPath = r.URL.Path
		body = decodeBody(t, r)
		_, _ = io.WriteString(w, `{}`)
	})
	err := c.UpdateListFilters(context.Background(), "26991", json.RawMessage(`{"filterBranchType":"OR"}`))
	if err != nil {
		t.Fatalf("UpdateListFilters: %v", err)
	}
	if gotPath != "/crm/v3/lists/26991/filter-branch" {
		t.Errorf("path = %q", gotPath)
	}
	fb, _ := body["filterBranch"].(map[string]any)
	if fb["filterBranchType"] != "OR" {
		t.Errorf("filterBranch not sent: %v", body)
	}
}

func TestListEventDefinitions_ReturnsFQNs(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events/v3/event-definitions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"results":[
			{"fullyQualifiedName":"pe8112310_event_registration","label":"Reg","name":"event_registration"}
		]}`)
	})
	defs, err := c.ListEventDefinitions(context.Background())
	if err != nil {
		t.Fatalf("ListEventDefinitions: %v", err)
	}
	if len(defs) != 1 || defs[0].FullyQualifiedName != "pe8112310_event_registration" {
		t.Errorf("defs = %+v", defs)
	}
}
