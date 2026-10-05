// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// kwCall records one API request the keyword levers made.
type kwCall struct {
	method, path, body string
}

// kwRecorder answers each request with handle(method, pathSuffix) and records it.
func kwRecorder(t *testing.T, handle func(method, path string) (int, string)) (*Client, func() []kwCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []kwCall
	)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path := r.URL.Path[strings.Index(r.URL.Path, "/v13/")+len("/v13/"):]
		mu.Lock()
		calls = append(calls, kwCall{r.Method, path, string(b)})
		mu.Unlock()
		status, body := handle(r.Method, path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	return c, func() []kwCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]kwCall(nil), calls...)
	}
}

func outcomesOf(out []KeywordActionOutcome) []string {
	s := make([]string, len(out))
	for i, o := range out {
		s[i] = o.Outcome
	}
	return s
}

// ---- validation -------------------------------------------------------------

func TestValidateKeywordActions_Bounds(t *testing.T) {
	ok := KeywordAction{AdGroupID: "654", KeywordID: "701", Action: "pause"}
	got, err := ValidateKeywordActions([]KeywordAction{ok})
	if err != nil || got[0].Action != KeywordActionPause {
		t.Fatalf("a valid action must pass and be normalised to PAUSE, got %+v, %v", got, err)
	}
	many := make([]KeywordAction, maxKeywordActions+1)
	for i := range many {
		many[i] = KeywordAction{AdGroupID: "654", KeywordID: string(rune('1'+i%9)) + strings.Repeat("0", i/9+1), Action: "PAUSE"}
	}
	for _, tc := range []struct {
		name string
		in   []KeywordAction
	}{
		{"empty", nil},
		{"over the cap", many},
		{"zero id", []KeywordAction{{AdGroupID: "654", KeywordID: "0", Action: "PAUSE"}}},
		{"leading zero", []KeywordAction{{AdGroupID: "0654", KeywordID: "701", Action: "PAUSE"}}},
		{"above int64", []KeywordAction{{AdGroupID: "654", KeywordID: "9223372036854775808", Action: "PAUSE"}}},
		{"padded", []KeywordAction{{AdGroupID: "654", KeywordID: " 701", Action: "PAUSE"}}},
		{"enable is not an action", []KeywordAction{{AdGroupID: "654", KeywordID: "701", Action: "ENABLE"}}},
		{"same keyword twice", []KeywordAction{ok, {AdGroupID: "654", KeywordID: "701", Action: "REMOVE"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ValidateKeywordActions(tc.in); err == nil {
				t.Fatal("expected a refusal")
			}
		})
	}
}

func TestValidateNegativeKeywords_Bounds(t *testing.T) {
	got, err := ValidateNegativeKeywords([]NegativeKeyword{{Text: "  free   download ", MatchType: "PHRASE"}, {Text: "Ångström & co.", MatchType: "exact"}})
	if err != nil {
		t.Fatalf("valid negatives refused: %v", err)
	}
	if got[0].Text != "free download" || got[0].MatchType != MatchTypePhrase || got[1].MatchType != MatchTypeExact {
		t.Errorf("normalisation = %+v", got)
	}
	tooMany := make([]NegativeKeyword, maxNegativeKeywords+1)
	for i := range tooMany {
		tooMany[i] = NegativeKeyword{Text: "kw" + strings.Repeat("x", i), MatchType: "Exact"}
	}
	for _, tc := range []struct {
		name string
		in   []NegativeKeyword
	}{
		{"empty", nil},
		{"over the cap", tooMany},
		{"blank", []NegativeKeyword{{Text: "   ", MatchType: "Exact"}}},
		{"101 characters", []NegativeKeyword{{Text: strings.Repeat("é", 101), MatchType: "Exact"}}},
		{"broad is not a negative match type", []NegativeKeyword{{Text: "free", MatchType: "Broad"}}},
		{"unknown match type", []NegativeKeyword{{Text: "free", MatchType: "Fuzzy"}}},
		{"control character", []NegativeKeyword{{Text: "free\tdownload", MatchType: "Exact"}}},
		{"disallowed symbol", []NegativeKeyword{{Text: "free@download", MatchType: "Exact"}}},
		{"quote syntax", []NegativeKeyword{{Text: `"free"`, MatchType: "Exact"}}},
		{"consecutive punctuation", []NegativeKeyword{{Text: "free--download", MatchType: "Exact"}}},
		{"case-folded duplicate", []NegativeKeyword{{Text: "Free", MatchType: "Exact"}, {Text: "free", MatchType: "exact"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ValidateNegativeKeywords(tc.in); err == nil {
				t.Fatal("expected a refusal")
			}
		})
	}
	// Exactly 100 characters is Microsoft's limit, not over it.
	if _, err := ValidateNegativeKeywords([]NegativeKeyword{{Text: strings.Repeat("é", 100), MatchType: "Exact"}}); err != nil {
		t.Errorf("a 100-character negative must pass: %v", err)
	}
	// The same text under the two match types is two distinct negatives.
	if _, err := ValidateNegativeKeywords([]NegativeKeyword{{Text: "free", MatchType: "Exact"}, {Text: "free", MatchType: "Phrase"}}); err != nil {
		t.Errorf("the same text under Exact and Phrase must pass: %v", err)
	}
}

// ---- GetAdGroupKeywords -------------------------------------------------------

func TestGetAdGroupKeywords_ReadsIdsAndStatus(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"Keywords":[{"Id":"701","Status":"Active"},{"Id":702,"Status":"Deleted"}]}`
	})
	got, err := c.GetAdGroupKeywords(context.Background(), "654")
	if err != nil {
		t.Fatalf("GetAdGroupKeywords: %v", err)
	}
	if len(got) != 2 || got[0].ID != "701" || got[0].IsDeleted() || !got[1].IsDeleted() {
		t.Errorf("keywords = %+v", got)
	}
	cs := calls()
	if len(cs) != 1 || cs[0].method != http.MethodPost || cs[0].path != "Keywords/QueryByAdGroupId" || cs[0].body != `{"AdGroupId":654}` {
		t.Errorf("request = %+v", cs)
	}
}

// A body that never answered (no Keywords field) is an error, not an empty ad group.
func TestGetAdGroupKeywords_MissingFieldIsAnError(t *testing.T) {
	c, _ := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, `{}` })
	if _, err := c.GetAdGroupKeywords(context.Background(), "654"); err == nil {
		t.Fatal("a response with no Keywords field must not read as an empty ad group")
	}
}

// ---- ApplyKeywordActions ------------------------------------------------------

func TestApplyKeywordActions_PauseThenDeleteWithExactBodies(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, `{"PartialErrors":[]}` })
	out, err := c.ApplyKeywordActions(context.Background(), "654", []KeywordAction{
		{AdGroupID: "654", KeywordID: "702", Action: "REMOVE"},
		{AdGroupID: "654", KeywordID: "701", Action: "PAUSE"},
		{AdGroupID: "654", KeywordID: "703", Action: "PAUSE"},
	})
	if err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}
	cs := calls()
	if len(cs) != 2 {
		t.Fatalf("want one PUT and one DELETE, got %+v", cs)
	}
	// PAUSE (reversible) first, REMOVE (irreversible) second, regardless of request order.
	if cs[0].method != http.MethodPut || cs[0].path != "Keywords" ||
		cs[0].body != `{"AdGroupId":654,"Keywords":[{"Id":701,"Status":"Paused"},{"Id":703,"Status":"Paused"}]}` {
		t.Errorf("pause call = %+v", cs[0])
	}
	if cs[1].method != http.MethodDelete || cs[1].path != "Keywords" || cs[1].body != `{"AdGroupId":654,"KeywordIds":[702]}` {
		t.Errorf("delete call = %+v", cs[1])
	}
	// Outcomes are POSITIONAL against the request, not against the calls.
	if len(out) != 3 || out[0].KeywordID != "702" || out[0].Action != KeywordActionRemove || out[1].KeywordID != "701" {
		t.Fatalf("outcomes not in request order: %+v", out)
	}
	for i, o := range out {
		if o.Outcome != OutcomeApplied {
			t.Errorf("outcome[%d] = %q, want APPLIED", i, o.Outcome)
		}
	}
}

func TestApplyKeywordActions_PartialErrorsArePositional(t *testing.T) {
	c, _ := kwRecorder(t, func(method, _ string) (int, string) {
		if method == http.MethodPut {
			// Index 1 of the PUT is the request's action 2.
			return http.StatusOK, `{"PartialErrors":[{"Code":1501,"ErrorCode":"CampaignServiceInvalidKeywordId","Index":1}]}`
		}
		return http.StatusOK, `{"PartialErrors":null}`
	})
	out, err := c.ApplyKeywordActions(context.Background(), "654", []KeywordAction{
		{AdGroupID: "654", KeywordID: "701", Action: "PAUSE"},
		{AdGroupID: "654", KeywordID: "702", Action: "REMOVE"},
		{AdGroupID: "654", KeywordID: "703", Action: "PAUSE"},
	})
	if err != nil {
		t.Fatalf("a per-item rejection must be reported per item, not as a whole-call error: %v", err)
	}
	if got := outcomesOf(out); strings.Join(got, ",") != "APPLIED,APPLIED,FAILED" {
		t.Fatalf("outcomes = %v, want [APPLIED APPLIED FAILED]", got)
	}
	if out[2].ErrorCode != "CampaignServiceInvalidKeywordId" {
		t.Errorf("error code = %q", out[2].ErrorCode)
	}
}

// An error Microsoft did not pin to an index may be any item's: nothing un-named can be called
// applied.
func TestApplyKeywordActions_UnattributableErrorIsUnconfirmed(t *testing.T) {
	c, _ := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"PartialErrors":[{"Code":1501,"Index":null},{"Code":1501,"Index":0}]}`
	})
	out, err := c.ApplyKeywordActions(context.Background(), "654", []KeywordAction{
		{AdGroupID: "654", KeywordID: "701", Action: "PAUSE"},
		{AdGroupID: "654", KeywordID: "702", Action: "PAUSE"},
	})
	if err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}
	if got := outcomesOf(out); strings.Join(got, ",") != "FAILED,UNCONFIRMED" {
		t.Fatalf("outcomes = %v, want [FAILED UNCONFIRMED]", got)
	}
}

