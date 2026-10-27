// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// unreadableBudgetCreate is a 2xx campaignBudgets:mutate response that commits a budget and
// says nothing usable about which one: a results array with an entry carrying no
// resourceName. This is the shape issue #387 is about — Google's success, with the id lost.
func unreadableBudgetCreate(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"results":[{}]}`)
}

// TestBudgetFromCreateResponse_ReadableResponseNeverTouchesTheAccount pins the cost of the
// recovery path on the path where nothing is wrong: a parseable 2xx must be answered from
// the response alone. The harness fails the test on any search at all, so a lookup that
// crept into the ordinary create would be a failure rather than a slow success.
func TestBudgetFromCreateResponse_ReadableResponseNeverTouchesTheAccount(t *testing.T) {
	srv, queries := newBudgetLookupServer(t) // zero pages: any search is an unexpected search
	c := newAccountsTestClient(t, srv)

	resource, id, recovered, err := c.budgetFromCreateResponse(context.Background(), "campaign", testBudgetName,
		[]byte(`{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`))
	if err != nil {
		t.Fatalf("a parseable response is not a recovery: %v", err)
	}
	if resource != "customers/1234567890/campaignBudgets/111" || id != "111" {
		t.Errorf("got (%q, %q), want the resource name the response carried", resource, id)
	}
	if recovered != "" {
		t.Errorf("recoveryStep = %q, want empty — nothing was recovered", recovered)
	}
	if q := queries(); len(q) != 0 {
		t.Errorf("searches = %d, want none: %v", len(q), q)
	}
}

// TestRecoverBudgetAfterUnreadableCreate_FindsTheBudgetGoogleCommitted is the whole point
// of #387. The create succeeded, only its id was lost, and the budget it left behind is
// found by the name this client composed — so the dispatch continues onto that budget
// instead of dead-ending on an orphan with no UI surface.
func TestRecoverBudgetAfterUnreadableCreate_FindsTheBudgetGoogleCommitted(t *testing.T) {
	srv, queries := newBudgetLookupServer(t,
		[]json.RawMessage{budgetRow(budgetRowOpts{name: testBudgetName})},
		nil, // attachment check: a budget created moments ago has no campaign on it
	)
	c := newAccountsTestClient(t, srv)

	budget, err := c.recoverBudgetAfterUnreadableCreate(context.Background(), testBudgetName)
	if err != nil {
		t.Fatalf("the budget Google committed must be found: %v", err)
	}
	if budget.ID != "777" {
		t.Fatalf("ID = %q, want 777", budget.ID)
	}
	// Rebuilt from this client's customer id and the canonical budget id, never echoed —
	// the response that reached this path is precisely the one that could not be trusted.
	if budget.ResourceName != "customers/1234567890/campaignBudgets/777" {
		t.Errorf("ResourceName = %q, want the rebuilt canonical one", budget.ResourceName)
	}
	if got := len(queries()); got != 2 {
		t.Errorf("searches = %d, want 2 — the lookup and its attachment check", got)
	}
}

// TestRecoverBudgetAfterUnreadableCreate_NotVisibleYetIsRetried is why this path shares the
// ambiguous arm's schedule rather than reading once. Here the commit is CONFIRMED — Google
// answered 2xx — so a first read that misses is pure replica lag and nothing else.
func TestRecoverBudgetAfterUnreadableCreate_NotVisibleYetIsRetried(t *testing.T) {
	srv, queries := newBudgetLookupServer(t,
		nil, // first look: the commit has not surfaced on the read replica yet
		[]json.RawMessage{budgetRow(budgetRowOpts{name: testBudgetName})},
		nil, // attachment check
	)
	c := newAccountsTestClient(t, srv)

	budget, err := c.recoverBudgetAfterUnreadableCreate(context.Background(), testBudgetName)
	if err != nil || budget == nil || budget.ID != "777" {
		t.Fatalf("got (%+v, %v), want the budget the second look found", budget, err)
	}
	if got := len(queries()); got != 3 {
		t.Errorf("searches = %d, want 3 — the retry, the hit, and its attachment check", got)
	}
}

// TestRecoverBudgetAfterUnreadableCreate_NoRowsIsAContradiction is the difference between
// this path and ambiguousBudgetCreateOutcome's default arm, and the reason #387 says to
// copy the thin wrapper rather than the adapter. There, a surviving absence is evidence
// that nothing was created. Here Google REPORTED SUCCESS, so a budget of this name exists;
// an account showing none has told us two things that cannot both be true, and neither is a
// safe footing for attaching a billable campaign.
func TestRecoverBudgetAfterUnreadableCreate_NoRowsIsAContradiction(t *testing.T) {
	pages := make([][]json.RawMessage, ambiguousBudgetResolveAttempts)
	srv, queries := newBudgetLookupServer(t, pages...)
	c := newAccountsTestClient(t, srv)

	budget, err := c.recoverBudgetAfterUnreadableCreate(context.Background(), testBudgetName)
	if err == nil || !strings.Contains(err.Error(), "contradict") {
		t.Fatalf("a surviving absence must be refused as a contradiction, got: %v", err)
	}
	if budget != nil {
		t.Errorf("budget = %+v, want none alongside the error", budget)
	}
	if got := len(queries()); got != ambiguousBudgetResolveAttempts {
		t.Errorf("searches = %d, want %d — the absence must be corroborated, not taken on the first read", got, ambiguousBudgetResolveAttempts)
	}
}

// TestRecoverBudgetAfterUnreadableCreate_AGateFailureRefuses: the gates are checked even
// though "Google said 2xx moments ago" predicts they will pass. The argument is an
// argument; the row that comes back is the evidence, and a shared budget is never adopted.
func TestRecoverBudgetAfterUnreadableCreate_AGateFailureRefuses(t *testing.T) {
	srv, _ := newBudgetLookupServer(t, []json.RawMessage{
		budgetRow(budgetRowOpts{name: testBudgetName, explicitShared: "true"}),
	})
	c := newAccountsTestClient(t, srv)

	if budget, err := c.recoverBudgetAfterUnreadableCreate(context.Background(), testBudgetName); err == nil || budget != nil {
		t.Fatalf("got (%+v, %v), want the sharing gate's refusal", budget, err)
	} else if !strings.Contains(err.Error(), "explicitly shared") {
		t.Errorf("error = %q, want the sharing gate's own sentence", err)
	}
}

// TestCreateCampaign_UnreadableBudgetCreateRecoversAndContinues is the end-to-end wiring
// proof on the Search path: a 2xx that carries no usable resource name used to fail the
// whole dispatch, leaving a budget nobody could see holding the name every later retry
// needs. Now the campaign is created AGAINST the recovered budget.
func TestCreateCampaign_UnreadableBudgetCreateRecoversAndContinues(t *testing.T) {
	var campaignBody map[string]any
	budgetName := ComposeName(budgetKindFor(""), sampleInput())
	c := newCampaignClientSearch(t,
		unreadableBudgetCreate,
		func(w http.ResponseWriter, r *http.Request) { campaignBody = decode(t, r); okCampaign(w, r) },
		okAdGroup, okAdGroupAd,
		searchPagesHandler(t,
			[]json.RawMessage{budgetRow(budgetRowOpts{name: budgetName})},
			nil, // attachment check
		),
	)

	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("a recoverable budget must not fail the dispatch: %v", err)
	}
	if res.CampaignBudgetID != "777" {
		t.Errorf("CampaignBudgetID = %q, want the recovered 777", res.CampaignBudgetID)
	}
	if res.CampaignID != "222" {
		t.Errorf("CampaignID = %q, want 222 — the campaign step must now be reached", res.CampaignID)
	}
	ops, _ := campaignBody["operations"].([]any)
	if len(ops) != 1 {
		t.Fatalf("campaign body = %v", campaignBody)
	}
	op, _ := ops[0].(map[string]any)
	create, _ := op["create"].(map[string]any)
	if got, _ := create["campaignBudget"].(string); got != "customers/1234567890/campaignBudgets/777" {
		t.Errorf("campaignBudget = %q, want the recovered budget's resource name", got)
	}
	// The operator is told the id came from a readback rather than from the create: that is
	// the difference between "Google told us" and "we went and looked".
	var sawRecovery bool
	for _, s := range res.Steps {
		if strings.Contains(s, "recovered by lookup") {
			sawRecovery = true
		}
	}
	if !sawRecovery {
		t.Errorf("the result must say the id was recovered: %v", res.Steps)
	}
}

// TestCreateCampaign_UnrecoverableBudgetCreateStillRefuses is the other half: when the
// account will not settle which budget it is, the dispatch fails exactly as it did before
// this existed — no campaign attempted, a name-carrying partial, and no budget id claimed
// on the strength of a read that found nothing.
func TestCreateCampaign_UnrecoverableBudgetCreateStillRefuses(t *testing.T) {
	c := newCampaignClient(t, // the default search handler reports no budget of that name
		unreadableBudgetCreate,
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("campaign must not be attempted when the budget cannot be identified")
			okCampaign(w, nil)
		},
	)

	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("an unidentifiable budget must still fail the dispatch")
	}
	// It says the budget WAS created, because a 2xx settles that — never "may exist", which
	// would send the operator looking for something that is definitely there.
	if !strings.Contains(err.Error(), "WAS created") {
		t.Errorf("the refusal must assert the budget exists: %v", err)
	}
	if !strings.Contains(err.Error(), "contradict") {
		t.Errorf("the refusal must carry the lookup's verdict: %v", err)
	}
	if res == nil || res.CampaignName == "" {
		t.Fatalf("expected a name-carrying partial for reconcile, got %+v", res)
	}
	if res.CampaignBudgetID != "" {
		t.Errorf("CampaignBudgetID = %q, want empty — nothing identified it", res.CampaignBudgetID)
	}
	if outcome, ok := CreateOutcome(res, err); !ok || outcome != domain.CreateOutcomeUnconfirmed {
		t.Errorf("outcome = %q (%v), want unconfirmed — a write landed and nothing downstream was reached", outcome, ok)
	}
}

// TestBudgetFromCreateResponse_WrapsTheParseFailureFirst pins the ordering every arm in
// budget_lookup.go depends on: errors.As stops at the first *apiError in the chain, so an
// apiError raised by the lookup READ must never be mistaken for an answer about the create.
func TestBudgetFromCreateResponse_WrapsTheParseFailureFirst(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := newAccountsTestClient(t, srv)

	_, _, _, err := c.budgetFromCreateResponse(context.Background(), "display", testBudgetName, []byte(`{"results":[{}]}`))
	if err == nil {
		t.Fatal("an unreadable account settles nothing and must be an error")
	}
	msg := err.Error()
	parseAt := strings.Index(msg, "carried no resource name")
	lookupAt := strings.Index(msg, "and the account did not settle it")
	if parseAt < 0 || lookupAt < 0 {
		t.Fatalf("both failures must be in the message: %v", err)
	}
	if parseAt > lookupAt {
		t.Errorf("the parse failure must be wrapped first: %v", err)
	}
	if !strings.Contains(msg, "display") {
		t.Errorf("the message must name the channel: %v", err)
	}
}
