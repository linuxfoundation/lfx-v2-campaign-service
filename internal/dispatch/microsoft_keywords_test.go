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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// Both levers are reached by TYPE ASSERTION; a drifted signature would silently answer
// "unsupported" for every Microsoft campaign.
var (
	_ service.KeywordActioner      = (*MicrosoftDispatcher)(nil)
	_ service.NegativeKeywordAdder = (*MicrosoftDispatcher)(nil)
)

type msKwCall struct{ Method, Path, Body string }

// msKeywordDispatcher wires a MicrosoftDispatcher against a fake Microsoft API whose answers
// come from handle(method, path-after-v13), recording every request with its body. Token and
// API requests are counted separately so a "refused before contacting Microsoft" test can
// assert neither happened.
func msKeywordDispatcher(t *testing.T, handle func(method, path string) (int, string)) (*MicrosoftDispatcher, func() []msKwCall, func() int) {
	t.Helper()
	var (
		mu     sync.Mutex
		calls  []msKwCall
		tokens int
	)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		tokens++
		mu.Unlock()
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path := r.URL.Path[strings.Index(r.URL.Path, "/v13/")+len("/v13/"):]
		mu.Lock()
		calls = append(calls, msKwCall{r.Method, path, string(b)})
		mu.Unlock()
		status, body := handle(r.Method, path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(apiSrv.Close)
	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	return d, func() []msKwCall {
			mu.Lock()
			defer mu.Unlock()
			return append([]msKwCall(nil), calls...)
		}, func() int {
			mu.Lock()
			defer mu.Unlock()
			return tokens
		}
}

// msKeywordCampaign records account 1234567 (what activeMicrosoftConn resolves to), campaign
// 321, ad group 654 and keywords 701/702.
func msKeywordCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "321",
		Result: json.RawMessage(`{"accountId":"1234567","campaignId":"321","adGroupId":"654","adId":"987","keywordIds":["701","702"]}`),
	}
}

const msLiveKeywords = `{"Keywords":[{"Id":701,"Status":"Active"},{"Id":702,"Status":"Paused"},{"Id":703,"Status":"Deleted"}]}`

// msKeywordAPI answers the live-keyword read with msLiveKeywords and every mutation with
// mutate.
func msKeywordAPI(mutate string) func(string, string) (int, string) {
	return func(_ string, path string) (int, string) {
		if path == "Keywords/QueryByAdGroupId" {
			return http.StatusOK, msLiveKeywords
		}
		return http.StatusOK, mutate
	}
}

func msMutations(calls []msKwCall) []msKwCall {
	var out []msKwCall
	for _, c := range calls {
		if c.Method == http.MethodPut || c.Method == http.MethodDelete || (c.Method == http.MethodPost && !strings.Contains(c.Path, "/Query")) {
			out = append(out, c)
		}
	}
	return out
}

// ---- keyword actions ----------------------------------------------------------

func TestMicrosoft_ApplyKeywordActions_HappyPathReadsThenMutates(t *testing.T) {
	d, calls, _ := msKeywordDispatcher(t, msKeywordAPI(`{"PartialErrors":[]}`))
	out, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderMicrosoftAds, msKeywordCampaign(), []model.KeywordAction{
		{AdGroupID: "654", CriterionID: "701", Action: model.KeywordActionPause},
		{AdGroupID: "654", CriterionID: "702", Action: model.KeywordActionRemove},
	})
	if err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}
	cs := calls()
	if len(cs) != 3 || cs[0].Path != "Keywords/QueryByAdGroupId" || cs[0].Body != `{"AdGroupId":654}` {
		t.Fatalf("want the ownership read first, then two mutations; got %+v", cs)
	}
	if cs[1].Method != http.MethodPut || cs[2].Method != http.MethodDelete {
		t.Errorf("want PUT (pause) then DELETE (remove), got %s then %s", cs[1].Method, cs[2].Method)
	}
	if len(out) != 2 || out[0].CriterionID != "701" || out[1].CriterionID != "702" {
		t.Fatalf("outcomes not positional: %+v", out)
	}
	for i, o := range out {
		if o.Outcome != model.KeywordOutcomeApplied || o.ResourceName != "" {
			t.Errorf("outcome[%d] = %+v, want APPLIED with no resource name", i, o)
		}
	}
}

