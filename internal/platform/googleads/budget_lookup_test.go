// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// budgetRowOpts are the fields of a campaign_budget row a gate test varies. The zero
// value is NOT the happy path on purpose — budgetRow fills the happy defaults, so a test
// states only the one field it is about and a new gate cannot be silently defaulted past.
type budgetRowOpts struct {
	id             string
	name           string
	status         string
	explicitShared string // JSON literal: "" defaults to false, "true", or "omit" to drop the field
	referenceCount string
	amountMicros   string
	period         string
	deliveryMethod string
	budgetType     string
	resourceName   string // "" means the canonical one for id; "none" sends it present-and-empty
}

// budgetRow renders one campaign_budget row in the shape gaqlSearch hands back, filling
// every unset field with what a budget this service created would carry.
func budgetRow(o budgetRowOpts) json.RawMessage {
	if o.id == "" {
		o.id = "777"
	}
	if o.status == "" {
		o.status = budgetStatusEnabled
	}
	if o.explicitShared == "" {
		o.explicitShared = "false"
	}
	if o.referenceCount == "" {
		o.referenceCount = "0"
	}
	if o.amountMicros == "" {
		o.amountMicros = "50000000"
	}
	if o.period == "" {
		o.period = budgetPeriodDaily
	}
	if o.deliveryMethod == "" {
		o.deliveryMethod = budgetDeliveryStandard
	}
	if o.budgetType == "" {
		o.budgetType = budgetTypeStandard
	}
	switch o.resourceName {
	case "":
		o.resourceName = "customers/1234567890/campaignBudgets/" + o.id
	case "none":
		// Present-and-empty: the id alone has to carry the row's identity.
		o.resourceName = ""
	}
	fields := []string{
		`"resourceName":` + jsonQuote(o.resourceName),
		`"id":` + jsonQuote(o.id),
		`"name":` + jsonQuote(o.name),
		`"status":` + jsonQuote(o.status),
		`"referenceCount":` + jsonQuote(o.referenceCount),
		`"amountMicros":` + jsonQuote(o.amountMicros),
		`"period":` + jsonQuote(o.period),
		`"deliveryMethod":` + jsonQuote(o.deliveryMethod),
		`"type":` + jsonQuote(o.budgetType),
	}
	// Omitted, not null: Google drops false booleans from REST responses in some shapes,
	// and "the field never came back" is the case the sharing gate has to refuse.
	if o.explicitShared != "omit" {
		fields = append(fields, `"explicitlyShared":`+o.explicitShared)
	}
	return json.RawMessage(`{"campaignBudget":{` + strings.Join(fields, ",") + `}}`)
}

// referencingCampaignRow renders one row of the attachment-check query.
func referencingCampaignRow(id, name, status, budgetResource string) json.RawMessage {
	return json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/` + id +
		`","id":` + jsonQuote(id) + `,"name":` + jsonQuote(name) + `,"status":` + jsonQuote(status) +
		`,"campaignBudget":` + jsonQuote(budgetResource) + `}}`)
}

// newBudgetLookupServer serves the token endpoint and one googleAds:search response per
// call, in order: the first is the budget lookup, the second the attachment check. A call
// past the end of pages fails the test rather than silently repeating the last page —
// an extra query is a behaviour change, not a detail.
func newBudgetLookupServer(t *testing.T, pages ...[]json.RawMessage) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu      sync.Mutex
		queries []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		var req searchRequest
		// t.Error, not t.Fatal: FailNow is only legal on the test goroutine.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding search request: %v", err)
		}
		mu.Lock()
		n := len(queries)
		queries = append(queries, req.Query)
		mu.Unlock()

		if n >= len(pages) {
			t.Errorf("unexpected extra search (#%d): %s", n+1, req.Query)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rows := pages[n]
		if rows == nil {
			// Never `{"results":null}` — a different response from an empty page.
			rows = []json.RawMessage{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(searchResponse{Results: rows})
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), queries...)
	}
}

