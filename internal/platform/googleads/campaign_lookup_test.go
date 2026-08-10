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
		// Never `{"results":null}`: encoding a nil slice would emit exactly the shape the
		// client now rejects, and the absence test would then assert on a page Google does
		// not send — passing while the real empty page (`[]` or an omitted key) went
		// unexercised.
		if rows == nil {
			rows = []json.RawMessage{}
		}
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

// TestFindCampaignByName_DuplicateNamesAreAmbiguous motivates erroring instead of taking
// the first hit. Two live campaigns sharing a name is ANOMALOUS, not routine — v23 rejects a
// mutate whose name another ENABLED/PAUSED campaign already holds (DUPLICATE_CAMPAIGN_NAME),
// so this response should not be possible.
//
// Which is precisely why the branch matters. A response that contradicts the API's own
// constraint is a response nothing about is trustworthy, and picking one of its rows would
// bind a brief to the wrong PAID campaign on no evidence at all. Fail-closing is least costly
// exactly where guessing is least defensible.
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

// TestFindCampaignByName_DuplicateRowsForOneCampaignAreNotAmbiguous is the other half of
// the rule: one campaign on two rows is ONE campaign, and calling it ambiguous would
// block a legitimate adoption.
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

// TestFindCampaignByName_QuoteInNameCannotInjectQuery is why gaqlStringLiteral exists:
// unescaped, the quote closes the literal and the rest becomes query syntax, turning an
// exact-match lookup into a match on the whole account.
func TestFindCampaignByName_QuoteInNameCannotInjectQuery(t *testing.T) {
	const evil = `x' OR campaign.id > '0`

	// The server returns a campaign NOT named `evil` — what an injected query surfaces.
	// The assertion is an ERROR, not a zero match: discarding the row would collapse an
	// injected query into ("", nil), the licence-to-create absence, so a test accepting
	// a clean absence here would be asserting the unsafe outcome.
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

// TestGAQLStringLiteral covers the escape set directly, including the ordering trap: the
// backslash must be doubled before quotes are escaped, or the backslash the quote escape
// introduces is itself escaped and the quote is released.
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

// TestGAQLStringLiteral_RejectsExactlyWhatGoogleForbids pins the exact width of the rejected
// set in both directions. Only NUL, LF and CR are prohibited in Campaign.name, so only those
// three cannot belong to a real campaign and rejecting them costs no reachable lookup.
// Rejecting anything MORE is the more important error: adoption targets never passed through
// sanitizeNamePart, so campaigns named with a TAB (Cc, but legal — what a blanket
// unicode.IsControl rule gets wrong), U+2028/U+2029 (Zl/Zp) or a zero-width joiner (Cf,
// ordinary in emoji sequences) really exist, and refusing one answers "no such campaign"
// about a campaign sitting right there — the false absence that licenses a duplicate PAID
// campaign.
func TestGAQLStringLiteral_RejectsExactlyWhatGoogleForbids(t *testing.T) {
	for _, in := range []string{"a\x00b", "a\nb", "a\rb"} {
		if _, err := gaqlStringLiteral(in); err == nil {
			t.Errorf("gaqlStringLiteral(%q) must reject a character Google Ads forbids", in)
		}
	}
	for _, in := range []string{"a\tb", "a\u2028b", "a\u2029b", "team \u200d rocket", "plain name"} {
		got, err := gaqlStringLiteral(in)
		if err != nil {
			t.Errorf("gaqlStringLiteral(%q) must not reject a name Google Ads accepts: %v", in, err)
			continue
		}
		if got != "'"+in+"'" {
			t.Errorf("gaqlStringLiteral(%q) = %s, want it quoted unchanged", in, got)
		}
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

// TestFindCampaignByName_RemovedRowWithUnusableIdentityIsNotAnAbsence pins the one
// CONTRACT CHANGE the campaignRowIdentity extraction makes to this method, so it is a
// decision on the record rather than a side effect.
//
// Before the extraction, status was judged first: a REMOVED row was skipped whatever else
// it said, so a tombstone naming another customer's campaign returned ("", nil) — the
// licence-to-create value. Identity is now established first, and this errors.
//
// That is the correct direction. "A tombstone is unadoptable however it arrived" is only
// true once we know WHICH campaign it is a tombstone for; a row we could not identify is
// not evidence that the campaign we asked about is absent, and the caller acts on that
// absence by creating a duplicate PAID campaign. The narrower half still holds —
// TestFindCampaignByName_RemovedRowIsSkipped keeps a well-formed tombstone a clean
// non-match, which is the ordinary case.
func TestFindCampaignByName_RemovedRowWithUnusableIdentityIsNotAnAbsence(t *testing.T) {
	srv, _ := newLookupServer(t, []json.RawMessage{
		json.RawMessage(`{"campaign":{"id":"300","name":"tombstoned","status":"REMOVED",` +
			`"resourceName":"customers/999/campaigns/42"}}`),
	})
	client := newAccountsTestClient(t, srv)

	got, err := client.FindCampaignByName(context.Background(), "tombstoned")
	if err == nil {
		t.Fatalf("a tombstone scoped to another customer must not read as an absence, got %q", got)
	}
	if !strings.Contains(err.Error(), "resource name") {
		t.Errorf("error does not say why the row was refused: %v", err)
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
		{"invalid utf-8", "bad\xffname"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if id, err := client.FindCampaignByName(context.Background(), tc.in); err == nil {
				t.Fatalf("expected rejection, got id %q", id)
			}
		})
	}
}

// TestFindCampaignByName_InvalidUTF8IsNotQueried pins the one rejection in gaqlStringLiteral
// that is NOT about what Google Ads forbids.
//
// A malformed byte survives every guard ahead of it: the length check counts it as one rune,
// and ranging the string yields utf8.RuneError, which is none of NUL, LF or CR. It does not
// survive encoding/json, which substitutes U+FFFD for it and returns NO error — so without
// this guard the query on the wire asks about a name the caller never passed. Its miss then
// comes back as ("", nil), the licence to create, and the row-level name re-check cannot
// catch it because a query that matches nothing returns no row to re-check.
//
// The assertion that no query was sent is the point: an error alone would also be produced
// by a guard placed after the request, which would have already asked the wrong question.
func TestFindCampaignByName_InvalidUTF8IsNotQueried(t *testing.T) {
	// The server holds a campaign under the SUBSTITUTED name. If the malformed byte ever
	// reached the wire, this row would match and the lookup would return an id for a
	// campaign whose name is not the one requested.
	srv, gotQuery := newLookupServer(t, []json.RawMessage{
		lookupRow("55", "bad\ufffdname", StatusEnabled),
	})
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), "bad\xffname")
	if err == nil {
		t.Fatalf("invalid UTF-8 must be rejected, got id %q", id)
	}
	if q := gotQuery(); q != "" {
		t.Errorf("a name that json.Marshal would rewrite was sent to the server: %s", q)
	}
}

