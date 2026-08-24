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
)

// settingsServer serves the OAuth token endpoint and one googleAds:search response built
// from rows, recording EVERY request path+method it sees.
//
// Recording every request, not just the search, is what makes the "issues no mutating
// call" assertion real rather than assumed: a mutate would arrive at this same handler on
// a :mutate path, and only a record of all traffic can prove none did.
func settingsServer(t *testing.T, rows []json.RawMessage) (*httptest.Server, func() string, func() []string) {
	t.Helper()
	var (
		mu       sync.Mutex
		gotQuery string
		seen     []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if writeAccountsToken(w, r) {
			return
		}
		var req searchRequest
		// t.Error, not t.Fatal: FailNow is only legal on the test goroutine.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding search request: %v", err)
		}
		mu.Lock()
		gotQuery = req.Query
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if rows == nil {
			rows = []json.RawMessage{}
		}
		_ = json.NewEncoder(w).Encode(searchResponse{Results: rows})
	}))
	t.Cleanup(srv.Close)
	return srv,
		func() string {
			mu.Lock()
			defer mu.Unlock()
			return gotQuery
		},
		func() []string {
			mu.Lock()
			defer mu.Unlock()
			out := make([]string, len(seen))
			copy(out, seen)
			return out
		}
}

// settingsRow builds a full settings row in the shape gaqlSearch hands back. Numeric
// fields are JSON STRINGS because that is how Google Ads REST renders int64.
func settingsRow(id, name, status, amountMicros, period string) json.RawMessage {
	return json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/` + id +
		`","id":"` + id + `","name":` + jsonQuote(name) + `,"status":"` + status +
		`","advertisingChannelType":"SEARCH","biddingStrategyType":"MANUAL_CPC",` +
		`"startDateTime":"2026-08-01 00:00:00","endDateTime":"2026-08-31 23:59:59"},` +
		`"campaignBudget":{"amountMicros":"` + amountMicros + `","period":"` + period +
		`","deliveryMethod":"STANDARD","explicitlyShared":false}}`)
}

// TestGetCampaignSettings_ReadsUpstreamBudget is the binding test at the client layer:
// the value the PLATFORM reported must reach the result.
//
// It asserts the specific upstream number, not merely that a budget came back. An
// implementation that echoed a caller-supplied value and never read the response would
// satisfy "a budget is present" and fail this.
func TestGetCampaignSettings_ReadsUpstreamBudget(t *testing.T) {
	srv, query, _ := settingsServer(t, []json.RawMessage{
		settingsRow("555", "LFX | Campaign | proj | evt", StatusEnabled, "750000000", "DAILY"),
	})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got.BudgetAmountMicros == nil {
		t.Fatal("BudgetAmountMicros is nil; the upstream budget did not reach the result")
	}
	if *got.BudgetAmountMicros != 750000000 {
		t.Errorf("BudgetAmountMicros = %d, want 750000000 (the value the platform reported)", *got.BudgetAmountMicros)
	}
	if got.BudgetPeriod == nil || *got.BudgetPeriod != "DAILY" {
		t.Errorf("BudgetPeriod = %v, want DAILY", got.BudgetPeriod)
	}
	if got.Name == nil || *got.Name != "LFX | Campaign | proj | evt" {
		t.Errorf("Name = %v, want the upstream name", got.Name)
	}
	if got.Status == nil || *got.Status != StatusEnabled {
		t.Errorf("Status = %v, want %s", got.Status, StatusEnabled)
	}

	// The v23 field names must be the ones actually sent. start_date/end_date were REPLACED
	// by start_date_time/end_date_time in v23 and are rejected as unrecognized, so a query
	// built from the request-side vocabulary fails outright against a real account.
	q := query()
	for _, want := range []string{
		"campaign_budget.amount_micros",
		"campaign_budget.total_amount_micros",
		"campaign_budget.period",
		"campaign.start_date_time",
		"campaign.end_date_time",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query is missing %s: %s", want, q)
		}
	}
	// The pre-v23 spellings must NOT appear. Checked with a trailing delimiter so
	// `campaign.start_date_time` does not satisfy a naive search for `campaign.start_date`.
	for _, banned := range []string{"campaign.start_date,", "campaign.end_date,", "campaign.start_date ", "campaign.end_date "} {
		if strings.Contains(q, banned) {
			t.Errorf("query uses the pre-v23 date field %q, which v23 rejects as unrecognized: %s", banned, q)
		}
	}
	// No metrics/segments field may be selected: either would segment the result and make
	// the single-row guard fire on healthy campaigns.
	if strings.Contains(q, "metrics.") || strings.Contains(q, "segments.") {
		t.Errorf("settings query must select no metrics/segments field (it would segment the result): %s", q)
	}
}

