// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// TestCreateCampaign_LocalesReachTheAdSet: locale keys are sent de-duplicated in order as
// `locales`, and a language alone is not an inclusion, so no Advantage+ flag is sent.
func TestCreateCampaign_LocalesReachTheAdSet(t *testing.T) {
	ts := newTargetingServer(t, nil)
	defer ts.Close()
	in := metaFieldInput()
	in.Targeting = &AudienceTargeting{Locales: []int{6, 1001, 6}}
	if _, err := biddingClient(ts.URL, 100).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	got := ts.adSetTargeting(t)
	if !reflect.DeepEqual(got["locales"], []any{float64(6), float64(1001)}) {
		t.Errorf("targeting.locales = %#v, want [6 1001]", got["locales"])
	}
	if _, has := got["targeting_automation"]; has {
		t.Errorf("targeting_automation = %v, want it absent for a locales-only block", got["targeting_automation"])
	}
}

// TestCreateCampaign_TargetingOptionRefusalsSendNothing: an invalid locale list is a config-invalid
// pre-flight refusal and no request reaches Meta.
func TestCreateCampaign_TargetingOptionRefusalsSendNothing(t *testing.T) {
	many := make([]int, MaxLocales+1)
	for i := range many {
		many[i] = i + 1
	}
	for _, tc := range []struct {
		name string
		tgt  AudienceTargeting
		want string
	}{
		{"zero locale", AudienceTargeting{Locales: []int{6, 0}}, "metaConfig.targeting: locales entry 0 is not a Meta locale key"},
		{"negative locale", AudienceTargeting{Locales: []int{-6}}, "locales entry -6"},
		{"too many locales", AudienceTargeting{Locales: many}, "at most 50 are allowed"},
		{"unknown device", AudienceTargeting{DevicePlatforms: []string{"mobile", "tablet"}}, `devicePlatforms entry "tablet" is not one of mobile, desktop`},
		{"desktop only with audience network", AudienceTargeting{DevicePlatforms: []string{" Desktop "}}, "cannot serve the audienceNetwork placement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.String())
			}))
			defer srv.Close()
			in := metaFieldInput()
			tgt := tc.tgt
			in.Targeting = &tgt
			in.Placements = Placement{AudienceNetwork: boolp(true)}
			res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateCampaign = %+v, %v; want a config-invalid pre-flight refusal containing %q", res, err, tc.want)
			}
		})
	}
	// The cap counts DISTINCT keys: exactly the cap, with repeats, is accepted.
	if _, err := NormalizeAudienceTargeting(&AudienceTargeting{Locales: append(many[:MaxLocales:MaxLocales], 1, 2)}); err != nil {
		t.Fatalf("%d distinct locales with duplicates refused: %v", MaxLocales, err)
	}
}

// TestAudienceTargeting_HasAudienceFields: only the six fields META_AUDIENCE_TARGETING_ENABLED
// gates count; the later fields with their own flags do not.
func TestAudienceTargeting_HasAudienceFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		tgt  *AudienceTargeting
		want bool
	}{
		{"nil", nil, false},
		{"empty", &AudienceTargeting{}, false},
		{"locales only", &AudienceTargeting{Locales: []int{6}}, false},
		{"devices only", &AudienceTargeting{DevicePlatforms: []string{"mobile"}}, false},
		{"ageMin", &AudienceTargeting{AgeMin: intp(30)}, true},
		{"ageMax", &AudienceTargeting{AgeMax: intp(30)}, true},
		{"genders", &AudienceTargeting{Genders: []string{"male"}}, true},
		{"customAudiences", &AudienceTargeting{CustomAudiences: []string{"1"}}, true},
		{"excludedCustomAudiences", &AudienceTargeting{ExcludedCustomAudiences: []string{"1"}}, true},
		{"advantageAudience", &AudienceTargeting{AdvantageAudience: boolp(false)}, true},
	} {
		if got := tc.tgt.HasAudienceFields(); got != tc.want {
			t.Errorf("%s: HasAudienceFields = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCreateCampaign_DevicePlatformsReachTheAdSet: one device is sent as device_platforms; both, in
// any spelling, are Meta's default and send nothing. No Advantage+ flag either way. Desktop with
// the default placements (no Audience Network) is accepted.
func TestCreateCampaign_DevicePlatformsReachTheAdSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		devices []string
		want    any
	}{
		{"mobile", []string{"Mobile", "mobile"}, []any{"mobile"}},
		{"desktop", []string{"desktop"}, []any{"desktop"}},
		{"both", []string{"desktop", " MOBILE "}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTargetingServer(t, nil)
			defer ts.Close()
			in := metaFieldInput()
			in.Targeting = &AudienceTargeting{DevicePlatforms: tc.devices}
			if _, err := biddingClient(ts.URL, 100).CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			got := ts.adSetTargeting(t)
			if g, has := got["device_platforms"]; (tc.want == nil) == has || (has && !reflect.DeepEqual(g, tc.want)) {
				t.Errorf("targeting.device_platforms = %#v (present %v), want %#v", g, has, tc.want)
			}
			if _, has := got["targeting_automation"]; has {
				t.Errorf("targeting_automation = %v, want it absent", got["targeting_automation"])
			}
		})
	}
}

