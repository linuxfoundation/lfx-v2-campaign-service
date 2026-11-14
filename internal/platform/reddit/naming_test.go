// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/campaignname"
)

func TestBuildRedditCampaignNameV2(t *testing.T) {
	cncf := campaignname.Naming{BusinessUnit: "Cloud Native Computing Foundation (CNCF)"}
	base := CampaignInput{EventName: "KubeCon | NA", Project: "cncf"}
	for _, tc := range []struct {
		name      string
		naming    campaignname.Naming
		objective string
		geos      []string
		mutate    func(*CampaignInput)
		want      string
	}{
		{"defaults: one bucket, Intent, Static", cncf, "traffic", []string{"US", "CA"}, nil,
			"Events | KubeCon - NA | NA | Traffic | Intent | Static | Cloud Native Computing Foundation (CNCF) | ToFU"},
		{"two buckets are Global, not the first country's", cncf, "traffic", []string{"US", "GB"}, nil,
			"Events | KubeCon - NA | Global | Traffic | Intent | Static | Cloud Native Computing Foundation (CNCF) | ToFU"},
		{"video views is Motion", cncf, "video_views", []string{"JP"}, nil,
			"Events | KubeCon - NA | APAC | Video Views | Intent | Motion | Cloud Native Computing Foundation (CNCF) | ToFU"},
		{"an existing post is Social", cncf, "traffic", []string{"US"}, func(in *CampaignInput) { in.PostURL = "https://www.reddit.com/r/kubernetes/comments/abc123/x/" },
			"Events | KubeCon - NA | NA | Traffic | Intent | Social | Cloud Native Computing Foundation (CNCF) | ToFU"},
		{"caller region and intent win", campaignname.Naming{Region: "EMEA", AudienceIntent: "Prospecting", BusinessUnit: "LF Events"}, "conversions", []string{"US"}, nil,
			"Events | KubeCon - NA | EMEA | Conversions | Prospecting | Static | LF Events | ToFU"},
		{"retargeting tactic, slot token and stage", cncf, "traffic", []string{"BR"}, func(in *CampaignInput) {
			in.Tactic, in.SlotToken, in.StageToken = "retargeting", "#2", "BoFU"
		}, "Events | KubeCon - NA | LATAM | Traffic | Retargeting retargeting #2 | Static | Cloud Native Computing Foundation (CNCF) | BoFU"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			got := buildRedditCampaignNameV2(in, tc.objective, tc.geos, tc.naming)
			if got != tc.want {
				t.Errorf("name = %q\nwant   %q", got, tc.want)
			}
			if n := strings.Count(got, " | "); n != 7 {
				t.Errorf("name has %d separators, want 7", n)
			}
		})
	}
}

// redditNamingCreate runs CreateCampaign against a fake Reddit API and returns the campaign POST
// names it saw, the result and the error.
func redditNamingCreate(t *testing.T, in CampaignInput) ([]string, *CampaignResult, error) {
	t.Helper()
	var mu sync.Mutex
	var posted []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			var req struct {
				Data struct {
					Name string `json:"name"`
				} `json:"data"`
			}
			_ = json.Unmarshal(body, &req)
			posted = append(posted, req.Data.Name)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": fmt.Sprintf("cmp_%d", len(posted))}})
		case strings.Contains(r.URL.Path, "ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	c := NewClient(testCreds, testAccount, WithBaseURL(api.URL+"/api/v3"), WithTokenURL(tok.URL), WithNowFunc(fixedRedditClock()))
	res, err := c.CreateCampaign(context.Background(), in)
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), posted...), res, err
}

const (
	redditLegacyName = "Events | KubeCon | NA | Traffic | Intent | Social | tlf | ToFU"
	redditV2Name     = "Events | KubeCon | NA | Traffic | Intent | Static | Cloud Native Computing Foundation (CNCF) | ToFU"
)

func TestCreateCampaign_RedditNamingV2NamesANewCampaign(t *testing.T) {
	for _, tc := range []struct {
		name   string
		naming *campaignname.Naming
		want   string
	}{
		{"nil naming: the legacy name", nil, redditLegacyName},
		{"naming v2", &campaignname.Naming{BusinessUnit: "cloud native computing foundation (cncf)"}, redditV2Name},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := validRedditInput()
			in.Naming = tc.naming
			posted, res, _ := redditNamingCreate(t, in)
			if len(posted) != 1 || posted[0] != tc.want {
				t.Fatalf("posted %q; want %q", posted, tc.want)
			}
			if res == nil || res.CampaignName != tc.want {
				t.Errorf("result %+v; want campaign name %q", res, tc.want)
			}
		})
	}
}

// A name the claimed row already records is sent verbatim, over both the legacy and the v2 name.
func TestCreateCampaign_RedditFrozenNameWinsOverNamingV2(t *testing.T) {
	in := validRedditInput()
	in.Naming = &campaignname.Naming{BusinessUnit: "LF Events"}
	in.FrozenCampaignName = " " + redditLegacyName + " "
	posted, res, _ := redditNamingCreate(t, in)
	if len(posted) != 1 || posted[0] != redditLegacyName {
		t.Fatalf("posted %q; want the frozen name %q", posted, redditLegacyName)
	}
	found := false
	for _, s := range res.Steps {
		found = found || strings.Contains(s, "recorded name")
	}
	if !found {
		t.Errorf("steps %q; want one recording the frozen name", res.Steps)
	}
}

func TestCreateCampaign_RedditNamingV2RefusedBeforeAnyRequest(t *testing.T) {
	for _, n := range []campaignname.Naming{{}, {BusinessUnit: "cncf"}, {Region: "Reddit", BusinessUnit: "LF Events"}} {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			http.Error(w, "unexpected", http.StatusNotFound)
		}))
		c := NewClient(testCreds, testAccount, WithBaseURL(srv.URL+"/api/v3"), WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))
		in := validRedditInput()
		in.Naming = &n
		res, err := c.CreateCampaign(context.Background(), in)
		srv.Close()
		if _, ok := domain.AsPreflightRefusal(err); res != nil || !ok || requests.Load() != 0 {
			t.Errorf("naming %+v: CreateCampaign = %+v, %v after %d request(s); want a pre-flight refusal before any", n, res, err, requests.Load())
		}
		if n.BusinessUnit == "cncf" && !errors.Is(err, ErrNamingInvalid) {
			t.Errorf("naming %+v: %v; want ErrNamingInvalid", n, err)
		}
	}
}