// TestGetCampaignSettings_IssuesNoMutatingCall proves the capability cannot spend money.
//
// The assertion is over EVERY request the client made, not over the one the test expected
// to see: a mutate introduced anywhere on this path — a budget "fix", a status write —
// would show up as a :mutate path here.
func TestGetCampaignSettings_IssuesNoMutatingCall(t *testing.T) {
	srv, _, requests := settingsServer(t, []json.RawMessage{
		settingsRow("555", "campaign", StatusEnabled, "500000000", "DAILY"),
	})
	client := newAccountsTestClient(t, srv)

	if _, err := client.GetCampaignSettings(context.Background(), "555"); err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}

	got := requests()
	if len(got) == 0 {
		t.Fatal("no requests recorded; the test cannot prove anything about what was sent")
	}
	for _, r := range got {
		if strings.Contains(r, ":mutate") {
			t.Errorf("settings readback issued a MUTATING call (%s); this capability must never write upstream. all requests: %v", r, got)
		}
	}
	// Exactly one search, so the read cannot be quietly doing more work than it claims.
	searches := 0
	for _, r := range got {
		if strings.Contains(r, "googleAds:search") {
			searches++
		}
	}
	if searches != 1 {
		t.Errorf("expected exactly 1 googleAds:search, got %d: %v", searches, got)
	}
}

// TestGetCampaignSettings_UnreadableFieldIsAbsentNotZero is the absent-vs-equal guarantee
// at the client layer: a budget Google did not report must be nil, never 0.
//
// A zero would be indistinguishable from a campaign with a genuinely zero budget, and
// downstream it would be COMPARED — manufacturing either a false match or a false
// divergence out of a field nobody read.
func TestGetCampaignSettings_UnreadableFieldIsAbsentNotZero(t *testing.T) {
	// A campaign row with NO campaignBudget object at all — the shape Google returns when
	// the budget resource is not readable for this row.
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"c","status":"ENABLED"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got.BudgetAmountMicros != nil {
		t.Errorf("BudgetAmountMicros = %d, want nil: an unread budget must be ABSENT, never 0", *got.BudgetAmountMicros)
	}
	if got.BudgetPeriod != nil {
		t.Errorf("BudgetPeriod = %q, want nil", *got.BudgetPeriod)
	}
	if got.BudgetExplicitlyShared != nil {
		t.Errorf("BudgetExplicitlyShared = %v, want nil", *got.BudgetExplicitlyShared)
	}
	// The campaign's own fields were readable and must still be reported: partial is the
	// normal case, and dropping the readable half would lose real information.
	if got.Status == nil || *got.Status != StatusEnabled {
		t.Errorf("Status = %v, want ENABLED (the campaign's own fields were readable)", got.Status)
	}
}

// TestGetCampaignSettings_CustomPeriodUsesTotalAmount pins the mutually-exclusive budget
// pair. Reading only amount_micros would report a CUSTOM_PERIOD campaign as having NO
// budget — a false absence that silently suppresses a real divergence.
func TestGetCampaignSettings_CustomPeriodUsesTotalAmount(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"c","status":"ENABLED"},` +
		`"campaignBudget":{"totalAmountMicros":"9000000000","period":"CUSTOM_PERIOD"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got.BudgetTotalAmountMicros == nil {
		t.Fatal("BudgetTotalAmountMicros is nil; a CUSTOM_PERIOD budget was not read")
	}
	if *got.BudgetTotalAmountMicros != 9000000000 {
		t.Errorf("BudgetTotalAmountMicros = %d, want 9000000000", *got.BudgetTotalAmountMicros)
	}
	if got.BudgetAmountMicros != nil {
		t.Errorf("BudgetAmountMicros = %d, want nil: the two budget fields are mutually exclusive", *got.BudgetAmountMicros)
	}
}

// TestGetCampaignSettings_AbsentCampaignIsNotAnError pins the clean-absence contract: the
// platform answered and holds no such campaign, which is (nil, nil) and NOT an error.
func TestGetCampaignSettings_AbsentCampaignIsNotAnError(t *testing.T) {
	srv, _, _ := settingsServer(t, nil)
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err != nil {
		t.Fatalf("an absent campaign must not be an error, got: %v", err)
	}
	if got != nil {
		t.Fatalf("settings = %+v, want nil for an absent campaign", got)
	}
}

