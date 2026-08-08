// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// lookupRow builds one campaign row in the shape gaqlSearch hands back.
func lookupRow(id, name, status string) json.RawMessage {
	return json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/` + id +
		`","id":"` + id + `","name":` + jsonQuote(name) + `,"status":"` + status + `"}}`)
}

// jsonQuote renders s as a JSON string literal, so a test name containing a quote or
// backslash produces a valid fixture rather than a broken one.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// newLookupServer serves the OAuth token endpoint and one googleAds:search response
// built from rows, capturing the GAQL query the client sent.
func newLookupServer(t *testing.T, rows []json.RawMessage) (*httptest.Server, func() string) {
	t.Helper()
	var (
		mu       sync.Mutex
		gotQuery string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		var req searchRequest
		// t.Error, not t.Fatal: FailNow is only legal on the test goroutine, and this
		// handler is not it.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding search request: %v", err)
		}
		mu.Lock()
		gotQuery = req.Query
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(searchResponse{Results: rows})
	}))
	t.Cleanup(srv.Close)
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return gotQuery
	}
}

// TestFindCampaignByName_Unique is the happy path: exactly one live match yields its id.
func TestFindCampaignByName_Unique(t *testing.T) {
	srv, query := newLookupServer(t, []json.RawMessage{
		lookupRow("555", "LFX | Campaign | proj | evt", StatusEnabled),
	})
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), "LFX | Campaign | proj | evt")
	if err != nil {
		t.Fatalf("FindCampaignByName: %v", err)
	}
	if id != "555" {
		t.Fatalf("id = %q, want 555", id)
	}
	// The name filter and the REMOVED exclusion must both be server-side; without them
	// a lookup walks the whole account and a tombstone tail can page the query out.
	q := query()
	if !strings.Contains(q, "campaign.name = 'LFX | Campaign | proj | evt'") {
		t.Errorf("query lacks the server-side name filter: %s", q)
	}
	if !strings.Contains(q, "campaign.status != 'REMOVED'") {
		t.Errorf("query lacks the REMOVED exclusion: %s", q)
	}
}

// TestFindCampaignByName_AbsentIsNotAnError pins the contract both callers depend on:
// a clean absence is ("", nil), because that is what licenses a create.
func TestFindCampaignByName_AbsentIsNotAnError(t *testing.T) {
	srv, _ := newLookupServer(t, nil)
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), "nothing named this")
	if err != nil {
		t.Fatalf("an absent campaign must not be an error, got: %v", err)
	}
	if id != "" {
		t.Fatalf("id = %q, want empty", id)
	}
}

// TestFindCampaignByName_DuplicateNamesAreAmbiguous covers the case that motivates
// returning an error instead of the first hit. Google Ads permits duplicate campaign
// names in one account, so picking arbitrarily would bind a brief to the wrong paid
// campaign.
func TestFindCampaignByName_DuplicateNamesAreAmbiguous(t *testing.T) {
	srv, _ := newLookupServer(t, []json.RawMessage{
		lookupRow("111", "dupe", StatusEnabled),
		lookupRow("222", "dupe", StatusPaused),
	})
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), "dupe")
	if err == nil {
		t.Fatalf("two same-name campaigns must be ambiguous, got id %q and no error", id)
	}
	if id != "" {
		t.Errorf("an ambiguous lookup must not return an id, got %q", id)
	}
	for _, want := range []string{"111", "222", "refusing to choose"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestFindCampaignByName_DuplicateRowsForOneCampaignAreNotAmbiguous is the other half
// of the ambiguity rule: the same campaign returned twice is ONE campaign, and
// reporting it as ambiguous would block a legitimate adoption.
func TestFindCampaignByName_DuplicateRowsForOneCampaignAreNotAmbiguous(t *testing.T) {
	srv, _ := newLookupServer(t, []json.RawMessage{
		lookupRow("777", "same campaign twice", StatusEnabled),
		lookupRow("777", "same campaign twice", StatusEnabled),
	})
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), "same campaign twice")
	if err != nil {
		t.Fatalf("one campaign on two rows must not be ambiguous: %v", err)
	}
	if id != "777" {
		t.Fatalf("id = %q, want 777", id)
	}
}