// TestFindCampaignByName_QueriesTheNameVerbatim pins the exact-match contract against
// the convenience of trimming.
//
// Trimming is a no-op for the create path (composeName's output is already trimmed), so
// it only ever changes ADOPTION, where the caller names a campaign this service did not
// create. There, silently trimming would answer a request for "  spaced  " with the
// campaign named "spaced" — a different campaign than the one asked for, and if both
// existed the ambiguity would be hidden rather than reported.
func TestFindCampaignByName_QueriesTheNameVerbatim(t *testing.T) {
	// The server holds only the TRIMMED campaign. Querying verbatim must therefore not
	// match it: the client-side re-check rejects the response as unhonoured rather than
	// binding a brief to a campaign with a different name.
	srv, query := newLookupServer(t, []json.RawMessage{lookupRow("88", "spaced", StatusEnabled)})
	client := newAccountsTestClient(t, srv)

	id, err := client.FindCampaignByName(context.Background(), "  spaced  ")
	if err == nil {
		t.Fatalf("a differently-named campaign must not satisfy an exact-match lookup, got id %q", id)
	}
	if !strings.Contains(query(), "campaign.name = '  spaced  '") {
		t.Errorf("name was not sent verbatim: %s", query())
	}
}

// TestFindCampaignByName_RejectsWhitespaceOnlyName: TrimSpace is used to DETECT an
// empty name, not to rewrite the query. A whitespace-only name identifies nothing, and
// must fail rather than query for it.
func TestFindCampaignByName_RejectsWhitespaceOnlyName(t *testing.T) {
	srv, query := newLookupServer(t, []json.RawMessage{lookupRow("1", " ", StatusEnabled)})
	client := newAccountsTestClient(t, srv)

	if id, err := client.FindCampaignByName(context.Background(), "   "); err == nil {
		t.Fatalf("a whitespace-only name must be rejected, got %q", id)
	}
	if q := query(); q != "" {
		t.Errorf("a whitespace-only name must not reach the API: %s", q)
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
			if !strings.Contains(err.Error(), "malformed or scoped to another customer") {
				t.Errorf("error does not explain the fail-closed reason: %v", err)
			}
		})
	}
}

