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
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// msNegAPI is a fake Microsoft API covering the campaign tree and the campaign negative-keyword
// add. negStatus/negBody override the EntityNegativeKeywords answer; campaignStatus overrides the
// campaign create.
type msNegAPI struct {
	mu             sync.Mutex
	calls          []string
	bodies         map[string]string
	negStatus      int
	negBody        string
	campaignStatus int
}

func (a *msNegAPI) count(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		if c == path {
			n++
		}
	}
	return n
}

func (a *msNegAPI) total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func newMSNegDispatcher(t *testing.T, api *msNegAPI) *MicrosoftDispatcher {
	t.Helper()
	api.bodies = map[string]string{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p := r.URL.Path[strings.Index(r.URL.Path, "/v13/")+len("/v13/"):]
		api.mu.Lock()
		api.calls = append(api.calls, p)
		api.bodies[p] = string(b)
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch p {
		case "Campaigns/QueryByAccountId":
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case "AdGroups/QueryByCampaignId":
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case "Ads/QueryByAdGroupId":
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case "Campaigns":
			if api.campaignStatus != 0 {
				w.WriteHeader(api.campaignStatus)
				_, _ = io.WriteString(w, `{}`)
				return
			}
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case "AdGroups":
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case "Ads":
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		case "Keywords":
			_, _ = io.WriteString(w, `{"KeywordIds":[1111],"PartialErrors":[]}`)
		case "EntityNegativeKeywords":
			if api.negStatus != 0 {
				w.WriteHeader(api.negStatus)
				_, _ = io.WriteString(w, api.negBody)
				return
			}
			_, _ = io.WriteString(w, `{"NegativeKeywordIds":[{"Ids":[9001,9002]}],"NestedPartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL))
}

func msNegConfig(extra string) json.RawMessage {
	return json.RawMessage(`{"microsoftConfig":{"budget":50,"keywords":[{"text":"kubecon","matchType":"Exact"}]` + extra + `}}`)
}

const msNegExtra = `,"negativeKeywords":[{"text":"  jobs ","matchType":"PHRASE"},{"text":"free","matchType":"exact"}]`

// Off (the default) and on any value but "true": a non-empty list is a synchronous 400 naming the
// flag, and Dispatch refuses it again, pre-create, with no Microsoft call at all.
func TestMicrosoftNegatives_GateOffRefusesBothPaths(t *testing.T) {
	for _, v := range []string{"", "TRUE", "1", "yes"} {
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, v)
			api := &msNegAPI{}
			d := newMSNegDispatcher(t, api)
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(msNegExtra))
			requireConfigInvalid(t, err, constants.EnvMicrosoftCreateNegativeKeywordsEnabled)
			requireConfigInvalid(t, err, "microsoftConfig.negativeKeywords")

			_, err = d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msNegExtra))
			requirePreCreate(t, err)
			if !strings.Contains(err.Error(), constants.EnvMicrosoftCreateNegativeKeywordsEnabled) {
				t.Errorf("Dispatch refusal must name the flag: %v", err)
			}
			if n := api.total(); n != 0 {
				t.Errorf("%d Microsoft API calls before the gate refused; want 0", n)
			}
		})
	}
}

// Absent, empty and null lists are not "set": no refusal with the gate off, and no
// negative-keyword call on Dispatch.
func TestMicrosoftNegatives_AbsentEmptyOrNullIsNotGated(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "")
	for _, extra := range []string{"", `,"negativeKeywords":[]`, `,"negativeKeywords":null`} {
		api := &msNegAPI{}
		d := newMSNegDispatcher(t, api)
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
		if !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%q: PreflightCreate = %v, want ErrPreflightNotApplicable", extra, err)
		}
		camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(extra))
		if err != nil {
			t.Fatalf("%q: Dispatch: %v", extra, err)
		}
		if camp.Status != campaignStatusCreated {
			t.Errorf("%q: status = %q, want created", extra, camp.Status)
		}
		if n := api.count("EntityNegativeKeywords"); n != 0 {
			t.Errorf("%q: %d negative-keyword calls, want 0", extra, n)
		}
	}
}

// The rules are the lever's (microsoft.ValidateNegativeKeywords), refused synchronously with the
// gate on; the shape check refuses a wrong JSON type, including under a case-variant key, and an
// unrelated mistyped field does not let a bad list through the best-effort decode.
func TestMicrosoftNegatives_PreflightRules(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "true")
	var many []string
	for i := 0; i < 61; i++ {
		many = append(many, `{"text":"term`+strings.Repeat("x", i)+`","matchType":"Exact"}`)
	}
	cases := []struct {
		name, cfg, want string
	}{
		{"broad refused", string(msNegConfig(`,"negativeKeywords":[{"text":"jobs","matchType":"Broad"}]`)), "unsupported match type"},
		{"unknown match type", string(msNegConfig(`,"negativeKeywords":[{"text":"jobs","matchType":"Fuzzy"}]`)), "unsupported match type"},
		{"empty text", string(msNegConfig(`,"negativeKeywords":[{"text":"   ","matchType":"Exact"}]`)), "is empty"},
		{"bad character", string(msNegConfig(`,"negativeKeywords":[{"text":"jobs/2026","matchType":"Exact"}]`)), "not allowed"},
		{"duplicate", string(msNegConfig(`,"negativeKeywords":[{"text":"Jobs","matchType":"Exact"},{"text":"jobs","matchType":"EXACT"}]`)), "repeats an earlier entry"},
		{"over cap", string(msNegConfig(`,"negativeKeywords":[` + strings.Join(many, ",") + `]`)), "at most 60"},
		{"not an array", string(msNegConfig(`,"negativeKeywords":"jobs"`)), "must be an array"},
		{"array of strings", string(msNegConfig(`,"negativeKeywords":["jobs"]`)), "must be an array"},
		{"case-variant key", string(msNegConfig(`,"NegativeKeywords":{"text":"jobs"}`)), "must be an array"},
		{"unrelated decode error", `{"microsoftConfig":{"budget":"bad","negativeKeywords":[{"text":"jobs","matchType":"Broad"}]}}`, "unsupported match type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newMSNegDispatcher(t, &msNegAPI{})
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, json.RawMessage(tc.cfg))
			requireConfigInvalid(t, err, "microsoftConfig.negativeKeywords: ")
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not contain %q", err.Error(), tc.want)
			}
			if strings.Contains(err.Error(), "microsoft-ads:") {
				t.Errorf("refusal must not carry the client's prefix: %v", err)
			}
		})
	}
	// A valid list passes preflight.
	d := newMSNegDispatcher(t, &msNegAPI{})
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(msNegExtra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("valid list: PreflightCreate = %v, want ErrPreflightNotApplicable", err)
	}
}

// Dispatch with the gate on: a rule breach is refused pre-create with no Microsoft call (the
// re-check for a path that skipped PreflightCreate).
func TestMicrosoftNegatives_DispatchRechecksRules(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "true")
	api := &msNegAPI{}
	d := newMSNegDispatcher(t, api)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds,
		msNegConfig(`,"negativeKeywords":[{"text":"jobs","matchType":"Broad"}]`))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), "microsoftConfig.negativeKeywords") {
		t.Errorf("Dispatch refusal must name the field: %v", err)
	}
	if n := api.total(); n != 0 {
		t.Errorf("%d Microsoft API calls before the refusal; want 0", n)
	}
}

type msNegResultBlob struct {
	NegativeKeywords []struct {
		Text              string `json:"text"`
		MatchType         string `json:"matchType"`
		Outcome           string `json:"outcome"`
		NegativeKeywordID string `json:"negativeKeywordId"`
		ErrorCode         string `json:"errorCode"`
	} `json:"negativeKeywords"`
	Steps []string `json:"steps"`
}

func decodeMSNegResult(t *testing.T, camp *model.Campaign) msNegResultBlob {
	t.Helper()
	var b msNegResultBlob
	if err := json.Unmarshal(camp.Result, &b); err != nil {
		t.Fatalf("result blob: %v", err)
	}
	return b
}

// The happy path: the normalized list goes to the CREATED campaign as campaign-level negatives,
// after the tree exists, and the positional outcomes and the snapshot are recorded.
func TestMicrosoftNegatives_DispatchAttachesToTheCreatedCampaign(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "true")
	api := &msNegAPI{}
	d := newMSNegDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msNegExtra))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	if n := api.count("EntityNegativeKeywords"); n != 1 {
		t.Fatalf("%d negative-keyword calls, want 1", n)
	}
	api.mu.Lock()
	last := api.calls[len(api.calls)-1]
	body := api.bodies["EntityNegativeKeywords"]
	api.mu.Unlock()
	if last != "EntityNegativeKeywords" {
		t.Errorf("the negatives must be added after the tree is built; last call = %s", last)
	}
	var sent struct {
		EntityNegativeKeywords []struct {
			EntityId         json.Number
			EntityType       string
			NegativeKeywords []struct{ Text, MatchType string }
		}
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil || len(sent.EntityNegativeKeywords) != 1 {
		t.Fatalf("body = %s (%v)", body, err)
	}
	e := sent.EntityNegativeKeywords[0]
	if e.EntityId.String() != "321" || e.EntityType != "Campaign" {
		t.Errorf("entity = %s/%s, want 321/Campaign", e.EntityId, e.EntityType)
	}
	if len(e.NegativeKeywords) != 2 || e.NegativeKeywords[0].Text != "jobs" || e.NegativeKeywords[0].MatchType != "Phrase" ||
		e.NegativeKeywords[1].Text != "free" || e.NegativeKeywords[1].MatchType != "Exact" {
		t.Errorf("negatives sent = %+v, want normalized jobs/Phrase, free/Exact", e.NegativeKeywords)
	}
	blob := decodeMSNegResult(t, camp)
	if len(blob.NegativeKeywords) != 2 || blob.NegativeKeywords[0].Outcome != "APPLIED" || blob.NegativeKeywords[0].NegativeKeywordID != "9001" ||
		blob.NegativeKeywords[1].NegativeKeywordID != "9002" {
		t.Errorf("result negativeKeywords = %+v", blob.NegativeKeywords)
	}
	if !strings.Contains(strings.Join(blob.Steps, "\n"), "Negative keywords: 2 attached") {
		t.Errorf("steps = %v", blob.Steps)
	}
	var snap microsoftConfig
	if err := json.Unmarshal(camp.ConfigSnapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.NegativeKeywords) != 2 || snap.NegativeKeywords[0].Text != "jobs" || snap.NegativeKeywords[0].MatchType != "Phrase" ||
		snap.NegativeKeywords[1].Text != "free" || snap.NegativeKeywords[1].MatchType != "Exact" {
		t.Errorf("snapshot negativeKeywords = %+v", snap.NegativeKeywords)
	}
}

// A dispatch that reuses a campaign by name gets 4335 for each negative already there: ALREADY_PRESENT,
// not a failure, so the campaign is not degraded.
func TestMicrosoftNegatives_AlreadyPresentIsNotDegraded(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "true")
	api := &msNegAPI{negStatus: http.StatusOK, negBody: `{"NegativeKeywordIds":[{"Ids":[null,null]}],"NestedPartialErrors":[{"BatchErrors":[` +
		`{"Code":4335,"ErrorCode":"CampaignServiceNegativeKeywordAlreadyExists","Index":0},` +
		`{"Code":4335,"ErrorCode":"CampaignServiceNegativeKeywordAlreadyExists","Index":1}],"Code":0,"ErrorCode":""}]}`}
	d := newMSNegDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msNegExtra))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	blob := decodeMSNegResult(t, camp)
	if len(blob.NegativeKeywords) != 2 || blob.NegativeKeywords[0].Outcome != "ALREADY_PRESENT" || blob.NegativeKeywords[1].Outcome != "ALREADY_PRESENT" {
		t.Errorf("outcomes = %+v", blob.NegativeKeywords)
	}
}

// A shortfall degrades the CREATED campaign and never fails the job; the step says whether to
// verify (UNCONFIRMED) or re-add (NOT attached / failed), and upstream text never reaches it.
func TestMicrosoftNegatives_ShortfallDegrades(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantStep string
		outcomes int
	}{
		{"one item failed", 200, `{"NegativeKeywordIds":[{"Ids":[9001,null]}],"NestedPartialErrors":[{"BatchErrors":[{"Code":1005,"ErrorCode":"CampaignServiceInvalidNegativeKeyword","Index":1,"Message":"` + msLeakText + `"}],"Code":0,"ErrorCode":""}]}`, "1 failed", 2},
		{"one item unattributed", 200, `{"NegativeKeywordIds":[{"Ids":[9001,null]}],"NestedPartialErrors":[]}`, "1 unconfirmed", 2},
		{"campaign refused", 200, `{"NegativeKeywordIds":[{"Ids":[null,null]}],"NestedPartialErrors":[{"BatchErrors":null,"Code":1100,"ErrorCode":"CampaignServiceInvalidCampaignId","Message":"` + msLeakText + `"}]}`, "Negative keywords NOT attached", 0},
		{"definite 400", 400, `{"Code":3000,"Message":"` + msLeakText + `"}`, "Negative keywords NOT attached", 0},
		{"ambiguous 502", 502, `{"Message":"` + msLeakText + `"}`, "Negative keywords UNCONFIRMED", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "true")
			api := &msNegAPI{negStatus: tc.status, negBody: tc.body}
			d := newMSNegDispatcher(t, api)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msNegExtra))
			if err != nil {
				t.Fatalf("the campaign exists, so the job must not fail: %v", err)
			}
			if camp.PlatformCampaignID != "321" || camp.Status != campaignStatusCreatedDegraded {
				t.Errorf("campaign = %q status %q, want 321 created_degraded", camp.PlatformCampaignID, camp.Status)
			}
			blob := decodeMSNegResult(t, camp)
			if !strings.Contains(strings.Join(blob.Steps, "\n"), "add-negative-keywords") {
				t.Errorf("a shortfall step must point at add-negative-keywords, got:\n%s", strings.Join(blob.Steps, "\n"))
			}
			if !strings.Contains(strings.Join(blob.Steps, "\n"), tc.wantStep) {
				t.Errorf("steps must record %q, got:\n%s", tc.wantStep, strings.Join(blob.Steps, "\n"))
			}
			if len(blob.NegativeKeywords) != tc.outcomes {
				t.Errorf("%d outcomes recorded, want %d", len(blob.NegativeKeywords), tc.outcomes)
			}
			if strings.Contains(string(camp.Result), msLeakText) {
				t.Error("upstream message text leaked into the persisted result")
			}
		})
	}
}

// An UNCONFIRMED campaign create skips the negatives: there is no confirmed campaign to add them to.
func TestMicrosoftNegatives_UnconfirmedCreateSkipsNegatives(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "true")
	api := &msNegAPI{campaignStatus: http.StatusBadGateway}
	d := newMSNegDispatcher(t, api)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msNegExtra))
	if err == nil {
		t.Fatal("an ambiguous campaign create must fail the dispatch")
	}
	if n := api.count("EntityNegativeKeywords"); n != 0 {
		t.Errorf("%d negative-keyword calls after an unconfirmed create, want 0", n)
	}
}
