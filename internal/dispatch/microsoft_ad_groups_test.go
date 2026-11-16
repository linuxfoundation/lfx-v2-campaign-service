// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// msGroupAPI is a fake Microsoft API for multi-ad-group creates: every AdGroups, Ads and Keywords
// create answers the next id (701.., 901.., 1101..), and failAdGroup makes that AdGroups create
// (1-based) a definite rejection (an ambiguous 502 when ambiguousFail); repeatAdGroupID makes
// every AdGroups create answer 701.
type msGroupAPI struct {
	mu              sync.Mutex
	calls           []string
	bodies          []string
	failAdGroup     int
	ambiguousFail   bool
	repeatAdGroupID bool
	adGroups        int
	ads             int
	keywords        int
}

func (a *msGroupAPI) snapshot() ([]string, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...), append([]string(nil), a.bodies...)
}

func newMSGroupDispatcher(t *testing.T, api *msGroupAPI) *MicrosoftDispatcher {
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
		defer api.mu.Unlock()
		api.calls = append(api.calls, p)
		api.bodies = append(api.bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		switch p {
		case "Audiences/QueryByIds":
			_, _ = io.WriteString(w, `{"Audiences":[{"Id":77,"Type":"RemarketingList"}],"PartialErrors":[]}`)
		case "Campaigns/QueryByAccountId":
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case "AdGroups/QueryByCampaignId":
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case "Ads/QueryByAdGroupId":
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case "Campaigns":
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case "AdGroups":
			api.adGroups++
			if api.adGroups == api.failAdGroup && api.ambiguousFail {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{}`)
				return
			}
			if api.adGroups == api.failAdGroup {
				_, _ = io.WriteString(w, `{"AdGroupIds":[null],"PartialErrors":[{"Index":0,"ErrorCode":"CampaignServiceInvalidAdGroupName","Code":1201}]}`)
				return
			}
			id := 700 + api.adGroups
			if api.repeatAdGroupID {
				id = 701
			}
			_, _ = fmt.Fprintf(w, `{"AdGroupIds":[%d],"PartialErrors":[]}`, id)
		case "Ads":
			api.ads++
			_, _ = fmt.Fprintf(w, `{"AdIds":[%d],"PartialErrors":[]}`, 900+api.ads)
		case "Keywords":
			api.keywords++
			_, _ = fmt.Fprintf(w, `{"KeywordIds":[%d],"PartialErrors":[]}`, 1100+api.keywords)
		case "EntityNegativeKeywords":
			_, _ = io.WriteString(w, `{"NegativeKeywordIds":[{"Ids":[4401]}],"NestedPartialErrors":[]}`)
		case "AdGroupCriterions":
			_, _ = io.WriteString(w, `{"AdGroupCriterionIds":[5001],"NestedPartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL))
}

// msGroupConfig is a campaign with the given adGroups and no single-group fields.
func msGroupConfig(groups string) json.RawMessage {
	return json.RawMessage(`{"microsoftConfig":{"budget":50,"adGroups":` + groups + `}}`)
}

const msThreeGroups = `[
 {"nameSuffix":"Alpha","keywords":[{"text":"kubecon","matchType":"Exact"}]},
 {"nameSuffix":"Beta","keywords":[{"text":"cloud native","matchType":"Phrase"}],"negativeKeywords":[{"text":"jobs","matchType":"Phrase"}]},
 {"nameSuffix":"Gamma","keywords":[{"text":"kubernetes","matchType":"Broad"}],"audienceIds":["77"],"audienceBidAdjustment":20}
]`

func msGroupResult(t *testing.T, camp *model.Campaign) (r struct {
	AdGroupID string                    `json:"adGroupId"`
	AdGroups  []microsoft.AdGroupResult `json:"adGroups"`
	Steps     []string                  `json:"steps"`
}) {
	t.Helper()
	if err := json.Unmarshal(camp.Result, &r); err != nil {
		t.Fatalf("result: %v", err)
	}
	return r
}

// Off (the default): adGroups is a synchronous 400 naming the flag, and Dispatch refuses it again
// before any Microsoft call.
func TestMicrosoftAdGroups_GateOffRefusesBothPaths(t *testing.T) {
	api := &msGroupAPI{}
	d := newMSGroupDispatcher(t, api)
	for _, v := range []string{"", "TRUE"} {
		t.Setenv(constants.EnvMicrosoftAdGroupsEnabled, v)
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msGroupConfig(`[{"nameSuffix":"A","keywords":[{"text":"k","matchType":"Exact"}]}]`))
		requireConfigInvalid(t, err, "microsoftConfig.adGroups: multiple Microsoft ad groups are disabled")
		requireConfigInvalid(t, err, constants.EnvMicrosoftAdGroupsEnabled)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msGroupConfig(msThreeGroups))
	requirePreCreate(t, err)
	if calls, _ := api.snapshot(); len(calls) != 0 {
		t.Errorf("%d Microsoft calls before the gate refused; want 0", len(calls))
	}
	for _, raw := range []string{`{"microsoftConfig":{"budget":50,"adGroups":[]}}`, `{"microsoftConfig":{"budget":50,"adGroups":null}}`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, json.RawMessage(raw)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%s: PreflightCreate = %v, want not applicable", raw, err)
		}
	}
}

// On: a wrong type or any rule breach is the synchronous 400 naming the entry, and each per-group
// field stays behind its single-group flag.
func TestMicrosoftAdGroups_PreflightRefusesBadInput(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdGroupsEnabled, "true")
	api := &msGroupAPI{}
	d := newMSGroupDispatcher(t, api)
	kw := `"keywords":[{"text":"k","matchType":"Exact"}]`
	six := strings.Repeat(`{"nameSuffix":"x",`+kw+`},`, 5) + `{"nameSuffix":"y",` + kw + `}`
	for raw, want := range map[string]string{
		`{"microsoftConfig":{"adGroups":{"nameSuffix":"A"}}}`:                                                                    "microsoftConfig.adGroups: must be an array of objects",
		`{"microsoftConfig":{"adGroups":[{"nameSuffix":7}]}}`:                                                                    "microsoftConfig.adGroups: must be an array of objects",
		`{"microsoftConfig":{"adGroups":[` + six + `]}}`:                                                                         "holds 6 ad groups; at most 5",
		`{"microsoftConfig":{"adGroups":[{` + kw + `}]}}`:                                                                        "adGroups entry 1: nameSuffix: is required",
		`{"microsoftConfig":{"adGroups":[{"nameSuffix":"A|B",` + kw + `}]}}`:                                                     `adGroups entry 1: nameSuffix: must not contain a control character or "|"`,
		`{"microsoftConfig":{"adGroups":[{"nameSuffix":"Alpha",` + kw + `},{"nameSuffix":"ALPHA",` + kw + `}]}}`:                 "adGroups entry 2: nameSuffix: repeats entry 1's suffix",
		`{"microsoftConfig":{"adGroups":[{"nameSuffix":"A"}]}}`:                                                                  "adGroups entry 1: keywords: at least one keyword is required",
		`{"microsoftConfig":{"adGroups":[{"nameSuffix":"A","keywords":[{"text":"k","matchType":"Fuzzy"}]}]}}`:                    "adGroups entry 1: keywords: keyword",
		`{"microsoftConfig":{"keywords":[{"text":"k","matchType":"Exact"}],"adGroups":[{"nameSuffix":"A",` + kw + `}]}}`:         "keywords cannot be combined with adGroups",
		`{"microsoftConfig":{"adGroups":[{"nameSuffix":"A",` + kw + `,"headlines":["a","b","c"]}]}}`:                             "microsoftConfig.adGroups entry 1: headlines disabled on this deployment",
		`{"microsoftConfig":{"adGroups":[{"nameSuffix":"A",` + kw + `,"audienceIds":["77"]}]}}`:                                  "microsoftConfig.adGroups entry 1: audienceIds: Microsoft remarketing lists are disabled",
		`{"microsoftConfig":{"adGroups":[{"nameSuffix":"A",` + kw + `,"negativeKeywords":[{"text":"j","matchType":"Exact"}]}]}}`: "microsoftConfig.adGroups entry 1: negativeKeywords: disabled on this deployment",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, json.RawMessage(raw))
		requireConfigInvalid(t, err, want)
	}
	// With the twins' flags on, their rules apply per entry.
	t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "true")
	t.Setenv(constants.EnvMicrosoftAdCopyEnabled, "true")
	requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds,
		msGroupConfig(`[{"nameSuffix":"A",`+kw+`},{"nameSuffix":"B",`+kw+`,"negativeKeywords":[{"text":"j","matchType":"Broad"}]}]`)),
		"microsoftConfig.adGroups entry 2: negativeKeywords:")
	requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds,
		msGroupConfig(`[{"nameSuffix":"A",`+kw+`,"headlines":["only one"]}]`)),
		"microsoftConfig.adGroups entry 1: headlines: must have 3 to 15 headlines")
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "true")
	requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds,
		json.RawMessage(`{"microsoftConfig":{"audienceIds":["1"],"adGroups":[{"nameSuffix":"A",`+kw+`}]}}`)),
		"audienceIds cannot be combined with adGroups")
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msGroupConfig(`[{"nameSuffix":"A"}]`))
	requirePreCreate(t, err)
	if calls, _ := api.snapshot(); len(calls) != 0 {
		t.Errorf("calls = %v, want none before the rules refused", calls)
	}
}

