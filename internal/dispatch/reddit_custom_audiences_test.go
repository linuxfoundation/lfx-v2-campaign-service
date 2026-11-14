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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// redditGatedFields are the redditConfig JSON keys that name an existing resource on the shared
// Reddit ad account and so require global_marketing_ops at create (#549).
var redditGatedFields = map[string]bool{
	"customAudienceIds":         true,
	"excludedCustomAudienceIds": true,
}

// redditUngatedFields are every other redditConfig key, each deliberately outside the check: the
// caller's own text, URLs, dates, money and bidding, Reddit's public targeting vocabulary
// (communities, interests, keywords, countries, the video goal), a stored asset this service
// owns, and the conversion pixel, which identifies the advertiser the connection already names.
var redditUngatedFields = map[string]bool{
	"budgetUsd": true, "startDate": true, "endDate": true, "objective": true, "geoTargets": true,
	"subreddits": true, "interests": true, "keywords": true, "variants": true, "postUrl": true,
	"imageUrl": true, "imageAssetId": true, "callToAction": true, "conversionPixelId": true,
	"videoGoal": true, "bidding": true,
	// Exclusions (#549) are public vocabulary too: community names and ISO country codes.
	"excludedSubreddits": true, "excludedGeoTargets": true,
	// conversionEvent (#549) names a pixel event from Reddit's public optimization-goal enum.
	"conversionEvent": true,
	// Placements, platforms and expansion (#549 gap 7) are Reddit's fixed targeting enums.
	"placements": true, "platforms": true, "expandTargeting": true,
	// budgetType (#549 gap 9) is the pacing of the caller's own budget.
	"budgetType": true,
}

// TestEveryRedditConfigFieldIsClassifiedForTheMarketingOpsGate fails when a redditConfig field is
// added without deciding whether it names a resource of the shared Reddit ad account. An
// unclassified field would otherwise pass the create path without the role check by default.
func TestEveryRedditConfigFieldIsClassifiedForTheMarketingOpsGate(t *testing.T) {
	typ := reflect.TypeOf(redditConfig{})
	for i := range typ.NumField() {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			t.Errorf("redditConfig.%s has no json key", typ.Field(i).Name)
			continue
		}
		if redditGatedFields[tag] == redditUngatedFields[tag] {
			t.Errorf("redditConfig field %q must be in exactly one of redditGatedFields / redditUngatedFields", tag)
		}
	}
}

// TestRedditSharedAccountInputDetectionAgreesWithTheCreateGate pins the create path's role check
// (service.RedditMarketingOpsInputs) to what this dispatcher sends, field for field: every list
// the dispatcher would send as a custom audience must be one the role check named.
func TestRedditSharedAccountInputDetectionAgreesWithTheCreateGate(t *testing.T) {
	for _, raw := range []string{
		`{"redditConfig":{"budgetUsd":5}}`,
		`{"redditConfig":{"customAudienceIds":["1"]}}`,
		`{"redditConfig":{"excludedCustomAudienceIds":["2"]}}`,
		`{"redditConfig":{"customAudienceIds":[""],"excludedCustomAudienceIds":[" "]}}`,
		`{"redditConfig":{"customAudienceIds":[],"excludedCustomAudienceIds":null}}`,
		// encoding/json matches keys case-insensitively, in both decoders.
		`{"redditConfig":{"CustomAudienceIds":["1"],"EXCLUDEDCUSTOMAUDIENCEIDS":["2"]}}`,
		`{"redditConfig":{"customAudienceIds":["1"],"budgetUsd":"x"}}`,
		`{"RedditConfig":{"customAudienceIds":["1"]}}`,
		`{"googleAdsConfig":{"customAudiences":["x"]}}`,
	} {
		var cfg redditConfig
		_ = unmarshalPlatformConfig([]byte(raw), "redditConfig", &cfg)
		var sent []string
		if len(cfg.CustomAudienceIDs) > 0 {
			sent = append(sent, "customAudienceIds")
		}
		if len(cfg.ExcludedCustomAudienceIDs) > 0 {
			sent = append(sent, "excludedCustomAudienceIds")
		}
		gate := service.RedditMarketingOpsInputs(json.RawMessage(raw))
		if !slices.Equal(gate, sent) {
			t.Errorf("%s: create gate names %q, dispatcher sends %q", raw, gate, sent)
		}
	}
}