// TestCheckDevicePlacements: only desktop-only with Audience Network is refused.
func TestCheckDevicePlacements(t *testing.T) {
	an := Placement{AudienceNetwork: boolp(true)}
	for _, tc := range []struct {
		name    string
		devices []string
		pl      Placement
		refused bool
	}{
		{"desktop with audience network", []string{"desktop"}, an, true},
		{"mobile with audience network", []string{"mobile"}, an, false},
		{"both with audience network", []string{"mobile", "desktop"}, an, false},
		{"desktop with default placements", []string{"desktop"}, Placement{}, false},
		{"none", nil, an, false},
	} {
		err := CheckDevicePlacements(&AudienceTargeting{DevicePlatforms: tc.devices}, tc.pl)
		if (err != nil) != tc.refused {
			t.Errorf("%s: CheckDevicePlacements = %v, want refused %v", tc.name, err, tc.refused)
		}
	}
	if err := CheckDevicePlacements(nil, an); err != nil {
		t.Errorf("nil targeting: %v", err)
	}
}

// interestServer answers the account read, the interest validation read (with answer, or a Graph
// error when answer is empty) and the create POSTs, recording the validation query and every POST.
type interestServer struct {
	*httptest.Server
	mu        sync.Mutex
	queries   []string
	posts     []string
	adsetBody map[string]any
}

func newInterestServer(t *testing.T, answer string) *interestServer {
	t.Helper()
	is := &interestServer{}
	is.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		is.mu.Lock()
		defer is.mu.Unlock()
		if r.Method == http.MethodPost {
			is.posts = append(is.posts, r.URL.Path)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/search":
			is.queries = append(is.queries, r.URL.Query().Get("type")+" "+r.URL.Query().Get("interest_fbid_list"))
			if answer == "" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":{"message":"An unknown error occurred","type":"OAuthException","code":1}}`)
				return
			}
			_, _ = io.WriteString(w, answer)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1,"currency":"USD"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			is.adsetBody = decodeBody(t, r)
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return is
}

func (is *interestServer) snapshot() (queries, posts []string, targeting map[string]any) {
	is.mu.Lock()
	defer is.mu.Unlock()
	targeting, _ = is.adsetBody["targeting"].(map[string]any)
	return append([]string(nil), is.queries...), append([]string(nil), is.posts...), targeting
}