// On: every group is built in order with its own name, keywords and copy, its negatives at the
// ad group level and its lists on its own ad group; every id is recorded and the first group is
// the top-level one.
func TestMicrosoftAdGroups_DispatchBuildsEveryGroup(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdGroupsEnabled, "true")
	t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "true")
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "true")
	api := &msGroupAPI{}
	d := newMSGroupDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msGroupConfig(msThreeGroups))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	calls, bodies := api.snapshot()
	var groupBodies, kwBodies, negBodies, critBodies []string
	for i, c := range calls {
		switch c {
		case "AdGroups":
			groupBodies = append(groupBodies, bodies[i])
		case "Keywords":
			kwBodies = append(kwBodies, bodies[i])
		case "EntityNegativeKeywords":
			negBodies = append(negBodies, bodies[i])
		case "AdGroupCriterions":
			critBodies = append(critBodies, bodies[i])
		}
	}
	if lookup, create := slices.Index(calls, "Audiences/QueryByIds"), slices.Index(calls, "Campaigns"); lookup < 0 || lookup > create {
		t.Errorf("the group's lists were not read before the campaign was created: %v", calls)
	}
	if len(groupBodies) != 3 || len(kwBodies) != 3 {
		t.Fatalf("ad group creates %d, keyword creates %d; want 3 each (%v)", len(groupBodies), len(kwBodies), calls)
	}
	for i, suffix := range []string{"Alpha", "Beta", "Gamma"} {
		if !strings.Contains(groupBodies[i], `"Name":"LFX | Ad Group | cncf | KubeCon NA 2026 | brief-1 | `+suffix+`"`) {
			t.Errorf("ad group %d body = %s", i+1, groupBodies[i])
		}
	}
	if !strings.Contains(kwBodies[1], `"AdGroupId":702`) || !strings.Contains(kwBodies[1], "cloud native") {
		t.Errorf("group 2 keywords = %s", kwBodies[1])
	}
	if len(negBodies) != 1 || !strings.Contains(negBodies[0], `"EntityId":702`) || !strings.Contains(negBodies[0], `"EntityType":"AdGroup"`) {
		t.Errorf("negatives = %v, want one ad-group-level add on 702", negBodies)
	}
	if len(critBodies) != 1 || !strings.Contains(critBodies[0], `703`) {
		t.Errorf("lists = %v, want one add on ad group 703", critBodies)
	}
	r := msGroupResult(t, camp)
	if r.AdGroupID != "701" || len(r.AdGroups) != 3 {
		t.Fatalf("result = %+v", r)
	}
	for i, g := range r.AdGroups {
		if g.Status != microsoft.AdGroupStatusCreated || g.AdGroupID != fmt.Sprint(701+i) || g.AdID != fmt.Sprint(901+i) || len(g.KeywordIDs) != 1 {
			t.Errorf("group %d = %+v", i+1, g)
		}
	}
	if len(r.AdGroups[1].NegativeKeywords) != 1 || len(r.AdGroups[2].Audiences) != 1 {
		t.Errorf("extras = %+v / %+v", r.AdGroups[1].NegativeKeywords, r.AdGroups[2].Audiences)
	}
}

