// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
)

// suppressionPortal is a fake HubSpot portal: lists found by exact name, read back with
// their filters, created and refreshed in place, with every write recorded.
type suppressionPortal struct {
	mu      sync.Mutex
	byName  map[string]string
	filters map[string]json.RawMessage
	created []map[string]any
	updated []string
}

// standardPortal holds every standard term's list (7001 onward, named by its search term)
// plus a brand opt-out list "6001".
func standardPortal() (*suppressionPortal, []string) {
	p := &suppressionPortal{byName: map[string]string{"Brand Opt-Outs": "6001"}, filters: map[string]json.RawMessage{}}
	ids := make([]string, 0, len(audience.StandardSuppressionTerms))
	for i, term := range audience.StandardSuppressionTerms {
		id := fmt.Sprintf("%d", 7001+i)
		p.byName[term.SearchTerm] = id
		ids = append(ids, id)
	}
	return p, ids
}

func (p *suppressionPortal) nameOf(id string) string {
	for name, v := range p.byName {
		if v == id {
			return name
		}
	}
	return "list " + id
}

func (p *suppressionPortal) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == hubSpotTokenInfoPath:
			_, _ = io.WriteString(w, `{"hubId":8112310}`)
		case r.URL.Path == "/crm/v3/lists/search":
			var body struct {
				Query string `json:"query"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			lists := []map[string]any{}
			if id, ok := p.byName[body.Query]; ok {
				lists = append(lists, map[string]any{"listId": id, "name": body.Query, "objectTypeId": "0-1"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lists": lists, "hasMore": false})
		case r.URL.Path == "/crm/v3/lists" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			p.created = append(p.created, body)
			id := fmt.Sprintf("%d", 9000+len(p.created))
			name, _ := body["name"].(string)
			p.byName[name] = id
			p.filters[id], _ = json.Marshal(body["filterBranch"])
			_ = json.NewEncoder(w).Encode(map[string]any{"list": map[string]any{"listId": id, "name": name, "objectTypeId": "0-1"}})
		case strings.HasSuffix(r.URL.Path, "/update-list-filters") && r.Method == http.MethodPut:
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/crm/v3/lists/"), "/update-list-filters")
			var body struct {
				FilterBranch json.RawMessage `json:"filterBranch"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			p.updated = append(p.updated, id)
			p.filters[id] = body.FilterBranch
			_, _ = io.WriteString(w, `{}`)
		case strings.HasPrefix(r.URL.Path, "/crm/v3/lists/") && r.Method == http.MethodGet:
			id := strings.TrimPrefix(r.URL.Path, "/crm/v3/lists/")
			list := map[string]any{"listId": id, "name": p.nameOf(id), "objectTypeId": "0-1", "size": 10}
			if f, ok := p.filters[id]; ok {
				list["filterBranch"] = f
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"list": list})
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (p *suppressionPortal) writes() (created []map[string]any, updated []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.created...), append([]string(nil), p.updated...)
}

func TestAttachExisting_AllStandardSelected_ReusesOneStandingList(t *testing.T) {
	portal, std := standardPortal()
	x := composeExplorer(portal.serve(t).URL)

	first, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, append(append([]string{}, std...), "6001"), "")
	require.NoError(t, err)
	require.NotNil(t, first.Suppression)
	assert.Equal(t, audience.StandardSuppressionName("proj-1"), first.Suppression.Name)
	assert.Equal(t, []string{"6001"}, first.AttachedSuppressionIDs, "a non-standard list stays its own entry")

	created, _ := portal.writes()
	require.Len(t, created, 1)
	filter, _ := json.Marshal(created[0]["filterBranch"])
	for _, id := range std {
		assert.Contains(t, string(filter), `"listId":"`+id+`"`, "the standing list is the union of every standard list")
	}
	assert.NotContains(t, string(filter), `"6001"`)

	// A different audience with the same standard selection reuses the same list.
	second, err := x.AttachExisting(context.Background(), "proj-1", []string{"31028"}, std, "")
	require.NoError(t, err)
	require.NotNil(t, second.Suppression)
	assert.Equal(t, first.Suppression.ListID, second.Suppression.ListID)
	assert.Empty(t, second.AttachedSuppressionIDs)
	created, updated := portal.writes()
	assert.Len(t, created, 1, "the standing list is never created twice")
	assert.Empty(t, updated, "an unchanged membership is not rewritten")
}

func TestAttachExisting_StandardSetChanged_RefreshesTheStandingListInPlace(t *testing.T) {
	portal, std := standardPortal()
	stale, err := audience.MasterListFilter(std[:len(std)-1])
	require.NoError(t, err)
	portal.byName[audience.StandardSuppressionName("proj-1")] = "77"
	portal.filters["77"] = stale
	x := composeExplorer(portal.serve(t).URL)

	outcome, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.NoError(t, err)
	require.NotNil(t, outcome.Suppression)
	assert.Equal(t, "77", outcome.Suppression.ListID)
	created, updated := portal.writes()
	assert.Empty(t, created)
	assert.Equal(t, []string{"77"}, updated, "a newly resolved standard list is added to the standing list")
	assert.ElementsMatch(t, std, audience.CoveredListIDs(portal.filters["77"]))
}