// TestGetCampaignSettings_RemovedCampaignIsReported is the deliberate difference from
// GetCampaign, which filters REMOVED server-side.
//
// "The campaign you are tracking was removed upstream" is the most actionable divergence
// this read can surface. Excluding it would report the campaign as absent and hide the
// finding behind a 404.
func TestGetCampaignSettings_RemovedCampaignIsReported(t *testing.T) {
	srv, query, _ := settingsServer(t, []json.RawMessage{
		settingsRow("555", "c", StatusRemoved, "500000000", "DAILY"),
	})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got == nil {
		t.Fatal("a REMOVED campaign must be REPORTED, not treated as absent")
	}
	if got.Status == nil || *got.Status != StatusRemoved {
		t.Errorf("Status = %v, want %s", got.Status, StatusRemoved)
	}
	if strings.Contains(query(), "status != '"+StatusRemoved+"'") {
		t.Errorf("settings query must NOT exclude REMOVED; removal upstream is the finding: %s", query())
	}
	// The negative assertion above is not sufficient on its own, and that is the whole
	// point of this second one. GAQL excludes removed resources BY DEFAULT: a query with
	// no status predicate at all satisfies "does not contain status != REMOVED" while
	// still returning nothing for a removed campaign. The stub server answers with
	// whatever rows the test handed it and performs no filtering, so it cannot reproduce
	// that default — only the query text can be checked. REMOVED must be named
	// AFFIRMATIVELY for the row to come back from the real platform.
	if !strings.Contains(query(), "'"+StatusRemoved+"'") {
		t.Errorf("settings query must name %s affirmatively — GAQL drops removed rows unless "+
			"the status filter lists it, so a removed campaign would return zero rows and be "+
			"reported as a 404 absence: %s", StatusRemoved, query())
	}
	for _, want := range []string{StatusEnabled, StatusPaused} {
		if !strings.Contains(query(), "'"+want+"'") {
			t.Errorf("settings query must also name %s; opting in to REMOVED replaces the "+
				"implicit default filter entirely, so every live status has to be listed: %s", want, query())
		}
	}
}

// TestGetCampaignSettings_MalformedValuesSurviveDecodeVerbatim is the test the direct
// googleAdsDateOnly/googleAdsBudgetTypeFromPeriod tests cannot be: those call the helpers
// with a literal, so they never exercise the DECODE step that reaches them.
//
// blankToNil sits on that step. While it trimmed the surviving value, it handed those
// helpers something well-formed that the platform never sent: "2026-08-01 " arrived as
// "2026-08-01", which googleAdsDateOnly's strict parse rejects and passes through whole —
// byte-equal to a recorded YYYY-MM-DD, so it reported `match` for a value that never parsed.
// " DAILY " had the same shape. Normalisation upstream of validation manufactures the
// agreement the whole readback exists to make impossible, so the malformed bytes must reach
// the consumer unchanged.
func TestGetCampaignSettings_MalformedValuesSurviveDecodeVerbatim(t *testing.T) {
	srv, _, _ := settingsServer(t, []json.RawMessage{json.RawMessage(
		`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"c",` +
			`"status":"ENABLED","startDateTime":"2026-08-01 ","endDateTime":"2026-08-31 garbage"},` +
			`"campaignBudget":{"amountMicros":"500000000","period":" DAILY "}}`)})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got == nil {
		t.Fatal("settings = nil")
	}
	for _, tc := range []struct {
		field string
		got   *string
		want  string
	}{
		{"startDateTime", got.StartDateTime, "2026-08-01 "},
		{"endDateTime", got.EndDateTime, "2026-08-31 garbage"},
		{"period", got.BudgetPeriod, " DAILY "},
	} {
		if tc.got == nil {
			t.Errorf("%s = nil, want %q verbatim", tc.field, tc.want)
			continue
		}
		if *tc.got != tc.want {
			t.Errorf("%s = %q, want %q VERBATIM — normalising it here hands the comparison a "+
				"well-formed value the platform never sent, which can then compare EQUAL to a "+
				"recorded value and fabricate a match", tc.field, *tc.got, tc.want)
		}
	}
}

// TestGetCampaignSettings_WhitespaceOnlyIsStillAbsent: preserving the value verbatim must
// not resurrect the blank-is-a-divergence bug. There is no value under the whitespace to
// preserve, so it stays absence — withholding a comparison rather than inventing one.
func TestGetCampaignSettings_WhitespaceOnlyIsStillAbsent(t *testing.T) {
	srv, _, _ := settingsServer(t, []json.RawMessage{json.RawMessage(
		`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"c",` +
			`"status":"ENABLED","advertisingChannelType":"   ","biddingStrategyType":""},` +
			`"campaignBudget":{"amountMicros":"500000000","period":"DAILY","deliveryMethod":"  "}}`)})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got.AdvertisingChannelType != nil {
		t.Errorf("AdvertisingChannelType = %q, want nil for a whitespace-only field", *got.AdvertisingChannelType)
	}
	if got.BiddingStrategyType != nil {
		t.Errorf("BiddingStrategyType = %q, want nil for an empty field", *got.BiddingStrategyType)
	}
	if got.BudgetDeliveryMethod != nil {
		t.Errorf("BudgetDeliveryMethod = %q, want nil for a whitespace-only field", *got.BudgetDeliveryMethod)
	}
}

