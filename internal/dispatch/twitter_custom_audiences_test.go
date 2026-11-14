// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// twitterGatedFields are the twitterConfig JSON keys that name an existing resource on the shared
// X ad account and so require global_marketing_ops at create (#564 gap 3).
var twitterGatedFields = map[string]bool{
	"customAudienceIds":         true,
	"excludedCustomAudienceIds": true,
}

// twitterUngatedFields are every other twitterConfig key, each deliberately outside the check:
// the caller's own text, dates, money and bidding, a tweet id or promotable user the connection
// already authorizes (asUserId is pinned by the connection), a stored asset this service owns,
// and X's public targeting vocabulary (countries, languages).
var twitterUngatedFields = map[string]bool{
	"budgetAmount": true, "startDate": true, "endDate": true, "tweetId": true, "tweetText": true,
	"asUserId": true, "imageAssetId": true, "bidding": true, "geoTargets": true, "languages": true,
	// #564 gap 4: keyword text and X's public interest ids name no account resource.
	"keywords": true, "interestIds": true,
	// #564 gap 8: X's public conversation topic values name no account resource.
	"conversationTopics": true,
	// #564 gap 9: X's public device platform values name no account resource.
	"platforms": true,
	// #564 gap 11: a line-item setting from X's closed set names no account resource.
	"audienceExpansion": true,
}

// TestEveryTwitterConfigFieldIsClassifiedForTheMarketingOpsGate fails when a twitterConfig field
// is added without deciding whether it names a resource of the shared X ad account. An
// unclassified field would otherwise pass the create path without the role check by default.
func TestEveryTwitterConfigFieldIsClassifiedForTheMarketingOpsGate(t *testing.T) {
	typ := reflect.TypeOf(twitterConfig{})
	for i := range typ.NumField() {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			t.Errorf("twitterConfig.%s has no json key", typ.Field(i).Name)
			continue
		}
		if twitterGatedFields[tag] == twitterUngatedFields[tag] {
			t.Errorf("twitterConfig field %q must be in exactly one of twitterGatedFields / twitterUngatedFields", tag)
		}
	}
}

// TestTwitterSharedAccountInputDetectionAgreesWithTheCreateGate pins the create path's role check
// (service.TwitterMarketingOpsInputs) to what this dispatcher sends, field for field.
func TestTwitterSharedAccountInputDetectionAgreesWithTheCreateGate(t *testing.T) {
	for _, raw := range []string{
		`{"twitterConfig":{"budgetAmount":5}}`,
		`{"twitterConfig":{"customAudienceIds":["1nmth"]}}`,
		`{"twitterConfig":{"excludedCustomAudienceIds":["2906h"]}}`,
		`{"twitterConfig":{"customAudienceIds":[""],"excludedCustomAudienceIds":[" "]}}`,
		`{"twitterConfig":{"customAudienceIds":[],"excludedCustomAudienceIds":null}}`,
		// encoding/json matches keys case-insensitively, in both decoders.
		`{"twitterConfig":{"CustomAudienceIds":["1"],"EXCLUDEDCUSTOMAUDIENCEIDS":["2"]}}`,
		`{"twitterConfig":{"customAudienceIds":["1"],"budgetAmount":"x"}}`,
		`{"TwitterConfig":{"customAudienceIds":["1"]}}`,
		`{"redditConfig":{"customAudienceIds":["x"]}}`,
	} {
		var cfg twitterConfig
		_ = unmarshalPlatformConfig([]byte(raw), "twitterConfig", &cfg)
		var sent []string
		if len(cfg.CustomAudienceIDs) > 0 {
			sent = append(sent, "customAudienceIds")
		}
		if len(cfg.ExcludedCustomAudienceIDs) > 0 {
			sent = append(sent, "excludedCustomAudienceIds")
		}
		gate := service.TwitterMarketingOpsInputs(json.RawMessage(raw))
		if !slices.Equal(gate, sent) {
			t.Errorf("%s: create gate names %q, dispatcher sends %q", raw, gate, sent)
		}
	}
}

