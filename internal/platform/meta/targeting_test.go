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

// targetingServer is metaFieldServer plus the custom-audience ownership read: a GET of
// /<audience id>?fields=id,account_id,subtype answers owners[id] — "<account id>" for a custom
// audience, or "<account id>|campaign" for a same-account node that is NOT an audience (no subtype)
// — or a Graph 100/33 error when the id is not in owners. It records every audience read and every POST path.
type targetingServer struct {
	*httptest.Server
	mu        sync.Mutex
	reads     []string
	posts     []string
	adsetBody map[string]any
}

func newTargetingServer(t *testing.T, owners map[string]string) *targetingServer {
	t.Helper()
	ts := &targetingServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ts.mu.Lock()
		defer ts.mu.Unlock()
		if r.Method == http.MethodPost {
			ts.posts = append(ts.posts, r.URL.Path)
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1,"currency":"USD"}`)
		case r.Method == http.MethodGet && r.URL.Query().Get("fields") == "id,account_id,subtype":
			id := strings.TrimPrefix(r.URL.Path, "/")
			ts.reads = append(ts.reads, id)
			owner, ok := owners[id]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"Unsupported get request","type":"GraphMethodException","code":100,"error_subcode":33}}`)
				return
			}
			if acct, ok := strings.CutSuffix(owner, "|campaign"); ok {
				_, _ = io.WriteString(w, `{"id":"`+id+`","account_id":"`+acct+`"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"`+id+`","account_id":"`+owner+`","subtype":"WEBSITE"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			ts.adsetBody = decodeBody(t, r)
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
	return ts
}

func (ts *targetingServer) adSetTargeting(t *testing.T) map[string]any {
	t.Helper()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	targeting, ok := ts.adsetBody["targeting"].(map[string]any)
	if !ok {
		t.Fatalf("ad set targeting = %#v, want an object", ts.adsetBody["targeting"])
	}
	return targeting
}

// readsSent and postsSent copy the recorded requests under the handler's lock, so the test
// goroutine never reads what the handler goroutine writes without a happens-before edge.
func (ts *targetingServer) readsSent() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.reads...)
}

func (ts *targetingServer) postsSent() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.posts...)
}

func boolp(b bool) *bool { return &b }

func intp(v int) *int { return &v }

// TestCreateCampaign_NoTargetingLeavesTheAdSetTargetingUnchanged: without a targeting block the
// ad set carries only geo and placements, no Advantage+ flag is sent, and no audience is read.
func TestCreateCampaign_NoTargetingLeavesTheAdSetTargetingUnchanged(t *testing.T) {
	for name, tgt := range map[string]*AudienceTargeting{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			ts := newTargetingServer(t, nil)
			defer ts.Close()
			in := metaFieldInput()
			in.Targeting = tgt
			if _, err := biddingClient(ts.URL, 100).CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			got := ts.adSetTargeting(t)
			for _, k := range []string{"age_min", "age_max", "genders", "custom_audiences", "excluded_custom_audiences", "targeting_automation"} {
				if _, has := got[k]; has {
					t.Errorf("targeting carries %s = %v, want it absent", k, got[k])
				}
			}
			if reads := ts.readsSent(); len(reads) != 0 {
				t.Errorf("audience reads = %v, want none", reads)
			}
		})
	}
}

// TestCreateCampaign_TargetingReachesTheAdSet pins the exact targeting keys for each shape the
// #550 comment specifies, including when the Advantage+ flag is and is not sent.
func TestCreateCampaign_TargetingReachesTheAdSet(t *testing.T) {
	// 222 answers with the act_ prefix: the comparison ignores it on either side (sameMetaAccount).
	owners := map[string]string{"111": "777", "222": "act_777", "333": "777"}
	type want map[string]any
	for _, tc := range []struct {
		name  string
		tgt   *AudienceTargeting
		want  want
		reads []string
	}{
		{"full inclusion, advantage defaults off",
			&AudienceTargeting{AgeMin: intp(25), AgeMax: intp(54), Genders: []string{"Female", " female "},
				CustomAudiences: []string{"111", " 111 ", "222"}, ExcludedCustomAudiences: []string{"333"}},
			want{"age_min": float64(25), "age_max": float64(54), "genders": []any{float64(2)},
				"custom_audiences":          []any{map[string]any{"id": "111"}, map[string]any{"id": "222"}},
				"excluded_custom_audiences": []any{map[string]any{"id": "333"}},
				"targeting_automation":      map[string]any{"advantage_audience": float64(0)}},
			[]string{"111", "222", "333"}},
		{"male only is an inclusion",
			&AudienceTargeting{Genders: []string{"male"}},
			want{"genders": []any{float64(1)}, "targeting_automation": map[string]any{"advantage_audience": float64(0)}}, nil},
		{"both genders send nothing",
			&AudienceTargeting{Genders: []string{"male", "female"}}, want{}, nil},
		{"default ages are not an inclusion",
			&AudienceTargeting{AgeMin: intp(18), AgeMax: intp(65)}, want{"age_min": float64(18), "age_max": float64(65)}, nil},
		{"exclusions only send no flag",
			&AudienceTargeting{ExcludedCustomAudiences: []string{"333"}},
			want{"excluded_custom_audiences": []any{map[string]any{"id": "333"}}}, []string{"333"}},
		{"advantage on, said explicitly",
			&AudienceTargeting{AgeMin: intp(21), CustomAudiences: []string{"111"}, AdvantageAudience: boolp(true)},
			want{"age_min": float64(21), "custom_audiences": []any{map[string]any{"id": "111"}},
				"targeting_automation": map[string]any{"advantage_audience": float64(1)}}, []string{"111"}},
		{"advantage off, said with no inclusion",
			&AudienceTargeting{AdvantageAudience: boolp(false)},
			want{"targeting_automation": map[string]any{"advantage_audience": float64(0)}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTargetingServer(t, owners)
			defer ts.Close()
			in := metaFieldInput()
			in.Targeting = tc.tgt
			if _, err := biddingClient(ts.URL, 100).CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			got := ts.adSetTargeting(t)
			for _, k := range []string{"age_min", "age_max", "genders", "custom_audiences", "excluded_custom_audiences", "targeting_automation"} {
				w, wantHas := tc.want[k]
				g, has := got[k]
				if wantHas != has || (has && !reflect.DeepEqual(g, w)) {
					t.Errorf("targeting[%s] = %#v (present %v), want %#v (present %v)", k, g, has, w, wantHas)
				}
			}
			if _, ok := got["geo_locations"]; !ok {
				t.Error("geo_locations was dropped")
			}
			if reads := ts.readsSent(); !reflect.DeepEqual(reads, tc.reads) {
				t.Errorf("audience reads = %v, want %v", reads, tc.reads)
			}
		})
	}
}

// TestCreateCampaign_TargetingRefusalsSendNothing: every invalid shape is a 400-mappable
// pre-flight refusal and no request at all reaches Meta — not even the account read.
func TestCreateCampaign_TargetingRefusalsSendNothing(t *testing.T) {
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "9" + strings.Repeat("0", 3) + string(rune('0'+i/10)) + string(rune('0'+i%10))
		}
		return out
	}
	for _, tc := range []struct {
		name string
		tgt  AudienceTargeting
		want string
	}{
		{"ageMin under 18", AudienceTargeting{AgeMin: intp(17)}, "ageMin 17 is out of range"},
		{"explicit ageMin 0", AudienceTargeting{AgeMin: intp(0)}, "ageMin 0 is out of range"},
		{"explicit ageMax 0", AudienceTargeting{AgeMax: intp(0)}, "ageMax 0 is out of range"},
		{"ageMax over 65", AudienceTargeting{AgeMax: intp(66)}, "ageMax 66 is out of range"},
		{"inverted ages", AudienceTargeting{AgeMin: intp(40), AgeMax: intp(30)}, "ageMin 40 is above ageMax 30"},
		{"unknown gender", AudienceTargeting{Genders: []string{"other"}}, "is not one of male, female"},
		{"non-numeric audience", AudienceTargeting{CustomAudiences: []string{"abc"}}, "customAudiences entry"},
		{"non-numeric exclusion", AudienceTargeting{ExcludedCustomAudiences: []string{"1/2"}}, "excludedCustomAudiences entry"},
		{"audience in both lists", AudienceTargeting{CustomAudiences: []string{"111"}, ExcludedCustomAudiences: []string{" 111"}}, `custom audience "111" is in both`},
		// A long id is quoted through domain.CallerValue, which truncates it rather than echoing it whole.
		{"long id in both lists", AudienceTargeting{CustomAudiences: []string{strings.Repeat("9", 80)}, ExcludedCustomAudiences: []string{strings.Repeat("9", 80)}}, "…\" (truncated) is in both"},
		{"too many audiences", AudienceTargeting{CustomAudiences: ids(MaxCustomAudiences + 1)}, "at most 25"},
		{"advantage with ageMin 30", AudienceTargeting{AgeMin: intp(30), AdvantageAudience: boolp(true)}, "ageMin of at most 25"},
		{"advantage with ageMax 54", AudienceTargeting{AgeMax: intp(54), AdvantageAudience: boolp(true)}, "fixes ageMax at 65"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.String())
			}))
			defer srv.Close()
			in := metaFieldInput()
			tgt := tc.tgt
			in.Targeting = &tgt
			res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
			if res != nil {
				t.Fatalf("result = %+v, want nil", res)
			}
			var refusal *domain.PreflightRefusal
			if !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a config-invalid pre-flight refusal containing %q", err, tc.want)
			}
		})
	}
	// The cap counts DISTINCT ids: exactly the cap is accepted.
	if _, err := NormalizeAudienceTargeting(&AudienceTargeting{CustomAudiences: append(ids(MaxCustomAudiences), ids(3)...)}); err != nil {
		t.Fatalf("%d distinct ids with duplicates refused: %v", MaxCustomAudiences, err)
	}
}

// TestCreateCampaign_ForeignOrUnreadableAudienceCreatesNothing: an audience Meta reports under
// another ad account is a refusal, and one that cannot be read is an error; either way no POST is
// sent, so no campaign exists.
func TestCreateCampaign_ForeignOrUnreadableAudienceCreatesNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		owners     map[string]string
		tgt        AudienceTargeting
		wantRefuse bool
		want       string
	}{
		{"included audience of another account", map[string]string{"111": "888"},
			AudienceTargeting{CustomAudiences: []string{"111"}}, true, `customAudiences audience "111" is not a custom audience of the connected ad account act_777`},
		{"excluded audience of another account", map[string]string{"111": "777", "333": "888"},
			AudienceTargeting{CustomAudiences: []string{"111"}, ExcludedCustomAudiences: []string{"333"}}, true, `excludedCustomAudiences audience "333"`},
		{"included id is a same-account campaign", map[string]string{"111": "777|campaign"},
			AudienceTargeting{CustomAudiences: []string{"111"}}, true, `customAudiences id "111" is not a custom audience`},
		{"excluded id is a same-account campaign", map[string]string{"333": "777|campaign"},
			AudienceTargeting{ExcludedCustomAudiences: []string{"333"}}, true, `excludedCustomAudiences id "333" is not a custom audience`},
		{"unreadable audience", map[string]string{},
			AudienceTargeting{ExcludedCustomAudiences: []string{"444"}}, false, `excludedCustomAudiences audience "444" could not be read`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTargetingServer(t, tc.owners)
			defer ts.Close()
			in := metaFieldInput()
			tgt := tc.tgt
			in.Targeting = &tgt
			res, err := biddingClient(ts.URL, 100).CreateCampaign(context.Background(), in)
			if res != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateCampaign = %+v, %v; want nil and an error containing %q", res, err, tc.want)
			}
			var refusal *domain.PreflightRefusal
			if got := errors.As(err, &refusal); got != tc.wantRefuse {
				t.Fatalf("pre-flight refusal = %v, want %v (%v)", got, tc.wantRefuse, err)
			}
			if posts := ts.postsSent(); len(posts) != 0 {
				t.Fatalf("POSTs sent = %v, want none", posts)
			}
		})
	}
}