func TestApplyKeywordActions_WholeCallAmbiguityIsUnconfirmed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"5xx", http.StatusBadGateway, `{}`},
		{"200 without PartialErrors", http.StatusOK, `{}`},
		{"undecodable 200", http.StatusOK, `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := kwRecorder(t, func(string, string) (int, string) { return tc.status, tc.body })
			_, err := c.ApplyKeywordActions(context.Background(), "654", []KeywordAction{{AdGroupID: "654", KeywordID: "701", Action: "REMOVE"}})
			if err == nil {
				t.Fatal("a call answered with nothing per item must be an error")
			}
			if !IsOutcomeUnconfirmed(err) {
				t.Errorf("must be UNCONFIRMED, got %v", err)
			}
		})
	}
}

// A 429 is retried (the calls are idempotent), and a refusal that FOLLOWS a 429 cannot prove the
// earlier attempt changed nothing: whole-call → unconfirmed error; per item → UNCONFIRMED.
func TestApplyKeywordActions_RefusalAfterA429IsUnconfirmed(t *testing.T) {
	t.Run("whole call", func(t *testing.T) {
		n := 0
		c, calls := kwRecorder(t, func(string, string) (int, string) {
			n++
			if n == 1 {
				return http.StatusTooManyRequests, `{}`
			}
			return http.StatusBadRequest, `{"ErrorCode":"CampaignServiceInvalidKeywordId"}`
		})
		_, err := c.ApplyKeywordActions(context.Background(), "654", []KeywordAction{{AdGroupID: "654", KeywordID: "701", Action: "REMOVE"}})
		if len(calls()) != 2 {
			t.Fatalf("the 429 must be retried once, got %d calls", len(calls()))
		}
		if err == nil || !IsOutcomeUnconfirmed(err) {
			t.Fatalf("a 400 after a 429 must be UNCONFIRMED, got %v", err)
		}
	})
	t.Run("per item", func(t *testing.T) {
		n := 0
		c, _ := kwRecorder(t, func(string, string) (int, string) {
			n++
			if n == 1 {
				return http.StatusTooManyRequests, `{}`
			}
			return http.StatusOK, `{"PartialErrors":[{"Code":1501,"Index":0}]}`
		})
		out, err := c.ApplyKeywordActions(context.Background(), "654", []KeywordAction{
			{AdGroupID: "654", KeywordID: "701", Action: "REMOVE"},
			{AdGroupID: "654", KeywordID: "702", Action: "REMOVE"},
		})
		if err != nil {
			t.Fatalf("ApplyKeywordActions: %v", err)
		}
		if got := outcomesOf(out); strings.Join(got, ",") != "UNCONFIRMED,APPLIED" {
			t.Fatalf("outcomes = %v, want [UNCONFIRMED APPLIED]", got)
		}
	})
}

// A definite refusal of the only call is a DEFINITE error — nothing was applied.
func TestApplyKeywordActions_DefiniteRefusalIsNotUnconfirmed(t *testing.T) {
	c, _ := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusBadRequest, `{"ErrorCode":"CampaignServiceInvalidKeywordId"}`
	})
	_, err := c.ApplyKeywordActions(context.Background(), "654", []KeywordAction{{AdGroupID: "654", KeywordID: "701", Action: "PAUSE"}})
	if err == nil || IsOutcomeUnconfirmed(err) {
		t.Fatalf("want a DEFINITE error, got %v", err)
	}
}

// One call answered and the other ambiguous: the answered half is reported, the other half is
// UNCONFIRMED — never a whole-request error that would hide the pauses that did land.
func TestApplyKeywordActions_MixedOutcomeAcrossCalls(t *testing.T) {
	c, _ := kwRecorder(t, func(method, _ string) (int, string) {
		if method == http.MethodDelete {
			return http.StatusServiceUnavailable, `{}`
		}
		return http.StatusOK, `{"PartialErrors":[]}`
	})
	out, err := c.ApplyKeywordActions(context.Background(), "654", []KeywordAction{
		{AdGroupID: "654", KeywordID: "701", Action: "REMOVE"},
		{AdGroupID: "654", KeywordID: "702", Action: "PAUSE"},
	})
	if err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}
	if got := outcomesOf(out); strings.Join(got, ",") != "UNCONFIRMED,APPLIED" {
		t.Fatalf("outcomes = %v, want [UNCONFIRMED APPLIED]", got)
	}
}

func TestApplyKeywordActions_ForeignAdGroupRefusedWithoutCalling(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, `{"PartialErrors":[]}` })
	if _, err := c.ApplyKeywordActions(context.Background(), "654", []KeywordAction{{AdGroupID: "999", KeywordID: "701", Action: "PAUSE"}}); err == nil {
		t.Fatal("an action naming another ad group must be refused")
	}
	if len(calls()) != 0 {
		t.Errorf("no request may be sent, got %+v", calls())
	}
}

// ---- AddCampaignNegativeKeywords -----------------------------------------------

func negOutcomes(out []NegativeKeywordOutcome) string {
	s := make([]string, len(out))
	for i, o := range out {
		s[i] = o.Outcome
	}
	return strings.Join(s, ",")
}

func TestAddCampaignNegativeKeywords_RequestShapeAndOutcomes(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"NegativeKeywordIds":[{"Ids":[9001,null,null,null]}],` +
			`"NestedPartialErrors":[{"BatchErrors":[` +
			`{"Code":4335,"ErrorCode":"CampaignServiceNegativeKeywordAlreadyExists","Index":1},` +
			`{"Code":1007,"ErrorCode":"CampaignServiceNegativeKeywordMatchesKeyword","Index":2}],"Code":null,"ErrorCode":null}]}`
	})
	out, err := c.AddCampaignNegativeKeywords(context.Background(), "321", []NegativeKeyword{
		{Text: "free", MatchType: "Exact"},
		{Text: "cheap", MatchType: "Phrase"},
		{Text: "kubernetes", MatchType: "Exact"},
		{Text: "torrent", MatchType: "Phrase"},
	})
	if err != nil {
		t.Fatalf("AddCampaignNegativeKeywords: %v", err)
	}
	if got := negOutcomes(out); got != "APPLIED,ALREADY_PRESENT,FAILED,UNCONFIRMED" {
		t.Fatalf("outcomes = %s", got)
	}
	if out[0].NegativeKeywordID != "9001" || out[2].ErrorCode != "CampaignServiceNegativeKeywordMatchesKeyword" || out[1].NegativeKeywordID != "" {
		t.Errorf("outcome details = %+v", out)
	}
	cs := calls()
	want := `{"EntityNegativeKeywords":[{"EntityId":321,"EntityType":"Campaign","NegativeKeywords":[` +
		`{"MatchType":"Exact","Text":"free"},{"MatchType":"Phrase","Text":"cheap"},{"MatchType":"Exact","Text":"kubernetes"},{"MatchType":"Phrase","Text":"torrent"}]}]}`
	if len(cs) != 1 || cs[0].method != http.MethodPost || cs[0].path != "EntityNegativeKeywords" || cs[0].body != want {
		t.Errorf("request = %+v\nwant body %s", cs, want)
	}
}