// A later group that fails leaves the earlier ones: the campaign is created_degraded with a step
// naming the failed group, nothing is rolled back, and the next group is still built.
func TestMicrosoftAdGroups_LaterGroupFailureDegrades(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdGroupsEnabled, "true")
	t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "true")
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "true")
	api := &msGroupAPI{failAdGroup: 2}
	d := newMSGroupDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msGroupConfig(msThreeGroups))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreatedDegraded {
		t.Fatalf("status = %q, want created_degraded", camp.Status)
	}
	r := msGroupResult(t, camp)
	if r.AdGroups[0].Status != microsoft.AdGroupStatusCreated || r.AdGroups[1].Status != microsoft.AdGroupStatusFailed || r.AdGroups[2].Status != microsoft.AdGroupStatusCreated {
		t.Fatalf("statuses = %+v", r.AdGroups)
	}
	if !strings.Contains(r.AdGroups[1].Error, "rejected") {
		t.Errorf("group 2 error = %q", r.AdGroups[1].Error)
	}
	if !strings.Contains(strings.Join(r.Steps, "\n"), "Ad groups NOT created: entry 2 (Beta)") {
		t.Errorf("steps = %v", r.Steps)
	}
	calls, _ := api.snapshot()
	if slices.Contains(calls, "EntityNegativeKeywords") {
		t.Errorf("the failed group's negatives were sent: %v", calls)
	}
	if camp.BiddingStrategy != nil {
		t.Errorf("bidding recorded though not every ad group carries it")
	}
}