// searchPagesHandler is newBudgetLookupServer's paging rule as a bare googleAds:search
// handler, for the channel harnesses that route search themselves: one response per call,
// in order, and a call past the end of pages fails the test rather than silently repeating
// the last page.
func searchPagesHandler(t *testing.T, pages ...[]json.RawMessage) http.HandlerFunc {
	t.Helper()
	var (
		mu sync.Mutex
		n  int
	)
	return func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		i := n
		n++
		mu.Unlock()
		if i >= len(pages) {
			// t.Error, not t.Fatal: FailNow is only legal on the test goroutine.
			t.Errorf("unexpected extra search (#%d)", i+1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rows := pages[i]
		if rows == nil {
			// Never `{"results":null}` — a different response from an empty page.
			rows = []json.RawMessage{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(searchResponse{Results: rows})
	}
}

const testBudgetName = "LFX | Budget | proj | evt | brief-1"

// TestFindReusableBudget_AdoptsTheOrphan is the happy path, and the whole point of the
// feature: a budget that is ours, live, non-shared, unattached and the same shape the
// create would have produced is returned for the campaign to attach to.
func TestFindReusableBudget_AdoptsTheOrphan(t *testing.T) {
	srv, queries := newBudgetLookupServer(t,
		[]json.RawMessage{budgetRow(budgetRowOpts{name: testBudgetName})},
		nil, // attachment check: no campaign references it
	)
	c := newAccountsTestClient(t, srv)

	b, err := c.findReusableBudget(context.Background(), testBudgetName)
	if err != nil {
		t.Fatalf("findReusableBudget: %v", err)
	}
	if b.ID != "777" {
		t.Errorf("ID = %q, want 777", b.ID)
	}
	// Rebuilt from our own customer id and the canonical budget id, never echoed.
	if want := "customers/1234567890/campaignBudgets/777"; b.ResourceName != want {
		t.Errorf("ResourceName = %q, want %q", b.ResourceName, want)
	}
	if b.AmountMicros != 50_000_000 {
		t.Errorf("AmountMicros = %d, want 50000000", b.AmountMicros)
	}

	q := queries()
	if len(q) != 2 {
		t.Fatalf("want exactly two queries (lookup, attachment check), got %d: %v", len(q), q)
	}
	// Both predicates must be server-side: without them the lookup walks every budget in
	// the account and a tombstone tail can page the query out.
	if !strings.Contains(q[0], "campaign_budget.name = '"+testBudgetName+"'") {
		t.Errorf("lookup lacks the server-side name filter: %s", q[0])
	}
	if !strings.Contains(q[0], "campaign_budget.status != 'REMOVED'") {
		t.Errorf("lookup lacks the REMOVED exclusion: %s", q[0])
	}
	// The attachment check must ask from the CAMPAIGN side — reference_count alone is
	// documented as campaigns "actively using" the budget, and this service creates every
	// campaign PAUSED, so that phrase is least clear about the shape most likely to exist.
	if !strings.Contains(q[1], "campaign.campaign_budget = 'customers/1234567890/campaignBudgets/777'") {
		t.Errorf("attachment check does not filter on our budget: %s", q[1])
	}
}

// TestFindReusableBudget_RefusesEachGate walks every precondition. Each case changes ONE
// field of an otherwise adoptable row, so a gate that stopped working fails here alone
// rather than being masked by another gate still refusing.
func TestFindReusableBudget_RefusesEachGate(t *testing.T) {
	cases := []struct {
		name string
		row  budgetRowOpts
		want string
	}{{
		name: "explicitly shared is never adopted",
		row:  budgetRowOpts{name: testBudgetName, explicitShared: "true"},
		want: "explicitly shared",
	}, {
		// Absent is not false: granting the verdict on a field the response never sent
		// is exactly the permission this gate exists to withhold.
		name: "a missing explicitly_shared is refused, not read as false",
		row:  budgetRowOpts{name: testBudgetName, explicitShared: "omit"},
		want: "whose sharing is unknown",
	}, {
		name: "a budget already referenced by a campaign",
		row:  budgetRowOpts{name: testBudgetName, referenceCount: "1"},
		want: "refusing to attach another campaign",
	}, {
		name: "an unreadable reference count",
		row:  budgetRowOpts{name: testBudgetName, referenceCount: "not-a-number"},
		want: "attachment count is unreadable",
	}, {
		// Period decides WHICH field holds the money, so a mismatch would compare a
		// daily amount against a whole-flight cap.
		name: "a custom-period budget",
		row:  budgetRowOpts{name: testBudgetName, period: budgetPeriodCustom},
		want: "different pacing model",
	}, {
		name: "a non-standard delivery method",
		row:  budgetRowOpts{name: testBudgetName, deliveryMethod: "ACCELERATED"},
		want: "delivery method",
	}, {
		name: "a non-standard budget type",
		row:  budgetRowOpts{name: testBudgetName, budgetType: "HOTEL_ADS_COMMISSION"},
		want: "has type",
	}, {
		// UNKNOWN is a real value Google can return, and an omitted status decodes to "".
		name: "an unrecognised status",
		row:  budgetRowOpts{name: testBudgetName, status: "UNKNOWN"},
		want: "unrecognised status",
	}, {
		name: "another customer's budget",
		row:  budgetRowOpts{name: testBudgetName, resourceName: "customers/9999999999/campaignBudgets/777"},
		want: "is not customers/1234567890/campaignBudgets",
	}, {
		name: "a row that disagrees with itself about which budget it is",
		row:  budgetRowOpts{name: testBudgetName, resourceName: "customers/1234567890/campaignBudgets/888"},
		want: "disagrees with itself",
	}, {
		// "007" is the same budget as "7" to the server and a different string here.
		name: "a non-canonical id spelling",
		row:  budgetRowOpts{name: testBudgetName, id: "007", resourceName: "none"},
		want: "canonical base-10 spelling",
	}, {
		// An escaping regression would return 2xx with rows for OTHER budgets. A
		// mismatch is an error, not a skip: the filter was not honoured, which
		// invalidates the whole response.
		name: "a name the filter did not honour",
		row:  budgetRowOpts{name: "some other budget"},
		want: "name filter was not honoured",
	}, {
		name: "an unreadable amount",
		row:  budgetRowOpts{name: testBudgetName, amountMicros: "not-a-number"},
		want: "not a positive integer",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Only the lookup page: every gate here must refuse before the attachment
			// check is worth spending, and the server fails the test on an extra query.
			srv, _ := newBudgetLookupServer(t, []json.RawMessage{budgetRow(tc.row)})
			c := newAccountsTestClient(t, srv)

			b, err := c.findReusableBudget(context.Background(), testBudgetName)
			if err == nil {
				t.Fatalf("expected a refusal, got budget %+v", b)
			}
			if b != nil {
				t.Errorf("a refusal must return no budget, got %+v", b)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not say why: want it to mention %q, got: %v", tc.want, err)
			}
		})
	}
}

// TestFindReusableBudget_NoRowsIsAContradiction pins the one place this lookup differs
// from FindCampaignByName: it is reached only because Google REFUSED the create as a
// duplicate name, so "no live budget holds that name" is two answers contradicting each
// other, not a clean absence. Reading it as an absence would be a licence to proceed on
// evidence nothing established.
func TestFindReusableBudget_NoRowsIsAContradiction(t *testing.T) {
	srv, _ := newBudgetLookupServer(t, nil)
	c := newAccountsTestClient(t, srv)

	if _, err := c.findReusableBudget(context.Background(), testBudgetName); err == nil ||
		!strings.Contains(err.Error(), "contradict") {
		t.Fatalf("an empty result must be refused as a contradiction, got: %v", err)
	}
}

// TestFindBudgetByName_NoRowsIsACleanAbsence is the other side of the same response, and
// the reason the contract was split in two: the SEARCH reports an absence, and only the
// DUPLICATE_NAME wrapper above turns it into a contradiction. The ambiguous-create arm
// reaches the account with Google having refused nothing, so for it an absence is the
// answer — proof the create never landed — and a lookup that errored on it would report
// "something may exist" about a budget that provably does not.
func TestFindBudgetByName_NoRowsIsACleanAbsence(t *testing.T) {
	srv, queries := newBudgetLookupServer(t, nil)
	c := newAccountsTestClient(t, srv)

	budget, found, err := c.findBudgetByName(context.Background(), testBudgetName)
	if err != nil {
		t.Fatalf("an empty result set is not an error here: %v", err)
	}
	if found || budget != nil {
		t.Fatalf("found = %v, budget = %+v, want a clean absence", found, budget)
	}
	// No attachment check: there is no budget to ask about. An extra query would mean the
	// absence was decided after reading something else.
	if q := queries(); len(q) != 1 {
		t.Errorf("want exactly the one lookup query, got %d: %v", len(q), q)
	}
}

// TestFindBudgetByName_AGateFailureIsNeverAnAbsence is the invariant that keeps the clean
// absence safe to act on. A budget that EXISTS under this name but fails a gate is an
// ERROR, never (nil, false, nil) — read as an absence, it would tell the ambiguous-create
// arm that nothing was created while a budget of that very name sat in the account.
//
// The shared case stands in for all of them: findBudgetByName returns the gate error
// budgetRowIdentity raised, and reports found=false alongside it so no caller can read the
// pair as a verdict about the account.
func TestFindBudgetByName_AGateFailureIsNeverAnAbsence(t *testing.T) {
	srv, _ := newBudgetLookupServer(t, []json.RawMessage{
		budgetRow(budgetRowOpts{name: testBudgetName, explicitShared: "true"}),
	})
	c := newAccountsTestClient(t, srv)

	budget, found, err := c.findBudgetByName(context.Background(), testBudgetName)
	if err == nil {
		t.Fatal("a shared budget of this name must be an error, not an absence")
	}
	if !strings.Contains(err.Error(), "explicitly shared") {
		t.Errorf("error = %q, want the sharing gate's own sentence", err)
	}
	if found || budget != nil {
		t.Errorf("found = %v, budget = %+v, want neither alongside an error", found, budget)
	}
}

// TestResolveBudgetAfterAmbiguousCreate_ReportsTheThreeOutcomes pins the adapter the
// ambiguous arms call, because each of its three answers drives a different operator
// sentence and a different retry.
func TestResolveBudgetAfterAmbiguousCreate_ReportsTheThreeOutcomes(t *testing.T) {
	t.Run("created", func(t *testing.T) {
		srv, _ := newBudgetLookupServer(t,
			[]json.RawMessage{budgetRow(budgetRowOpts{name: testBudgetName})},
			nil, // attachment check: a never-attached budget is referenced by nothing
		)
		id, created, err := newAccountsTestClient(t, srv).
			resolveBudgetAfterAmbiguousCreate(context.Background(), testBudgetName)
		if err != nil || !created || id != "777" {
			t.Fatalf("got (%q, %v, %v), want the committed budget's id", id, created, err)
		}
	})
	t.Run("not visible", func(t *testing.T) {
		// One empty page per attempt: the absence is only reported once the whole
		// schedule has asked, and the harness fails the test on a search past the end,
		// so this pins the attempt count from both sides.
		pages := make([][]json.RawMessage, ambiguousBudgetResolveAttempts)
		srv, queries := newBudgetLookupServer(t, pages...)
		id, created, err := newAccountsTestClient(t, srv).
			resolveBudgetAfterAmbiguousCreate(context.Background(), testBudgetName)
		if err != nil || created || id != "" {
			t.Fatalf("got (%q, %v, %v), want an absence", id, created, err)
		}
		if got := len(queries()); got != ambiguousBudgetResolveAttempts {
			t.Errorf("searches = %d, want %d — the absence must be corroborated, not taken on the first read", got, ambiguousBudgetResolveAttempts)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if writeAccountsToken(w, r) {
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		id, created, err := newAccountsTestClient(t, srv).
			resolveBudgetAfterAmbiguousCreate(context.Background(), testBudgetName)
		if err == nil {
			t.Fatal("an unreadable account settles nothing and must be an error")
		}
		if created || id != "" {
			t.Errorf("got (%q, %v) alongside the error, want neither", id, created)
		}
	})
}

// TestResolveBudgetAfterAmbiguousCreate_FindsABudgetThatWasNotVisibleYet is the whole
// reason the no-row outcome is retried. The arm is reached when campaignBudgets:mutate
// failed without saying whether the write landed — including while Google was still
// committing it — so the budget that most needs reporting is exactly the one a single
// immediate read can miss. One empty page, then the row, then its attachment check.
func TestResolveBudgetAfterAmbiguousCreate_FindsABudgetThatWasNotVisibleYet(t *testing.T) {
	srv, queries := newBudgetLookupServer(t,
		nil, // first look: the commit has not surfaced yet
		[]json.RawMessage{budgetRow(budgetRowOpts{name: testBudgetName})},
		nil, // attachment check: a never-attached budget is referenced by nothing
	)
	id, created, err := newAccountsTestClient(t, srv).
		resolveBudgetAfterAmbiguousCreate(context.Background(), testBudgetName)
	if err != nil || !created || id != "777" {
		t.Fatalf("got (%q, %v, %v), want the budget the second look found", id, created, err)
	}
	if got := len(queries()); got != 3 {
		t.Errorf("searches = %d, want 3 — the retry, the hit, and its attachment check", got)
	}
}

// TestAmbiguousBudgetResolveScheduleSpans computes the schedule from the constants, so a
// change to any of the three fails a test rather than only a comment. N attempts sleep
// N-1 times.
func TestAmbiguousBudgetResolveScheduleSpans(t *testing.T) {
	var (
		total time.Duration
		delay = ambiguousBudgetResolveMinDelay
	)
	for i := 1; i < ambiguousBudgetResolveAttempts; i++ {
		total += delay
		if delay *= 2; delay > ambiguousBudgetResolveMaxDelay {
			delay = ambiguousBudgetResolveMaxDelay
		}
	}
	if want := 1750 * time.Millisecond; total != want {
		t.Errorf("schedule spans %v, want %v — update the doc comment alongside the constants", total, want)
	}
	if total >= ambiguousBudgetResolveTimeout {
		t.Errorf("schedule (%v) must stay well inside the %v ceiling, which also has to leave room for the four searches to answer", total, ambiguousBudgetResolveTimeout)
	}
}

// TestFindReusableBudget_AmbiguousIsNeverASilentPick: two live budgets of one name is
// anomalous, which is exactly when guessing is least defensible.
func TestFindReusableBudget_AmbiguousIsNeverASilentPick(t *testing.T) {
	srv, _ := newBudgetLookupServer(t, []json.RawMessage{
		budgetRow(budgetRowOpts{id: "777", name: testBudgetName}),
		budgetRow(budgetRowOpts{id: "888", name: testBudgetName}),
	})
	c := newAccountsTestClient(t, srv)

	_, err := c.findReusableBudget(context.Background(), testBudgetName)
	if err == nil || !strings.Contains(err.Error(), "refusing to choose one") {
		t.Fatalf("two same-name budgets must refuse, got: %v", err)
	}
}

// TestFindReusableBudget_DuplicateRowsAreOneBudget: GAQL can return one resource on
// several rows, and counting rows rather than distinct budgets would report a single
// budget as ambiguous and block a reuse that is in fact unambiguous.
func TestFindReusableBudget_DuplicateRowsAreOneBudget(t *testing.T) {
	srv, _ := newBudgetLookupServer(t,
		[]json.RawMessage{
			budgetRow(budgetRowOpts{id: "777", name: testBudgetName}),
			budgetRow(budgetRowOpts{id: "777", name: testBudgetName}),
		},
		nil,
	)
	c := newAccountsTestClient(t, srv)

	b, err := c.findReusableBudget(context.Background(), testBudgetName)
	if err != nil {
		t.Fatalf("the same budget twice is one budget: %v", err)
	}
	if b.ID != "777" {
		t.Errorf("ID = %q, want 777", b.ID)
	}
}

// TestFindReusableBudget_RemovedRowIsSkippedNotAdopted: a tombstone can never be
// attached to anything, so it is the one state safe to drop — but dropping the ONLY row
// then leaves the contradiction above rather than a quiet success.
func TestFindReusableBudget_RemovedRowIsSkippedNotAdopted(t *testing.T) {
	srv, _ := newBudgetLookupServer(t, []json.RawMessage{
		budgetRow(budgetRowOpts{id: "777", name: testBudgetName, status: budgetStatusRemoved}),
	})
	c := newAccountsTestClient(t, srv)

	_, err := c.findReusableBudget(context.Background(), testBudgetName)
	if err == nil || !strings.Contains(err.Error(), "contradict") {
		t.Fatalf("a removed-only result must not be adopted, got: %v", err)
	}
}

// TestFindReusableBudget_LiveReferencingCampaignRefuses is the authoritative half of the
// unattached proof: reference_count says zero, the campaign side says otherwise, and the
// disagreement refuses. Attaching here would split one daily amount across two campaigns
// nobody agreed to fund together.
func TestFindReusableBudget_LiveReferencingCampaignRefuses(t *testing.T) {
	srv, _ := newBudgetLookupServer(t,
		[]json.RawMessage{budgetRow(budgetRowOpts{name: testBudgetName})},
		[]json.RawMessage{referencingCampaignRow("222", "someone else's campaign", StatusPaused,
			"customers/1234567890/campaignBudgets/777")},
	)
	c := newAccountsTestClient(t, srv)

	_, err := c.findReusableBudget(context.Background(), testBudgetName)
	if err == nil || !strings.Contains(err.Error(), "already used by campaign 222") {
		t.Fatalf("a budget with a live campaign on it must be refused, got: %v", err)
	}
}

// A REMOVED campaign cannot serve and cannot spend, so its tombstone does not make the
// budget in use.
func TestFindReusableBudget_RemovedReferencingCampaignDoesNotBlock(t *testing.T) {
	srv, _ := newBudgetLookupServer(t,
		[]json.RawMessage{budgetRow(budgetRowOpts{name: testBudgetName})},
		[]json.RawMessage{referencingCampaignRow("222", "a dead campaign", StatusRemoved,
			"customers/1234567890/campaignBudgets/777")},
	)
	c := newAccountsTestClient(t, srv)

	b, err := c.findReusableBudget(context.Background(), testBudgetName)
	if err != nil {
		t.Fatalf("a removed campaign does not hold a budget: %v", err)
	}
	if b.ID != "777" {
		t.Errorf("ID = %q, want 777", b.ID)
	}
}

// The attachment check re-applies its own filter client-side for the same reason the name
// lookup does: a row for a DIFFERENT budget means the predicate did not take effect, which
// invalidates the whole response — including its silence about ours.
func TestFindReusableBudget_AttachmentCheckFilterNotHonoured(t *testing.T) {
	srv, _ := newBudgetLookupServer(t,
		[]json.RawMessage{budgetRow(budgetRowOpts{name: testBudgetName})},
		[]json.RawMessage{referencingCampaignRow("222", "unrelated", StatusPaused,
			"customers/1234567890/campaignBudgets/999")},
	)
	c := newAccountsTestClient(t, srv)

	_, err := c.findReusableBudget(context.Background(), testBudgetName)
	if err == nil || !strings.Contains(err.Error(), "filter was not honoured") {
		t.Fatalf("a row for another budget invalidates the check, got: %v", err)
	}
}

// A name Google cannot be holding is not a query worth sending: it is unmatchable, and
// the bound is the create path's own — maxBudgetNameBytes, in UTF-8 BYTES, not runes.
func TestFindReusableBudget_OverLongNameIsRefusedBeforeQuerying(t *testing.T) {
	// No pages at all: the server fails the test if any query is sent.
	srv, _ := newBudgetLookupServer(t)
	c := newAccountsTestClient(t, srv)

	_, err := c.findReusableBudget(context.Background(), strings.Repeat("é", maxBudgetNameBytes))
	if err == nil || !strings.Contains(err.Error(), "UTF-8 bytes") {
		t.Fatalf("an over-long name must be refused in bytes before any query, got: %v", err)
	}
}

// TestReuseBudgetAfterDuplicateName_AlignsAmountToTheBrief: the orphan carries what the
// PREVIOUS brief asked for. Refusing on the difference would re-impose the dead end this
// feature closes, so the amount is brought into line — and the alignment is recorded in
// the steps rather than done quietly.
func TestReuseBudgetAfterDuplicateName_AlignsAmountToTheBrief(t *testing.T) {
	var (
		mu         sync.Mutex
		searches   int
		updateBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		if strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate") {
			body := decode(t, r)
			mu.Lock()
			updateBody = body
			mu.Unlock()
			_, _ = w.Write([]byte(`{"results":[{"resourceName":"customers/1234567890/campaignBudgets/777"}]}`))
			return
		}
		mu.Lock()
		n := searches
		searches++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 0 {
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []json.RawMessage{
				budgetRow(budgetRowOpts{name: testBudgetName, amountMicros: "500000000"}),
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []json.RawMessage{}})
	}))
	t.Cleanup(srv.Close)
	c := newAccountsTestClient(t, srv)

	resource, id, steps, err := c.reuseBudgetAfterDuplicateName(context.Background(), testBudgetName, 50_000_000, 50)
	if err != nil {
		t.Fatalf("reuseBudgetAfterDuplicateName: %v", err)
	}
	if id != "777" || resource != "customers/1234567890/campaignBudgets/777" {
		t.Errorf("resource/id = %q / %q, want the canonical pair for 777", resource, id)
	}
	mu.Lock()
	defer mu.Unlock()
	ops, _ := updateBody["operations"].([]any)
	if len(ops) != 1 {
		t.Fatalf("the amount must be aligned with exactly one update operation, got body %v", updateBody)
	}
	op, _ := ops[0].(map[string]any)
	if mask, _ := op["updateMask"].(string); mask != "amount_micros" {
		t.Errorf("updateMask = %q, want amount_micros — a DAILY budget's money is in amount_micros", mask)
	}
	upd, _ := op["update"].(map[string]any)
	// JSON numbers decode as float64 here; the wire value is what matters, not its Go type.
	if got := amountMicrosOf(upd["amountMicros"]); got != 50_000_000 {
		t.Errorf("amountMicros = %v, want the brief's 50000000", upd["amountMicros"])
	}
	// The operator must be able to see from the result that the amount moved.
	var sawAlign bool
	for _, s := range steps {
		if strings.Contains(s, "amount aligned to the brief") {
			sawAlign = true
		}
	}
	if !sawAlign {
		t.Errorf("an amount change must be recorded in the steps, got: %v", steps)
	}
}

// TestCreateCampaign_DuplicateBudgetNameReusesTheOrphan is the end-to-end wiring proof on
// the Search path: Google refuses the budget create as a duplicate, the lookup proves the
// existing budget adoptable, and the campaign is created AGAINST THAT BUDGET instead of
// the dispatch dead-ending — which before this change it always did.
func TestCreateCampaign_DuplicateBudgetNameReusesTheOrphan(t *testing.T) {
	var (
		mu           sync.Mutex
		searches     int
		campaignBody map[string]any
	)
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			gaqlError(http.StatusBadRequest, "campaignBudgetError", "DUPLICATE_NAME")(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			body := decode(t, r)
			mu.Lock()
			campaignBody = body
			mu.Unlock()
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			okAdGroup(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			okAdGroupAd(w, r)
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			mu.Lock()
			n := searches
			searches++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if n == 0 {
				// The orphan, carrying the amount this brief asks for (50 units), so no
				// alignment mutate is needed and the budget handler above stays a refusal.
				_ = json.NewEncoder(w).Encode(searchResponse{Results: []json.RawMessage{
					budgetRow(budgetRowOpts{name: ComposeName(budgetKindFor(""), sampleInput()), amountMicros: "50000000"}),
				}})
				return
			}
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []json.RawMessage{}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))

	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("a reusable orphan budget must not fail the dispatch: %v", err)
	}
	if res.CampaignBudgetID != "777" {
		t.Errorf("CampaignBudgetID = %q, want the reused 777", res.CampaignBudgetID)
	}
	if res.CampaignID != "222" {
		t.Errorf("CampaignID = %q, want 222 — the campaign step must now be reached", res.CampaignID)
	}
	// The campaign must attach to the REUSED budget, not to a resource name from nowhere.
	mu.Lock()
	defer mu.Unlock()
	ops, _ := campaignBody["operations"].([]any)
	if len(ops) != 1 {
		t.Fatalf("campaign body = %v", campaignBody)
	}
	op, _ := ops[0].(map[string]any)
	create, _ := op["create"].(map[string]any)
	if got, _ := create["campaignBudget"].(string); got != "customers/1234567890/campaignBudgets/777" {
		t.Errorf("campaignBudget = %q, want the reused budget's resource name", got)
	}
	var sawReuse bool
	for _, s := range res.Steps {
		if strings.Contains(s, "Campaign budget reused: 777") {
			sawReuse = true
		}
	}
	if !sawReuse {
		t.Errorf("the result must say the budget was reused rather than created: %v", res.Steps)
	}
}

