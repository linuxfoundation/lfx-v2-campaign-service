// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
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
	// foreign holds non-contact lists by name; every search returns them with objectTypeId 0-2,
	// as HubSpot does for a search that does not constrain the type.
	foreign map[string]string
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
			if id, ok := p.foreign[body.Query]; ok {
				lists = append(lists, map[string]any{"listId": id, "name": body.Query, "objectTypeId": "0-2"})
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
	// The fingerprint covers the RESOLVED exclusions, the registrants list's id among them.
	withRegistrants := audience.SelectionFingerprint([]string{"111"}, []string{"6001", "9001"})
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
// of looks and one look across object types.
func TestAttachExisting_StandingCreateRefusedAndNeverFound_Fails(t *testing.T) {
	portal, std := standardPortal()
	portal.refuseCreate = http.StatusBadRequest
	x := composeExplorer(portal.serve(t).URL)

	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.Error(t, err)
	assert.NotErrorIs(t, err, audience.ErrStandingListConflict)
	assert.Equal(t, 1+3+1, portal.searchCount(audience.StandardSuppressionName("proj-1")))
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

// composeExplorerProjects is composeExplorer for several projects whose connections reach the
// SAME portal, as two foundations on the shared LF portal do.
func composeExplorerProjects(srvURL string, projects ...string) *AudienceExplorer {
	rows := map[string]*model.Connection{}
	for _, p := range projects {
		rows[p] = activeHubSpotConn(goodHubSpotCreds)
	}
	x := NewAudienceExplorer(NewAudienceBuilder(&scopedConnReader{rows: rows}, identityEncryptor{}, nil, hubspot.WithBaseURL(srvURL)), nil, nil, nil)
	x.sleep = func(context.Context, time.Duration) error { return nil }
	return x
}

// Two projects on one portal compose the same named selection. Each excludes its OWN standing
// list, so the second must not reuse the first's fingerprinted master.
func TestComposeMaster_TwoProjectsOnOnePortal_NeverShareAMaster(t *testing.T) {
	portal, std := standardPortal()
	portal.byName["AGNTCon Japan - Master"] = "31349" // the derived name is taken, so masters are fingerprinted
	x := composeExplorerProjects(portal.serve(t).URL, "proj-1", "proj-2")
	in := audience.ComposeInput{ListIDs: []string{"111"}, ExcludeListIDs: std, Name: "AGNTCon Japan - Master"}

	first, err := x.ComposeMaster(context.Background(), "proj-1", in)
	require.NoError(t, err)
	second, err := x.ComposeMaster(context.Background(), "proj-2", in)
	require.NoError(t, err)

	require.NotNil(t, first.Suppression)
	require.NotNil(t, second.Suppression)
	assert.NotEqual(t, first.Suppression.ListID, second.Suppression.ListID, "one standing list per project")
	assert.NotEqual(t, first.Master.ListID, second.Master.ListID, "proj-2 must not reuse proj-1's master")
	master, _ := json.Marshal(portal.filterOf(second.Master.ListID))
	assert.Contains(t, string(master), `"listId":"`+second.Suppression.ListID+`"`)
	assert.NotContains(t, string(master), `"listId":"`+first.Suppression.ListID+`"`)
}

// A recreated standing list has a new id, so the same compose is a new master that excludes it,
// never the old master that excludes the list that is gone.
func TestComposeMaster_RecreatedStandingList_IsANewMaster(t *testing.T) {
	portal, std := standardPortal()
	portal.byName["AGNTCon Japan - Master"] = "31349"
	x := composeExplorer(portal.serve(t).URL)
	in := audience.ComposeInput{ListIDs: []string{"111"}, ExcludeListIDs: std, Name: "AGNTCon Japan - Master"}

	first, err := x.ComposeMaster(context.Background(), "proj-1", in)
	require.NoError(t, err)
	again, err := x.ComposeMaster(context.Background(), "proj-1", in)
	require.NoError(t, err)
	assert.Equal(t, first.Master.ListID, again.Master.ListID, "an identical compose still reuses its master")

	recreated, err := audience.MasterListFilter(std)
	require.NoError(t, err)
	portal.mu.Lock()
	portal.byName[audience.StandardSuppressionName("proj-1")] = "7777"
	portal.filters["7777"] = recreated
	portal.mu.Unlock()

	third, err := x.ComposeMaster(context.Background(), "proj-1", in)
	require.NoError(t, err)
	require.NotNil(t, third.Suppression)
	assert.Equal(t, "7777", third.Suppression.ListID)
	assert.NotEqual(t, first.Master.ListID, third.Master.ListID)
	master, _ := json.Marshal(portal.filterOf(third.Master.ListID))
	assert.Contains(t, string(master), `"listId":"7777"`)
}

// The edition's registrants list is also one of the lists the send goes to: refused as a 400
// naming it, before anything is created or rewritten, on attach and on compose.
func TestResolveSuppressions_RegistrantsListAlsoIncluded_IsRefusedBeforeAnyWrite(t *testing.T) {
	const edition = "KubeCon + CloudNativeCon Europe 2026"
	seed := func(t *testing.T) *suppressionPortal {
		t.Helper()
		portal, _ := standardPortal()
		filter, err := audience.CurrentRegistrantsFilter(edition)
		require.NoError(t, err)
		portal.byName[audience.CurrentRegistrantsName(edition)] = "88"
		portal.filters["88"] = filter
		return portal
	}

	portal := seed(t)
	_, std := standardPortal()
	x := composeExplorer(portal.serve(t).URL)
	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027", "88"}, std, edition)
	var overlap *audience.ExclusionOverlapError
	require.ErrorAs(t, err, &overlap)
	require.ErrorIs(t, err, audience.ErrInvalidRequest)
	assert.Equal(t, "88", overlap.ListID)
	assert.Contains(t, err.Error(), "current-registrants list")
	created, updated := portal.writes()
	assert.Empty(t, created, "the standing list is not created for a refused request")
	assert.Empty(t, updated)

	portal = seed(t)
	x = composeExplorer(portal.serve(t).URL)
	_, err = x.ComposeMaster(context.Background(), "proj-1", audience.ComposeInput{
		ListIDs: []string{"88", "111"}, ExcludeListIDs: []string{"6001"}, Name: "KubeCon EU - Master", CurrentRegistrantsEvent: edition,
	})
	require.ErrorAs(t, err, &overlap)
	created, _ = portal.writes()
	assert.Empty(t, created, "no master that excludes its own inclusion branch")
}

// The standing standard list stands in for the selected standard lists; including it while
// selecting them is the same contradiction.
func TestAttachExisting_StandingListAlsoIncluded_IsRefused(t *testing.T) {
	portal, std := standardPortal()
	standingWith(t, portal, std)
	x := composeExplorer(portal.serve(t).URL)

	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"77"}, std, "")
	var overlap *audience.ExclusionOverlapError
	require.ErrorAs(t, err, &overlap)
	assert.Equal(t, "77", overlap.ListID)
	assert.Contains(t, err.Error(), "standing standard suppression list")
	_, updated := portal.writes()
	assert.Empty(t, updated)
}

