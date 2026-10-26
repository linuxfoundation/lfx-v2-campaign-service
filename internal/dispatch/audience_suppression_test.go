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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// suppressionPortal is a fake HubSpot portal: lists found by exact name, read back with
// their filters, created and refreshed in place, with every write recorded.
type suppressionPortal struct {
	mu      sync.Mutex
	byName  map[string]string
	filters map[string]json.RawMessage
	created []map[string]any
	updated []string
	// failSearch answers a search for this query with a 500, as a rate-limited or flaky
	// HubSpot would.
	failSearch string
	// forbidSearch answers a search for this query with a 403, as a revoked token would.
	forbidSearch string
	// processing overrides a list's processingType on read; every other list is DYNAMIC.
	processing map[string]string
	// lagSearches makes the next N searches for a name miss it, as HubSpot's search index
	// does for a list created moments ago.
	lagSearches map[string]int
	// refuseCreate answers every create with this status and creates nothing; when
	// refuseCreateStores is set the list "already exists" under the requested name (stored
	// with refuseFilter), as when a concurrent caller created it first.
	refuseCreate       int
	refuseCreateStores bool
	refuseFilter       json.RawMessage
	searches           map[string]int
	// objectType overrides a list's objectTypeId on read; every other list is a contact list.
	objectType map[string]string
	// afterUpdate runs, under the lock, right after a filter update is applied: a concurrent
	// writer acting between this service's write and its confirming read.
	afterUpdate func(p *suppressionPortal, id string)
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
			if body.Query == p.forbidSearch {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"status":"error","message":"forbidden"}`)
				return
			}
			if body.Query == p.failSearch {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"status":"error","message":"upstream"}`)
				return
			}
			if p.searches == nil {
				p.searches = map[string]int{}
			}
			p.searches[body.Query]++
			lists := []map[string]any{}
			if p.lagSearches[body.Query] > 0 {
				p.lagSearches[body.Query]--
			} else if id, ok := p.byName[body.Query]; ok {
				lists = append(lists, map[string]any{"listId": id, "name": body.Query, "objectTypeId": "0-1"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lists": lists, "hasMore": false})
		case r.URL.Path == "/crm/v3/lists" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if p.refuseCreate != 0 {
				if name, _ := body["name"].(string); p.refuseCreateStores {
					p.byName[name] = "4242"
					p.filters["4242"] = p.refuseFilter
				}
				w.WriteHeader(p.refuseCreate)
				_, _ = io.WriteString(w, `{"status":"error","category":"VALIDATION_ERROR","message":"list name taken"}`)
				return
			}
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
			if p.afterUpdate != nil {
				p.afterUpdate(p, id)
			}
			_, _ = io.WriteString(w, `{}`)
		case strings.HasPrefix(r.URL.Path, "/crm/v3/lists/") && r.Method == http.MethodGet:
			id := strings.TrimPrefix(r.URL.Path, "/crm/v3/lists/")
			processing := "DYNAMIC"
			if v, ok := p.processing[id]; ok {
				processing = v
			}
			objectType := "0-1"
			if v, ok := p.objectType[id]; ok {
				objectType = v
			}
			list := map[string]any{"listId": id, "name": p.nameOf(id), "objectTypeId": objectType, "size": 10, "processingType": processing}
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

// filterOf is list id's current filter, read under the lock the handler writes it under.
func (p *suppressionPortal) filterOf(id string) json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append(json.RawMessage(nil), p.filters[id]...)
}

// searchCount is how many searches the portal answered for query.
func (p *suppressionPortal) searchCount(query string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.searches[query]
}

// setList names list id, under the lock, for a test that changes the portal between calls.
func (p *suppressionPortal) setList(name, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byName[name] = id
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
	assert.ElementsMatch(t, std, audience.CoveredListIDs(portal.filterOf("77")))
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

	portal.setList(audience.CurrentRegistrantsName(edition), "88")
	portal.setList(audience.StandardSuppressionName("proj-1"), "77")
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

// A failed search on one standard term must never shrink the shared standing list: every audience
// that excludes it would lose that hygiene list. The attach fails instead, and writes nothing.
func TestAttachExisting_StandardSearchFails_NeverShrinksTheStandingList(t *testing.T) {
	portal, std := standardPortal()
	full, err := audience.MasterListFilter(std)
	require.NoError(t, err)
	portal.byName[audience.StandardSuppressionName("proj-1")] = "77"
	portal.filters["77"] = full
	portal.failSearch = audience.StandardSuppressionTerms[0].SearchTerm
	x := composeExplorer(portal.serve(t).URL)

	_, err = x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.Error(t, err)
	created, updated := portal.writes()
	assert.Empty(t, created)
	assert.Empty(t, updated, "a partial resolve must not rewrite the standing list")
	assert.ElementsMatch(t, std, audience.CoveredListIDs(portal.filterOf("77")))

	// The picker is a read, and stays lenient: the failed term is shown as unresolved.
	rows, _, err := x.SuppressionLists(context.Background(), "proj-1", "", "", "")
	require.NoError(t, err)
	assert.Empty(t, rows[0].ListID)
}

// Compose with a registrants edition excludes that list too, and the edition is part of the
// selection fingerprint, so the same lists with and without it are different masters.
func TestComposeMaster_CurrentRegistrants_ExcludedAndFingerprinted(t *testing.T) {
	portal, _ := standardPortal()
	portal.byName["AGNTCon Japan - Master"] = "31349"
	x := composeExplorer(portal.serve(t).URL)
	const edition = "AGNTCon + MCPCon Japan 2026"

	outcome, err := x.ComposeMaster(context.Background(), "proj-1", audience.ComposeInput{
		ListIDs:                 []string{"111"},
		ExcludeListIDs:          []string{"6001"},
		Name:                    "AGNTCon Japan - Master",
		CurrentRegistrantsEvent: edition,
	})
	require.NoError(t, err)
	created, _ := portal.writes()
	require.Len(t, created, 2, "the registrants list, then the master")
	assert.Equal(t, audience.CurrentRegistrantsName(edition), created[0]["name"])
	withRegistrants := audience.SelectionFingerprint([]string{"111"}, []string{"6001", "registrants:" + edition})
	assert.Equal(t, audience.WithFingerprint("AGNTCon Japan - Master", withRegistrants), created[1]["name"])
	assert.NotEqual(t, audience.SelectionFingerprint([]string{"111"}, []string{"6001"}), withRegistrants)
	master, _ := json.Marshal(created[1]["filterBranch"])
	assert.Contains(t, string(master), `"listId":"9001"`, "the registrants list is excluded on every path")
	assert.Equal(t, []string{"6001", "9001"}, outcome.AttachedSuppressionIDs)
}

// A year-less edition is known to be invalid from the request alone, so it is refused before the
// standing list is created or rewritten: a 400 must leave the portal untouched.
func TestAttachExisting_YearlessEdition_RefusedBeforeAnyWrite(t *testing.T) {
	portal, std := standardPortal()
	x := composeExplorer(portal.serve(t).URL)

	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "KubeCon India")
	require.ErrorIs(t, err, audience.ErrInvalidRequest)
	created, updated := portal.writes()
	assert.Empty(t, created, "the standing list must not be created by a request that is then refused")
	assert.Empty(t, updated)
}

// standingWith seeds the project's standing list "77" covering ids.
func standingWith(t *testing.T, p *suppressionPortal, ids []string) {
	t.Helper()
	filter, err := audience.MasterListFilter(ids)
	require.NoError(t, err)
	p.byName[audience.StandardSuppressionName("proj-1")] = "77"
	p.filters["77"] = filter
}

// A quarter rotation: the term now resolves to the new quarter's list while the old one still
// holds people not yet copied over. The new list is added and the old one is kept.
func TestAttachExisting_QuarterRotation_GrowsTheStandingListAndKeepsTheOldList(t *testing.T) {
	portal, std := standardPortal()
	standingWith(t, portal, std)
	portal.byName[audience.StandardSuppressionTerms[0].SearchTerm] = "8001"
	x := composeExplorer(portal.serve(t).URL)
	selection := append([]string{"8001"}, std[1:]...)

	outcome, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, selection, "")
	require.NoError(t, err)
	require.NotNil(t, outcome.Suppression)
	assert.Equal(t, "77", outcome.Suppression.ListID)
	_, updated := portal.writes()
	assert.Equal(t, []string{"77"}, updated)
	assert.ElementsMatch(t, append([]string{"8001"}, std...), audience.CoveredListIDs(portal.filterOf("77")),
		"the old quarter's list stays: dropping it would stop suppressing its members on every send")
}

// A term that stops resolving is not a decision to stop suppressing its list.
func TestAttachExisting_TermNoLongerResolves_KeepsItsListAndWritesNothing(t *testing.T) {
	portal, std := standardPortal()
	standingWith(t, portal, std)
	delete(portal.byName, audience.StandardSuppressionTerms[0].SearchTerm)
	x := composeExplorer(portal.serve(t).URL)

	outcome, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std[1:], "")
	require.NoError(t, err)
	require.NotNil(t, outcome.Suppression)
	created, updated := portal.writes()
	assert.Empty(t, created)
	assert.Empty(t, updated, "nothing new to add, so nothing is written")
	assert.ElementsMatch(t, std, audience.CoveredListIDs(portal.filterOf("77")))
}