func TestMicrosoft_ApplyKeywordActions_PartialErrorsArePositional(t *testing.T) {
	d, _, _ := msKeywordDispatcher(t, func(method, path string) (int, string) {
		switch {
		case path == "Keywords/QueryByAdGroupId":
			return http.StatusOK, msLiveKeywords
		case method == http.MethodDelete:
			return http.StatusOK, `{"PartialErrors":[{"Code":1501,"ErrorCode":"CampaignServiceInvalidKeywordId","Index":0}]}`
		default:
			return http.StatusOK, `{"PartialErrors":[]}`
		}
	})
	out, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderMicrosoftAds, msKeywordCampaign(), []model.KeywordAction{
		{AdGroupID: "654", CriterionID: "702", Action: model.KeywordActionRemove},
		{AdGroupID: "654", CriterionID: "701", Action: model.KeywordActionPause},
	})
	if err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}
	if out[0].Outcome != model.KeywordOutcomeFailed || out[0].ErrorCode != "CampaignServiceInvalidKeywordId" || out[1].Outcome != model.KeywordOutcomeApplied {
		t.Fatalf("outcomes = %+v, want [FAILED APPLIED]", out)
	}
}

// Every refusal that is a fact about the request or the row happens before Microsoft is
// contacted at all — no token, no read, no mutation.
func TestMicrosoft_ApplyKeywordActions_RefusedBeforeAnyCall(t *testing.T) {
	foreign := msKeywordCampaign()
	foreign.Result = json.RawMessage(`{"campaignId":"321","adGroupId":"654","adId":"987"}`)
	noAdGroup := msKeywordCampaign()
	noAdGroup.Result = json.RawMessage(`{"accountId":"1234567","campaignId":"321"}`)
	for _, tc := range []struct {
		name     string
		campaign *model.Campaign
		actions  []model.KeywordAction
		want     error
	}{
		{"another ad group", msKeywordCampaign(), []model.KeywordAction{{AdGroupID: "655", CriterionID: "701", Action: "PAUSE"}}, domain.ErrKeywordActionInvalid},
		{"malformed id", msKeywordCampaign(), []model.KeywordAction{{AdGroupID: "654", CriterionID: "07", Action: "PAUSE"}}, domain.ErrKeywordActionInvalid},
		{"malformed batch beats unprovisioned", noAdGroup, []model.KeywordAction{{AdGroupID: "654", CriterionID: "701", Action: "ENABLE"}}, domain.ErrKeywordActionInvalid},
		{"no ad group", noAdGroup, []model.KeywordAction{{AdGroupID: "654", CriterionID: "701", Action: "PAUSE"}}, domain.ErrCampaignNotProvisioned},
		{"provenance unknown fails closed", foreign, []model.KeywordAction{{AdGroupID: "654", CriterionID: "701", Action: "PAUSE"}}, domain.ErrCampaignProvenanceUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, calls, tokens := msKeywordDispatcher(t, msKeywordAPI(`{"PartialErrors":[]}`))
			_, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderMicrosoftAds, tc.campaign, tc.actions)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if n := len(calls()); n != 0 || tokens() != 0 {
				t.Errorf("Microsoft was contacted (%d API calls, %d token calls)", n, tokens())
			}
		})
	}
}

func TestMicrosoft_ApplyKeywordActions_AccountMismatchNeverMutates(t *testing.T) {
	camp := msKeywordCampaign()
	camp.Result = json.RawMessage(`{"accountId":"7654321","campaignId":"321","adGroupId":"654"}`)
	d, calls, _ := msKeywordDispatcher(t, msKeywordAPI(`{"PartialErrors":[]}`))
	_, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderMicrosoftAds, camp, []model.KeywordAction{{AdGroupID: "654", CriterionID: "701", Action: "REMOVE"}})
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Fatalf("err = %v, want ErrCampaignAccountMismatch", err)
	}
	if n := len(calls()); n != 0 {
		t.Errorf("Microsoft API was called %d times for a foreign-account campaign", n)
	}
}

// A keyword id that is not a live keyword of THIS ad group — another ad group's, or a deleted
// one — is refused after the read and before any mutation.
func TestMicrosoft_ApplyKeywordActions_KeywordNotInAdGroupRefusedBeforeMutating(t *testing.T) {
	for _, id := range []string{"999", "703"} {
		t.Run(id, func(t *testing.T) {
			d, calls, _ := msKeywordDispatcher(t, msKeywordAPI(`{"PartialErrors":[]}`))
			_, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderMicrosoftAds, msKeywordCampaign(), []model.KeywordAction{
				{AdGroupID: "654", CriterionID: "701", Action: "PAUSE"},
				{AdGroupID: "654", CriterionID: id, Action: "REMOVE"},
			})
			if !errors.Is(err, domain.ErrKeywordActionInvalid) {
				t.Fatalf("err = %v, want ErrKeywordActionInvalid", err)
			}
			if m := msMutations(calls()); len(m) != 0 {
				t.Errorf("a mutation was sent: %+v", m)
			}
		})
	}
}