func redditAudienceCfg(extra string) json.RawMessage {
	return json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic"` + extra + `}}`)
}

func TestReddit_PreflightCreate_CustomAudiences(t *testing.T) {
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{})
	for _, tc := range []struct {
		name    string
		enabled bool
		extra   string
		wantErr string // "" = ErrPreflightNotApplicable
	}{
		{"none set, flag off", false, ``, ""},
		{"empty lists, flag off", false, `,"customAudienceIds":[],"excludedCustomAudienceIds":null`, ""},
		{"set while off", false, `,"customAudienceIds":["123"]`, "redditConfig.customAudienceIds: Reddit custom audiences are disabled"},
		{"excluded set while off", false, `,"excludedCustomAudienceIds":["123"]`, "redditConfig.excludedCustomAudienceIds: Reddit custom audiences are disabled"},
		{"both set while off", false, `,"customAudienceIds":["1"],"excludedCustomAudienceIds":["2"]`, constants.EnvRedditCustomAudiencesEnabled},
		// An unrelated mistyped field must not turn a gated or invalid list into an accepted job.
		{"off despite unrelated decode error", false, `,"startDate":7,"customAudienceIds":["123"]`, constants.EnvRedditCustomAudiencesEnabled},
		{"invalid despite unrelated decode error", true, `,"startDate":7,"customAudienceIds":[""]`, "customAudienceIds entry 1 is not"},
		{"valid while on", true, `,"customAudienceIds":["123","t2_x"],"excludedCustomAudienceIds":["456"]`, ""},
		{"not an array", true, `,"customAudienceIds":"123"`, "customAudienceIds must be an array of strings"},
		{"numbers, not strings", true, `,"excludedCustomAudienceIds":[123]`, "excludedCustomAudienceIds must be an array of strings"},
		{"case variant mistyped", true, `,"CustomAudienceIds":{}`, "customAudienceIds must be an array of strings"},
		{"mistyped while off is still a type error", false, `,"customAudienceIds":7`, "customAudienceIds must be an array of strings"},
		{"blank id", true, `,"customAudienceIds":[""]`, "customAudienceIds entry 1 is not"},
		{"path characters", true, `,"customAudienceIds":["1","../2"]`, "customAudienceIds entry 2 is not"},
		{"padded id", true, `,"customAudienceIds":[" 1"]`, "customAudienceIds entry 1 is not"},
		{"duplicate", true, `,"excludedCustomAudienceIds":["1","1"]`, "excludedCustomAudienceIds entry 2 repeats"},
		{"both targeted and excluded", true, `,"customAudienceIds":["1"],"excludedCustomAudienceIds":["2","1"]`, "excludedCustomAudienceIds entry 2 is also in customAudienceIds"},
		{"over the cap", true, `,"customAudienceIds":[` + idList(101) + `]`, "at most 100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.enabled {
				t.Setenv(constants.EnvRedditCustomAudiencesEnabled, "true")
			} else {
				t.Setenv(constants.EnvRedditCustomAudiencesEnabled, "")
			}
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAudienceCfg(tc.extra))
			if tc.wantErr == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want ErrPreflightNotApplicable", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("PreflightCreate = %v, want ErrCreateConfigInvalid containing %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "redditConfig.") {
				t.Errorf("refusal %q must name the redditConfig field", err)
			}
		})
	}
}

// idList is n distinct valid ids, comma-separated, as JSON strings.
func idList(n int) string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = `"a` + strconv.Itoa(i) + `"`
	}
	return strings.Join(ids, ",")
}

// fakeRedditAPI serves a token and a Reddit API that records each ad-group create body. When
// rejectCommunitiesOnce is set, the first ad-group create answers Reddit's "invalid communities"
// 400, so the dispatcher's single fallback retry runs.
type fakeRedditAPI struct {
	mu                    sync.Mutex
	adGroupBodies         []map[string]any
	rejectCommunitiesOnce bool
	requests              int
}