// TestGetCampaignSettings_UnhonouredIDFilterIsRefused: a row for a DIFFERENT campaign
// means the WHERE clause was not honoured, which invalidates the whole response. Reporting
// it would attribute another campaign's configuration to this one.
func TestGetCampaignSettings_UnhonouredIDFilterIsRefused(t *testing.T) {
	srv, _, _ := settingsServer(t, []json.RawMessage{
		settingsRow("999", "someone else's campaign", StatusEnabled, "1000000", "DAILY"),
	})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err == nil {
		t.Fatalf("expected an error for an unhonoured id filter, got settings: %+v", got)
	}
	if !strings.Contains(err.Error(), "not honoured") {
		t.Errorf("error should name the unhonoured filter, got: %v", err)
	}
}

// TestGetCampaignSettings_MultipleRowsRefused: the query filters on a unique id and selects
// nothing that segments, so >1 row means the response does not mean what this code reads it
// to mean. Silently taking rows[0] would build a divergence report on a response that
// described several campaigns.
func TestGetCampaignSettings_MultipleRowsRefused(t *testing.T) {
	srv, _, _ := settingsServer(t, []json.RawMessage{
		settingsRow("555", "c", StatusEnabled, "500000000", "DAILY"),
		settingsRow("555", "c", StatusEnabled, "600000000", "DAILY"),
	})
	client := newAccountsTestClient(t, srv)

	if _, err := client.GetCampaignSettings(context.Background(), "555"); err == nil {
		t.Fatal("expected an error when the response carries more than one row")
	}
}