// A later group that resolves to an earlier group's ad group is not a group of its own: it is
// recorded failed, never created, so the levers cannot address that ad group twice.
func TestMicrosoftAdGroups_LaterGroupResolvingToAnEarlierAdGroupFails(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdGroupsEnabled, "true")
	t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "true")
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "true")
	api := &msGroupAPI{repeatAdGroupID: true}
	d := newMSGroupDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msGroupConfig(msThreeGroups))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreatedDegraded {
		t.Fatalf("status = %q, want created_degraded", camp.Status)
	}
	r := msGroupResult(t, camp)
	if r.AdGroups[0].Status != microsoft.AdGroupStatusCreated {
		t.Errorf("group 1 = %+v, want created", r.AdGroups[0])
	}
	for _, i := range []int{1, 2} {
		if r.AdGroups[i].Status != microsoft.AdGroupStatusFailed || !strings.Contains(r.AdGroups[i].Error, "resolved to ad group 1's ad group 701") {
			t.Errorf("group %d = %+v, want failed as a repeat of group 1", i+1, r.AdGroups[i])
		}
	}
}

// The first group failing is today's failure: no ad group exists, so the job reports the partial
// campaign and no later group is attempted.
func TestMicrosoftAdGroups_FirstGroupFailureIsTodaysFailure(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdGroupsEnabled, "true")
	t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "true")
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "true")
	api := &msGroupAPI{failAdGroup: 1}
	d := newMSGroupDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msGroupConfig(msThreeGroups))
	if err == nil || camp == nil {
		t.Fatalf("Dispatch = (%v, %v), want the partial campaign and an error", camp, err)
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
		t.Fatalf("a campaign exists, so the claim must be retained: %v", err)
	}
	calls, _ := api.snapshot()
	n := 0
	for _, c := range calls {
		if c == "AdGroups" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("a later group was attempted after the first failed: %v", calls)
	}
}

