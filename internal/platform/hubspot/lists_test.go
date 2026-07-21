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
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/crm/v3/lists/search" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body = decodeBody(t, r)
		// Size comes back as hs_list_size (a STRING) under additionalProperties;
		// objectTypeId is a per-hit RESPONSE property.
		_, _ = io.WriteString(w, `{"lists":[{"listId":"26991","name":"CNCF Master","objectTypeId":"0-1","additionalProperties":{"hs_list_size":"1200"}}]}`)
	})
	got, err := c.SearchLists(context.Background(), "CNCF")
	if err != nil {
		t.Fatalf("SearchLists: %v", err)
	}
	if len(got) != 1 || got[0].ListID != "26991" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Size != 1200 {
		t.Errorf("Size must be parsed from hs_list_size string, got %d", got[0].Size)
	}
	if !strings.Contains(got[0].AppURL, "/lists/26991") {
		t.Errorf("AppURL = %q", got[0].AppURL)
	}
	// The search body must request hs_list_size and NOT send objectTypeId or
	// includeFilters (neither is a valid ListSearchRequest field).
	if _, bad := body["objectTypeId"]; bad {
		t.Error("search must NOT send objectTypeId (not a ListSearchRequest field; filtered client-side)")
	}
	if _, bad := body["includeFilters"]; bad {
		t.Error("search must NOT send includeFilters (invalid on the search route)")
	}
	ap, _ := body["additionalProperties"].([]any)
	if len(ap) != 1 || ap[0] != "hs_list_size" {
		t.Errorf("search must request hs_list_size, got %v", body["additionalProperties"])
	}
}

func TestSearchLists_FiltersToContactListsClientSide(t *testing.T) {
	// The v3 search body can't constrain the object type, so the server may return
	// company/deal/custom lists that match the name. SearchLists must keep only
	// contact lists (objectTypeId 0-1).
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"lists":[`+
			`{"listId":"1","name":"Ops","objectTypeId":"0-1"},`+
			`{"listId":"2","name":"Ops","objectTypeId":"0-2"},`+
			`{"listId":"3","name":"Ops","objectTypeId":"2-123"}`+
			`]}`)
	})
	got, err := c.SearchLists(context.Background(), "Ops")
	if err != nil {
		t.Fatalf("SearchLists: %v", err)
	}
	if len(got) != 1 || got[0].ListID != "1" {
		t.Errorf("must keep only the contact (0-1) list, got %+v", got)
	}
}

func TestSearchLists_FollowsOffsetPagination(t *testing.T) {
	// Page 1 returns hasMore + an advanced offset; page 2 returns hasMore=false.
	var offsets []int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		off := int(body["offset"].(float64))
		offsets = append(offsets, off)
		if off == 0 {
			_, _ = io.WriteString(w, `{"lists":[{"listId":"1","name":"A","objectTypeId":"0-1"}],"hasMore":true,"offset":100}`)
			return
		}
		_, _ = io.WriteString(w, `{"lists":[{"listId":"2","name":"B","objectTypeId":"0-1"}],"hasMore":false,"offset":100}`)
	})
	got, err := c.SearchLists(context.Background(), "q")
	if err != nil {
		t.Fatalf("SearchLists: %v", err)
	}
	if len(got) != 2 || got[0].ListID != "1" || got[1].ListID != "2" {
		t.Fatalf("both pages must aggregate, got %+v", got)
	}
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != 100 {
		t.Errorf("offset not forwarded across pages: %v", offsets)
	}
}

func TestSearchLists_RepeatedPageErrors(t *testing.T) {
	// A server that returns the SAME rows every request (with hasMore=true) must not
	// loop and hand back duplicates — the walker errors once a page adds no new ids.
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"lists":[{"listId":"1","name":"A","objectTypeId":"0-1"}],"hasMore":true,"offset":100}`)
	})
	if _, err := c.SearchLists(context.Background(), "q"); err == nil {
		t.Fatal("a repeated page with hasMore=true must error, not return duplicates")
	}
}

