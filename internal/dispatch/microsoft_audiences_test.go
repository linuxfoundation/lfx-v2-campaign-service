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
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// microsoftGatedFields are the microsoftConfig JSON keys that name an existing resource on the
// shared Microsoft Advertising account and so require global_marketing_ops at create (#563 gap 4).
var microsoftGatedFields = map[string]bool{
	"audienceIds":         true,
	"excludedAudienceIds": true,
}

// microsoftUngatedFields are every other microsoftConfig key, each deliberately outside the check:
// money, dates, the caller's own copy and keywords, ISO countries, a stored asset this service
// owns, and a bid adjustment that names no resource.
var microsoftUngatedFields = map[string]bool{
	"budget": true, "timeZone": true, "keywords": true, "cpcBid": true, "bidding": true, "geoTargets": true,
	"imageAssetId": true, "imageAltText": true, "startDate": true, "endDate": true, "headlines": true,
	"descriptions": true, "path1": true, "path2": true, "sitelinks": true, "callouts": true,
	"negativeKeywords": true, "audienceBidAdjustment": true,
	// #563 gaps 5-6: ISO countries, coordinates and languages name no account resource.
	"excludedGeoTargets": true, "proximityTargets": true, "languages": true,
	// #563 gap 11: a search network choice names no account resource.
	"searchPartners": true,
}

// TestEveryMicrosoftConfigFieldIsClassifiedForTheMarketingOpsGate fails when a microsoftConfig
// field is added without deciding whether it names a resource of the shared account.
func TestEveryMicrosoftConfigFieldIsClassifiedForTheMarketingOpsGate(t *testing.T) {
	typ := reflect.TypeOf(microsoftConfig{})
	for i := range typ.NumField() {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			t.Errorf("microsoftConfig.%s has no json key", typ.Field(i).Name)
			continue
		}
		if microsoftGatedFields[tag] == microsoftUngatedFields[tag] {
			t.Errorf("microsoftConfig field %q must be in exactly one of microsoftGatedFields / microsoftUngatedFields", tag)
		}
	}
}

// TestMicrosoftSharedAccountInputDetectionAgreesWithTheCreateGate pins the role check
// (service.MicrosoftMarketingOpsInputs) to what this dispatcher sends, field for field.
func TestMicrosoftSharedAccountInputDetectionAgreesWithTheCreateGate(t *testing.T) {
	for _, raw := range []string{
		`{"microsoftConfig":{"budget":5}}`,
		`{"microsoftConfig":{"audienceIds":["1"]}}`,
		`{"microsoftConfig":{"excludedAudienceIds":["2"]}}`,
		`{"microsoftConfig":{"audienceIds":[""],"excludedAudienceIds":[" "]}}`,
		`{"microsoftConfig":{"audienceIds":[],"excludedAudienceIds":null,"audienceBidAdjustment":5}}`,
		`{"microsoftConfig":{"AudienceIds":["1"],"EXCLUDEDAUDIENCEIDS":["2"]}}`,
		`{"microsoftConfig":{"audienceIds":["1"],"budget":"x"}}`,
		`{"MicrosoftConfig":{"audienceIds":["1"]}}`,
	} {
		var cfg microsoftConfig
		_ = unmarshalPlatformConfig([]byte(raw), "microsoftConfig", &cfg)
		var sent []string
		if len(cfg.AudienceIDs) > 0 {
			sent = append(sent, "audienceIds")
		}
		if len(cfg.ExcludedAudienceIDs) > 0 {
			sent = append(sent, "excludedAudienceIds")
		}
		gate := service.MicrosoftMarketingOpsInputs(json.RawMessage(raw))
		if !slices.Equal(gate, sent) {
			t.Errorf("%s: create gate names %q, dispatcher sends %q", raw, gate, sent)
		}
	}
}

// msAudAPI is a fake Microsoft API covering the campaign tree, the remarketing list lookup and
// the ad group criterion adds. lookupBody overrides the lookup answer; critStatus/critBody
// override the FIRST AdGroupCriterions answer.
type msAudAPI struct {
	mu         sync.Mutex
	calls      []string
	bodies     []string
	lookupBody string
	critStatus int
	critBody   string
	critCalls  int
}