// A list an operator added to the standing list by hand survives both a read and a growth.
func TestAttachExisting_ManuallyAddedList_IsKept(t *testing.T) {
	portal, std := standardPortal()
	standingWith(t, portal, append(append([]string{}, std...), "5555"))
	x := composeExplorer(portal.serve(t).URL)

	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.NoError(t, err)
	_, updated := portal.writes()
	assert.Empty(t, updated, "an extra member is not a reason to rewrite")

	portal.setList(audience.StandardSuppressionTerms[0].SearchTerm, "8001")
	_, err = x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, append([]string{"8001"}, std[1:]...), "")
	require.NoError(t, err)
	_, updated = portal.writes()
	assert.Equal(t, []string{"77"}, updated)
	assert.ElementsMatch(t, append(append([]string{"8001"}, std...), "5555"), audience.CoveredListIDs(portal.filterOf("77")))
}

func TestAttachExisting_StandingListNotDynamic_IsAConflictNamingIt(t *testing.T) {
	portal, std := standardPortal()
	standingWith(t, portal, std)
	portal.processing = map[string]string{"77": "MANUAL"}
	x := composeExplorer(portal.serve(t).URL)

	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.ErrorIs(t, err, audience.ErrStandingListConflict)
	assert.Contains(t, err.Error(), `"LFX Standard Suppressions - proj-1" (id 77)`)
	created, updated := portal.writes()
	assert.Empty(t, created)
	assert.Empty(t, updated)
}

