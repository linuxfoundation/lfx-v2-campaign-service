// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// GoogleAdsDispatcher must satisfy NegativeKeywordAdder at COMPILE time. The capability is
// reached by type assertion in the orchestrator, so a signature that drifts does not fail the
// build — it silently stops being a negative-keyword platform and every request 400s with
// "unsupported", which is a behaviour change no test of the method body would catch.
var _ service.NegativeKeywordAdder = (*GoogleAdsDispatcher)(nil)

// negDispatcher answers the GAQL read with searchJSON and campaignCriteria:mutate with
// mutateStatus/mutateBody, recording every request. Both arms are the test's to set because the
// pre-read and the mutate are two different failure surfaces with opposite classifications.
func negDispatcher(t *testing.T, searchJSON string, mutateStatus int, mutateBody string) (*GoogleAdsDispatcher, func() []budgetRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []budgetRequest
	)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, budgetRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "campaignCriteria:mutate") {
			w.WriteHeader(mutateStatus)
			_, _ = io.WriteString(w, mutateBody)
			return
		}
		_, _ = io.WriteString(w, searchJSON)
	}))
	t.Cleanup(apiSrv.Close)

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	return d, func() []budgetRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]budgetRequest, len(seen))
		copy(out, seen)
		return out
	}
}

// negCampaign is the row every case starts from: provisioned, and recording the customer it was
// created under.
func negCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result: settingsProvenance(),
	}
}

// negRow renders one campaign_criterion row of the existing-negatives read.
func negRow(criterionID, text, matchType string) string {
	return `{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"` + criterionID +
		`","negative":true,"type":"KEYWORD","keyword":{"text":"` + text + `","matchType":"` + matchType + `"}}}`
}

// negSearchJSON wraps rows as a googleAds:search answer; with no rows it is the campaign that
// excludes nothing yet.
func negSearchJSON(rows ...string) string {
	return `{"results":[` + strings.Join(rows, ",") + `]}`
}

// criteriaMutateCalls returns just the campaignCriteria:mutate requests out of a recording.
func criteriaMutateCalls(reqs []budgetRequest) []budgetRequest {
	var out []budgetRequest
	for _, r := range reqs {
		if strings.Contains(r.Path, "campaignCriteria:mutate") {
			out = append(out, r)
		}
	}
	return out
}

func assertNoCriteriaMutate(t *testing.T, reqs []budgetRequest) {
	t.Helper()
	if m := criteriaMutateCalls(reqs); len(m) != 0 {
		t.Fatalf("a refusal still issued %d campaignCriteria:mutate call(s): %+v — the caller is told nothing was added while exclusions were attached", len(m), m)
	}
}

