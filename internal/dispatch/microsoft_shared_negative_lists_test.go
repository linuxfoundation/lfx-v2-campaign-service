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
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// msSharedAPI is a fake Microsoft API covering the campaign tree, the shared-entity read and the
// association write. lookupBody / setStatus / setBody override those two answers.
type msSharedAPI struct {
	mu         sync.Mutex
	calls      []string
	bodies     []string
	lookupBody string
	setStatus  int
	setBody    string
}

func (a *msSharedAPI) snapshot() ([]string, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...), append([]string(nil), a.bodies...)
}

func newMSSharedDispatcher(t *testing.T, api *msSharedAPI) *MicrosoftDispatcher {
	t.Helper()
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
		api.bodies = append(api.bodies, string(b))
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch p {
		case "SharedEntities/Query":
			body := api.lookupBody
			if body == "" {
				body = `{"SharedEntities":[{"Id":4001,"Type":"NegativeKeywordList"},{"Id":4002,"Type":"NegativeKeywordList"}]}`
			}
			_, _ = io.WriteString(w, body)
		case "SharedEntityAssociations/Set":
			if api.setStatus != 0 {
				w.WriteHeader(api.setStatus)
			}
			body := api.setBody
			if body == "" {
				body = `{"PartialErrors":[]}`
			}
			_, _ = io.WriteString(w, body)
		case "Campaigns/QueryByAccountId":
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case "AdGroups/QueryByCampaignId":
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case "Ads/QueryByAdGroupId":
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case "Campaigns":
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case "AdGroups":
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case "Ads":
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		case "Keywords":
			_, _ = io.WriteString(w, `{"KeywordIds":[1111],"PartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL))
}

const msSharedExtra = `,"sharedNegativeKeywordListIds":["4001","4002"]`

// Off (the default, and any value but "true"): a non-empty list is a synchronous 400 naming the
// flag, and Dispatch refuses it again before any Microsoft call. Empty and null are absent.
func TestMicrosoftSharedNegativeLists_GateOffRefusesBothPaths(t *testing.T) {
	api := &msSharedAPI{}
	d := newMSSharedDispatcher(t, api)
	for _, v := range []string{"", "TRUE"} {
		t.Setenv(constants.EnvMicrosoftSharedNegativeListsEnabled, v)
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(msSharedExtra))
		requireConfigInvalid(t, err, "microsoftConfig.sharedNegativeKeywordListIds: Microsoft shared negative keyword lists are disabled")
		requireConfigInvalid(t, err, constants.EnvMicrosoftSharedNegativeListsEnabled)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msSharedExtra))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), constants.EnvMicrosoftSharedNegativeListsEnabled) {
		t.Errorf("Dispatch refusal must name the flag: %v", err)
	}
	if calls, _ := api.snapshot(); len(calls) != 0 {
		t.Errorf("%d Microsoft calls before the gate refused; want 0", len(calls))
	}
	for _, extra := range []string{``, `,"sharedNegativeKeywordListIds":[]`, `,"sharedNegativeKeywordListIds":null`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%q: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// On: a wrong type or a rule breach is the synchronous 400 naming the field, and Dispatch refuses
// the same input before any Microsoft call.
func TestMicrosoftSharedNegativeLists_PreflightRefusesBadInput(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftSharedNegativeListsEnabled, "true")
	api := &msSharedAPI{}
	d := newMSSharedDispatcher(t, api)
	for extra, want := range map[string]string{
		`,"sharedNegativeKeywordListIds":"4001"`:                 "microsoftConfig.sharedNegativeKeywordListIds: must be an array of strings",
		`,"SHAREDNEGATIVEKEYWORDLISTIDS":[4001]`:                 "microsoftConfig.sharedNegativeKeywordListIds: must be an array of strings",
		`,"sharedNegativeKeywordListIds":["04001"]`:              "microsoftConfig.sharedNegativeKeywordListIds: entry 1 is not a Microsoft negative keyword list id",
		`,"budget":"x","sharedNegativeKeywordListIds":["1","1"]`: "microsoftConfig.sharedNegativeKeywordListIds: entry 2 repeats",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
		requireConfigInvalid(t, err, want)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(`,"sharedNegativeKeywordListIds":["04001"]`))
	requirePreCreate(t, err)
	if calls, _ := api.snapshot(); len(calls) != 0 {
		t.Errorf("calls = %v, want none before the rules refused", calls)
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(msSharedExtra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("valid: PreflightCreate = %v, want not applicable", err)
	}
}

// On: the lists are read BEFORE the campaign is created and associated AFTER it is confirmed; the
// campaign is a clean created with every outcome recorded.
func TestMicrosoftSharedNegativeLists_DispatchChecksThenAssociates(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftSharedNegativeListsEnabled, "true")
	api := &msSharedAPI{}
	d := newMSSharedDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msSharedExtra))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	calls, bodies := api.snapshot()
	idx := func(p string) int { return slices.Index(calls, p) }
	if idx("SharedEntities/Query") < 0 || idx("SharedEntities/Query") > idx("Campaigns") || idx("SharedEntityAssociations/Set") < idx("Campaigns") {
		t.Fatalf("call order = %v", calls)
	}
	if set := bodies[idx("SharedEntityAssociations/Set")]; !strings.Contains(set, `"EntityId":321`) || !strings.Contains(set, `"SharedEntityId":4002`) {
		t.Errorf("association body = %s", set)
	}
	var blob struct {
		Lists []microsoft.SharedListOutcome `json:"sharedNegativeKeywordLists"`
	}
	if jerr := json.Unmarshal(camp.Result, &blob); jerr != nil || len(blob.Lists) != 2 || blob.Lists[1].ListID != "4002" || blob.Lists[1].Outcome != microsoft.OutcomeApplied {
		t.Fatalf("result lists = %+v (%v)", blob.Lists, jerr)
	}
}

// A list that is not on the account fails the job with nothing created.
func TestMicrosoftSharedNegativeLists_LookupRefusalCreatesNothing(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftSharedNegativeListsEnabled, "true")
	api := &msSharedAPI{lookupBody: `{"SharedEntities":[{"Id":4001,"Type":"NegativeKeywordList"}]}`}
	d := newMSSharedDispatcher(t, api)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msSharedExtra))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), `no negative keyword list "4002"`) {
		t.Errorf("err = %v", err)
	}
	if calls, _ := api.snapshot(); slices.Contains(calls, "Campaigns") {
		t.Errorf("a campaign was created after a refused lookup: %v", calls)
	}
}

// A refused or ambiguous association degrades the campaign and never fails the job.
func TestMicrosoftSharedNegativeLists_AssociationShortfallDegrades(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftSharedNegativeListsEnabled, "true")
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"one refused", 0, `{"PartialErrors":[{"Index":0,"ErrorCode":"CampaignServiceInvalidSharedEntityId","Code":4000}]}`, "1 associated, 1 failed, 0 unconfirmed"},
		{"ambiguous", http.StatusInternalServerError, `{}`, "0 associated, 0 failed, 2 unconfirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &msSharedAPI{setStatus: tc.status, setBody: tc.body}
			d := newMSSharedDispatcher(t, api)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msSharedExtra))
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if camp.Status != campaignStatusCreatedDegraded {
				t.Fatalf("status = %q, want created_degraded", camp.Status)
			}
			var blob struct {
				Steps []string `json:"steps"`
			}
			_ = json.Unmarshal(camp.Result, &blob)
			steps := strings.Join(blob.Steps, "\n")
			for _, want := range []string{tc.want, "Shared negative keyword lists not all associated"} {
				if !strings.Contains(steps, want) {
					t.Errorf("steps lack %q:\n%s", want, steps)
				}
			}
		})
	}
}