func (a *msAudAPI) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func newMSAudDispatcher(t *testing.T, api *msAudAPI) *MicrosoftDispatcher {
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
		case "Audiences/QueryByIds":
			body := api.lookupBody
			if body == "" {
				body = `{"Audiences":[{"Id":77,"Type":"RemarketingList"},{"Id":11,"Type":"RemarketingList"}],"PartialErrors":[]}`
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
		case "CampaignCriterions":
			_, _ = io.WriteString(w, `{"CampaignCriterionIds":[8001],"NestedPartialErrors":[]}`)
		case "AdGroupCriterions":
			api.mu.Lock()
			api.critCalls++
			first := api.critCalls == 1
			api.mu.Unlock()
			if first && api.critStatus != 0 {
				w.WriteHeader(api.critStatus)
				_, _ = io.WriteString(w, api.critBody)
				return
			}
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

const msAudExtra = `,"audienceIds":["11"],"excludedAudienceIds":["77"],"audienceBidAdjustment":25`

// Off (the default): any of the three fields is a synchronous 400 naming the flag, and Dispatch
// refuses it again before any Microsoft call. Empty lists and null are absent.
func TestMicrosoftAudiences_GateOffRefusesBothPaths(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	for extra, field := range map[string]string{
		`,"audienceIds":["11"]`:              "microsoftConfig.audienceIds",
		`,"excludedAudienceIds":["77"]`:      "microsoftConfig.excludedAudienceIds",
		`,"audienceBidAdjustment":10`:        "microsoftConfig.audienceBidAdjustment",
		`,"budget":"x","audienceIds":["11"]`: "microsoftConfig.audienceIds",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
		requireConfigInvalid(t, err, field+": Microsoft remarketing lists are disabled")
		requireConfigInvalid(t, err, constants.EnvMicrosoftAudiencesEnabled)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msAudExtra))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), constants.EnvMicrosoftAudiencesEnabled) {
		t.Errorf("Dispatch refusal must name the flag: %v", err)
	}
	if n := len(api.snapshot()); n != 0 {
		t.Errorf("%d Microsoft calls before the gate refused; want 0", n)
	}
	for _, extra := range []string{``, `,"audienceIds":[]`, `,"excludedAudienceIds":null,"audienceBidAdjustment":null`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%q: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// On: a wrong type or a rule breach is the synchronous 400 naming the field.
func TestMicrosoftAudiences_PreflightRefusesBadInput(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "true")
	d := newMSAudDispatcher(t, &msAudAPI{})
	for extra, want := range map[string]string{
		`,"audienceIds":"11"`:                                     "microsoftConfig.audienceIds: must be an array of strings",
		`,"EXCLUDEDAUDIENCEIDS":[77]`:                             "microsoftConfig.excludedAudienceIds: must be an array of strings",
		`,"audienceIds":["11"],"audienceBidAdjustment":"25"`:      "microsoftConfig.audienceBidAdjustment: must be a number",
		`,"audienceIds":["0011"]`:                                 "microsoftConfig.audienceIds: entry 1 is not a Microsoft audience id",
		`,"audienceIds":["11"],"excludedAudienceIds":["11"]`:      "microsoftConfig.excludedAudienceIds: entry 1 is also in audienceIds",
		`,"excludedAudienceIds":["77"],"audienceBidAdjustment":5`: "microsoftConfig.audienceBidAdjustment: applies to audienceIds",
		`,"audienceIds":["11"],"audienceBidAdjustment":901`:       "microsoftConfig.audienceBidAdjustment: must be between -90 and 900",
		`,"budget":"x","audienceIds":["11","11"]`:                 "microsoftConfig.audienceIds: entry 2 repeats",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
		requireConfigInvalid(t, err, want)
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(msAudExtra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("valid: PreflightCreate = %v, want not applicable", err)
	}
}

// On: the lists are checked BEFORE the campaign is created and attached AFTER the keywords, the
// exclusion first; the campaign is a clean created.
func TestMicrosoftAudiences_DispatchChecksThenAttaches(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "true")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msAudExtra))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	calls := api.snapshot()
	idx := func(p string) int { return slices.Index(calls, p) }
	if idx("Audiences/QueryByIds") < 0 || idx("Audiences/QueryByIds") > idx("Campaigns") || idx("AdGroupCriterions") < idx("Keywords") {
		t.Fatalf("call order = %v", calls)
	}
	var blob struct {
		Audiences []microsoft.AudienceOutcome `json:"audiences"`
	}
	if jerr := json.Unmarshal(camp.Result, &blob); jerr != nil || len(blob.Audiences) != 2 || !blob.Audiences[0].Excluded || blob.Audiences[0].Outcome != microsoft.OutcomeApplied {
		t.Fatalf("result audiences = %+v (%v)", blob.Audiences, jerr)
	}
	api.mu.Lock()
	first := api.bodies[slices.Index(calls, "AdGroupCriterions")]
	api.mu.Unlock()
	if !strings.Contains(first, `"Type":"NegativeAdGroupCriterion"`) || !strings.Contains(first, `"AdGroupId":654`) {
		t.Errorf("first criterion call = %s, want the exclusion on ad group 654", first)
	}
}