// Without adGroups the result carries no adGroups key: the single-group path is unchanged.
func TestMicrosoftAdGroups_SingleGroupResultUnchanged(t *testing.T) {
	api := &msGroupAPI{}
	d := newMSGroupDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(``))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if strings.Contains(string(camp.Result), `"adGroups"`) {
		t.Errorf("single-group result carries adGroups: %s", camp.Result)
	}
	_, bodies := api.snapshot()
	if !slices.ContainsFunc(bodies, func(b string) bool {
		return strings.Contains(b, `"Name":"LFX | Ad Group | cncf | KubeCon NA 2026 | brief-1"`)
	}) {
		t.Errorf("single ad group name changed: %v", bodies)
	}
}

// msMultiGroupCampaign records three ad groups, the second of which the create did not make.
func msMultiGroupCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "321",
		Result: json.RawMessage(`{"accountId":"1234567","campaignId":"321","adGroupId":"654","adId":"987","keywordIds":["701"],"adGroups":[
			{"nameSuffix":"A","adGroupId":"654","adId":"987","keywordIds":["701"],"status":"created"},
			{"nameSuffix":"B","status":"failed","error":"rejected"},
			{"nameSuffix":"C","adGroupId":"655","adId":"988","keywordIds":["702"],"status":"created"}]}`),
	}
}

// ACTIVATE opens every created ad group (its ad and live keywords) BEFORE the campaign gate; a
// group the create did not make is not addressed.
func TestMicrosoftAdGroups_ToggleActivateOpensEveryGroupBeforeTheGate(t *testing.T) {
	d, calls, _ := msLeverDispatcher(t, func(_, path string) (int, string) {
		if path == "Keywords/QueryByAdGroupId" {
			return http.StatusOK, `{"Keywords":[{"Id":701,"Status":"Paused"},{"Id":702,"Status":"Paused"}]}`
		}
		return http.StatusOK, `{"PartialErrors":[]}`
	})
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msMultiGroupCampaign(), model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	var order []string
	for _, c := range msMutations(calls()) {
		id := ""
		switch {
		case strings.Contains(c.Body, "655"):
			id = "655"
		case strings.Contains(c.Body, "654"):
			id = "654"
		}
		order = append(order, c.Path+"@"+id)
	}
	want := []string{"AdGroups@655", "Ads@655", "Keywords@655", "AdGroups@654", "Ads@654", "Keywords@654", "Campaigns@"}
	if !slices.Equal(order, want) {
		t.Fatalf("mutation order = %v, want %v", order, want)
	}
}

// PAUSE closes the campaign gate FIRST, then every group.
func TestMicrosoftAdGroups_TogglePauseClosesTheGateFirst(t *testing.T) {
	d, calls, _ := msLeverDispatcher(t, func(string, string) (int, string) { return http.StatusOK, `{"PartialErrors":[]}` })
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msMultiGroupCampaign(), model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	m := msMutations(calls())
	if len(m) == 0 || m[0].Path != "Campaigns" || !strings.Contains(m[len(m)-3].Body, "655") || m[len(m)-1].Path != "Keywords" {
		t.Fatalf("mutations = %+v", m)
	}
}

// A created further ad group with no recorded keywords cannot be activated: nothing changes.
func TestMicrosoftAdGroups_ToggleActivateRefusesAnUnprovisionedGroup(t *testing.T) {
	d, calls, _ := msLeverDispatcher(t, func(string, string) (int, string) { return http.StatusOK, `{"PartialErrors":[]}` })
	camp := msMultiGroupCampaign()
	camp.Result = json.RawMessage(strings.Replace(string(camp.Result), `"keywordIds":["702"],`, ``, 1))
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunActive)
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) || !strings.Contains(err.Error(), "655") {
		t.Fatalf("err = %v", err)
	}
	if m := msMutations(calls()); len(m) != 0 {
		t.Errorf("mutations = %+v, want none", m)
	}
}