// A non-contact list holds the standing name: SearchLists never returns it, so the create is
// refused with nothing found. The cross-type lookup names it as the 409 conflict.
func TestAttachExisting_NonContactListHoldsTheStandingName_IsANamedConflict(t *testing.T) {
	portal, std := standardPortal()
	portal.foreign = map[string]string{audience.StandardSuppressionName("proj-1"): "5050"}
	portal.refuseCreate = http.StatusBadRequest
	x := composeExplorer(portal.serve(t).URL)

	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.ErrorIs(t, err, audience.ErrStandingListConflict)
	assert.Contains(t, err.Error(), "(id 5050)")
	assert.Contains(t, err.Error(), "not a contact list")
}

// A master named under the pre-LFXV2-2770 fingerprint excluded one combined wrapper list, which
// may since have been edited or deleted. A selection whose resolved exclusions equal the
// requested ones (no full standard set, no registrants edition) must not reuse it.
func TestComposeMaster_PreChangeFingerprintedMaster_IsNotReused(t *testing.T) {
	portal, _ := standardPortal()
	const base = "AGNTCon Japan - Master"
	include, exclude := []string{"111"}, []string{"6001"}
	sum := sha256.Sum256([]byte(strings.Join(include, ",") + "|" + strings.Join(exclude, ",")))
	legacy := hex.EncodeToString(sum[:])[:8] // the fingerprint before the scheme was versioned
	portal.byName[base] = "31349"
	portal.byName[audience.WithFingerprint(base, legacy)] = "31350"
	x := composeExplorer(portal.serve(t).URL)

	outcome, err := x.ComposeMaster(context.Background(), "proj-1", audience.ComposeInput{ListIDs: include, ExcludeListIDs: exclude, Name: base})
	require.NoError(t, err)
	assert.NotEqual(t, legacy, audience.SelectionFingerprint(include, exclude))
	assert.NotEqual(t, "31350", outcome.Master.ListID, "the pre-change master is never reused")
	created, _ := portal.writes()
	require.Len(t, created, 1)
	assert.Equal(t, audience.WithFingerprint(base, audience.SelectionFingerprint(include, exclude)), created[0]["name"])
}