// A standing list edited by hand into something other than a union of lists is not
// rewritten when it must grow: the rewrite would silently drop the hand-made condition.
func TestAttachExisting_StandingListNotAUnion_RefusesToRewrite(t *testing.T) {
	portal, std := standardPortal()
	narrowed, err := audience.MasterListWithSuppressionFilter(std[:1], std[1])
	require.NoError(t, err)
	portal.byName[audience.StandardSuppressionName("proj-1")] = "77"
	portal.filters["77"] = narrowed
	x := composeExplorer(portal.serve(t).URL)

	_, err = x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.ErrorIs(t, err, audience.ErrStandingListConflict)
	_, updated := portal.writes()
	assert.Empty(t, updated)
}

// The create is refused because the name exists (a concurrent caller made it) while search
// has not indexed it yet: the list is looked up again and reused, not reported as a 500.
func TestAttachExisting_StandingCreateRaced_RelooksAndReuses(t *testing.T) {
	portal, std := standardPortal()
	name := audience.StandardSuppressionName("proj-1")
	full, err := audience.MasterListFilter(std)
	require.NoError(t, err)
	portal.refuseCreate, portal.refuseCreateStores, portal.refuseFilter = http.StatusBadRequest, true, full
	portal.lagSearches = map[string]int{name: 2}
	x := composeExplorer(portal.serve(t).URL)

	outcome, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.NoError(t, err)
	require.NotNil(t, outcome.Suppression)
	assert.Equal(t, "4242", outcome.Suppression.ListID)
	created, updated := portal.writes()
	assert.Empty(t, created)
	assert.Empty(t, updated)
	assert.Equal(t, 3, portal.searchCount(name), "one look before the create, two after it")
}