func TestAttachExisting_PartialStandardSelection_AttachesEachListAndCreatesNothing(t *testing.T) {
	portal, std := standardPortal()
	x := composeExplorer(portal.serve(t).URL)

	outcome, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std[1:], "")
	require.NoError(t, err)
	assert.Nil(t, outcome.Suppression, "the standing union would exclude the list the operator unticked")
	assert.Equal(t, std[1:], outcome.AttachedSuppressionIDs)
	created, updated := portal.writes()
	assert.Empty(t, created)
	assert.Empty(t, updated)
}

func TestAttachExisting_CurrentRegistrants_FoundOrCreatedAsItsOwnSuppression(t *testing.T) {
	portal, _ := standardPortal()
	x := composeExplorer(portal.serve(t).URL)
	const edition = "KubeCon + CloudNativeCon North America 2026"

	outcome, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, []string{"6001"}, edition)
	require.NoError(t, err)
	created, _ := portal.writes()
	require.Len(t, created, 1)
	assert.Equal(t, audience.CurrentRegistrantsName(edition), created[0]["name"])
	filter, _ := json.Marshal(created[0]["filterBranch"])
	assert.Contains(t, string(filter), audience.RegistrationEventTypeID)
	assert.Contains(t, string(filter), edition)
	assert.Equal(t, []string{"6001", "9001"}, outcome.AttachedSuppressionIDs)

	again, err := x.AttachExisting(context.Background(), "proj-1", []string{"31028"}, nil, edition)
	require.NoError(t, err)
	assert.Equal(t, []string{"9001"}, again.AttachedSuppressionIDs)
	created, _ = portal.writes()
	assert.Len(t, created, 1, "the edition's registrants list is reused, not recreated")
}

func TestSuppressionLists_RegistrantsRowAndStandingList(t *testing.T) {
	portal, std := standardPortal()
	x := composeExplorer(portal.serve(t).URL)
	const edition = "Open Source Summit Europe 2026"

	rows, standing, err := x.SuppressionLists(context.Background(), "proj-1", "", "", edition)
	require.NoError(t, err)
	assert.Nil(t, standing, "no standing list exists yet, and a read creates none")
	last := rows[len(rows)-1]
	assert.Equal(t, audience.SuppressionCategoryCurrentRegistrants, last.Category)
	assert.True(t, last.WillCreate)
	assert.Empty(t, last.ListID)
	assert.Len(t, rows, len(std)+1)

	portal.byName[audience.CurrentRegistrantsName(edition)] = "88"
	portal.byName[audience.StandardSuppressionName("proj-1")] = "77"
	rows, standing, err = x.SuppressionLists(context.Background(), "proj-1", "", "", edition)
	require.NoError(t, err)
	require.NotNil(t, standing)
	assert.Equal(t, "77", standing.ListID)
	last = rows[len(rows)-1]
	assert.Equal(t, "88", last.ListID)
	assert.False(t, last.WillCreate)
	created, _ := portal.writes()
	assert.Empty(t, created)

	rows, _, err = x.SuppressionLists(context.Background(), "proj-1", "", "", "Open Source Summit Europe")
	require.NoError(t, err)
	assert.Len(t, rows, len(std), "an edition without its year gets no registrants row")
}

func TestAttachExisting_RegistrantsWithoutYear_IsRefusedBeforeAnyCreate(t *testing.T) {
	portal, _ := standardPortal()
	x := composeExplorer(portal.serve(t).URL)

	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, []string{"6001"}, "AGNTCon + MCPCon Japan")
	require.ErrorIs(t, err, audience.ErrInvalidRequest)
	created, _ := portal.writes()
	assert.Empty(t, created)
}

func TestComposeMaster_ExcludesTheStandingListsDirectly(t *testing.T) {
	portal, std := standardPortal()
	x := composeExplorer(portal.serve(t).URL)

	outcome, err := x.ComposeMaster(context.Background(), "proj-1", audience.ComposeInput{
		ListIDs:        []string{"111"},
		ExcludeListIDs: append(append([]string{}, std...), "6001"),
		Name:           "AGNTCon Japan - Master",
	})
	require.NoError(t, err)
	created, _ := portal.writes()
	require.Len(t, created, 2, "the standing standard list, then the master -- no per-compose suppression list")
	assert.Equal(t, audience.StandardSuppressionName("proj-1"), created[0]["name"])
	assert.Equal(t, "AGNTCon Japan - Master", created[1]["name"])

	master, _ := json.Marshal(created[1]["filterBranch"])
	assert.Contains(t, string(master), `"listId":"9001"`)
	assert.Contains(t, string(master), `"listId":"6001"`)
	assert.NotContains(t, string(master), `"listId":"7001"`, "standard lists are excluded through the standing list")
	assert.Equal(t, strings.Count(string(master), "NOT_IN_LIST")/2, 2, "one NOT_IN_LIST filter per excluded list")
	require.NotNil(t, outcome.Suppression)
	assert.Equal(t, "9001", outcome.Suppression.ListID)
	assert.Equal(t, []string{"6001"}, outcome.AttachedSuppressionIDs)
}