// TestGetCampaignSettings_MalformedBudgetIsAnErrorNotAnAbsence: a budget that is PRESENT
// but unparseable must not collapse into the same nil that means "not reported". That
// would tell an operator no budget was reported when one was, hiding a decoding defect
// behind a benign-looking gap.
func TestGetCampaignSettings_MalformedBudgetIsAnErrorNotAnAbsence(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"c","status":"ENABLED"},` +
		`"campaignBudget":{"amountMicros":"not-a-number","period":"DAILY"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err == nil {
		t.Fatalf("expected an error for a malformed budget, got: %+v", got)
	}
	// The malformed VALUE must never be echoed: these errors reach logs, and an upstream
	// string could inject arbitrary text including newlines.
	if strings.Contains(err.Error(), "not-a-number") {
		t.Errorf("error echoes the raw upstream value, which must never reach a log: %v", err)
	}
	if !strings.Contains(err.Error(), "amount_micros") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

// TestGetCampaignSettings_BlankFieldIsAbsent: a field that arrives whitespace-only is not
// a value a caller can interpret, and carrying it through would let "" be compared and
// reported as a divergence invented by the read.
func TestGetCampaignSettings_BlankFieldIsAbsent(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"   ","status":"ENABLED"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got.Name != nil {
		t.Errorf("Name = %q, want nil: a blank field is not a value, it is an absence", *got.Name)
	}
}

// TestGetCampaignSettings_MalformedIDNeverReachesThePlatform: a malformed id is a permanent
// input fault, so it must be refused before any request is made.
func TestGetCampaignSettings_MalformedIDNeverReachesThePlatform(t *testing.T) {
	srv, _, requests := settingsServer(t, nil)
	client := newAccountsTestClient(t, srv)

	if _, err := client.GetCampaignSettings(context.Background(), "007"); err == nil {
		t.Fatal("expected an error for a leading-zero campaign id")
	}
	if got := requests(); len(got) != 0 {
		t.Errorf("a malformed id must not reach the platform, but requests were made: %v", got)
	}
}

// The tests below cover the decode-integrity and identity guards. Each was confirmed
// unreached before being written: deleting the guard left the suite green, which means the
// guard was unverified — the exact "if reverting a fix changes no test, it did nothing"
// case. GetCampaign pins the same guards for the same reasons; these are their settings
// counterparts.

// TestGetCampaignSettings_DuplicateKeysRefused pins the CONTRACT — a response declaring a
// key twice is refused rather than silently resolved in favour of the last value — but it
// does NOT independently bind the row-level guard in GetCampaignSettings, and it says so
// rather than pretending otherwise.
//
// Mutation-tested: disabling the row-level `hasDuplicateKeys(raw)` check leaves this test
// GREEN. The reason is that gaqlSearch runs the same check over the WHOLE response envelope
// (client.go) before any row is handed back, and that scan covers the bytes of every row, so
// over HTTP the envelope check always fires first. The row-level guard is therefore
// unreachable through this path today.
//
// It is kept anyway, for the reason the repo keeps other duplicated preconditions: it is a
// local invariant that must hold for any future caller handing rows in by another route, and
// deleting it would make that divergence silent. What is NOT acceptable is claiming
// revert-verified coverage for a line that no revert can break — hence this comment.
func TestGetCampaignSettings_DuplicateKeysRefused(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"first","name":"second","status":"ENABLED"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err == nil {
		t.Fatalf("expected an error for a duplicated JSON key, got: %+v", got)
	}
	// Either layer's refusal satisfies the contract; the point is that the ambiguous row is
	// never resolved into a confident answer.
	if !strings.Contains(err.Error(), "same JSON key twice") {
		t.Errorf("error should name the duplicate key, got: %v", err)
	}
}

// TestGetCampaignSettings_UnpairedSurrogateRefused pins the CONTRACT — a row whose name
// cannot survive JSON decoding intact is refused rather than returned under a substituted
// name — and, like the duplicate-key test above, is NOT independently revert-binding.
//
// Mutation-tested: disabling the row-level `!utf8.Valid(raw) || hasUnpairedSurrogateEscape(raw)`
// check leaves this test GREEN, because gaqlSearch (client.go) runs the identical check over
// the whole response envelope first and those bytes include every row.
//
// The mechanism is still worth pinning, and it is subtle: every byte of `"bad\uD800name"` is
// valid ASCII, so utf8.Valid alone passes it. An unpaired surrogate is not a Unicode scalar,
// so encoding/json substitutes U+FFFD with NO error — which is why the check must run on the
// RAW bytes and why byte validity alone is insufficient.
func TestGetCampaignSettings_UnpairedSurrogateRefused(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"bad\uD800name","status":"ENABLED"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err == nil {
		t.Fatalf("expected an error for an unpaired surrogate escape, got: %+v", got)
	}
}

// TestGetCampaignSettings_ResourceNameSuppliesTheIDWhenIDIsAbsent proves the fallback
// WORKS, rather than merely that it exists. The resource name is parsed strictly against
// THIS client's customer, so it is identity evidence a row from another account cannot
// supply.
func TestGetCampaignSettings_ResourceNameSuppliesTheIDWhenIDIsAbsent(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","name":"n","status":"ENABLED"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got.CampaignID != "555" {
		t.Errorf("CampaignID = %q, want 555 recovered from the resource name", got.CampaignID)
	}
}

// TestGetCampaignSettings_NoUsableIDRefused: a row that identifies no campaign cannot have
// settings attributed to it. Reporting it would present some campaign's configuration under
// an id nothing in the response supports.
func TestGetCampaignSettings_NoUsableIDRefused(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"name":"n","status":"ENABLED"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err == nil {
		t.Fatalf("expected an error for a row with no usable id, got: %+v", got)
	}
	if !strings.Contains(err.Error(), "no usable id") {
		t.Errorf("error should name the missing id, got: %v", err)
	}
}

// TestGetCampaignSettings_CrossAccountResourceNameRefused: the resource-name parser is
// strict about the CUSTOMER segment, so a row naming another customer's campaign cannot
// supply an identity — it yields no usable id rather than being accepted.
func TestGetCampaignSettings_CrossAccountResourceNameRefused(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/9999999999/campaigns/555","name":"n","status":"ENABLED"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err == nil {
		t.Fatalf("expected an error for another customer's resource name, got: %+v", got)
	}
}