func TestAttachExisting_RegistrantsCreateRaced_RelooksAndReuses(t *testing.T) {
	portal, _ := standardPortal()
	const edition = "Open Source Summit Europe 2026"
	filter, err := audience.CurrentRegistrantsFilter(edition)
	require.NoError(t, err)
	portal.refuseCreate, portal.refuseCreateStores, portal.refuseFilter = http.StatusConflict, true, filter
	portal.lagSearches = map[string]int{audience.CurrentRegistrantsName(edition): 1}
	x := composeExplorer(portal.serve(t).URL)

	outcome, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, []string{"6001"}, edition)
	require.NoError(t, err)
	assert.Equal(t, []string{"6001", "4242"}, outcome.AttachedSuppressionIDs)
}

// A refused create whose list never appears keeps the create's error, after a bounded number
// of looks.
func TestAttachExisting_StandingCreateRefusedAndNeverFound_Fails(t *testing.T) {
	portal, std := standardPortal()
	portal.refuseCreate = http.StatusBadRequest
	x := composeExplorer(portal.serve(t).URL)

	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.Error(t, err)
	assert.NotErrorIs(t, err, audience.ErrStandingListConflict)
	assert.Equal(t, 1+3, portal.searchCount(audience.StandardSuppressionName("proj-1")))
}

func TestAttachExisting_RegistrantsListForAnotherEdition_IsAConflict(t *testing.T) {
	portal, _ := standardPortal()
	const edition = "KubeCon + CloudNativeCon Europe 2026"
	other, err := audience.CurrentRegistrantsFilter("KubeCon + CloudNativeCon Europe 2025")
	require.NoError(t, err)
	portal.byName[audience.CurrentRegistrantsName(edition)] = "88"
	portal.filters["88"] = other
	x := composeExplorer(portal.serve(t).URL)

	_, err = x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, []string{"6001"}, edition)
	require.ErrorIs(t, err, audience.ErrStandingListConflict)
	assert.Contains(t, err.Error(), "(id 88)")
	created, _ := portal.writes()
	assert.Empty(t, created)
}

func TestAttachExisting_RegistrantsListNotDynamic_IsAConflict(t *testing.T) {
	portal, _ := standardPortal()
	const edition = "KubeCon + CloudNativeCon Europe 2026"
	filter, err := audience.CurrentRegistrantsFilter(edition)
	require.NoError(t, err)
	portal.byName[audience.CurrentRegistrantsName(edition)] = "88"
	portal.filters["88"] = filter
	portal.processing = map[string]string{"88": "SNAPSHOT"}
	x := composeExplorer(portal.serve(t).URL)

	_, err = x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, []string{"6001"}, edition)
	require.ErrorIs(t, err, audience.ErrStandingListConflict)
}