// TestFindCampaignByName_NonCanonicalIDIsRejected drives the guard on the id field itself
// (as opposed to the resource-name fallback above), and pins that "all digits" is NOT the
// test. Google exposes campaign ids as int64, so a digits-only value can still name no
// campaign: "0" is not an id, a value past math.MaxInt64 cannot be one, and "007" is the
// same campaign as "7" to the server but a different string to the identity comparison —
// the spelling difference is precisely what makes it dangerous rather than merely untidy.
// A padded value is a malformed row, and trimming it would answer with campaign 4242 for
// a response that never came from this API.
func TestFindCampaignByName_NonCanonicalIDIsRejected(t *testing.T) {
	// The resource name is OMITTED in each case so the row's only identity evidence is
	// the id field; with one present the row would be rejected by the resource-name
	// guard first and these cases would pass without reaching the canonical check.
	for _, id := range []string{
		"not-a-number",
		"0",                   // a valid int64, not a campaign
		"-1",                  // digits-only fails to match this one; ParseInt does not
		"9223372036854775808", // math.MaxInt64 + 1
		"007",                 // non-canonical spelling of 7
		" 4242 ",              // padded — malformed, not campaign 4242
		"4242\n",              // trailing newline, same reasoning
		"+4242",               // signed canonical form is still not the canonical form
	} {
		srv, _ := newLookupServer(t, []json.RawMessage{
			json.RawMessage(`{"campaign":{"id":` + jsonQuote(id) + `,"name":"bad id","status":"ENABLED"}}`),
		})
		client := newAccountsTestClient(t, srv)

		got, err := client.FindCampaignByName(context.Background(), "bad id")
		if err == nil {
			t.Fatalf("id %q must be rejected, got %q", id, got)
		}
		if !strings.Contains(err.Error(), "canonical spelling") {
			t.Errorf("id %q: error does not say why it was refused: %v", id, err)
		}
	}
}