// A failed ownership READ is a definite failure: nothing was mutated, so it must not be
// reported as unconfirmed.
func TestMicrosoft_ApplyKeywordActions_ReadFailureIsDefinite(t *testing.T) {
	d, calls, _ := msKeywordDispatcher(t, func(string, string) (int, string) { return http.StatusBadGateway, `{}` })
	_, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderMicrosoftAds, msKeywordCampaign(), []model.KeywordAction{{AdGroupID: "654", CriterionID: "701", Action: "PAUSE"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	var u interface{ Unconfirmed() bool }
	if errors.As(err, &u) && u.Unconfirmed() {
		t.Errorf("a failed read must not be unconfirmed: %v", err)
	}
	if m := msMutations(calls()); len(m) != 0 {
		t.Errorf("a mutation was sent: %+v", m)
	}
}

// Whole-call ambiguity, including a refusal that followed a 429, reaches the service as
// Unconfirmed().
func TestMicrosoft_ApplyKeywordActions_AmbiguityIsUnconfirmed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func() func(string, string) (int, string)
	}{
		{"5xx", func() func(string, string) (int, string) {
			return func(_, path string) (int, string) {
				if path == "Keywords/QueryByAdGroupId" {
					return http.StatusOK, msLiveKeywords
				}
				return http.StatusInternalServerError, `{}`
			}
		}},
		{"429 then refusal", func() func(string, string) (int, string) {
			n := 0
			return func(_, path string) (int, string) {
				if path == "Keywords/QueryByAdGroupId" {
					return http.StatusOK, msLiveKeywords
				}
				n++
				if n == 1 {
					return http.StatusTooManyRequests, `{}`
				}
				return http.StatusBadRequest, `{"ErrorCode":"CampaignServiceInvalidKeywordId"}`
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := msKeywordDispatcher(t, tc.answer())
			_, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderMicrosoftAds, msKeywordCampaign(), []model.KeywordAction{{AdGroupID: "654", CriterionID: "701", Action: "REMOVE"}})
			var u interface{ Unconfirmed() bool }
			if !errors.As(err, &u) || !u.Unconfirmed() {
				t.Fatalf("want an Unconfirmed() error, got %T: %v", err, err)
			}
		})
	}
}

// ---- negative keywords --------------------------------------------------------

func TestMicrosoft_AddNegativeKeywords_HappyPath(t *testing.T) {
	d, calls, _ := msKeywordDispatcher(t, func(string, string) (int, string) {
		return http.StatusOK, `{"NegativeKeywordIds":[{"Ids":[9001,null]}],"NestedPartialErrors":[{"BatchErrors":[{"Code":4335,"Index":1}]}]}`
	})
	out, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderMicrosoftAds, msKeywordCampaign(), []model.NegativeKeyword{
		{Text: "free", MatchType: "Exact"}, {Text: "cheap", MatchType: "Phrase"},
	})
	if err != nil {
		t.Fatalf("AddNegativeKeywords: %v", err)
	}
	if out[0].Outcome != model.KeywordOutcomeApplied || out[0].NegativeKeywordID != "9001" || out[1].Outcome != model.KeywordOutcomeAlreadyPresent {
		t.Fatalf("outcomes = %+v", out)
	}
	cs := calls()
	if len(cs) != 1 || cs[0].Path != "EntityNegativeKeywords" || !strings.Contains(cs[0].Body, `"EntityId":321,"EntityType":"Campaign"`) {
		t.Errorf("request = %+v — the campaign id must come from the row", cs)
	}
}

func TestMicrosoft_AddNegativeKeywords_RefusedBeforeAnyCall(t *testing.T) {
	foreign := msKeywordCampaign()
	foreign.Result = json.RawMessage(`{"accountId":"7654321","campaignId":"321"}`)
	unknown := msKeywordCampaign()
	unknown.Result = json.RawMessage(`{"campaignId":"321"}`)
	unprovisioned := msKeywordCampaign()
	unprovisioned.PlatformCampaignID = ""
	good := []model.NegativeKeyword{{Text: "free", MatchType: "Exact"}}
	for _, tc := range []struct {
		name     string
		campaign *model.Campaign
		keywords []model.NegativeKeyword
		want     error
	}{
		{"broad match", msKeywordCampaign(), []model.NegativeKeyword{{Text: "free", MatchType: "Broad"}}, domain.ErrNegativeKeywordInvalid},
		{"over 100 characters", msKeywordCampaign(), []model.NegativeKeyword{{Text: strings.Repeat("a", 101), MatchType: "Exact"}}, domain.ErrNegativeKeywordInvalid},
		{"malformed batch beats unprovisioned", unprovisioned, []model.NegativeKeyword{{Text: "a@b", MatchType: "Exact"}}, domain.ErrNegativeKeywordInvalid},
		{"unprovisioned", unprovisioned, good, domain.ErrCampaignNotProvisioned},
		{"provenance unknown fails closed", unknown, good, domain.ErrCampaignProvenanceUnknown},
		{"account mismatch", foreign, good, domain.ErrCampaignAccountMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, calls, _ := msKeywordDispatcher(t, func(string, string) (int, string) { return http.StatusOK, `{}` })
			_, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderMicrosoftAds, tc.campaign, tc.keywords)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if n := len(calls()); n != 0 {
				t.Errorf("Microsoft API was called %d times", n)
			}
		})
	}
}