// Search lag hides an included standing list from the first read; the refused create's
// re-lookup recovers it. The overlap is refused BEFORE the list is grown.
func TestAttachExisting_RecoveredStandingListAlsoIncluded_IsRefusedBeforeGrowing(t *testing.T) {
	portal, std := standardPortal()
	standingWith(t, portal, std[:len(std)-1]) // would need growth
	name := audience.StandardSuppressionName("proj-1")
	portal.lagSearches = map[string]int{name: 1}
	portal.refuseCreate = http.StatusBadRequest
	x := composeExplorer(portal.serve(t).URL)

	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027", "77"}, std, "")
	var overlap *audience.ExclusionOverlapError
	require.ErrorAs(t, err, &overlap)
	assert.Equal(t, "77", overlap.ListID)
	_, updated := portal.writes()
	assert.Empty(t, updated, "a rejected request must not grow the standing list")
}

// A late (recovered) registrants hit that is an inclusion is refused before the standing list,
// found normally and needing growth, is grown.
func TestAttachExisting_RecoveredRegistrantsListAlsoIncluded_IsRefusedBeforeGrowing(t *testing.T) {
	portal, std := standardPortal()
	standingWith(t, portal, std[:len(std)-1])
	const edition = "KubeCon + CloudNativeCon Europe 2026"
	filter, err := audience.CurrentRegistrantsFilter(edition)
	require.NoError(t, err)
	portal.byName[audience.CurrentRegistrantsName(edition)] = "88"
	portal.filters["88"] = filter
	portal.lagSearches = map[string]int{audience.CurrentRegistrantsName(edition): 1}
	portal.refuseCreate = http.StatusBadRequest
	x := composeExplorer(portal.serve(t).URL)

	_, err = x.AttachExisting(context.Background(), "proj-1", []string{"31027", "88"}, std, edition)
	var overlap *audience.ExclusionOverlapError
	require.ErrorAs(t, err, &overlap)
	assert.Equal(t, "88", overlap.ListID)
	_, updated := portal.writes()
	assert.Empty(t, updated, "the standing list is not grown for a request that is then refused")
}

// An existing registrants list that does not select the edition is a 409, and the standing
// list, which needs growth, is not grown first: growing changes a list existing audiences use.
func TestAttachExisting_IncompatibleRegistrantsList_IsRefusedBeforeGrowing(t *testing.T) {
	for _, compose := range []bool{false, true} {
		portal, std := standardPortal()
		standingWith(t, portal, std[:len(std)-1])
		const edition = "KubeCon + CloudNativeCon Europe 2026"
		other, err := audience.CurrentRegistrantsFilter("KubeCon + CloudNativeCon Europe 2025")
		require.NoError(t, err)
		portal.byName[audience.CurrentRegistrantsName(edition)] = "88"
		portal.filters["88"] = other
		x := composeExplorer(portal.serve(t).URL)

		if compose {
			_, err = x.ComposeMaster(context.Background(), "proj-1", audience.ComposeInput{
				ListIDs:                 []string{"31027"},
				ExcludeListIDs:          std,
				Name:                    "KubeCon Europe - Master",
				CurrentRegistrantsEvent: edition,
			})
		} else {
			_, err = x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, edition)
		}
		var conflict *audience.StandingListConflictError
		require.ErrorAs(t, err, &conflict, "compose=%v", compose)
		_, updated := portal.writes()
		assert.Empty(t, updated, "compose=%v: the standing list is not grown for a request that is then refused", compose)
	}
}

// A hand-edited standing list that already covers every standard list but also ORs in a contact
// condition suppresses more than the standard lists. A full standard selection needs no growth,
// yet it is still refused, on attach and compose, rather than silently reused.
func TestResolveSuppressions_HandEditedStandingListWithAllStandardIDs_IsNotReused(t *testing.T) {
	for _, compose := range []bool{false, true} {
		portal, std := standardPortal()
		union, err := audience.MasterListFilter(std)
		require.NoError(t, err)
		var tree map[string]any
		require.NoError(t, json.Unmarshal(union, &tree))
		tree["filterBranches"] = append(tree["filterBranches"].([]any), map[string]any{
			"filterBranchType": "AND", "filterBranches": []any{},
			"filters": []any{map[string]any{"filterType": "PROPERTY", "property": "jobtitle",
				"operation": map[string]any{"operationType": "MULTISTRING", "operator": "IS_EQUAL_TO", "values": []any{"Engineer"}}}},
		})
		edited, err := json.Marshal(tree)
		require.NoError(t, err)
		portal.byName[audience.StandardSuppressionName("proj-1")] = "77"
		portal.filters["77"] = edited
		x := composeExplorer(portal.serve(t).URL)

		if compose {
			_, err = x.ComposeMaster(context.Background(), "proj-1", audience.ComposeInput{
				ListIDs: []string{"31027"}, ExcludeListIDs: std, Name: "KubeCon Europe - Master",
			})
		} else {
			_, err = x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
		}
		var conflict *audience.StandingListConflictError
		require.ErrorAs(t, err, &conflict, "compose=%v", compose)
		assert.Equal(t, "77", conflict.ListID)
		created, updated := portal.writes()
		assert.Empty(t, created, "compose=%v", compose)
		assert.Empty(t, updated, "compose=%v", compose)
	}
}