func TestSearchLists_EmptyPageWithHasMoreErrors(t *testing.T) {
	// hasMore=true with an empty page means the server can't advance us — returning a
	// silent partial would under-target the audience, so this must be a hard error.
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		off := int(decodeBody(t, r)["offset"].(float64))
		if off == 0 {
			_, _ = io.WriteString(w, `{"lists":[{"listId":"1","name":"A","objectTypeId":"0-1"}],"hasMore":true,"offset":100}`)
			return
		}
		_, _ = io.WriteString(w, `{"lists":[],"hasMore":true,"offset":200}`)
	})
	if _, err := c.SearchLists(context.Background(), "q"); err == nil {
		t.Fatal("an empty page with hasMore=true must error, not return a silent partial")
	}
}

func TestUpdateListFilters_TrimsListID(t *testing.T) {
	// A whitespace-padded list id must be trimmed before it reaches the URL path.
	var gotPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{}`)
	})
	if err := c.UpdateListFilters(context.Background(), "  42  ", json.RawMessage(`{"filterBranchType":"OR"}`)); err != nil {
		t.Fatalf("UpdateListFilters: %v", err)
	}
	if gotPath != "/crm/v3/lists/42/update-list-filters" {
		t.Errorf("padded list id must be trimmed in the path, got %q", gotPath)
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
		// No `list` wrapper — fields at the top level, including the integer `size`
		// (GET/CREATE use a top-level size, unlike search hits' hs_list_size string).
		_, _ = io.WriteString(w, `{"listId":"555","name":"Top","size":42}`)
	})
	l, err := c.GetList(context.Background(), "555")
	if err != nil {
		t.Fatalf("GetList: %v", err)
	}
	if l.ListID != "555" {
		t.Errorf("listId = %q", l.ListID)
	}
	if l.Size != 42 {
		t.Errorf("GetList must decode the top-level integer size, got %d", l.Size)
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

func TestCreateList_TrimsName(t *testing.T) {
	// A padded name must be trimmed before it is posted — leading/trailing spaces
	// would otherwise become part of the HubSpot list name.
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"list":{"listId":"77","name":"New"}}`)
	})
	if _, err := c.CreateList(context.Background(), "  New  ", json.RawMessage(`{"filterBranchType":"OR"}`)); err != nil {
		t.Fatalf("CreateList: %v", err)
	}
	if body["name"] != "New" {
		t.Errorf("create body name must be trimmed, got %v", body["name"])
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
	if gotPath != "/crm/v3/lists/26991/update-list-filters" {
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
		// The real HubSpot envelope nests the label under labels.singular/plural,
		// NOT a top-level `label` string, and must NOT set includeProperties.
		if r.URL.Query().Get("includeProperties") != "" {
			t.Error("ListEventDefinitions must not request includeProperties")
		}
		_, _ = io.WriteString(w, `{"results":[
			{"fullyQualifiedName":"pe8112310_event_registration","name":"event_registration","labels":{"singular":"Registration","plural":"Registrations"}}
		]}`)
	})
	defs, err := c.ListEventDefinitions(context.Background())
	if err != nil {
		t.Fatalf("ListEventDefinitions: %v", err)
	}
	if len(defs) != 1 || defs[0].FullyQualifiedName != "pe8112310_event_registration" {
		t.Fatalf("defs = %+v", defs)
	}
	if defs[0].Label != "Registration" {
		t.Errorf("Label must be decoded from labels.singular, got %q", defs[0].Label)
	}
}

func TestListEventDefinitions_FollowsCursorPagination(t *testing.T) {
	// Page 1 returns paging.next.after; page 2 omits it. A portal with >1 page of
	// definitions must not silently lose the later ones.
	var afters []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after")
		afters = append(afters, after)
		if after == "" {
			_, _ = io.WriteString(w, `{"results":[{"fullyQualifiedName":"pe_a","name":"a"}],"paging":{"next":{"after":"C2"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"fullyQualifiedName":"pe_b","name":"b"}]}`)
	})
	defs, err := c.ListEventDefinitions(context.Background())
	if err != nil {
		t.Fatalf("ListEventDefinitions: %v", err)
	}
	if len(defs) != 2 || defs[0].FullyQualifiedName != "pe_a" || defs[1].FullyQualifiedName != "pe_b" {
		t.Fatalf("both pages must aggregate, got %+v", defs)
	}
	if len(afters) != 2 || afters[0] != "" || afters[1] != "C2" {
		t.Errorf("cursor not forwarded: %v", afters)
	}
}