// twitterCustomAudienceServer answers a full create, the custom-audience lookup and list, recording
// each criterion POST as "<operator> <type>=<value>" and counting every request. Every audience
// it lists is targetable.
func twitterCustomAudienceServer(t *testing.T) (*TwitterDispatcher, func() []string, *atomic.Int64) {
	t.Helper()
	var mu sync.Mutex
	var criteria []string
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1/custom_audiences"):
			var els []string
			ids := strings.Split(q.Get("custom_audience_ids"), ",")
			if q.Get("custom_audience_ids") == "" {
				ids = []string{"zz9", "aa1"}
			}
			for _, id := range ids {
				els = append(els, `{"id":"`+id+`","name":"Aud `+id+`","audience_type":"WEB","targetable":true,"deleted":false}`)
			}
			_, _ = w.Write([]byte(`{"data":[` + strings.Join(els, ",") + `],"next_cursor":null}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/targeting_criteria"):
			mu.Lock()
			criteria = append(criteria, q.Get("operator_type")+" "+q.Get("targeting_type")+"="+q.Get("targeting_value"))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"data":{"id":"tc1","line_item_id":"` + q.Get("line_item_id") + `","targeting_type":"` + q.Get("targeting_type") +
				`","targeting_value":"` + q.Get("targeting_value") + `","operator_type":"` + q.Get("operator_type") + `"}}`))
		case strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
	return d, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), criteria...) }, &calls
}

// Off (the default): either list is a synchronous 400 naming the flag and the list set, and
// Dispatch refuses it again, pre-create, before any X call. Empty and null lists are absent.
func TestTwitterCustomAudiences_GateOffRefusesBothPaths(t *testing.T) {
	t.Setenv(constants.EnvTwitterCustomAudiencesEnabled, "")
	d, _, calls := twitterCustomAudienceServer(t)
	for extra, field := range map[string]string{
		`,"customAudienceIds":["1nmth"]`:                                    "twitterConfig.customAudienceIds",
		`,"excludedCustomAudienceIds":["2906h"]`:                            "twitterConfig.excludedCustomAudienceIds",
		`,"customAudienceIds":["1nmth"],"excludedCustomAudienceIds":["2a"]`: "twitterConfig.customAudienceIds",
		// An unrelated mistyped field must not let a gated list through.
		`,"startDate":7,"customAudienceIds":["1nmth"]`: "twitterConfig.customAudienceIds",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(extra))
		requireConfigInvalid(t, err, field+": X custom audiences are disabled")
		requireConfigInvalid(t, err, constants.EnvTwitterCustomAudiencesEnabled)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(`,"customAudienceIds":["1nmth"]`))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), constants.EnvTwitterCustomAudiencesEnabled) {
		t.Errorf("Dispatch refusal must name the flag: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d X calls before the gate refused; want 0", n)
	}
	for _, extra := range []string{``, `,"customAudienceIds":[]`, `,"excludedCustomAudienceIds":null`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%q: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// On: a bad id, a repeat, an overlap, an over-long list or a wrong JSON type is the synchronous
// 400 naming the field; valid lists are not refused.
func TestTwitterCustomAudiences_PreflightRefusesBadLists(t *testing.T) {
	t.Setenv(constants.EnvTwitterCustomAudiencesEnabled, "true")
	d, _, _ := twitterCustomAudienceServer(t)
	ids := make([]string, 0, twitter.MaxCustomAudienceIDs+1)
	for i := range twitter.MaxCustomAudienceIDs + 1 {
		ids = append(ids, `"a`+strconv.Itoa(i)+`"`)
	}
	many := strings.Join(ids, ",")
	for extra, want := range map[string]string{
		`,"customAudienceIds":"1nmth"`:                                       "twitterConfig.customAudienceIds: must be an array of strings",
		`,"EXCLUDEDCUSTOMAUDIENCEIDS":[7]`:                                   "twitterConfig.excludedCustomAudienceIds: must be an array of strings",
		`,"customAudienceIds":[""]`:                                          "twitterConfig.customAudienceIds: entry 1 is not an X custom audience id",
		`,"customAudienceIds":["1nm-th"]`:                                    "twitterConfig.customAudienceIds: entry 1 is not",
		`,"customAudienceIds":[" 1nmth"]`:                                    "twitterConfig.customAudienceIds: entry 1 is not",
		`,"excludedCustomAudienceIds":["a","b","a"]`:                         "twitterConfig.excludedCustomAudienceIds: entry 3 repeats",
		`,"customAudienceIds":["a1"],"excludedCustomAudienceIds":["b","a1"]`: "twitterConfig.excludedCustomAudienceIds: entry 2 is also in customAudienceIds",
		`,"budgetAmount":"bad","customAudienceIds":["x_y"]`:                  "twitterConfig.customAudienceIds: entry 1 is not",
		`,"customAudienceIds":[` + many + `]`:                                "twitterConfig.customAudienceIds: holds",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(extra))
		requireConfigInvalid(t, err, want)
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(`,"customAudienceIds":["1nmth"],"excludedCustomAudienceIds":["2906h"]`)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("valid lists: PreflightCreate = %v, want not applicable", err)
	}
}