// An already-exists travelling with a genuine rejection for the SAME item stays FAILED.
func TestAddCampaignNegativeKeywords_AlreadyExistsWithAnotherErrorIsFailed(t *testing.T) {
	c, _ := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"NegativeKeywordIds":[{"Ids":[null]}],"NestedPartialErrors":[{"BatchErrors":[` +
			`{"Code":4335,"Index":0},{"Code":1005,"ErrorCode":"CampaignServiceInvalidNegativeKeyword","Index":0}]}]}`
	})
	out, err := c.AddCampaignNegativeKeywords(context.Background(), "321", []NegativeKeyword{{Text: "free", MatchType: "Exact"}})
	if err != nil {
		t.Fatalf("AddCampaignNegativeKeywords: %v", err)
	}
	if out[0].Outcome != OutcomeFailed {
		t.Errorf("outcome = %q, want FAILED", out[0].Outcome)
	}
}

// The campaign itself refused: nothing was added, DEFINITELY.
func TestAddCampaignNegativeKeywords_EntityErrorIsDefinite(t *testing.T) {
	c, _ := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"NegativeKeywordIds":[{"Ids":[null]}],"NestedPartialErrors":[{"BatchErrors":null,"Code":1100,"ErrorCode":"CampaignServiceInvalidCampaignId","Index":0}]}`
	})
	_, err := c.AddCampaignNegativeKeywords(context.Background(), "321", []NegativeKeyword{{Text: "free", MatchType: "Exact"}})
	if err == nil || IsOutcomeUnconfirmed(err) {
		t.Fatalf("want a DEFINITE error, got %v", err)
	}
}