// TestFindCampaignByName_PresentButUnusableResourceNameIsRejected separates "the field is
// absent" from "the field is present and garbage". Only the first may fall back to the id.
//
// The whitespace-only case is the one that matters: an earlier revision tested the TRIMMED
// resource name for emptiness, which folded "   " into "absent" and adopted the row on its id
// alone — a row whose two identity fields do not agree, accepted as though only one had been
// selected.
func TestFindCampaignByName_PresentButUnusableResourceNameIsRejected(t *testing.T) {
	for _, rn := range []string{
		"   ",                             // whitespace-only: present, and not a resource name
		"garbage/4242",                    // the shape a lenient trailing-segment parser accepts
		"customers/999/campaigns/42",      // another customer's campaign
		"customers/1234567890/campaigns/", // this client's customer, no campaign segment
	} {
		srv, _ := newLookupServer(t, []json.RawMessage{
			json.RawMessage(`{"campaign":{"id":"4242","name":"bad rn","status":"ENABLED","resourceName":` + jsonQuote(rn) + `}}`),
		})
		client := newAccountsTestClient(t, srv)

		got, err := client.FindCampaignByName(context.Background(), "bad rn")
		if err == nil {
			t.Fatalf("resource name %q must be rejected, got %q", rn, got)
		}
		if !strings.Contains(err.Error(), "resource name") {
			t.Errorf("resource name %q: error does not say why it was refused: %v", rn, err)
		}
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

// TestFindCampaignByName_IdentityFieldsMustAgree covers a row whose two selected identity
// fields contradict each other. Validating the resource name only as a FALLBACK for a
// missing id made the check reachable exactly when the row was least suspicious — yet a
// row carrying both fields is the one adoption acts on.
func TestFindCampaignByName_IdentityFieldsMustAgree(t *testing.T) {
	for _, tc := range []struct{ name, resourceName, want string }{
		{"another customer", "customers/9999999999/campaigns/4242", "malformed or scoped to another customer"},
		{"malformed", "garbage/4242", "malformed or scoped to another customer"},
		{"names a different campaign", "customers/1234567890/campaigns/777", "identity fields disagree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newLookupServer(t, []json.RawMessage{
				json.RawMessage(`{"campaign":{"resourceName":` + jsonQuote(tc.resourceName) +
					`,"id":"4242","name":"disagree","status":"ENABLED"}}`),
			})
			id, err := newAccountsTestClient(t, srv).FindCampaignByName(context.Background(), "disagree")
			if err == nil {
				t.Fatalf("resourceName %q beside id 4242 must not yield an id, got %q", tc.resourceName, id)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestFindCampaignByName_NullIsNeverAnAbsence pins the null guard at all THREE levels. A
// top-level null, an explicit `"results":null` one level in, and an explicit
// `"nextPageToken":null` each unmarshal WITHOUT error into a zero-valued field — byte for
// byte what a genuine empty page or a genuine final page produces. Before the guards that
// reads as the clean ("", nil) absence: licence to create a duplicate paid campaign.
//
// The third case is the subtle one, and it fails DIFFERENTLY from the first two. `results`
// is legitimately empty there; it is the CURSOR that is null, and a null cursor decodes to
// "" — the value this loop reads as "that was the last page". So the response is not read as
// an empty result set, it is read as a COMPLETE one, and pagination stops at page 1. A
// campaign sitting on page 2 is then reported absent. Same false absence, reached by
// truncation rather than by an empty page.
//
// TestFindCampaignByName_AbsentIsNotAnError is the companion — `[]`, the shape Google really
// sends, must still license a create.
func TestFindCampaignByName_NullIsNeverAnAbsence(t *testing.T) {
	for _, body := range []string{"null", `{"results":null,"nextPageToken":""}`, `{"results":[],"nextPageToken":null}`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "token") {
					_, _ = w.Write([]byte(`{"access_token":"t","expires_in":3600}`))
					return
				}
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			id, err := newAccountsTestClient(t, srv).FindCampaignByName(context.Background(), "anything")
			if err == nil {
				t.Fatalf("a null result set must not report a clean absence, got %q", id)
			}
			if id != "" {
				t.Errorf("id = %q, want empty alongside the error", id)
			}
		})
	}
}

// TestFindCampaignByName_MalformedUTF8RowCannotEchoTheRequestedName covers the one case the
// echo-check below it cannot: a requested name that already contains U+FFFD.
//
// For every other name the echo-check IS the guard — encoding/json substitutes U+FFFD for a
// malformed byte without erroring, the decoded name then differs from the one asked for, and
// the lookup fails loudly as an unhonoured filter. U+FFFD is a legal campaign name character,
// though, so a caller can ask for one; the substitution then produces exactly the requested
// string, the comparison passes on a value nobody ever saw intact, and an id is returned from
// a response nothing verified. An adoption binds paid spend to that id.
//
// Both routes in are covered, because byte validity catches only the first: a raw malformed
// byte, and an unpaired surrogate escape whose bytes are pure ASCII and which only becomes
// U+FFFD when encoding/json resolves it.
func TestFindCampaignByName_MalformedUTF8RowCannotEchoTheRequestedName(t *testing.T) {
	const requested = "bad�name"
	for _, tc := range []struct{ name, rawName string }{
		// Hand-built, not via jsonQuote: quoting would encode the bad byte as a valid
		// � escape, which is the substitution under test rather than its cause.
		{"a malformed byte", "bad\xffname"},
		{"an unpaired surrogate escape", `bad\uD800name`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newLookupServer(t, []json.RawMessage{
				json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/4242",` +
					`"id":"4242","name":"` + tc.rawName + `","status":"` + StatusEnabled + `"}}`),
			})
			id, err := newAccountsTestClient(t, srv).FindCampaignByName(context.Background(), requested)
			if err == nil {
				t.Fatalf("FindCampaignByName = %q, want an error: the row's name decodes to the "+
					"requested string only because it was rewritten, so the match is not evidence", id)
			}
			if id != "" {
				t.Errorf("id = %q alongside the error; a caller ignoring err would adopt an unverified campaign", id)
			}
			if !strings.Contains(err.Error(), "UTF-8") && !strings.Contains(err.Error(), "surrogate") {
				t.Errorf("error = %v, want it to name the encoding fault", err)
			}
		})
	}

	// The narrowing half: U+FFFD is legal in a campaign name, so a row that genuinely
	// carries one — encoded properly — must still match rather than trip the guard.
	srv, _ := newLookupServer(t, []json.RawMessage{lookupRow("4242", requested, StatusEnabled)})
	id, err := newAccountsTestClient(t, srv).FindCampaignByName(context.Background(), requested)
	if err != nil {
		t.Fatalf("FindCampaignByName(%q) = %v; a name that genuinely contains U+FFFD is legal and must adopt", requested, err)
	}
	if id != "4242" {
		t.Errorf("id = %q, want 4242", id)
	}
}