// TestGoogleAds_AddNegativeKeywords_AppliesNewOnes is the happy path, asserting the WIRE as well
// as the outcomes: one atomic batch carrying every keyword as its own create with negative:true,
// addressed at the CAMPAIGN resource — campaign-level criteria, the same level the create path's
// exclusion list uses, not ad-group-level ones that would miss every other ad group.
func TestGoogleAds_AddNegativeKeywords_AppliesNewOnes(t *testing.T) {
	d, requests := negDispatcher(t, negSearchJSON(), http.StatusOK,
		`{"results":[{"resourceName":"customers/1234567890/campaignCriteria/777~11"},{"resourceName":"customers/1234567890/campaignCriteria/777~12"}]}`)

	out, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderGoogleAds, negCampaign(),
		[]model.NegativeKeyword{{Text: "free download", MatchType: "Phrase"}, {Text: "torrent", MatchType: "Exact"}}, "")
	if err != nil {
		t.Fatalf("AddNegativeKeywords: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d outcomes for 2 keywords: %+v", len(out), out)
	}
	for i, want := range []string{"11", "12"} {
		if out[i].Outcome != model.KeywordOutcomeApplied {
			t.Errorf("outcome[%d] = %q, want applied", i, out[i].Outcome)
		}
		// Ids are paired POSITIONALLY with what was sent, so a reordered pairing would persist a
		// criterion id against a keyword it does not identify.
		if out[i].NegativeKeywordID != want {
			t.Errorf("outcome[%d] criterion id = %q, want %q", i, out[i].NegativeKeywordID, want)
		}
	}

	calls := criteriaMutateCalls(requests())
	if len(calls) != 1 {
		t.Fatalf("got %d campaignCriteria:mutate calls, want exactly 1 (the batch is atomic): %+v", len(calls), calls)
	}
	var req struct {
		PartialFailure *bool `json:"partialFailure"`
		Operations     []struct {
			Create struct {
				Campaign string `json:"campaign"`
				Negative bool   `json:"negative"`
				Keyword  struct {
					Text      string `json:"text"`
					MatchType string `json:"matchType"`
				} `json:"keyword"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(calls[0].Body), &req); err != nil {
		t.Fatalf("mutate body is not the expected shape: %v\nbody: %s", err, calls[0].Body)
	}
	// partialFailure is deliberately never set: a half-applied exclusion list is worse than
	// none, because the campaign then LOOKS protected.
	if req.PartialFailure != nil {
		t.Errorf("partialFailure was sent (%v); the batch must stay atomic", *req.PartialFailure)
	}
	if len(req.Operations) != 2 {
		t.Fatalf("got %d operations, want 2: %s", len(req.Operations), calls[0].Body)
	}
	for i, want := range []struct{ text, match string }{{"free download", "PHRASE"}, {"torrent", "EXACT"}} {
		op := req.Operations[i].Create
		if !op.Negative {
			t.Errorf("operation %d is not negative:true — it would ADD the keyword as a positive target", i)
		}
		if op.Campaign != "customers/1234567890/campaigns/777" {
			t.Errorf("operation %d addresses %q, want the campaign resource", i, op.Campaign)
		}
		if op.Keyword.Text != want.text || op.Keyword.MatchType != want.match {
			t.Errorf("operation %d = %q/%q, want %q/%q", i, op.Keyword.Text, op.Keyword.MatchType, want.text, want.match)
		}
	}
}

// TestGoogleAds_AddNegativeKeywords_AlreadyPresentIsNeverSent is the whole reason the pre-read
// exists. campaignCriteria:mutate is ATOMIC, so ONE duplicate fails the entire batch and takes
// down every keyword that would have been accepted — unlike Microsoft, where a duplicate comes
// back as its own per-item outcome and costs the batch nothing.
//
// The comparison folds case: a campaign already excluding "Free Download" must match a repeat
// request for "free download", or the duplicate is sent and the batch dies.
func TestGoogleAds_AddNegativeKeywords_AlreadyPresentIsNeverSent(t *testing.T) {
	d, requests := negDispatcher(t,
		negSearchJSON(negRow("11", "Free Download", "PHRASE")),
		http.StatusOK, `{"results":[{"resourceName":"customers/1234567890/campaignCriteria/777~12"}]}`)

	out, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderGoogleAds, negCampaign(),
		[]model.NegativeKeyword{{Text: "free download", MatchType: "Phrase"}, {Text: "torrent", MatchType: "Exact"}}, "")
	if err != nil {
		t.Fatalf("AddNegativeKeywords: %v", err)
	}
	if out[0].Outcome != model.KeywordOutcomeAlreadyPresent {
		t.Errorf("outcome[0] = %q, want already-present", out[0].Outcome)
	}
	// The id reported is the one the campaign ALREADY holds, not one this request created.
	if out[0].NegativeKeywordID != "11" {
		t.Errorf("outcome[0] criterion id = %q, want the existing 11", out[0].NegativeKeywordID)
	}
	if out[1].Outcome != model.KeywordOutcomeApplied || out[1].NegativeKeywordID != "12" {
		t.Errorf("outcome[1] = %q/%q, want applied/12", out[1].Outcome, out[1].NegativeKeywordID)
	}

	calls := criteriaMutateCalls(requests())
	if len(calls) != 1 {
		t.Fatalf("got %d mutate calls, want 1", len(calls))
	}
	if strings.Contains(strings.ToLower(calls[0].Body), "free download") {
		t.Errorf("the already-present keyword was sent anyway, which fails the whole atomic batch: %s", calls[0].Body)
	}
}

// TestGoogleAds_AddNegativeKeywords_MatchTypeIsPartOfTheKey: the same text under a DIFFERENT
// match type is a different criterion and Google accepts both, so folding on text alone would
// drop a keyword the caller legitimately asked for — the opposite failure to the duplicate one,
// and the one that is silent.
func TestGoogleAds_AddNegativeKeywords_MatchTypeIsPartOfTheKey(t *testing.T) {
	d, requests := negDispatcher(t,
		negSearchJSON(negRow("11", "torrent", "PHRASE")),
		http.StatusOK, `{"results":[{"resourceName":"customers/1234567890/campaignCriteria/777~12"}]}`)

	out, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderGoogleAds, negCampaign(),
		[]model.NegativeKeyword{{Text: "torrent", MatchType: "Exact"}}, "")
	if err != nil {
		t.Fatalf("AddNegativeKeywords: %v", err)
	}
	if out[0].Outcome != model.KeywordOutcomeApplied {
		t.Fatalf("outcome = %q, want applied: an EXACT negative is not the PHRASE one already present", out[0].Outcome)
	}
	if len(criteriaMutateCalls(requests())) != 1 {
		t.Error("the keyword was not sent, so a legitimate exclusion was silently dropped")
	}
}

// TestGoogleAds_AddNegativeKeywords_AllAlreadyPresentSendsNothing: every outcome is DEFINITE
// without a mutate, so there is nothing to send and no error to report. Sending an empty batch
// would be a pointless write; erroring would be false.
func TestGoogleAds_AddNegativeKeywords_AllAlreadyPresentSendsNothing(t *testing.T) {
	d, requests := negDispatcher(t,
		negSearchJSON(negRow("11", "free download", "PHRASE"), negRow("12", "torrent", "EXACT")),
		http.StatusInternalServerError, `{"error":{"message":"the mutate must never be reached"}}`)

	out, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderGoogleAds, negCampaign(),
		[]model.NegativeKeyword{{Text: "free download", MatchType: "Phrase"}, {Text: "torrent", MatchType: "Exact"}}, "")
	if err != nil {
		t.Fatalf("AddNegativeKeywords: %v", err)
	}
	for i := range out {
		if out[i].Outcome != model.KeywordOutcomeAlreadyPresent {
			t.Errorf("outcome[%d] = %q, want already-present", i, out[i].Outcome)
		}
	}
	assertNoCriteriaMutate(t, requests())
}

// TestGoogleAds_AddNegativeKeywords_DefiniteFailureIsReportedPerItem: the batch is atomic, so a
// definite refusal means NONE of the sent keywords was attached and the campaign's existing
// exclusions are untouched. That is a per-item answer, not a call-level error — the caller gets
// the list and can retry it.
func TestGoogleAds_AddNegativeKeywords_DefiniteFailureIsReportedPerItem(t *testing.T) {
	d, _ := negDispatcher(t, negSearchJSON(), http.StatusBadRequest, `{"error":{"message":"invalid keyword"}}`)

	out, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderGoogleAds, negCampaign(),
		[]model.NegativeKeyword{{Text: "free download", MatchType: "Phrase"}, {Text: "torrent", MatchType: "Exact"}}, "")
	if err != nil {
		t.Fatalf("a definite per-item failure must not be returned as a call-level error: %v", err)
	}
	for i := range out {
		if out[i].Outcome != model.KeywordOutcomeFailed {
			t.Errorf("outcome[%d] = %q, want failed", i, out[i].Outcome)
		}
		if out[i].ErrorCode != model.KeywordTargetingErrRejected {
			t.Errorf("outcome[%d] error code = %q, want rejected (Google refused it; the request did leave)", i, out[i].ErrorCode)
		}
		if out[i].NegativeKeywordID != "" {
			t.Errorf("outcome[%d] carries a criterion id %q for a keyword that was never attached", i, out[i].NegativeKeywordID)
		}
	}
}

// TestGoogleAds_AddNegativeKeywords_AmbiguousMutateIsUnconfirmed covers both halves of the
// no-definite-item rule. Every case is one where the criteria MAY exist, so neither "added" nor
// "not added" may be reported; with nothing definite to hand back, the call errors so the service
// answers "verify upstream before retrying" rather than a list of unknowns.
func TestGoogleAds_AddNegativeKeywords_AmbiguousMutateIsUnconfirmed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"5xx from the mutate", http.StatusServiceUnavailable, `{"error":{"message":"backend error"}}`},
		{"2xx with a malformed body", http.StatusOK, `{`},
		{"2xx acknowledging fewer criteria than were sent", http.StatusOK, `{"results":[{"resourceName":"customers/1234567890/campaignCriteria/777~11"}]}`},
		{"2xx naming another campaign's criterion", http.StatusOK,
			`{"results":[{"resourceName":"customers/1234567890/campaignCriteria/888~11"},{"resourceName":"customers/1234567890/campaignCriteria/888~12"}]}`},
		{"2xx naming another account's criterion", http.StatusOK,
			`{"results":[{"resourceName":"customers/9999999999/campaignCriteria/777~11"},{"resourceName":"customers/9999999999/campaignCriteria/777~12"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := negDispatcher(t, negSearchJSON(), tc.status, tc.body)

			out, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderGoogleAds, negCampaign(),
				[]model.NegativeKeyword{{Text: "free download", MatchType: "Phrase"}, {Text: "torrent", MatchType: "Exact"}}, "")
			if err == nil {
				t.Fatalf("got outcomes %+v, want an error: no item has a definite outcome", out)
			}
			if !isUnconfirmedWrite(err) {
				t.Fatalf("error does not report Unconfirmed(): %v — the criteria may exist, so this must not read as a definite failure", err)
			}
		})
	}
}

// TestGoogleAds_AddNegativeKeywords_UnconfirmedKeepsDefiniteOutcomes is the other side of that
// rule: one ALREADY_PRESENT item IS definite, so the batch comes back as outcomes rather than a
// blanket error. Erroring here would throw away a fact the caller needs — that the campaign
// already excludes that keyword — on account of a different keyword's ambiguity.
func TestGoogleAds_AddNegativeKeywords_UnconfirmedKeepsDefiniteOutcomes(t *testing.T) {
	d, _ := negDispatcher(t,
		negSearchJSON(negRow("11", "free download", "PHRASE")),
		http.StatusServiceUnavailable, `{"error":{"message":"backend error"}}`)

	out, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderGoogleAds, negCampaign(),
		[]model.NegativeKeyword{{Text: "free download", MatchType: "Phrase"}, {Text: "torrent", MatchType: "Exact"}}, "")
	if err != nil {
		t.Fatalf("one definite outcome must be preserved rather than collapsed into an error: %v", err)
	}
	if out[0].Outcome != model.KeywordOutcomeAlreadyPresent || out[0].NegativeKeywordID != "11" {
		t.Errorf("outcome[0] = %q/%q, want already-present/11", out[0].Outcome, out[0].NegativeKeywordID)
	}
	if out[1].Outcome != model.KeywordOutcomeUnconfirmed {
		t.Errorf("outcome[1] = %q, want unconfirmed", out[1].Outcome)
	}
	if out[1].ErrorCode != "" {
		t.Errorf("outcome[1] carries error code %q; an unconfirmed item has not failed", out[1].ErrorCode)
	}
}

// TestGoogleAds_AddNegativeKeywords_FailedPreReadIsNotUnconfirmed: the read changes nothing, so
// its failure is DEFINITE however ambiguous the HTTP status. It is still FATAL — proceeding
// without it would send a duplicate into an atomic batch, which is the outcome it exists to
// prevent — but it must not be dressed up as "the keywords may have been added".
func TestGoogleAds_AddNegativeKeywords_FailedPreReadIsNotUnconfirmed(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []budgetRequest
	)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, budgetRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"backend error"}}`)
	}))
	t.Cleanup(apiSrv.Close)
	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)

	out, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderGoogleAds, negCampaign(),
		[]model.NegativeKeyword{{Text: "torrent", MatchType: "Exact"}}, "")
	if err == nil {
		t.Fatalf("got outcomes %+v, want an error: without the pre-read a duplicate would fail the whole batch", out)
	}
	if isUnconfirmedWrite(err) {
		t.Fatalf("a failed pre-READ was classified unconfirmed: %v — no mutate was built, so no criterion can exist", err)
	}
	mu.Lock()
	defer mu.Unlock()
	assertNoCriteriaMutate(t, seen)
}

// TestGoogleAds_AddNegativeKeywords_RefusesBeforeTouchingThePlatform covers every guard that runs
// ahead of the first request. Each returns its own sentinel so the service maps it to the right
// status, and each must leave the platform untouched — including the pre-read, since a request
// refused on its own row should not be spending an API call either.
func TestGoogleAds_AddNegativeKeywords_RefusesBeforeTouchingThePlatform(t *testing.T) {
	good := []model.NegativeKeyword{{Text: "torrent", MatchType: "Exact"}}

	for _, tc := range []struct {
		name     string
		campaign func() *model.Campaign
		keywords []model.NegativeKeyword
		revision string
		want     error
	}{
		{
			// Reddit's keyword lever is the only one with a revision to guard; accepting the
			// field silently here would let a caller believe a concurrency check was honoured.
			name: "a revision", campaign: negCampaign, keywords: good, revision: "7",
			want: domain.ErrNegativeKeywordInvalid,
		},
		{
			name: "an empty batch", campaign: negCampaign, keywords: nil,
			want: domain.ErrNegativeKeywordInvalid,
		},
		{
			name: "a match type Google does not offer", campaign: negCampaign,
			keywords: []model.NegativeKeyword{{Text: "torrent", MatchType: "Near"}},
			want:     domain.ErrNegativeKeywordInvalid,
		},
		{
			// 81 runes: inside the endpoint's published 100 (Microsoft's NegativeKeyword.Text
			// limit, which the shared batch validator applies) and outside Google's own 80. The
			// client refuses it too, but with a plain error that the classification arm would
			// report as a platform REJECTION of every keyword in the batch — a claim that Google
			// refused them, when Google was never contacted. It belongs here, as a 400.
			name: "a text inside the shared limit but past Google's", campaign: negCampaign,
			keywords: []model.NegativeKeyword{{Text: strings.Repeat("k", googleads.MaxKeywordTextRunes+1), MatchType: "Exact"}},
			want:     domain.ErrNegativeKeywordInvalid,
		},
		{
			name: "an unprovisioned campaign", keywords: good,
			campaign: func() *model.Campaign {
				c := negCampaign()
				c.PlatformCampaignID = "  "
				return c
			},
			want: domain.ErrCampaignNotProvisioned,
		},
		{
			name: "a campaign id that is not a Google Ads id", keywords: good,
			campaign: func() *model.Campaign {
				c := negCampaign()
				c.PlatformCampaignID = "77-7"
				return c
			},
			want: domain.ErrCampaignNotProvisioned,
		},
		{
			// One customer is shared across every foundation, so a row that cannot prove which
			// customer it was created under cannot prove the campaign id it carries is its own —
			// and the cost of being wrong is silently stopping a stranger's ads from serving.
			name: "no recorded provenance", keywords: good,
			campaign: func() *model.Campaign {
				c := negCampaign()
				c.Result = nil
				return c
			},
			want: domain.ErrCampaignProvenanceUnknown,
		},
		{
			name: "a customer that is not the project's", keywords: good,
			campaign: func() *model.Campaign {
				c := negCampaign()
				c.Result = []byte(`{"customerId":"9876543210"}`)
				return c
			},
			want: domain.ErrCampaignAccountMismatch,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, requests := negDispatcher(t, negSearchJSON(), http.StatusOK,
				`{"results":[{"resourceName":"customers/1234567890/campaignCriteria/777~11"}]}`)

			out, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderGoogleAds,
				tc.campaign(), tc.keywords, tc.revision)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if out != nil {
				t.Errorf("a refusal returned outcomes %+v; nothing was attempted", out)
			}
			assertNoCriteriaMutate(t, requests())
		})
	}
}

// TestGoogleAds_AddNegativeKeywords_AbsentProvenanceAlsoCarriesTheMismatch pins the join. The
// absence sentinel is new vocabulary; a caller that only knows ErrCampaignAccountMismatch must
// still map this to the same refusal rather than fall through to a 500.
func TestGoogleAds_AddNegativeKeywords_AbsentProvenanceAlsoCarriesTheMismatch(t *testing.T) {
	d, _ := negDispatcher(t, negSearchJSON(), http.StatusOK, `{"results":[]}`)

	camp := negCampaign()
	camp.Result = nil
	_, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderGoogleAds, camp,
		[]model.NegativeKeyword{{Text: "torrent", MatchType: "Exact"}}, "")
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Fatalf("err = %v, want it to also carry ErrCampaignAccountMismatch", err)
	}
}

// TestGoogleAds_AddNegativeKeywords_RefusesAnUntrustworthyPreRead: the present-set decides what is
// WITHHELD, so a response that cannot be trusted would silently suppress a keyword the caller
// asked to add — the one failure the read's re-applied predicates exist to prevent. Refusing is
// the only safe answer; proceeding as if the campaign had no negatives risks the atomic batch.
func TestGoogleAds_AddNegativeKeywords_RefusesAnUntrustworthyPreRead(t *testing.T) {
	for _, tc := range []struct{ name, rows string }{
		{"a positive criterion", `{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","type":"KEYWORD","keyword":{"text":"torrent","matchType":"EXACT"}}}`},
		{"a non-keyword criterion", `{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"LOCATION","keyword":{"text":"torrent","matchType":"EXACT"}}}`},
		{"another campaign's row", `{"campaign":{"id":"888"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"KEYWORD","keyword":{"text":"torrent","matchType":"EXACT"}}}`},
		{"a row with no criterion id", `{"campaign":{"id":"777"},"campaignCriterion":{"negative":true,"type":"KEYWORD","keyword":{"text":"torrent","matchType":"EXACT"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, requests := negDispatcher(t, negSearchJSON(tc.rows), http.StatusOK,
				`{"results":[{"resourceName":"customers/1234567890/campaignCriteria/777~11"}]}`)

			out, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderGoogleAds, negCampaign(),
				[]model.NegativeKeyword{{Text: "torrent", MatchType: "Exact"}}, "")
			if err == nil {
				t.Fatalf("got outcomes %+v, want a refusal", out)
			}
			if isUnconfirmedWrite(err) {
				t.Fatalf("a refused READ was classified unconfirmed: %v", err)
			}
			assertNoCriteriaMutate(t, requests())
		})
	}
}

// TestGoogleAdsNegativeKey_AgreesAcrossBothSources pins the one property this key exists for: the
// PRESENT side (whatever Google returned, normalised by nobody) and the REQUEST side (normalised
// by validateNegativeKeywordBatch through NormalizeNegativeKeywordText) must produce the same key
// for the same keyword.
//
// Without the collapse inside googleAdsNegativeKey these rows disagree, and a disagreement is not
// a cosmetic miss: the criterion reads as ABSENT, is sent again, and a duplicate fails the ATOMIC
// campaignCriteria:mutate, taking every keyword that would have been accepted down with it.
func TestGoogleAdsNegativeKey_AgreesAcrossBothSources(t *testing.T) {
	for _, tc := range []struct{ name, stored, requested string }{
		{"identical", "free download", "free download"},
		{"leading and trailing space on the stored row", " free download ", "free download"},
		{"a doubled internal space on the stored row", "free  download", "free download"},
		{"a non-breaking space on the stored row", "free download", "free download"},
		{"a tab on the stored row", "free\tdownload", "free download"},
		{"case difference, which the fold already covered", "Free Download", "free download"},
		{"both divergences at once", "  Free   DOWNLOAD ", "free download"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The request side as the dispatcher actually builds it: validated first, so the
			// test cannot pass by normalising the input itself.
			batch, err := validateNegativeKeywordBatch("google", []model.NegativeKeyword{
				{Text: tc.requested, MatchType: negativeMatchPhrase},
			}, 60, negativeMatchExact, negativeMatchPhrase, negativeMatchBroad)
			if err != nil {
				t.Fatalf("the requested text must survive validation: %v", err)
			}
			want := googleAdsNegativeKey(googleads.MatchTypePhrase, batch[0].Text)
			got := googleAdsNegativeKey(googleads.MatchTypePhrase, tc.stored)
			if got != want {
				t.Errorf("stored %q keys as %q, requested %q keys as %q — the criterion would read as absent and be resent into an atomic batch",
					tc.stored, got, tc.requested, want)
			}
		})
	}
}

// TestGoogleAdsNegativeKey_StillSeparatesDifferentKeywords is the other half: the collapse above
// must not make the key match MORE than Google's own notion of one keyword. Over-matching reports
// a keyword ALREADY_PRESENT and never sends it, which silently drops an exclusion the caller
// asked for — a worse failure than the one the collapse fixes, because nothing surfaces it.
func TestGoogleAdsNegativeKey_StillSeparatesDifferentKeywords(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"different words", "free download", "paid download"},
		{"a word boundary that the collapse must not erase", "free download", "freedownload"},
		{"a prefix is not the whole text", "free", "freedom"},
		{"punctuation is part of the text", "free-download", "free download"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if googleAdsNegativeKey(googleads.MatchTypePhrase, tc.a) == googleAdsNegativeKey(googleads.MatchTypePhrase, tc.b) {
				t.Errorf("%q and %q must not share a key: one would be reported ALREADY_PRESENT and never sent", tc.a, tc.b)
			}
		})
	}
}

// TestGoogleAdsNegativeKey_MatchTypeStillSeparates keeps the other half of the key honest: the
// same text at two match types is two different criteria, and Google holds both.
func TestGoogleAdsNegativeKey_MatchTypeStillSeparates(t *testing.T) {
	text := "free download"
	if googleAdsNegativeKey(googleads.MatchTypePhrase, text) == googleAdsNegativeKey(googleads.MatchTypeExact, text) {
		t.Fatal("PHRASE and EXACT must not share a key")
	}
}