// TestFindCampaignByName_QuoteInNameCannotInjectQuery is the reason gaqlStringLiteral
// exists. Without escaping, the quote closes the literal and the rest of the name is
// parsed as query syntax — turning an exact-match lookup into a match on the whole
// account, after which the caller binds a brief to an unrelated campaign.
func TestFindCampaignByName_QuoteInNameCannotInjectQuery(t *testing.T) {
	const evil = `x' OR campaign.id > '0`

	// The server returns a campaign that is NOT named `evil` — exactly what an injected
	// query would surface. The client-side equality re-check must reject the whole
	// response as UNVERIFIABLE.
	//
	// The assertion is an ERROR, not a zero match. Discarding the row would collapse an
	// injected query into ("", nil), which is the licence-to-create absence, so a test
	// that accepted a clean absence here would be asserting the unsafe outcome.
	srv, query := newLookupServer(t, []json.RawMessage{
		lookupRow("999", "some other campaign", StatusEnabled),
	})
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), evil)
	if err == nil {
		t.Fatalf("a row that does not match the exact-match query must fail closed, got id %q", id)
	}
	if id != "" {
		t.Errorf("a non-matching row must not be returned as a match, got %q", id)
	}
	if !strings.Contains(err.Error(), "name filter was not honoured") {
		t.Errorf("error does not explain the unhonoured filter: %v", err)
	}

	q := query()
	// The literal must be escaped: the embedded quote appears as \' and the clause is
	// still a single balanced literal.
	if !strings.Contains(q, `campaign.name = 'x\' OR campaign.id > \'0'`) {
		t.Errorf("name was not escaped into a single literal: %s", q)
	}
	// And the injected fragment must not have become live syntax.
	if strings.Contains(q, `campaign.name = 'x' OR`) {
		t.Errorf("query was injected: %s", q)
	}
}

// TestGAQLStringLiteral covers the escape set directly, including the ordering trap:
// backslash must be doubled before quotes are escaped, or the backslash introduced by
// the quote escape is itself escaped and the quote is released.
func TestGAQLStringLiteral(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain", "hello", `'hello'`},
		{"quote", "it's", `'it\'s'`},
		{"backslash", `a\b`, `'a\\b'`},
		{"backslash then quote", `a\'b`, `'a\\\'b'`},
		{"pipe is not special", "LFX | x", `'LFX | x'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := gaqlStringLiteral(tc.in)
			if err != nil {
				t.Fatalf("gaqlStringLiteral(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("gaqlStringLiteral(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestGAQLStringLiteral_RejectsControlChars pins the decision to reject rather than
// escape: GAQL has no portable escape for these, and Google Ads forbids them in a
// campaign name, so such a name cannot match anything real.
// The last two cases are the ones unicode.IsControl does NOT catch: U+2028 and U+2029
// are categories Zl/Zp, so IsControl returns false for both even though they are line
// terminators. Deleting the explicit checks for them makes exactly these two fail.
func TestGAQLStringLiteral_RejectsControlChars(t *testing.T) {
	for _, in := range []string{"a\x00b", "a\nb", "a\tb", "a\u2028b", "a\u2029b"} {
		if _, err := gaqlStringLiteral(in); err == nil {
			t.Errorf("gaqlStringLiteral(%q) must reject a control character", in)
		}
	}
}

// TestGAQLStringLiteral_AllowsFormatCharacters is the other side of that decision.
// Category Cf (zero-width joiner, variation selector) appears inside ordinary emoji
// sequences and terminates nothing in GAQL, so rejecting it would fail a lookup for a
// campaign that genuinely exists — a false absence, which licenses a duplicate create.
func TestGAQLStringLiteral_AllowsFormatCharacters(t *testing.T) {
	const withZWJ = "team \u200d rocket"
	got, err := gaqlStringLiteral(withZWJ)
	if err != nil {
		t.Fatalf("a zero-width joiner must not be rejected: %v", err)
	}
	if got != "'"+withZWJ+"'" {
		t.Fatalf("gaqlStringLiteral(%q) = %s, want it quoted unchanged", withZWJ, got)
	}
}

// TestFindCampaignByName_MatchWithoutIDFailsClosed: the server says a campaign with
// this name exists but we cannot name it. Reporting that as an absence would license
// a duplicate create.
func TestFindCampaignByName_MatchWithoutIDFailsClosed(t *testing.T) {
	srv, _ := newLookupServer(t, []json.RawMessage{
		json.RawMessage(`{"campaign":{"resourceName":"","id":"","name":"idless","status":"ENABLED"}}`),
	})
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), "idless")
	if err == nil {
		t.Fatalf("a matched row with no id must fail closed, got id %q", id)
	}
	if !strings.Contains(err.Error(), "rather than report it absent") {
		t.Errorf("error does not explain the fail-closed reason: %v", err)
	}
}

// TestFindCampaignByName_IDFallsBackToResourceName covers a row whose int64 id field is
// absent but whose resourceName carries it.
func TestFindCampaignByName_IDFallsBackToResourceName(t *testing.T) {
	srv, _ := newLookupServer(t, []json.RawMessage{
		json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/4242","name":"no id field","status":"ENABLED"}}`),
	})
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), "no id field")
	if err != nil {
		t.Fatalf("FindCampaignByName: %v", err)
	}
	if id != "4242" {
		t.Fatalf("id = %q, want 4242 from the resource name", id)
	}
}