// TestGetCampaignSettings_MalformedTotalAmountIsAnErrorNotAnAbsence closes the asymmetry
// with amount_micros, which already had this test. Without it the CUSTOM_PERIOD arm's
// "present but unparseable is an error, not an absence" contract was unverified — and that
// is the arm a lifetime-budget campaign takes.
func TestGetCampaignSettings_MalformedTotalAmountIsAnErrorNotAnAbsence(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"n","status":"ENABLED"},` +
		`"campaignBudget":{"totalAmountMicros":"nope","period":"CUSTOM_PERIOD"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err == nil {
		t.Fatalf("expected an error for a malformed total budget, got: %+v", got)
	}
	if strings.Contains(err.Error(), "nope") {
		t.Errorf("error echoes the raw upstream value, which must never reach a log: %v", err)
	}
	if !strings.Contains(err.Error(), "total_amount_micros") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

// TestGetCampaignSettings_DisagreeingIdentityFieldsRefused: a row carrying a plausible id
// beside a resource name naming a DIFFERENT campaign identifies no single campaign, so its
// settings cannot be attributed to either. Validating the resource name only as a fallback
// for a missing id would make this check reachable exactly when the row is least suspicious.
func TestGetCampaignSettings_DisagreeingIdentityFieldsRefused(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"555","name":"n","status":"ENABLED"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err == nil {
		t.Fatalf("expected an error when the two identity fields disagree, got: %+v", got)
	}
	if !strings.Contains(err.Error(), "identity fields disagree") {
		t.Errorf("error should name the disagreement, got: %v", err)
	}
}

// TestGetCampaignSettings_MalformedResourceNameBesideAGoodIDRefused is the case a
// fallback-only validation misses entirely: the id looks fine, so the resource name is
// never examined, and a cross-customer row is accepted as this campaign.
func TestGetCampaignSettings_MalformedResourceNameBesideAGoodIDRefused(t *testing.T) {
	for _, tc := range []struct{ name, resourceName string }{
		{"cross-customer", "customers/9999999999/campaigns/555"},
		{"malformed", "garbage/555"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := json.RawMessage(`{"campaign":{"resourceName":"` + tc.resourceName + `","id":"555","name":"n","status":"ENABLED"}}`)
			srv, _, _ := settingsServer(t, []json.RawMessage{row})
			client := newAccountsTestClient(t, srv)

			got, err := client.GetCampaignSettings(context.Background(), "555")
			if err == nil {
				t.Fatalf("expected an error for a %s resource name beside a plausible id, got: %+v", tc.name, got)
			}
			if !strings.Contains(err.Error(), "malformed or scoped to another customer") {
				t.Errorf("error should name the bad resource name, got: %v", err)
			}
		})
	}
}

// TestGetCampaignSettings_PaddedIDRefused: an id is identity evidence, so a padded value is
// a MALFORMED row rather than a value to normalise into a match. Trimming it would let a row
// whose identity field is malformed be reported as a clean answer.
func TestGetCampaignSettings_PaddedIDRefused(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":" 555 ","name":"n","status":"ENABLED"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err == nil {
		t.Fatalf("expected an error for a whitespace-padded id, got: %+v", got)
	}
}

// TestGetCampaignSettings_BothBudgetFieldsRefused: amount_micros and total_amount_micros are
// mutually exclusive upstream, so a row carrying both contradicts the API's own invariant.
// Resolving it by preference would compare a DAILY amount against a lifetime recorded budget
// and report a divergence that is really a field-selection bug.
//
// The period is deliberately UNKNOWN rather than DAILY. With DAILY this row is ALSO caught by
// the period/amount cross-check below, so the test would pass with the mutual-presence check
// deleted and would stop being evidence for the check it names. UNKNOWN is a period neither
// cross-check arm can fire on, so mutual presence is the only thing left that can reject it.
func TestGetCampaignSettings_BothBudgetFieldsRefused(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"n","status":"ENABLED"},` +
		`"campaignBudget":{"amountMicros":"500000000","totalAmountMicros":"9000000000","period":"UNKNOWN"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err == nil {
		t.Fatalf("expected an error when both budget fields are present, got: %+v", got)
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should name the contradiction, got: %v", err)
	}
}

// TestGetCampaignSettings_DailyPeriodWithTotalAmountRefused and its CUSTOM_PERIOD twin pin the
// budget period against the budget AMOUNT FIELD. The mutual-presence check above only rejects a
// row carrying BOTH amounts; a row carrying exactly one, paired with the OTHER period, decoded
// cleanly and was just as self-contradictory.
//
// It matters as a RESPONSE-INTEGRITY check: the row's two budget identity halves disagree, so no
// reading of its budget can be trusted and there is no way to tell which half is wrong. Reporting
// a budget from such a row — by either field — would attribute to the campaign a number the
// platform's own invariant says cannot describe it.
//
// This is independent of how the dispatcher selects between the two fields. Since LFXV2-3067
// googleAdsUpstreamBudgetAmount selects via googleAdsBudgetTypeFromPeriod rather than by presence,
// so a consistent pair is required for a comparison to happen at all; this guard is what stops a
// self-contradictory row being passed on as though it were coherent.
func TestGetCampaignSettings_DailyPeriodWithTotalAmountRefused(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"n","status":"ENABLED"},` +
		`"campaignBudget":{"totalAmountMicros":"9000000000","period":"DAILY"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err == nil {
		t.Fatalf("expected an error for a DAILY period carrying total_amount_micros, got: %+v", got)
	}
	// The error must name the offending field and the period it contradicts; that pair IS the
	// diagnosis a responder acts on.
	if !strings.Contains(err.Error(), "total_amount_micros") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
	if !strings.Contains(err.Error(), "DAILY") {
		t.Errorf("error should name the contradicting period, got: %v", err)
	}
}

