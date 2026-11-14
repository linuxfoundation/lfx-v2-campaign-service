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

// msGoalAPI is a fake Microsoft API covering the campaign tree and the conversion goal read.
// lookupBody overrides the read's answer; existingCampaign makes the name lookup find the
// campaign (a retry that reuses it).
type msGoalAPI struct {
	mu               sync.Mutex
	calls            []string
	bodies           []string
	lookupBody       string
	existingCampaign bool
}

func (a *msGoalAPI) snapshot() ([]string, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...), append([]string(nil), a.bodies...)
}

func newMSGoalDispatcher(t *testing.T, api *msGoalAPI) *MicrosoftDispatcher {
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
		case "ConversionGoals/QueryByIds":
			body := api.lookupBody
			if body == "" {
				body = `{"ConversionGoals":[{"Id":5001,"Name":"Registration","Status":"Active","Type":"Url","Scope":"Account"},{"Id":5002,"Name":"Purchase","Status":"Active","Type":"Event","Scope":"Customer"}],"PartialErrors":[]}`
			}
			_, _ = io.WriteString(w, body)
		case "Campaigns/QueryByAccountId":
			if api.existingCampaign {
				_, _ = io.WriteString(w, `{"Campaigns":[{"Id":321,"Name":"LFX | Search Campaign | cncf | KubeCon NA 2026 | brief-1"}]}`)
				return
			}
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

const (
	msGoalExtra      = `,"conversionGoalIds":["5001","5002"]`
	msGoalTargetCpa  = msGoalExtra + `,"bidding":{"strategy":"targetCpa","targetCpa":25.5}`
	msGoalMaxConvert = msGoalExtra + `,"bidding":{"strategy":"maxConversions"}`
)

// Off (the default, and any value but "true"): the goals AND the conversion strategies are each a
// synchronous 400 naming the flag, and Dispatch refuses them again before any Microsoft call.
func TestMicrosoftConversionGoals_GateOffRefusesBothPaths(t *testing.T) {
	api := &msGoalAPI{}
	d := newMSGoalDispatcher(t, api)
	for _, v := range []string{"", "TRUE"} {
		t.Setenv(constants.EnvMicrosoftConversionGoalsEnabled, v)
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(msGoalExtra))
		requireConfigInvalid(t, err, "microsoftConfig.conversionGoalIds: Microsoft conversion goals are disabled")
		requireConfigInvalid(t, err, constants.EnvMicrosoftConversionGoalsEnabled)
		err = d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(msGoalMaxConvert))
		requireConfigInvalid(t, err, `microsoftConfig.bidding: strategy "maxConversions" is disabled`)
		requireConfigInvalid(t, err, constants.EnvMicrosoftConversionGoalsEnabled)
	}
	for _, extra := range []string{msGoalExtra, msGoalTargetCpa} {
		_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(extra))
		requirePreCreate(t, err)
		if !strings.Contains(err.Error(), constants.EnvMicrosoftConversionGoalsEnabled) {
			t.Errorf("Dispatch refusal must name the flag: %v", err)
		}
	}
	if calls, _ := api.snapshot(); len(calls) != 0 {
		t.Errorf("%d Microsoft calls before the gate refused; want 0", len(calls))
	}
	for _, extra := range []string{``, `,"conversionGoalIds":[]`, `,"conversionGoalIds":null`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%q: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// On: a wrong type, a rule breach or a conversion strategy without its preconditions is the
// synchronous 400 naming the field, and Dispatch refuses the same input before any call.
func TestMicrosoftConversionGoals_PreflightRefusesBadInput(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftConversionGoalsEnabled, "true")
	api := &msGoalAPI{}
	d := newMSGoalDispatcher(t, api)
	for extra, want := range map[string]string{
		`,"conversionGoalIds":"5001"`:                                                                                            "microsoftConfig.conversionGoalIds: must be an array of strings",
		`,"CONVERSIONGOALIDS":[5001]`:                                                                                            "microsoftConfig.conversionGoalIds: must be an array of strings",
		`,"conversionGoalIds":["05001"]`:                                                                                         "microsoftConfig.conversionGoalIds: entry 1 is not a Microsoft conversion goal id",
		`,"budget":"x","conversionGoalIds":["1","1"]`:                                                                            "microsoftConfig.conversionGoalIds: entry 2 repeats",
		`,"bidding":{"strategy":"maxConversions"}`:                                                                               `strategy "maxConversions" needs at least one existing conversion goal`,
		`,"bidding":{"strategy":"targetCpa","targetCpa":10}`:                                                                     `strategy "targetCpa" needs at least one existing conversion goal`,
		msGoalExtra + `,"bidding":{"strategy":"targetCpa"}`:                                                                      `strategy "targetCpa" requires targetCpa`,
		msGoalExtra + `,"bidding":{"strategy":"targetCpa","targetCpa":null}`:                                                     "targetCpa must be a number",
		msGoalExtra + `,"bidding":{"strategy":"targetCpa","targetCpa":"25"}`:                                                     "targetCpa must be a number",
		msGoalExtra + `,"bidding":{"strategy":"targetCpa","targetCpa":0}`:                                                        "targetCpa must be greater than zero",
		msGoalExtra + `,"bidding":{"strategy":"targetCpa","targetCpa":12.345}`:                                                   "has more than two decimal places",
		msGoalExtra + `,"bidding":{"strategy":"maxConversions","targetCpa":10}`:                                                  `targetCpa is only accepted with strategy "targetCpa"`,
		msGoalExtra + `,"bidding":{"strategy":"maxConversions","bidAmount":1}`:                                                   `bidAmount is not accepted with strategy "maxConversions"`,
		msGoalExtra + `,"cpcBid":1.5,"bidding":{"strategy":"maxConversions"}`:                                                    `cpcBid sets a manual ad group bid, which strategy "maxConversions" ignores`,
		msGoalExtra + `,"bidding":{"strategy":"manual","bidAmount":1,"targetCpa":3}`:                                             `targetCpa is only accepted by microsoftConfig with strategy "targetCpa"`,
		msGoalExtra + `,"bidding":{"strategy":"MaxConversions"}`:                                                                 `strategy must be "automatic" or "manual"`,
		msGoalExtra + `,"bidding":{"strategy":"targetCpa","TargetCpa":10}`:                                                       `use "targetCpa" (exact spelling)`,
		msGoalExtra + `,"bidding":{"strategy":"targetCpa","targetCpa":10,"targetRoas":2}`:                                        `unknown bidding field "targetRoas"`,
		`,"conversionGoalIds":["1","2","3","4","5","6","7","8","9","10","11","12","13","14","15","16","17","18","19","20","21"]`: "holds 21 ids",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
		requireConfigInvalid(t, err, want)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(`,"bidding":{"strategy":"maxConversions"}`))
	requirePreCreate(t, err)
	if calls, _ := api.snapshot(); len(calls) != 0 {
		t.Errorf("calls = %v, want none before the rules refused", calls)
	}
	for _, extra := range []string{msGoalExtra, msGoalTargetCpa, msGoalMaxConvert} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%s: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// targetCpa is microsoftConfig's alone: on any other platform's bidding it is an unknown field,
// exactly as before it existed.
func TestBiddingTargetCpaIsMicrosoftOnly(t *testing.T) {
	for _, key := range []string{"linkedInConfig", "redditConfig", "metaConfig", "twitterConfig"} {
		raw := json.RawMessage(`{"` + key + `":{"bidding":{"strategy":"automatic","targetCpa":5}}}`)
		requireConfigInvalid(t, checkBiddingShape(raw, key), `unknown bidding field "targetCpa"`)
	}
	if err := checkBiddingShape(json.RawMessage(`{"microsoftConfig":{"bidding":{"strategy":"targetCpa","targetCpa":5}}}`), "microsoftConfig"); err != nil {
		t.Errorf("microsoftConfig targetCpa refused by the shape check: %v", err)
	}
	if _, err := resolveBidding("redditConfig", &biddingConfig{Strategy: biddingStrategyMaxConversions, strategyPresent: true}); err == nil {
		t.Error("another platform accepted the maxConversions strategy")
	}
}

// On: the goals are read BEFORE the campaign is created; the campaign is created with them as
// GoalIds and the MaxConversions scheme with the target, and the row records automatic bidding.
func TestMicrosoftConversionGoals_DispatchChecksThenCreates(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftConversionGoalsEnabled, "true")
	for _, tc := range []struct {
		name, extra, scheme string
	}{
		{"targetCpa", msGoalTargetCpa, `"BiddingScheme":{"Type":"MaxConversions","TargetCpa":25.5}`},
		{"maxConversions", msGoalMaxConvert, `"BiddingScheme":{"Type":"MaxConversions"}`},
		{"goals only", msGoalExtra, `"BiddingScheme":{"Type":"MaxClicks"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &msGoalAPI{}
			d := newMSGoalDispatcher(t, api)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(tc.extra))
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if camp.Status != campaignStatusCreated {
				t.Errorf("status = %q, want created", camp.Status)
			}
			calls, bodies := api.snapshot()
			lookup, create := slices.Index(calls, "ConversionGoals/QueryByIds"), slices.Index(calls, "Campaigns")
			if lookup < 0 || create < 0 || lookup > create {
				t.Fatalf("call order = %v", calls)
			}
			if !strings.Contains(bodies[lookup], `"ConversionGoalIds":[5001,5002]`) || !strings.Contains(bodies[lookup], `"ConversionGoalTypes":"Url Duration PagesViewedPerVisit Event AppInstall OfflineConversion InStoreTransaction"`) {
				t.Errorf("lookup body = %s", bodies[lookup])
			}
			if !strings.Contains(bodies[create], `"GoalIds":[5001,5002]`) || !strings.Contains(bodies[create], tc.scheme) {
				t.Errorf("campaign body = %s, want GoalIds and %s", bodies[create], tc.scheme)
			}
			var blob struct {
				Goals []string `json:"conversionGoalIds"`
			}
			if jerr := json.Unmarshal(camp.Result, &blob); jerr != nil || !slices.Equal(blob.Goals, []string{"5001", "5002"}) {
				t.Errorf("result goals = %v (%v)", blob.Goals, jerr)
			}
			if camp.BiddingStrategy == nil || *camp.BiddingStrategy != model.BiddingAutomatic || camp.MaxCPCBid != nil {
				t.Errorf("row bidding = %v / %v, want automatic with no bid", camp.BiddingStrategy, camp.MaxCPCBid)
			}
		})
	}
}

// A goal the account does not return, or returns Paused, fails the job with nothing created.
func TestMicrosoftConversionGoals_LookupRefusalCreatesNothing(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftConversionGoalsEnabled, "true")
	for body, want := range map[string]string{
		`{"ConversionGoals":[{"Id":5001,"Status":"Active"},null],"PartialErrors":[{"Index":1,"ErrorCode":"InvalidConversionGoalId","Code":4000}]}`: `no conversion goal "5002"`,
		`{"ConversionGoals":[{"Id":5001,"Status":"Active"},{"Id":5002,"Status":"Paused"}]}`:                                                        `conversion goal "5002" is Paused`,
		`{"ConversionGoals":[{"Id":5001,"Status":"Active"}]}`:                                                                                      "could not be confirmed",
		`{}`: "could not be confirmed",
	} {
		api := &msGoalAPI{lookupBody: body}
		d := newMSGoalDispatcher(t, api)
		_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msGoalTargetCpa))
		requirePreCreate(t, err)
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", body, err, want)
		}
		if calls, _ := api.snapshot(); slices.Contains(calls, "Campaigns") {
			t.Errorf("%s: a campaign was created after a refused lookup: %v", body, calls)
		}
	}
}

// A campaign found by name keeps its goals and bidding: none is recorded, and a step says so.
func TestMicrosoftConversionGoals_ReusedCampaignNotChanged(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftConversionGoalsEnabled, "true")
	api := &msGoalAPI{existingCampaign: true}
	d := newMSGoalDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msGoalTargetCpa))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if calls, _ := api.snapshot(); slices.Contains(calls, "Campaigns") {
		t.Fatalf("a reused campaign was re-created: %v", calls)
	}
	var blob struct {
		Goals []string `json:"conversionGoalIds"`
		Steps []string `json:"steps"`
	}
	_ = json.Unmarshal(camp.Result, &blob)
	if len(blob.Goals) != 0 {
		t.Errorf("goals recorded on a reused campaign: %v", blob.Goals)
	}
	if !strings.Contains(strings.Join(blob.Steps, "\n"), "conversion goals NOT applied to reused campaign 321") {
		t.Errorf("steps = %v", blob.Steps)
	}
	if camp.BiddingStrategy != nil {
		t.Errorf("bidding recorded on a reused campaign: %v", *camp.BiddingStrategy)
	}
}

// The picker: off answers ErrTargetingLookupUnsupported without contacting Microsoft; on lists the
// account's goals on the create's own connection with the pre-create check's call.
func TestMicrosoftConversionGoals_PickerGateAndRead(t *testing.T) {
	api := &msGoalAPI{}
	d := newMSGoalDispatcher(t, api)
	for _, v := range []string{"", "TRUE"} {
		t.Setenv(constants.EnvMicrosoftConversionGoalsEnabled, v)
		_, called, err := d.ListMicrosoftConversionGoals(context.Background(), "cncf", model.ProviderMicrosoftAds)
		if !errors.Is(err, domain.ErrTargetingLookupUnsupported) || called || !strings.Contains(err.Error(), constants.EnvMicrosoftConversionGoalsEnabled) {
			t.Fatalf("flag %q: called=%v err=%v", v, called, err)
		}
	}
	if calls, _ := api.snapshot(); len(calls) != 0 {
		t.Fatalf("%d Microsoft calls with the flag off; want 0", len(calls))
	}
	t.Setenv(constants.EnvMicrosoftConversionGoalsEnabled, "true")
	res, called, err := d.ListMicrosoftConversionGoals(context.Background(), "cncf", model.ProviderMicrosoftAds)
	if err != nil || !called {
		t.Fatalf("flag on: called=%v err=%v", called, err)
	}
	if res.AccountID != "1234567" || len(res.Goals) != 2 || res.Goals[1] != (model.MicrosoftConversionGoal{ID: "5002", Name: "Purchase", Status: "Active", Type: "Event", Scope: "Customer"}) {
		t.Fatalf("res = %+v", res)
	}
	if calls, bodies := api.snapshot(); len(calls) != 1 || calls[0] != "ConversionGoals/QueryByIds" || !strings.Contains(bodies[0], `"ConversionGoalIds":null`) {
		t.Fatalf("calls = %v %v", calls, bodies)
	}
	d2 := newMSGoalDispatcher(t, &msGoalAPI{lookupBody: `{}`})
	if res, called, err := d2.ListMicrosoftConversionGoals(context.Background(), "cncf", model.ProviderMicrosoftAds); !errors.Is(err, microsoft.ErrConversionGoalsMalformed) || !called || res != nil {
		t.Fatalf("unbelievable answer: got (%+v, %v, %v)", res, called, err)
	}
}