func TestMicrosoft_AddNegativeKeywords_AmbiguityIsUnconfirmed(t *testing.T) {
	d, calls, _ := msKeywordDispatcher(t, func(string, string) (int, string) { return http.StatusTooManyRequests, `{}` })
	_, err := d.AddNegativeKeywords(context.Background(), "proj", model.ProviderMicrosoftAds, msKeywordCampaign(), []model.NegativeKeyword{{Text: "free", MatchType: "Exact"}})
	var u interface{ Unconfirmed() bool }
	if !errors.As(err, &u) || !u.Unconfirmed() {
		t.Fatalf("a 429 on the add must be Unconfirmed(), got %T: %v", err, err)
	}
	if len(calls()) != 1 {
		t.Errorf("the add must not be retried, got %d calls", len(calls()))
	}
}

// ---- interaction with the status cascade -------------------------------------

// A keyword removed through ApplyKeywordActions stays in the row's keywordIds (nothing is
// persisted). The cascade must skip it, or every later toggle would be a partial cascade.
func TestMicrosoft_ToggleStatus_SkipsKeywordsRemovedUpstream(t *testing.T) {
	for _, status := range []string{model.CampaignRunActive, model.CampaignRunPaused} {
		t.Run(status, func(t *testing.T) {
			d, calls, _ := msKeywordDispatcher(t, func(_, path string) (int, string) {
				if path == "Keywords/QueryByAdGroupId" {
					// 702 was removed: Microsoft no longer lists it.
					return http.StatusOK, `{"Keywords":[{"Id":701,"Status":"Paused"}]}`
				}
				return http.StatusOK, `{"PartialErrors":[]}`
			})
			if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msKeywordCampaign(), status); err != nil {
				t.Fatalf("ToggleStatus: %v", err)
			}
			var kwPut *msKwCall
			for _, c := range calls() {
				if c.Method == http.MethodPut && c.Path == "Keywords" {
					c := c
					kwPut = &c
				}
			}
			if kwPut == nil || strings.Contains(kwPut.Body, "702") || !strings.Contains(kwPut.Body, "701") {
				t.Fatalf("keyword PUT = %+v, want only the live keyword 701", kwPut)
			}
		})
	}
}

func TestMicrosoft_ToggleStatus_ActivateWithEveryKeywordRemovedIsNotProvisioned(t *testing.T) {
	d, calls, _ := msKeywordDispatcher(t, func(_, path string) (int, string) {
		if path == "Keywords/QueryByAdGroupId" {
			return http.StatusOK, `{"Keywords":[]}`
		}
		return http.StatusOK, `{"PartialErrors":[]}`
	})
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msKeywordCampaign(), model.CampaignRunActive)
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Fatalf("err = %v, want ErrCampaignNotProvisioned", err)
	}
	if m := msMutations(calls()); len(m) != 0 {
		t.Errorf("nothing may be activated, got %+v", m)
	}
}

// Stopping delivery must not depend on the read: a PAUSE whose read fails still pauses, with
// the recorded keyword ids.
func TestMicrosoft_ToggleStatus_PauseSurvivesAFailedKeywordRead(t *testing.T) {
	d, calls, _ := msKeywordDispatcher(t, func(_, path string) (int, string) {
		if path == "Keywords/QueryByAdGroupId" {
			return http.StatusBadGateway, `{}`
		}
		return http.StatusOK, `{"PartialErrors":[]}`
	})
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msKeywordCampaign(), model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	sawCampaignPause := false
	for _, c := range calls() {
		if c.Method == http.MethodPut && c.Path == "Campaigns" && strings.Contains(c.Body, `"Paused"`) {
			sawCampaignPause = true
		}
	}
	if !sawCampaignPause {
		t.Error("the campaign gate must still be paused")
	}
}