// Another writer replaces the standing list's filter from an older read right after this
// service's write, dropping the id it added. The confirming read sees the loss and merges again,
// keeping the other writer's addition too.
func TestAttachExisting_StandingListOverwrittenAfterTheWrite_MergesAgain(t *testing.T) {
	portal, std := standardPortal()
	standingWith(t, portal, std[:len(std)-1])
	theirs, err := audience.MasterListFilter(append(append([]string{}, std[:len(std)-1]...), "5555"))
	require.NoError(t, err)
	portal.afterUpdate = func(p *suppressionPortal, id string) {
		if len(p.updated) == 1 {
			p.filters[id] = theirs
		}
	}
	x := composeExplorer(portal.serve(t).URL)

	outcome, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.NoError(t, err)
	require.NotNil(t, outcome.Suppression)
	_, updated := portal.writes()
	assert.Equal(t, []string{"77", "77"}, updated, "the first write was lost, so it is merged and written again")
	assert.ElementsMatch(t, append(append([]string{}, std...), "5555"), audience.CoveredListIDs(portal.filterOf("77")),
		"both this write's addition and the other writer's survive")
}

// A list replaced under every write is a failure after a bounded number of writes, not a
// standing list reported as covering what it does not.
func TestAttachExisting_StandingListAlwaysOverwritten_FailsAfterBoundedWrites(t *testing.T) {
	portal, std := standardPortal()
	standingWith(t, portal, std[:len(std)-1])
	stale := portal.filterOf("77")
	portal.afterUpdate = func(p *suppressionPortal, id string) { p.filters[id] = stale }
	x := composeExplorer(portal.serve(t).URL)

	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.Error(t, err)
	_, updated := portal.writes()
	assert.Len(t, updated, standingGrowAttempts)
}

func TestAttachExisting_StandingListsNotContactLists_AreConflicts(t *testing.T) {
	portal, std := standardPortal()
	standingWith(t, portal, std)
	portal.objectType = map[string]string{"77": "0-2"}
	x := composeExplorer(portal.serve(t).URL)
	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.ErrorIs(t, err, audience.ErrStandingListConflict)
	assert.Contains(t, err.Error(), "not a contact list")

	portal, _ = standardPortal()
	const edition = "KubeCon + CloudNativeCon Europe 2026"
	filter, err := audience.CurrentRegistrantsFilter(edition)
	require.NoError(t, err)
	portal.byName[audience.CurrentRegistrantsName(edition)] = "88"
	portal.filters["88"] = filter
	portal.objectType = map[string]string{"88": "0-2"}
	x = composeExplorer(portal.serve(t).URL)
	_, err = x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, []string{"6001"}, edition)
	require.ErrorIs(t, err, audience.ErrStandingListConflict)
}

// The picker's registrants row and standing-list link are lenient like its standard rows: a
// failed search leaves them unresolved instead of failing the whole picker. A permission
// rejection still fails it.
func TestSuppressionLists_StandingLookupsAreLenient(t *testing.T) {
	const edition = "Open Source Summit Europe 2026"

	portal, std := standardPortal()
	portal.byName[audience.StandardSuppressionName("proj-1")] = "77"
	portal.failSearch = audience.CurrentRegistrantsName(edition)
	x := composeExplorer(portal.serve(t).URL)
	rows, standing, err := x.SuppressionLists(context.Background(), "proj-1", "", "", edition)
	require.NoError(t, err)
	require.Len(t, rows, len(std)+1)
	last := rows[len(rows)-1]
	assert.Equal(t, audience.SuppressionCategoryCurrentRegistrants, last.Category)
	assert.Empty(t, last.ListID)
	assert.False(t, last.WillCreate, "unknown is not the same as absent")
	require.NotNil(t, standing)

	portal, _ = standardPortal()
	portal.failSearch = audience.StandardSuppressionName("proj-1")
	x = composeExplorer(portal.serve(t).URL)
	rows, standing, err = x.SuppressionLists(context.Background(), "proj-1", "", "", edition)
	require.NoError(t, err)
	assert.Nil(t, standing)
	assert.True(t, rows[len(rows)-1].WillCreate)

	portal, _ = standardPortal()
	portal.forbidSearch = audience.StandardSuppressionName("proj-1")
	x = composeExplorer(portal.serve(t).URL)
	_, _, err = x.SuppressionLists(context.Background(), "proj-1", "", "", edition)
	require.Error(t, err)
	assert.True(t, hubspot.IsPermissionRejection(err))
}