func TestGetCampaignSettings_CustomPeriodWithDailyAmountRefused(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"n","status":"ENABLED"},` +
		`"campaignBudget":{"amountMicros":"500000000","period":"CUSTOM_PERIOD"}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err == nil {
		t.Fatalf("expected an error for a CUSTOM_PERIOD period carrying amount_micros, got: %+v", got)
	}
	if !strings.Contains(err.Error(), "amount_micros") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
	if !strings.Contains(err.Error(), "CUSTOM_PERIOD") {
		t.Errorf("error should name the contradicting period, got: %v", err)
	}
}

// TestGetCampaignSettings_ConsistentPeriodAmountPairsAccepted is the OTHER direction, and it is
// what stops the cross-check from being a guard that rejects everything. A fix that made every
// budget row an error would be exactly as useless as the bug it replaced: the two consistent
// pairings are the ordinary shape of a healthy campaign, and both must decode with the amount
// reaching the result.
//
// The absent-period and UNKNOWN cases are pinned here too, and they are the subtle ones. Absence
// already means "Google did not report this field" on every CampaignSettings pointer, so it
// cannot also start meaning "inconsistent pair" — and an absent period is SAFE downstream, where
// googleAdsUpstreamBudgetAmount selects the amount through googleAdsBudgetTypeFromPeriod, so a
// period naming neither real value selects no amount and the field reads `unknown` rather than
// comparing a quantity of unestablished meaning. UNKNOWN is a value Google explicitly declined to
// name, which contradicts nothing and selects nothing either.
func TestGetCampaignSettings_ConsistentPeriodAmountPairsAccepted(t *testing.T) {
	tests := []struct {
		name       string
		budget     string
		wantDaily  *int64
		wantTotal  *int64
		wantPeriod string
	}{
		{
			name:      "DAILY with amount_micros",
			budget:    `{"amountMicros":"750000000","period":"DAILY"}`,
			wantDaily: int64Ptr(750000000), wantPeriod: "DAILY",
		},
		{
			name:      "CUSTOM_PERIOD with total_amount_micros",
			budget:    `{"totalAmountMicros":"9000000000","period":"CUSTOM_PERIOD"}`,
			wantTotal: int64Ptr(9000000000), wantPeriod: "CUSTOM_PERIOD",
		},
		{
			name:      "absent period with amount_micros is a partial read, not a contradiction",
			budget:    `{"amountMicros":"750000000"}`,
			wantDaily: int64Ptr(750000000),
		},
		{
			name:      "absent period with total_amount_micros is a partial read, not a contradiction",
			budget:    `{"totalAmountMicros":"9000000000"}`,
			wantTotal: int64Ptr(9000000000),
		},
		{
			name:      "UNKNOWN period contradicts neither amount field",
			budget:    `{"totalAmountMicros":"9000000000","period":"UNKNOWN"}`,
			wantTotal: int64Ptr(9000000000), wantPeriod: "UNKNOWN",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"n","status":"ENABLED"},` +
				`"campaignBudget":` + tc.budget + `}`)
			srv, _, _ := settingsServer(t, []json.RawMessage{row})
			client := newAccountsTestClient(t, srv)

			got, err := client.GetCampaignSettings(context.Background(), "555")
			if err != nil {
				t.Fatalf("GetCampaignSettings: a consistent budget pair must decode, got: %v", err)
			}
			// Assert the VALUE reached the result, not merely that no error was returned: a
			// guard that dropped the budget while returning nil would pass a bare error check.
			if !int64PtrEqual(got.BudgetAmountMicros, tc.wantDaily) {
				t.Errorf("BudgetAmountMicros = %v, want %v", fmtInt64Ptr(got.BudgetAmountMicros), fmtInt64Ptr(tc.wantDaily))
			}
			if !int64PtrEqual(got.BudgetTotalAmountMicros, tc.wantTotal) {
				t.Errorf("BudgetTotalAmountMicros = %v, want %v", fmtInt64Ptr(got.BudgetTotalAmountMicros), fmtInt64Ptr(tc.wantTotal))
			}
			switch {
			case tc.wantPeriod == "" && got.BudgetPeriod != nil:
				t.Errorf("BudgetPeriod = %q, want nil", *got.BudgetPeriod)
			case tc.wantPeriod != "" && (got.BudgetPeriod == nil || *got.BudgetPeriod != tc.wantPeriod):
				t.Errorf("BudgetPeriod = %v, want %q", got.BudgetPeriod, tc.wantPeriod)
			}
		})
	}
}

// int64Ptr, int64PtrEqual and fmtInt64Ptr keep the table above readable while comparing
// PRESENCE as well as value — nil and 0 are different answers here, which is the whole reason
// CampaignSettings uses pointers.
func int64Ptr(v int64) *int64 { return &v }

func int64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func fmtInt64Ptr(v *int64) string {
	if v == nil {
		return "<nil>"
	}
	return strconv.FormatInt(*v, 10)
}

// TestGetCampaignSettings_PaddedPeriodWithContradictingAmountRefused pins the cross-check
// against a PADDED period.
//
// The refusal above compares the period with exact equality against DAILY/CUSTOM_PERIOD,
// while blankToNil carries a non-blank value through VERBATIM — padding and all. So a row
// reporting " DAILY " alongside total_amount_micros matched NEITHER arm and decoded cleanly,
// which is precisely the pair the cross-check exists to refuse — a row malformed at the source
// passing as coherent. Downstream the padded value is unnameable and selects no amount, so today
// the visible result would be a needlessly `unknown` field rather than a phantom divergence; the
// ROW is still self-contradictory, and this check is what names it.
//
// The period is normalised for the COMPARISON ONLY. It is an ENUM from a closed set, so
// recognising " DAILY " as DAILY discovers what the value already is; it does not invent a
// value. That is why identity fields are deliberately left untrimmed — trimming an opaque
// identifier would manufacture a well-formed id the platform never reported.
func TestGetCampaignSettings_PaddedPeriodWithContradictingAmountRefused(t *testing.T) {
	for _, tc := range []struct {
		name       string
		budgetJSON string
		wantField  string
		wantPeriod string
	}{
		{"padded DAILY carrying a total amount", `{"totalAmountMicros":"9000000000","period":" DAILY "}`, "total_amount_micros", "DAILY"},
		{"padded CUSTOM_PERIOD carrying a daily amount", `{"amountMicros":"500000000","period":" CUSTOM_PERIOD "}`, "amount_micros", "CUSTOM_PERIOD"},
		{"tab/newline padded DAILY carrying a total amount", `{"totalAmountMicros":"9000000000","period":"\tDAILY\n"}`, "total_amount_micros", "DAILY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"n","status":"ENABLED"},` +
				`"campaignBudget":` + tc.budgetJSON + `}`)
			srv, _, _ := settingsServer(t, []json.RawMessage{row})
			client := newAccountsTestClient(t, srv)

			got, err := client.GetCampaignSettings(context.Background(), "555")
			if err == nil {
				t.Fatalf("expected an error for a padded %s period carrying the contradicting amount, got: %+v", tc.wantPeriod, got)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error should name the offending field, got: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantPeriod) {
				t.Errorf("error should name the contradicting period, got: %v", err)
			}
		})
	}
}

