// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
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

// validationServer answers the account read, the targeting validation read (with answer, or a
// Graph error when answer is empty) and the create POSTs, recording the validation list and every
// POST.
type validationServer struct {
	*httptest.Server
	mu        sync.Mutex
	lists     []string
	posts     []string
	adsetBody map[string]any
}

func newValidationServer(t *testing.T, answer string) *validationServer {
	t.Helper()
	vs := &validationServer{}
	vs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		vs.mu.Lock()
		defer vs.mu.Unlock()
		if r.Method == http.MethodPost {
			vs.posts = append(vs.posts, r.URL.Path)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/act_777/targetingvalidation":
			vs.lists = append(vs.lists, r.URL.Query().Get("targeting_list"))
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
			vs.adsetBody = decodeBody(t, r)
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
	return vs
}

func (vs *validationServer) snapshot() (lists, posts []string, targeting map[string]any) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	targeting, _ = vs.adsetBody["targeting"].(map[string]any)
	return append([]string(nil), vs.lists...), append([]string(nil), vs.posts...), targeting
}

// TestCreateCampaign_BehaviorsAndWorkPositionsShareTheInterestsEntry: the ids are validated in ONE
// read before the first write and sent in the same flexible_spec entry as the interests (Meta ORs
// one entry's keys); being inclusions they make the Advantage+ flag explicit.
func TestCreateCampaign_BehaviorsAndWorkPositionsShareTheInterestsEntry(t *testing.T) {
	vs := newValidationServer(t, `{"data":[{"id":6002714895372,"type":"behaviors","valid":true},{"id":"105763692790962","type":"work_positions","valid":true}]}`)
	defer vs.Close()
	in := metaFieldInput()
	in.Targeting = &AudienceTargeting{Behaviors: []string{"6002714895372", "6002714895372"}, WorkPositions: []string{" 105763692790962"}}
	if _, err := biddingClient(vs.URL, 100).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	lists, posts, targeting := vs.snapshot()
	if want := []string{`[{"type":"behaviors","id":6002714895372},{"type":"work_positions","id":105763692790962}]`}; !reflect.DeepEqual(lists, want) {
		t.Errorf("validation reads = %v, want %v", lists, want)
	}
	if len(posts) == 0 {
		t.Fatal("nothing was created")
	}
	want := []any{map[string]any{
		"behaviors":      []any{map[string]any{"id": "6002714895372"}},
		"work_positions": []any{map[string]any{"id": "105763692790962"}},
	}}
	if !reflect.DeepEqual(targeting["flexible_spec"], want) {
		t.Errorf("flexible_spec = %#v, want %#v", targeting["flexible_spec"], want)
	}
	if !reflect.DeepEqual(targeting["targeting_automation"], map[string]any{"advantage_audience": float64(0)}) {
		t.Errorf("targeting_automation = %#v, want advantage_audience 0", targeting["targeting_automation"])
	}
}

// TestAudienceTargetingApply_DetailedTargetingIsOneEntry: interests, behaviors and work positions
// together are one flexible_spec entry, not three ANDed ones.
func TestAudienceTargetingApply_DetailedTargetingIsOneEntry(t *testing.T) {
	spec := map[string]any{}
	(&AudienceTargeting{Interests: []string{"1"}, Behaviors: []string{"2"}, WorkPositions: []string{"3"}}).apply(spec)
	raw, _ := json.Marshal(spec["flexible_spec"])
	if want := `[{"behaviors":[{"id":"2"}],"interests":[{"id":"1"}],"work_positions":[{"id":"3"}]}]`; string(raw) != want {
		t.Errorf("flexible_spec = %s, want %s", raw, want)
	}
}

// TestCreateCampaign_InvalidOrUnverifiableBehaviorCreatesNothing: an id Meta marks invalid is a
// refusal; a failed read, missing data, an unanswered id or a row without valid is an error; either
// way no POST is sent.
func TestCreateCampaign_InvalidOrUnverifiableBehaviorCreatesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, answer, want string
		refusal            bool
	}{
		{"retired work position", `{"data":[{"id":"6002714895372","valid":true},{"id":"105763692790962","valid":false}]}`, `work position "105763692790962" is not a detailed targeting option Meta currently offers`, true},
		{"no valid field", `{"data":[{"id":"6002714895372","valid":true},{"id":"105763692790962"}]}`, `did not say whether work position "105763692790962" is valid`, false},
		{"an id not answered", `{"data":[{"id":"105763692790962","valid":true}]}`, `did not answer for behavior "6002714895372"`, false},
		{"an id that is not a number", `{"data":[{"id":true,"valid":true}]}`, "returned no rows", false},
		{"no data", `{}`, "returned no rows", false},
		{"read fails", "", "could not be validated, so nothing was created", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := newValidationServer(t, tc.answer)
			defer vs.Close()
			in := metaFieldInput()
			in.Targeting = &AudienceTargeting{Behaviors: []string{"6002714895372"}, WorkPositions: []string{"105763692790962"}}
			res, err := biddingClient(vs.URL, 100).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || err == nil || !strings.Contains(err.Error(), tc.want) || errors.As(err, &refusal) != tc.refusal {
				t.Fatalf("CreateCampaign = %+v, %v; want an error containing %q (refusal %v)", res, err, tc.want, tc.refusal)
			}
			if _, posts, _ := vs.snapshot(); len(posts) != 0 {
				t.Fatalf("posts = %v, want none", posts)
			}
		})
	}
}

// TestNormalizeAudienceTargeting_BehaviorsAndWorkPositions: the ids are numeric, de-duplicated,
// capped, count as inclusions and not as the six audience fields.
func TestNormalizeAudienceTargeting_BehaviorsAndWorkPositions(t *testing.T) {
	many := make([]string, MaxBehaviors+1)
	for i := range many {
		many[i] = "600" + string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	for _, tc := range []struct {
		name string
		in   AudienceTargeting
		want string
	}{
		{"behavior name", AudienceTargeting{Behaviors: []string{"travelers"}}, `behaviors entry "travelers" is not a Meta behavior id`},
		{"too many behaviors", AudienceTargeting{Behaviors: many}, "behaviors names 26 ids; at most 25 are allowed"},
		{"work position name", AudienceTargeting{WorkPositions: []string{"Engineer"}}, `workPositions entry "Engineer" is not a Meta work position id`},
		{"too many work positions", AudienceTargeting{WorkPositions: many}, "workPositions names 26 ids; at most 25 are allowed"},
	} {
		_, err := NormalizeAudienceTargeting(&tc.in)
		var refusal *domain.PreflightRefusal
		if !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal containing %q", tc.name, err, tc.want)
		}
	}
	got, err := NormalizeAudienceTargeting(&AudienceTargeting{Behaviors: []string{" 1", "1"}, WorkPositions: []string{"2", "2 "}})
	if err != nil || !reflect.DeepEqual(got.Behaviors, []string{"1"}) || !reflect.DeepEqual(got.WorkPositions, []string{"2"}) {
		t.Fatalf("normalized = %+v, %v", got, err)
	}
	for _, tgt := range []*AudienceTargeting{{Behaviors: []string{"1"}}, {WorkPositions: []string{"1"}}} {
		if tgt.HasAudienceFields() || tgt.IsZero() || !tgt.inclusion() {
			t.Errorf("%+v: HasAudienceFields %v, IsZero %v, inclusion %v; want false, false, true", tgt, tgt.HasAudienceFields(), tgt.IsZero(), tgt.inclusion())
		}
	}
}