// TestFindCampaignByName_RemovedRowIsSkipped: a tombstone can never serve or be
// re-enabled, so it must not be adopted or read as an idempotent create hit — even if
// the server ignores the WHERE clause.
func TestFindCampaignByName_RemovedRowIsSkipped(t *testing.T) {
	srv, _ := newLookupServer(t, []json.RawMessage{
		lookupRow("300", "tombstoned", StatusRemoved),
	})
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), "tombstoned")
	if err != nil {
		t.Fatalf("FindCampaignByName: %v", err)
	}
	if id != "" {
		t.Fatalf("a REMOVED campaign must not match, got %q", id)
	}
}

// TestFindCampaignByName_MalformedRowIsNotAnAbsence: an undecodable 2xx row must fail
// the lookup, not read as "no match" and license a duplicate create.
func TestFindCampaignByName_MalformedRowIsNotAnAbsence(t *testing.T) {
	srv, _ := newLookupServer(t, []json.RawMessage{json.RawMessage(`{"campaign":"not an object"}`)})
	client := newAccountsTestClient(t, srv)

	if id, err := client.FindCampaignByName(context.Background(), "whatever"); err == nil {
		t.Fatalf("a malformed row must fail the lookup, got id %q", id)
	}
}

// TestFindCampaignByName_RejectsUnusableNames covers the input guards. Each of these is
// unmatchable against a real campaign, so failing early keeps an unbounded or
// unrepresentable string out of the query builder.
func TestFindCampaignByName_RejectsUnusableNames(t *testing.T) {
	srv, _ := newLookupServer(t, nil)
	client := newAccountsTestClient(t, srv)

	for _, tc := range []struct{ name, in string }{
		{"empty", ""},
		{"whitespace only", "   "},
		{"over the character limit", strings.Repeat("a", maxCampaignNameRunes+1)},
		{"control character", "bad\x00name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if id, err := client.FindCampaignByName(context.Background(), tc.in); err == nil {
				t.Fatalf("expected rejection, got id %q", id)
			}
		})
	}
}

// TestFindCampaignByName_TrimsToMatchTheCreatePath: composeName runs its parts through
// sanitizeNamePart, which trims, so the stored name never carries surrounding space.
// Looking up the untrimmed form would miss the campaign the create path just made.
func TestFindCampaignByName_TrimsToMatchTheCreatePath(t *testing.T) {
	srv, query := newLookupServer(t, []json.RawMessage{lookupRow("88", "trimmed", StatusEnabled)})
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), "  trimmed  ")
	if err != nil {
		t.Fatalf("FindCampaignByName: %v", err)
	}
	if id != "88" {
		t.Fatalf("id = %q, want 88", id)
	}
	if !strings.Contains(query(), "campaign.name = 'trimmed'") {
		t.Errorf("name was not trimmed before querying: %s", query())
	}
}

// TestFindCampaignByName_UnrecognisedStatusFailsClosed: REMOVED is the only status that
// may be silently skipped. Google can answer UNSPECIFIED or UNKNOWN, and an omitted
// field decodes to "" — accepting any of those as live would return the id of a
// campaign whose serving state was never established.
func TestFindCampaignByName_UnrecognisedStatusFailsClosed(t *testing.T) {
	for _, status := range []string{"UNSPECIFIED", "UNKNOWN", ""} {
		t.Run("status "+status, func(t *testing.T) {
			srv, _ := newLookupServer(t, []json.RawMessage{lookupRow("42", "odd status", status)})
			client := newAccountsTestClient(t, srv)

			id, err := client.FindCampaignByName(context.Background(), "odd status")
			if err == nil {
				t.Fatalf("status %q must fail closed, got id %q", status, id)
			}
			if !strings.Contains(err.Error(), "unrecognised status") {
				t.Errorf("error does not name the unrecognised status: %v", err)
			}
		})
	}
}