func (f *fakeRedditAPI) start(t *testing.T) (apiURL, tokURL string) {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ad_groups"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			f.adGroupBodies = append(f.adGroupBodies, body)
			if f.rejectCommunitiesOnce && len(f.adGroupBodies) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"You cannot set invalid communities"}}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	return api.URL + "/api/v3", tok.URL
}

func targetingOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	data, _ := body["data"].(map[string]any)
	targeting, ok := data["targeting"].(map[string]any)
	if !ok {
		t.Fatalf("ad-group body has no targeting object: %v", body)
	}
	return targeting
}

// TestReddit_Dispatch_SendsCustomAudiencesOnEveryAdGroupAttempt drives the whole create: the two
// lists reach the ad group's targeting as custom_audience_ids / excluded_custom_audience_ids,
// verbatim and in order, and the communities fallback retry still carries them.
func TestReddit_Dispatch_SendsCustomAudiencesOnEveryAdGroupAttempt(t *testing.T) {
	t.Setenv(constants.EnvRedditCustomAudiencesEnabled, "true")
	fake := &fakeRedditAPI{rejectCommunitiesOnce: true}
	apiURL, tokURL := fake.start(t)
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(apiURL), reddit.WithTokenURL(tokURL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := redditAudienceCfg(`,"subreddits":["nosuchsub"],"customAudienceIds":["67547345732543","t2_b"],"excludedCustomAudienceIds":["999"]`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "cmp_1" {
		t.Fatalf("Dispatch = %+v, want the created campaign", camp)
	}
	if len(fake.adGroupBodies) != 2 {
		t.Fatalf("ad-group creates = %d, want 2 (the rejected one and the fallback retry)", len(fake.adGroupBodies))
	}
	for i, body := range fake.adGroupBodies {
		tg := targetingOf(t, body)
		if got := tg["custom_audience_ids"]; !reflect.DeepEqual(got, []any{"67547345732543", "t2_b"}) {
			t.Errorf("attempt %d custom_audience_ids = %v", i+1, got)
		}
		if got := tg["excluded_custom_audience_ids"]; !reflect.DeepEqual(got, []any{"999"}) {
			t.Errorf("attempt %d excluded_custom_audience_ids = %v", i+1, got)
		}
	}
	if _, ok := targetingOf(t, fake.adGroupBodies[1])["communities"]; ok {
		t.Error("the fallback retry must drop communities")
	}
	var result map[string]any
	_ = json.Unmarshal(camp.Result, &result)
	steps, _ := json.Marshal(result["steps"])
	if !strings.Contains(string(steps), "2 custom audiences") || !strings.Contains(string(steps), "1 excluded custom audiences") {
		t.Errorf("steps %s must report the custom audiences sent", steps)
	}
}

// TestReddit_Dispatch_NoCustomAudiencesSendsNoMembers: a config without them sends neither
// member, so a create from before #549 is byte-for-byte what it was.
func TestReddit_Dispatch_NoCustomAudiencesSendsNoMembers(t *testing.T) {
	t.Setenv(constants.EnvRedditCustomAudiencesEnabled, "true")
	fake := &fakeRedditAPI{}
	apiURL, tokURL := fake.start(t)
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(apiURL), reddit.WithTokenURL(tokURL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }))
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAudienceCfg(``)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(fake.adGroupBodies) != 1 {
		t.Fatalf("ad-group creates = %d, want 1", len(fake.adGroupBodies))
	}
	tg := targetingOf(t, fake.adGroupBodies[0])
	for _, k := range []string{"custom_audience_ids", "excluded_custom_audience_ids"} {
		if _, ok := tg[k]; ok {
			t.Errorf("targeting carries %s without a custom audience input", k)
		}
	}
}