// TestCreateCampaign_InterestsAreValidatedThenSentAsFlexibleSpec: the interests are validated in
// one read before the first write, then sent as one flexible_spec entry; being an inclusion, they
// make the Advantage+ flag explicit (0).
func TestCreateCampaign_InterestsAreValidatedThenSentAsFlexibleSpec(t *testing.T) {
	is := newInterestServer(t, `{"data":[{"name":"Kubernetes","valid":true,"id":"6003"},{"name":"DevOps","valid":true,"id":"6004"}]}`)
	defer is.Close()
	in := metaFieldInput()
	in.Targeting = &AudienceTargeting{Interests: []string{"6003", " 6004", "6003"}}
	if _, err := biddingClient(is.URL, 100).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	queries, posts, targeting := is.snapshot()
	if !reflect.DeepEqual(queries, []string{`adinterestvalid ["6003","6004"]`}) {
		t.Errorf("validation reads = %v, want one adinterestvalid read of both ids", queries)
	}
	if len(posts) == 0 {
		t.Fatal("nothing was created")
	}
	wantSpec := []any{map[string]any{"interests": []any{map[string]any{"id": "6003"}, map[string]any{"id": "6004"}}}}
	if !reflect.DeepEqual(targeting["flexible_spec"], wantSpec) {
		t.Errorf("flexible_spec = %#v, want %#v", targeting["flexible_spec"], wantSpec)
	}
	if !reflect.DeepEqual(targeting["targeting_automation"], map[string]any{"advantage_audience": float64(0)}) {
		t.Errorf("targeting_automation = %#v, want advantage_audience 0", targeting["targeting_automation"])
	}
}

// TestCreateCampaign_InvalidOrUnverifiableInterestCreatesNothing: an interest Meta marks invalid is
// a refusal, and a failed read or an answer missing an id is an error; either way no POST is sent.
func TestCreateCampaign_InvalidOrUnverifiableInterestCreatesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, answer, want string
		refusal            bool
	}{
		{"retired interest", `{"data":[{"id":"6003","valid":true},{"id":"6004","valid":false}]}`, `interest "6004" is not a detailed targeting interest Meta currently offers`, true},
		{"no valid field", `{"data":[{"id":"6003","valid":true},{"id":"6004"}]}`, `interest "6004" is not`, true},
		{"an id not answered", `{"data":[{"id":"6003","valid":true}]}`, `did not answer for interest "6004"`, false},
		{"no data", `{}`, "returned no rows", false},
		{"read fails", "", "could not be validated, so nothing was created", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is := newInterestServer(t, tc.answer)
			defer is.Close()
			in := metaFieldInput()
			in.Targeting = &AudienceTargeting{Interests: []string{"6003", "6004"}}
			res, err := biddingClient(is.URL, 100).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || err == nil || !strings.Contains(err.Error(), tc.want) || errors.As(err, &refusal) != tc.refusal {
				t.Fatalf("CreateCampaign = %+v, %v; want an error containing %q (refusal %v)", res, err, tc.want, tc.refusal)
			}
			if _, posts, _ := is.snapshot(); len(posts) != 0 {
				t.Fatalf("posts = %v, want none", posts)
			}
		})
	}
}

// TestNormalizeAudienceTargeting_Interests: interest ids are numeric, de-duplicated and capped.
func TestNormalizeAudienceTargeting_Interests(t *testing.T) {
	many := make([]string, MaxInterests+1)
	for i := range many {
		many[i] = "60" + strings.Repeat("0", 3) + string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	for _, tc := range []struct {
		name string
		ids  []string
		want string
	}{
		{"non-numeric", []string{"kubernetes"}, `interests entry "kubernetes" is not a Meta interest id`},
		{"too many", many, "interests names 26 ids; at most 25 are allowed"},
	} {
		_, err := NormalizeAudienceTargeting(&AudienceTargeting{Interests: tc.ids})
		var refusal *domain.PreflightRefusal
		if !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal containing %q", tc.name, err, tc.want)
		}
	}
	got, err := NormalizeAudienceTargeting(&AudienceTargeting{Interests: append(many[:MaxInterests:MaxInterests], many[0])})
	if err != nil || len(got.Interests) != MaxInterests {
		t.Fatalf("%d distinct ids with a repeat: %v, %v", MaxInterests, got, err)
	}
	if (&AudienceTargeting{Interests: []string{"1"}}).HasAudienceFields() {
		t.Error("interests alone must not count as an audience field")
	}
	if (&AudienceTargeting{Interests: []string{"1"}}).IsZero() {
		t.Error("interests alone must not be zero targeting")
	}
}