// TestFindCampaignByName_ResourceNameFallbackValidatesShape covers the identity guard on
// the id fallback. Taking the trailing path segment (what the package's resourceID does)
// would read "garbage/4242" as campaign 4242, and would accept a campaign belonging to a
// DIFFERENT customer — binding a brief to an id never verified as ours.
//
// Every case here has an absent id field, so the fallback is the only source of identity;
// deleting the shape check makes each of them return an id instead of failing.
func TestFindCampaignByName_ResourceNameFallbackValidatesShape(t *testing.T) {
	for _, tc := range []struct{ name, resourceName string }{
		{"not a resource path", "garbage/4242"},
		{"wrong collection", "customers/1234567890/adGroups/4242"},
		{"another customer's campaign", "customers/9999999999/campaigns/4242"},
		{"non-numeric id segment", "customers/1234567890/campaigns/not-a-number"},
		// The extra segment is DIGITS on purpose: with the shape check reverted to a
		// trailing-segment read, "extra" would be caught by the numeric guard downstream
		// and the case would pass vacuously. "1" gets through it, so this case binds the
		// segment-count check specifically.
		{"trailing segments", "customers/1234567890/campaigns/4242/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newLookupServer(t, []json.RawMessage{
				json.RawMessage(`{"campaign":{"resourceName":` + jsonQuote(tc.resourceName) +
					`,"name":"shape test","status":"ENABLED"}}`),
			})
			client := newAccountsTestClient(t, srv)

			id, err := client.FindCampaignByName(context.Background(), "shape test")
			if err == nil {
				t.Fatalf("resourceName %q must not yield an id, got %q", tc.resourceName, id)
			}
			if !strings.Contains(err.Error(), "rather than report it absent") {
				t.Errorf("error does not explain the fail-closed reason: %v", err)
			}
		})
	}
}

// TestFindCampaignByName_NonNumericIDIsRejected drives the guard on the id field itself
// (as opposed to the resource-name fallback above). The id is interpolated into resource
// paths by every later call, so a non-numeric one must never leave this function.
func TestFindCampaignByName_NonNumericIDIsRejected(t *testing.T) {
	srv, _ := newLookupServer(t, []json.RawMessage{
		json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/x","id":"not-a-number","name":"bad id","status":"ENABLED"}}`),
	})
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), "bad id")
	if err == nil {
		t.Fatalf("a non-numeric id must be rejected, got %q", id)
	}
	if !strings.Contains(err.Error(), "non-numeric id") {
		t.Errorf("error does not name the non-numeric id: %v", err)
	}
}

// TestFindCampaignByName_RejectsInvalidAccount pins the CONTRACT, not one line: a client
// whose customer id is malformed cannot address the account the answer would be about, so
// the lookup must error rather than report the clean absence that licenses a create.
//
// Two guards enforce it — the explicit validateAccountIDs call at the top of the lookup
// (matching every other exported method here) and the same call inside doRequest. Deleting
// either alone leaves the other covering the outcome, so this test does not bind a single
// statement; deleting BOTH fails it. The explicit one is kept for the convention and to
// fail before a query is built, which is what the second assertion below records.
func TestFindCampaignByName_RejectsInvalidAccount(t *testing.T) {
	srv, query := newLookupServer(t, []json.RawMessage{lookupRow("1", "anything", StatusEnabled)})
	client := NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "token", RefreshToken: "refresh"},
		AccountConfig{CustomerID: "123-456-7890"}, // dashes: not digits-only
		WithBaseURL(srv.URL),
		WithTokenURL(srv.URL+"/token"),
		WithAPIVersion("v23"),
	)

	if id, err := client.FindCampaignByName(context.Background(), "anything"); err == nil {
		t.Fatalf("an invalid customer id must fail the lookup, got %q", id)
	}
	// And it must fail BEFORE the network call, so a malformed account cannot spend a
	// request or surface a row.
	if q := query(); q != "" {
		t.Errorf("lookup queried the API despite an invalid customer id: %s", q)
	}
}

// TestFindCampaignByName_SearchFailurePropagates: a transport or API failure is not an
// absence. It must reach the caller wrapped, so nothing reads it as licence to create.
func TestFindCampaignByName_SearchFailurePropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"backend error"}}`))
	}))
	t.Cleanup(srv.Close)
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), "anything")
	if err == nil {
		t.Fatalf("a failing search must not report an absence, got id %q and nil error", id)
	}
	if id != "" {
		t.Errorf("a failed lookup must not return an id, got %q", id)
	}
	if !strings.Contains(err.Error(), "google-ads campaign lookup") {
		t.Errorf("error is not wrapped with the lookup context: %v", err)
	}
}