// TestReddit_Dispatch_CustomAudiencesRefusedBeforeAnyRedditCall: the flag off, or a bad list,
// refuses the create pre-create (claim released) with a refusal the job copies, and Reddit is
// never contacted.
func TestReddit_Dispatch_CustomAudiencesRefusedBeforeAnyRedditCall(t *testing.T) {
	for _, tc := range []struct {
		name, flag, extra, want string
	}{
		{"flag off", "", `,"customAudienceIds":["1"]`, constants.EnvRedditCustomAudiencesEnabled},
		{"flag typo", "TRUE", `,"excludedCustomAudienceIds":["1"]`, constants.EnvRedditCustomAudiencesEnabled},
		{"overlap", "true", `,"customAudienceIds":["1"],"excludedCustomAudienceIds":["1"]`, "also in customAudienceIds"},
		// Mistyped: the raw type check runs before the decode, so the job gets the field sentence.
		{"mistyped list", "true", `,"customAudienceIds":7`, "customAudienceIds must be an array of strings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditCustomAudiencesEnabled, tc.flag)
			fake := &fakeRedditAPI{}
			apiURL, tokURL := fake.start(t)
			d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
				reddit.WithBaseURL(apiURL), reddit.WithTokenURL(tokURL))
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAudienceCfg(tc.extra))
			if camp != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Dispatch = (%v, %v), want a refusal containing %q", camp, err, tc.want)
			}
			var nuc interface{ NoUpstreamCreate() bool }
			if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("refusal must release the claim, got %T", err)
			}
			var refusal *domain.PreflightRefusal
			if !errors.As(err, &refusal) {
				t.Errorf("refusal must be a domain.PreflightRefusal so the job reports it, got %T", err)
			}
			if fake.requests != 0 {
				t.Errorf("Reddit was contacted %d time(s) for a refused create", fake.requests)
			}
		})
	}
}

// TestReddit_ListCustomAudiences_Gate: off, the read is the lookup-unsupported sentinel (400)
// and nothing is resolved or sent; on, it lists through the create's account.
func TestReddit_ListCustomAudiences_Gate(t *testing.T) {
	var hits []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		_, _ = io.WriteString(w, `{"data":[{"id":"67547345732543","name":" KubeCon visitors ","type":"PIXEL_RETARGETING","status":"VALID","size_range_lower":1000,"size_range_upper":5000},{"id":"2","name":"Registrants","type":"CUSTOMER_LIST","status":"PENDING"}],"pagination":{"next_url":null}}`)
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL))

	t.Setenv(constants.EnvRedditCustomAudiencesEnabled, "")
	res, called, err := d.ListRedditCustomAudiences(context.Background(), "cncf", model.ProviderRedditAds)
	if res != nil || called || !errors.Is(err, domain.ErrTargetingLookupUnsupported) {
		t.Fatalf("flag off: (%v, %v, %v), want the unsupported sentinel and no call", res, called, err)
	}
	if len(hits) != 0 {
		t.Fatalf("flag off contacted Reddit: %v", hits)
	}

	t.Setenv(constants.EnvRedditCustomAudiencesEnabled, "true")
	res, called, err = d.ListRedditCustomAudiences(context.Background(), "cncf", model.ProviderRedditAds)
	if err != nil || !called {
		t.Fatalf("flag on: called=%v err=%v", called, err)
	}
	if res.AccountID != "t2_acct" || res.Truncated || len(res.Audiences) != 2 {
		t.Fatalf("result = %+v", res)
	}
	a := res.Audiences[0]
	if a.ID != "67547345732543" || a.Name != "KubeCon visitors" || a.Type != "PIXEL_RETARGETING" || a.Status != "VALID" ||
		a.SizeLower == nil || *a.SizeLower != 1000 || a.SizeUpper == nil || *a.SizeUpper != 5000 {
		t.Errorf("first audience = %+v", a)
	}
	if b := res.Audiences[1]; b.SizeLower != nil || b.SizeUpper != nil {
		t.Errorf("absent sizes must stay nil, got %+v", b)
	}
	if len(hits) != 1 || hits[0] != "/api/v3/ad_accounts/t2_acct/custom_audiences" {
		t.Errorf("requests = %v, want one GET of the connection account's custom audiences", hits)
	}
}