// The add is NOT retried on 429: a 429 is answered at once as UNCONFIRMED, as are 5xx and
// unreadable answers.
func TestAddCampaignNegativeKeywords_AmbiguityIsUnconfirmed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"429", http.StatusTooManyRequests, `{}`},
		{"5xx", http.StatusInternalServerError, `{}`},
		{"200 answering nothing", http.StatusOK, `{}`},
		{"undecodable 200", http.StatusOK, `[`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, calls := kwRecorder(t, func(string, string) (int, string) { return tc.status, tc.body })
			_, err := c.AddCampaignNegativeKeywords(context.Background(), "321", []NegativeKeyword{{Text: "free", MatchType: "Exact"}})
			if err == nil || !IsOutcomeUnconfirmed(err) {
				t.Fatalf("want UNCONFIRMED, got %v", err)
			}
			if len(calls()) != 1 {
				t.Errorf("the add must not be retried, got %d calls", len(calls()))
			}
		})
	}
}

func TestAddCampaignNegativeKeywords_InvalidInputSendsNothing(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, `{}` })
	_, err := c.AddCampaignNegativeKeywords(context.Background(), "321", []NegativeKeyword{{Text: "free", MatchType: "Broad"}})
	if err == nil {
		t.Fatal("Broad must be refused")
	}
	if _, err := c.AddCampaignNegativeKeywords(context.Background(), "abc", []NegativeKeyword{{Text: "free", MatchType: "Exact"}}); err == nil {
		t.Fatal("a non-numeric campaign id must be refused")
	}
	if len(calls()) != 0 {
		t.Errorf("no request may be sent, got %+v", calls())
	}
}

// Sanity: the outcome errors carry no Unconfirmed marker when definite, so callers can switch
// on errors.As without matching a definite failure.
func TestKeywordLeverErrors_DefiniteHasNoUnconfirmedMarker(t *testing.T) {
	var u interface{ Unconfirmed() bool }
	if errors.As(&negativeKeywordEntityError{code: "x"}, &u) {
		t.Error("an entity-level refusal must not carry Unconfirmed()")
	}
	if !IsOutcomeUnconfirmed(&keywordMutationUnconfirmedError{what: "x", err: errMalformedKeywordResponse}) {
		t.Error("keywordMutationUnconfirmedError must be unconfirmed")
	}
}