// GetList returns the standing list without a filterBranch. Its real filter is unknown, so it is
// never rewritten from the resolved ids alone, which would drop every suppression it carries.
func TestAttachExisting_StandingListReadWithoutAFilter_IsNeverRewritten(t *testing.T) {
	portal, std := standardPortal()
	portal.byName[audience.StandardSuppressionName("proj-1")] = "77" // no filters["77"]: GET omits filterBranch
	x := composeExplorer(portal.serve(t).URL)

	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "without its filter")
	created, updated := portal.writes()
	assert.Empty(t, created)
	assert.Empty(t, updated, "an unknown filter is not overwritten")
}

// The standing list covers more than the selected standard lists: here a hand-added list 5555.
// Selecting that list as an INCLUSION while the standing list replaces the standard set would
// exclude the whole inclusion, so it is the overlap 400, before any write, on attach and compose.
func TestResolveSuppressions_ListCoveredByTheStandingListAlsoIncluded_IsRefused(t *testing.T) {
	portal, std := standardPortal()
	standingWith(t, portal, append(append([]string{}, std[:len(std)-1]...), "5555")) // also needs growth
	x := composeExplorer(portal.serve(t).URL)

	_, err := x.AttachExisting(context.Background(), "proj-1", []string{"5555"}, std, "")
	var overlap *audience.ExclusionOverlapError
	require.ErrorAs(t, err, &overlap)
	assert.Equal(t, "5555", overlap.ListID)
	assert.Contains(t, err.Error(), `covered by the project's standing standard suppression list`)
	_, updated := portal.writes()
	assert.Empty(t, updated, "refused before the standing list is grown")

	_, err = x.ComposeMaster(context.Background(), "proj-1", audience.ComposeInput{
		ListIDs: []string{"5555", "111"}, ExcludeListIDs: std, Name: "KubeCon EU - Master",
	})
	require.ErrorAs(t, err, &overlap)
	assert.Equal(t, "5555", overlap.ListID)
	created, updated := portal.writes()
	assert.Empty(t, created, "no master that excludes its own inclusion")
	assert.Empty(t, updated)
}

// An inclusion that only becomes covered by the growth's confirming read (a concurrent writer
// added it) is refused too, before the master is composed or the audience recorded.
func TestAttachExisting_IncludeCoveredAfterGrowth_IsRefused(t *testing.T) {
	portal, std := standardPortal()
	standingWith(t, portal, std[:len(std)-1])
	withInclude, err := audience.MasterListFilter(append(append([]string{}, std...), "31027"))
	require.NoError(t, err)
	portal.afterUpdate = func(p *suppressionPortal, id string) { p.filters[id] = withInclude }
	x := composeExplorer(portal.serve(t).URL)

	_, err = x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
	var overlap *audience.ExclusionOverlapError
	require.ErrorAs(t, err, &overlap)
	assert.Equal(t, "31027", overlap.ListID)
}

// A standing list whose filter is untyped -- "{}", or a nested node with no type -- is never
// rewritten from the resolved ids: it is the named 409, with zero writes.
func TestAttachExisting_UntypedStandingFilter_IsNeverRewritten(t *testing.T) {
	for name, raw := range map[string]string{
		"empty object": `{}`,
		"nested untyped node": `{"filterBranchType":"OR","filters":[],"filterBranches":[` +
			`{"filters":[{"filterType":"IN_LIST","operator":"IN_LIST","listId":"7001"}],"filterBranches":[]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			portal, std := standardPortal()
			portal.byName[audience.StandardSuppressionName("proj-1")] = "77"
			portal.filters["77"] = json.RawMessage(raw)
			x := composeExplorer(portal.serve(t).URL)

			_, err := x.AttachExisting(context.Background(), "proj-1", []string{"31027"}, std, "")
			require.ErrorIs(t, err, audience.ErrStandingListConflict)
			created, updated := portal.writes()
			assert.Empty(t, created)
			assert.Empty(t, updated, "an untyped filter is not overwritten")
		})
	}
}