// The keyword lever acts on any of the campaign's ad groups, one per batch.
func TestMicrosoftAdGroups_KeywordLeverAddressesAnyOwnedGroup(t *testing.T) {
	d, calls, _ := msLeverDispatcher(t, func(_, path string) (int, string) {
		if path == "Keywords/QueryByAdGroupId" {
			return http.StatusOK, `{"Keywords":[{"Id":702,"Status":"Active"}]}`
		}
		return http.StatusOK, `{"PartialErrors":[]}`
	})
	out, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderMicrosoftAds, msMultiGroupCampaign(), []model.KeywordAction{
		{AdGroupID: "655", CriterionID: "702", Action: model.KeywordActionPause},
	})
	if err != nil || len(out) != 1 || out[0].Outcome != model.KeywordOutcomeApplied {
		t.Fatalf("ApplyKeywordActions = %+v, %v", out, err)
	}
	if cs := calls(); len(cs) == 0 || cs[0].Body != `{"AdGroupId":655}` {
		t.Errorf("read = %+v, want ad group 655", cs)
	}
	for _, actions := range [][]model.KeywordAction{
		{{AdGroupID: "999", CriterionID: "702", Action: model.KeywordActionPause}},
		{{AdGroupID: "655", CriterionID: "702", Action: model.KeywordActionPause}, {AdGroupID: "654", CriterionID: "702", Action: model.KeywordActionPause}},
	} {
		if _, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderMicrosoftAds, msMultiGroupCampaign(), actions); !errors.Is(err, domain.ErrKeywordActionInvalid) {
			t.Errorf("%+v: err = %v, want ErrKeywordActionInvalid", actions, err)
		}
	}
}

// The bid lever re-bids EVERY ad group the create made, first group first; a group the create did
// not make is not addressed.
func TestMicrosoftAdGroups_WriteBidRebidsEveryGroup(t *testing.T) {
	d, calls := msBudgetDispatcher(t, msBidStrategy("EnhancedCpc"), http.StatusOK, `{"PartialErrors":[]}`)
	if err := writeMSBid(d, msMultiGroupCampaign(), 2.35); err != nil {
		t.Fatalf("WriteBid: %v", err)
	}
	writes := msWrites(calls())
	if len(writes) != 2 || !strings.Contains(writes[0].Body, `"Id":654`) || !strings.Contains(writes[1].Body, `"Id":655`) {
		t.Fatalf("writes = %+v, want ad groups 654 then 655", writes)
	}
}