// A list that is not a remarketing list on the account fails the job with nothing created.
func TestMicrosoftAudiences_LookupRefusalCreatesNothing(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "true")
	api := &msAudAPI{lookupBody: `{"Audiences":[{"Id":77,"Type":"RemarketingList"},null]}`}
	d := newMSAudDispatcher(t, api)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msAudExtra))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), `no remarketing list "11"`) {
		t.Errorf("err = %v", err)
	}
	if calls := api.snapshot(); slices.Contains(calls, "Campaigns") {
		t.Errorf("a campaign was created after a refused lookup: %v", calls)
	}
}

// An exclusion that does not land degrades the campaign, never fails the job, and says not to
// enable it until the exclusion is attached.
func TestMicrosoftAudiences_ExclusionFailureDegrades(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "true")
	api := &msAudAPI{critStatus: http.StatusBadRequest, critBody: `{"Code":1001,"ErrorCode":"InvalidRequest"}`}
	d := newMSAudDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msAudExtra))
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
	for _, want := range []string{"1 attached, 0 already present, 1 failed, 0 unconfirmed", "do not enable the campaign until the exclusion is attached"} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps lack %q:\n%s", want, steps)
		}
	}
}

// On an ad group REUSED from an earlier attempt no list is written (its status and audience
// setting were not read): every list is reported not sent, and the campaign degrades with a step
// saying what to check.
func TestMicrosoftAudiences_ReusedAdGroupWritesNothing(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "true")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	res, err := d.creds.resolve(context.Background(), "cncf", model.ProviderMicrosoftAds)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	creds, accountID, err := validateMicrosoftConnection("cncf", res)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	client := d.cachedMicrosoftClient("cncf", model.ProviderMicrosoftAds, res, creds, accountID)
	result := &microsoft.CampaignResult{CampaignID: "321", AdGroupID: "654", AdGroupReused: true}
	var cfg microsoftConfig
	if err := json.Unmarshal([]byte(`{"audienceIds":["11"],"excludedAudienceIds":["77"]}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !applyMicrosoftAudiences(context.Background(), client, result, cfg) {
		t.Fatal("lists skipped on a reused ad group must degrade the campaign")
	}
	if calls := api.snapshot(); len(calls) != 0 {
		t.Fatalf("calls = %v, want none", calls)
	}
	if len(result.Audiences) != 2 || !result.Audiences[0].Excluded || result.Audiences[0].ErrorCode != audienceSkippedReusedAdGroup ||
		result.Audiences[1].AudienceID != "11" || result.Audiences[1].Outcome != microsoft.OutcomeFailed {
		t.Errorf("audiences = %+v", result.Audiences)
	}
	if steps := strings.Join(result.Steps, "\n"); !strings.Contains(steps, "ad group 654 was reused from an earlier attempt") || !strings.Contains(steps, "Do not enable the campaign") {
		t.Errorf("steps = %v", result.Steps)
	}
}

// A list Microsoft reports as already on the ad group (4152) counts as attached: the campaign is
// a clean created, with no "do not enable" step.
func TestMicrosoftAudiences_AlreadyPresentIsNotDegraded(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAudiencesEnabled, "true")
	api := &msAudAPI{critStatus: http.StatusOK, critBody: `{"AdGroupCriterionIds":[null],"NestedPartialErrors":[{"Index":0,"Code":4152,"ErrorCode":"CampaignServiceDuplicateAdGroupCriterion"}]}`}
	d := newMSAudDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msAudExtra))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Fatalf("status = %q, want created", camp.Status)
	}
	var blob struct {
		Steps []string `json:"steps"`
	}
	_ = json.Unmarshal(camp.Result, &blob)
	if steps := strings.Join(blob.Steps, "\n"); !strings.Contains(steps, "1 attached, 1 already present, 0 failed") || strings.Contains(steps, "do not enable") {
		t.Errorf("steps = %s", steps)
	}
}
