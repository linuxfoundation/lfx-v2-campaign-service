// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// The exclusions reach the ad group's targeting object under Reddit's v3 field names, normalized:
// r/ stripped, case-insensitive duplicates dropped (first spelling kept), codes uppercased.
func TestCreateCampaign_SendsTargetingExclusions(t *testing.T) {
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()

	in := validRedditInput()
	in.ExcludedSubreddits = []string{"r/politics", " news ", "Politics"}
	in.ExcludedGeoTargets = []string{"ru", "BY", "ru"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, adGroup := bodies()
	targeting, _ := adGroup["targeting"].(map[string]any)
	if got, want := targeting["excluded_communities"], []any{"politics", "news"}; !reflect.DeepEqual(got, want) {
		t.Errorf("excluded_communities = %#v, want %#v", got, want)
	}
	if got, want := targeting["excluded_geolocations"], []any{"RU", "BY"}; !reflect.DeepEqual(got, want) {
		t.Errorf("excluded_geolocations = %#v, want %#v", got, want)
	}
	if joined := strings.Join(res.Steps, " | "); !strings.Contains(joined, "2 excluded communities") || !strings.Contains(joined, "2 excluded geos") {
		t.Errorf("the Targeting step must count the exclusions; steps: %s", joined)
	}
}

// Without exclusions the targeting object carries neither key: an empty list is not sent.
func TestCreateCampaign_NoExclusionsSendsNoKeys(t *testing.T) {
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()

	if _, err := c.CreateCampaign(context.Background(), validRedditInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, adGroup := bodies()
	targeting, _ := adGroup["targeting"].(map[string]any)
	for _, k := range []string{"excluded_communities", "excluded_geolocations"} {
		if _, ok := targeting[k]; ok {
			t.Errorf("targeting carries %s with no exclusions supplied", k)
		}
	}
}

// The communities/interests 400 retry must keep the exclusions: dropping a brand-safety exclusion
// to rescue the ad group would widen the campaign into places the operator ruled out.
func TestCreateCampaign_RetryKeepsExclusions(t *testing.T) {
	c, bodies, cleanup := interestRejectingServers(t, "interests")
	defer cleanup()

	in := interestInput()
	in.ExcludedSubreddits = []string{"politics"}
	in.ExcludedGeoTargets = []string{"RU"}
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	sent := bodies()
	if len(sent) != 2 {
		t.Fatalf("expected the rejected attempt and the retry, got %d ad-group POSTs", len(sent))
	}
	for i, b := range sent {
		targeting, _ := b["targeting"].(map[string]any)
		if _, ok := targeting["excluded_communities"]; !ok {
			t.Errorf("attempt %d dropped excluded_communities", i+1)
		}
		if _, ok := targeting["excluded_geolocations"]; !ok {
			t.Errorf("attempt %d dropped excluded_geolocations", i+1)
		}
	}
}

// A bad exclusion is refused before ANY upstream call, as an operator-safe PreflightRefusal.
func TestCreateCampaign_RefusesBadExclusionBeforeAnyCall(t *testing.T) {
	cases := map[string]func(*CampaignInput){
		"blank subreddit":     func(in *CampaignInput) { in.ExcludedSubreddits = []string{"r/"} },
		"bad subreddit shape": func(in *CampaignInput) { in.ExcludedSubreddits = []string{"no spaces"} },
		"targeted and excluded subreddit": func(in *CampaignInput) {
			in.Subreddits = []string{"r/Kubernetes"}
			in.ExcludedSubreddits = []string{"kubernetes"}
		},
		"unassigned country":        func(in *CampaignInput) { in.ExcludedGeoTargets = []string{"ZZ"} },
		"targeted and excluded geo": func(in *CampaignInput) { in.ExcludedGeoTargets = []string{"US"} },
		"excluding the US default":  func(in *CampaignInput) { in.GeoTargets = nil; in.ExcludedGeoTargets = []string{"us"} },
		"too many excluded subreddits": func(in *CampaignInput) {
			in.ExcludedSubreddits = make([]string, MaxExcludedSubreddits+1)
			for i := range in.ExcludedSubreddits {
				in.ExcludedSubreddits[i] = "politics"
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c, calls, cleanup := newCountingServers(t)
			defer cleanup()
			in := validRedditInput()
			mutate(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil || res != nil {
				t.Fatalf("want a refusal and no result, got res=%v err=%v", res, err)
			}
			var refusal *domain.PreflightRefusal
			if !errors.As(err, &refusal) {
				t.Errorf("refusal is not a domain.PreflightRefusal: %v", err)
			}
			if n := calls(); n != 0 {
				t.Errorf("%d upstream calls before the refusal; want 0", n)
			}
		})
	}
}

// newCountingServers is a token and API server pair that counts every request either receives
// and answers each with a 500, so a test can assert that a refusal happened before any call.
func newCountingServers(t *testing.T) (*Client, func() int64, func()) {
	t.Helper()
	var n atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	tokenSrv := httptest.NewServer(h)
	apiSrv := httptest.NewServer(h)
	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
	return c, n.Load, func() { apiSrv.Close(); tokenSrv.Close() }
}

func TestNormalizeTargetingExclusions_NamesTheField(t *testing.T) {
	_, err := NormalizeTargetingExclusions(nil, []string{"US"}, nil, []string{"US"})
	var exErr *ExclusionError
	if !errors.As(err, &exErr) || exErr.Field != FieldExcludedGeoTargets {
		t.Fatalf("want an ExclusionError on %s, got %v", FieldExcludedGeoTargets, err)
	}
	_, err = NormalizeTargetingExclusions(nil, nil, []string{"x"}, nil)
	if !errors.As(err, &exErr) || exErr.Field != FieldExcludedSubreddits {
		t.Fatalf("want an ExclusionError on %s, got %v", FieldExcludedSubreddits, err)
	}
	if _, err := NormalizeTargetingExclusions(nil, nil, nil, make([]string, MaxExcludedGeoTargets+1)); !errors.As(err, &exErr) || !strings.Contains(exErr.Msg, "at most") {
		t.Fatalf("want the geo cap refusal, got %v", err)
	}
}