// A refused reuse must leave the pre-reuse behaviour exactly as it was: the original
// already-exists refusal, a name-carrying partial, and no campaign attempted.
func TestCreateCampaign_UnreusableDuplicateBudgetStillRefuses(t *testing.T) {
	c := newCampaignClient(t, // its default search handler reports no budget of that name
		gaqlError(http.StatusBadRequest, "campaignBudgetError", "DUPLICATE_NAME"),
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("campaign must not be attempted when the budget cannot be reused")
			okCampaign(w, nil)
		},
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("an unreusable duplicate must still read as already-exists, got: %v", err)
	}
	// The refusal has to say WHY the reuse was declined, or the operator is sent to the
	// Google Ads UI with no idea what to look for.
	if !strings.Contains(err.Error(), "could not be safely reused") {
		t.Errorf("the refusal must carry the reuse verdict: %v", err)
	}
	if res == nil || res.CampaignName == "" {
		t.Fatalf("expected a name-carrying partial for reconcile, got %+v", res)
	}
	if res.CampaignBudgetID != "" {
		t.Errorf("CampaignBudgetID = %q, want empty — this attempt created and adopted nothing", res.CampaignBudgetID)
	}
}

// amountMicrosOf reads an amount out of a decoded JSON body whether the client sent it as
// a number or as the string form Google also accepts.
func amountMicrosOf(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case string:
		parsed, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return -1
		}
		return parsed
	default:
		return -1
	}
}