// A later group's write that fails ambiguously (5xx) leaves THAT group's outcome unknown: the
// error says it is unconfirmed and the campaign MAY be partly re-bid, never that it "was not"
// re-bid. A definite refusal of the later group still says it was not.
func TestMicrosoftAdGroups_WriteBidLaterGroupFailureWording(t *testing.T) {
	for _, tc := range []struct {
		name, want, notWant string
		status              int
		body                string
	}{
		{"5xx is unconfirmed", "the outcome for ad group 655 is unconfirmed (it may or may not have been re-bid), so the campaign may be partly re-bid", "655 was not", http.StatusInternalServerError, `{}`},
		{"definite refusal", "ad group 655 was not, so the campaign is partly re-bid", "may be partly", http.StatusBadRequest, `{"Code":1001,"ErrorCode":"InvalidRequest"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			var mu sync.Mutex
			d, _ := msBudgetDispatcherWith(t, msBidStrategy("EnhancedCpc"), func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				n++
				first := n == 1
				mu.Unlock()
				if first {
					_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			err := writeMSBid(d, msMultiGroupCampaign(), 2.35)
			var u interface{ Unconfirmed() bool }
			if !errors.As(err, &u) || !u.Unconfirmed() {
				t.Fatalf("err = %v, want an unconfirmed bid write", err)
			}
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("err = %v\nwant it to contain %q and not %q", err, tc.want, tc.notWant)
			}
		})
	}
}

// msConfirmedFailedGroupCampaign is msMultiGroupCampaign whose failed group B recorded a
// CONFIRMED ad group id (656) and keyword (703) before its ad step failed, plus a later entry
// that resolved to 655 again (never addressed twice).
func msConfirmedFailedGroupCampaign() *model.Campaign {
	c := msMultiGroupCampaign()
	c.Result = json.RawMessage(strings.Replace(string(c.Result),
		`{"nameSuffix":"B","status":"failed","error":"rejected"}`,
		`{"nameSuffix":"B","adGroupId":"656","keywordIds":["703"],"status":"failed","error":"ad rejected"}`, 1))
	c.Result = json.RawMessage(strings.Replace(string(c.Result), `"status":"created"}]}`,
		`"status":"created"},{"nameSuffix":"D","adGroupId":"655","status":"failed","error":"same name"}]}`, 1))
	return c
}

// A later group whose ad group id was confirmed but whose ad step failed still exists upstream
// at the old bid, so the bid lever re-bids it too (each distinct confirmed group once).
func TestMicrosoftAdGroups_WriteBidRebidsAConfirmedFailedGroup(t *testing.T) {
	d, calls := msBudgetDispatcher(t, msBidStrategy("EnhancedCpc"), http.StatusOK, `{"PartialErrors":[]}`)
	if err := writeMSBid(d, msConfirmedFailedGroupCampaign(), 2.35); err != nil {
		t.Fatalf("WriteBid: %v", err)
	}
	writes := msWrites(calls())
	var ids []string
	for _, w := range writes {
		for _, id := range []string{"654", "655", "656"} {
			if strings.Contains(w.Body, `"Id":`+id) {
				ids = append(ids, id)
			}
		}
	}
	if strings.Join(ids, ",") != "654,656,655" {
		t.Fatalf("re-bid ad groups = %v, want 654,656,655", ids)
	}
}

// The keyword lever owns a confirmed group whose ad step failed: its confirmed keywords are the
// campaign's to act on. Activation still refuses nothing new and never addresses that group.
func TestMicrosoftAdGroups_KeywordLeverOwnsAConfirmedFailedGroup(t *testing.T) {
	d, calls, _ := msLeverDispatcher(t, func(_, path string) (int, string) {
		if path == "Keywords/QueryByAdGroupId" {
			return http.StatusOK, `{"Keywords":[{"Id":703,"Status":"Active"}]}`
		}
		return http.StatusOK, `{"PartialErrors":[]}`
	})
	out, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderMicrosoftAds, msConfirmedFailedGroupCampaign(), []model.KeywordAction{
		{AdGroupID: "656", CriterionID: "703", Action: model.KeywordActionPause},
	})
	if err != nil || len(out) != 1 || out[0].Outcome != model.KeywordOutcomeApplied {
		t.Fatalf("ApplyKeywordActions = %+v, %v", out, err)
	}
	if cs := calls(); len(cs) == 0 || cs[0].Body != `{"AdGroupId":656}` {
		t.Errorf("read = %+v, want ad group 656", cs)
	}
}

// Activation keeps its completeness rule: the failed group, confirmed id or not, is not one of
// the groups it opens.
func TestMicrosoftAdGroups_ToggleActivateSkipsAConfirmedFailedGroup(t *testing.T) {
	d, calls, _ := msLeverDispatcher(t, func(_, path string) (int, string) {
		if path == "Keywords/QueryByAdGroupId" {
			return http.StatusOK, `{"Keywords":[{"Id":701,"Status":"Paused"},{"Id":702,"Status":"Paused"}]}`
		}
		return http.StatusOK, `{"PartialErrors":[]}`
	})
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msConfirmedFailedGroupCampaign(), model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	for _, c := range msMutations(calls()) {
		if strings.Contains(c.Body, "656") {
			t.Errorf("activation addressed the failed group: %+v", c)
		}
	}
}

// A later group recorded failed after its ad and some keywords were created (a later keyword
// failed) has a confirmed ad group id, so the bid lever writes it; when that write fails
// ambiguously the bid write is UNCONFIRMED naming that group, never a success.
func TestMicrosoftAdGroups_WriteBidConfirmedFailedGroupWriteFailureIsUnconfirmed(t *testing.T) {
	camp := msMultiGroupCampaign()
	camp.Result = json.RawMessage(strings.Replace(string(camp.Result),
		`{"nameSuffix":"B","status":"failed","error":"rejected"}`,
		`{"nameSuffix":"B","adGroupId":"656","adId":"989","keywordIds":["703"],"status":"failed","error":"keyword 2 rejected"}`, 1))
	// The helper has already read the PUT body, so the writes are told apart by order: 654 is
	// first, 656 second (asserted below).
	var (
		mu sync.Mutex
		n  int
	)
	d, calls := msBudgetDispatcherWith(t, msBidStrategy("EnhancedCpc"), func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})
	err := writeMSBid(d, camp, 2.35)
	var u interface{ Unconfirmed() bool }
	if !errors.As(err, &u) || !u.Unconfirmed() || !strings.Contains(err.Error(), "the outcome for ad group 656 is unconfirmed") {
		t.Fatalf("err = %v, want an unconfirmed bid write naming ad group 656", err)
	}
	writes := msWrites(calls())
	if len(writes) != 2 || !strings.Contains(writes[1].Body, `"Id":656`) {
		t.Fatalf("writes = %+v, want 654 then 656 (stopping at the failure)", writes)
	}
}

// A later group whose ad group create was AMBIGUOUS (5xx) records no id and is marked
// unconfirmed; a definite rejection is not.
func TestMicrosoftAdGroups_AmbiguousLaterGroupIsRecordedUnconfirmed(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdGroupsEnabled, "true")
	t.Setenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled, "true")
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "true")
	for _, ambiguous := range []bool{true, false} {
		api := &msGroupAPI{failAdGroup: 2, ambiguousFail: ambiguous}
		d := newMSGroupDispatcher(t, api)
		camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msGroupConfig(msThreeGroups))
		if err != nil {
			t.Fatalf("ambiguous=%v: Dispatch: %v", ambiguous, err)
		}
		g := msGroupResult(t, camp).AdGroups[1]
		if g.Status != microsoft.AdGroupStatusFailed || g.AdGroupID != "" || g.Unconfirmed != ambiguous {
			t.Errorf("ambiguous=%v: group 2 = %+v", ambiguous, g)
		}
	}
}

// While a later group is unconfirmed with no recorded id, an ad group may exist upstream at the
// old bid that no write can reach: the bid write is refused (ErrBidUnwritable, nothing written)
// naming the entry and asking for reconciliation, never a success.
func TestMicrosoftAdGroups_WriteBidRefusesWhileALaterGroupIsUnconfirmed(t *testing.T) {
	camp := msMultiGroupCampaign()
	camp.Result = json.RawMessage(strings.Replace(string(camp.Result),
		`{"nameSuffix":"B","status":"failed","error":"rejected"}`,
		`{"nameSuffix":"B","status":"failed","error":"ad group create UNCONFIRMED","unconfirmed":true}`, 1))
	d, calls := msBudgetDispatcher(t, msBidStrategy("EnhancedCpc"), http.StatusOK, `{"PartialErrors":[]}`)
	err := writeMSBid(d, camp, 2.35)
	if !errors.Is(err, domain.ErrBidUnwritable) || !strings.Contains(err.Error(), "entry 2 (B)") || !strings.Contains(err.Error(), "reconcile it") {
		t.Fatalf("err = %v, want ErrBidUnwritable naming entry 2 (B) and reconciliation", err)
	}
	if w := msWrites(calls()); len(w) != 0 {
		t.Fatalf("writes = %+v, want none", w)
	}
	// A definite failure with no id (msMultiGroupCampaign as is) does not block the write.
	if err := writeMSBid(d, msMultiGroupCampaign(), 2.35); err != nil {
		t.Fatalf("definite failure: WriteBid = %v", err)
	}
}