// On: the lists reach the client, which excludes first (NE) and targets last (EQ), on the new
// line item, and the create stays a clean created.
func TestTwitterCustomAudiences_DispatchSendsTheCriteria(t *testing.T) {
	t.Setenv(constants.EnvTwitterCustomAudiencesEnabled, "true")
	d, criteria, _ := twitterCustomAudienceServer(t)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds,
		twitterTargetingCfg(`,"customAudienceIds":["1nmth","3abc"],"excludedCustomAudienceIds":["2906h"]`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	if got := strings.Join(criteria(), ","); got != "NE CUSTOM_AUDIENCE=2906h,EQ CUSTOM_AUDIENCE=1nmth,EQ CUSTOM_AUDIENCE=3abc" {
		t.Fatalf("criteria = %q", got)
	}
}

// The picker read: off answers ErrTargetingLookupUnsupported without contacting X; on lists the
// account's audiences on the create's own connection.
func TestTwitterCustomAudiences_ListGateAndRead(t *testing.T) {
	d, _, calls := twitterCustomAudienceServer(t)
	t.Setenv(constants.EnvTwitterCustomAudiencesEnabled, "")
	_, called, err := d.ListTwitterCustomAudiences(context.Background(), "cncf", model.ProviderTwitterAds)
	if !errors.Is(err, domain.ErrTargetingLookupUnsupported) || called {
		t.Fatalf("flag off: called=%v err=%v, want not called and ErrTargetingLookupUnsupported", called, err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d X calls with the flag off; want 0", n)
	}
	t.Setenv(constants.EnvTwitterCustomAudiencesEnabled, "true")
	res, called, err := d.ListTwitterCustomAudiences(context.Background(), "cncf", model.ProviderTwitterAds)
	if err != nil || !called {
		t.Fatalf("flag on: called=%v err=%v", called, err)
	}
	if res.AccountID != "acc1" || len(res.Audiences) != 2 || res.Audiences[0].ID != "zz9" || !res.Audiences[0].Targetable || res.Truncated {
		t.Fatalf("res = %+v", res)
	}
}

// All the targeting lists together are held to twitter.MaxCreateTargetingCriteria, synchronously,
// and again at Dispatch before any X call: each criterion is a paced write inside the dispatch
// budget. Countries are counted after de-duplication.
func TestTwitterCustomAudiences_TotalCriteriaAreCapped(t *testing.T) {
	t.Setenv(constants.EnvTwitterCustomAudiencesEnabled, "true")
	t.Setenv(constants.EnvTwitterCreateTargetingEnabled, "true")
	d, _, calls := twitterCustomAudienceServer(t)
	quoted := func(prefix string, n int) string {
		ids := make([]string, 0, n)
		for i := range n {
			ids = append(ids, `"`+prefix+strconv.Itoa(i)+`"`)
		}
		return strings.Join(ids, ",")
	}
	over := `,"geoTargets":["US","us"],"languages":["en"],"customAudienceIds":[` + quoted("a", twitter.MaxCreateTargetingCriteria-3) +
		`],"excludedCustomAudienceIds":[` + quoted("x", 2) + `]`
	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(over))
	requireConfigInvalid(t, err, "twitterConfig: geoTargets, languages, keywords, interestIds, conversationTopics, platforms, customAudienceIds and excludedCustomAudienceIds together name 61 targeting criteria; at most 60")
	_, err = d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(over))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), "together name 61 targeting criteria") {
		t.Errorf("Dispatch refusal = %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d X calls; want 0", n)
	}
	atCap := `,"geoTargets":["US","us"],"languages":["en"],"customAudienceIds":[` + quoted("a", twitter.MaxCreateTargetingCriteria-3) +
		`],"excludedCustomAudienceIds":[` + quoted("x", 1) + `]`
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(atCap)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("at the cap: PreflightCreate = %v, want not applicable", err)
	}
}