// TestGetCampaignSettings_PaddedPeriodIsNotTrimmedInTheReadValue is the narrowing half, and
// it is what keeps the fix from becoming the very defect blankToNil documents.
//
// Normalisation is applied to the COMPARISON only. The period that reaches the caller must
// still be the bytes Google sent, so a consumer that has to judge the value sees it as it
// arrived rather than as a well-formed enum this decoder manufactured.
func TestGetCampaignSettings_PaddedPeriodIsNotTrimmedInTheReadValue(t *testing.T) {
	row := json.RawMessage(`{"campaign":{"resourceName":"customers/1234567890/campaigns/555","id":"555","name":"n","status":"ENABLED"},` +
		`"campaignBudget":{"amountMicros":"500000000","period":" DAILY "}}`)
	srv, _, _ := settingsServer(t, []json.RawMessage{row})
	client := newAccountsTestClient(t, srv)

	got, err := client.GetCampaignSettings(context.Background(), "555")
	if err != nil {
		t.Fatalf("a padded period with the CONSISTENT amount field is not a contradiction and must decode: %v", err)
	}
	if got.BudgetPeriod == nil {
		t.Fatal("BudgetPeriod is nil: a present, non-blank period must survive the read")
	}
	if *got.BudgetPeriod != " DAILY " {
		t.Errorf("BudgetPeriod = %q, want the verbatim %q: trimming the value the caller receives would "+
			"normalise a malformed field into a well-formed one behind the consumers that validate it",
			*got.BudgetPeriod, " DAILY ")
	}
}
