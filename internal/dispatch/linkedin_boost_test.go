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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// linkedInBoostConfig is a create (#673) whose linkedInConfig carries the given extra fields; the
// base is a valid automatic-bid create with no creatives.
func linkedInBoostConfig(extra string) json.RawMessage {
	cfg := `{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}]`
	if extra != "" {
		cfg += "," + extra
	}
	return json.RawMessage(cfg + `}}`)
}

const validBoost = `"format":"boost","boostPost":"urn:li:ugcPost:6856810298419044352"`

func TestLinkedIn_BoostPreflight(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsBoostPostEnabled, "true")
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct {
		name string
		cfg  json.RawMessage
		want string // "" not applicable
	}{
		{"valid ugcPost", linkedInBoostConfig(validBoost), ""},
		{"valid share", linkedInBoostConfig(`"format":"boost","boostPost":"urn:li:share:7001"`), ""},
		{"manual bidding", linkedInBoostConfig(validBoost + `,"bidding":{"strategy":"manual","bidAmount":5.04}`), ""},
		{"objective conversions", linkedInBoostConfig(validBoost + `,"objective":"conversions","bidding":{"strategy":"manual","bidAmount":5.04}`), ""},
		{"empty variants", linkedInBoostConfig(validBoost + `,"variants":[]`), ""},
		{"no boost post", linkedInBoostConfig(`"format":"boost"`), "requires boostPost"},
		{"empty boost post", linkedInBoostConfig(`"format":"boost","boostPost":""`), "requires boostPost"},
		{"activity URN", linkedInBoostConfig(`"format":"boost","boostPost":"urn:li:activity:7001"`), "boostPost must be a LinkedIn post URN"},
		{"feed URL", linkedInBoostConfig(`"format":"boost","boostPost":"https://www.linkedin.com/feed/update/urn:li:share:7001/"`), "boostPost must be a LinkedIn post URN"},
		{"padded URN", linkedInBoostConfig(`"format":"boost","boostPost":" urn:li:share:7001"`), "boostPost must be a LinkedIn post URN"},
		{"non-numeric id", linkedInBoostConfig(`"format":"boost","boostPost":"urn:li:share:abc"`), "boostPost must be a LinkedIn post URN"},
		{"boost post as a number", linkedInBoostConfig(`"format":"boost","boostPost":7001`), "boostPost must be a string"},
		{"boost post null", linkedInBoostConfig(`"format":"boost","boostPost":null`), "boostPost must be a string"},
		{"boost post without format", linkedInBoostConfig(`"boostPost":"urn:li:share:7001","variants":[{"introText":"i","headline":"h"}]`), "boostPost is accepted only with format \"boost\""},
		{"boost post with text format", linkedInBoostConfig(`"format":"text","boostPost":"urn:li:share:7001"`), "boostPost is accepted only with format \"boost\""},
		{"boost post with carousel format", linkedInBoostConfig(`"format":"carousel","boostPost":"urn:li:share:7001"`), "boostPost is accepted only with format \"boost\""},
		{"boost with a document", linkedInBoostConfig(validBoost + `,"documentTitle":"t"`), "accepted only with format \"document\""},
		{"boost with variants", linkedInBoostConfig(validBoost + `,"variants":[{"introText":"i","headline":"h"}]`), "omit variants"},
		{"boost with text ads", linkedInBoostConfig(validBoost + `,"textAds":[{"headline":"h","description":"d"}]`), "textAds are accepted only with format"},
		{"upper-case format", linkedInBoostConfig(`"format":"BOOST","boostPost":"urn:li:share:7001"`), "format must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, tc.cfg)
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// TestLinkedIn_BoostRefusedWhileTheFlagIsOff: only the exact value "true" enables the format; any
// other value refuses it with a 400 and Dispatch sends nothing.
func TestLinkedIn_BoostRefusedWhileTheFlagIsOff(t *testing.T) {
	for _, v := range []string{"", "false", "TRUE", "1", " true"} {
		t.Run("flag="+v, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsBoostPostEnabled, v)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer srv.Close()
			d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
			cfg := linkedInBoostConfig(validBoost)
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, cfg)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), constants.EnvLinkedInAdsBoostPostEnabled) {
				t.Fatalf("PreflightCreate = %v, want a 400 naming %s", err, constants.EnvLinkedInAdsBoostPostEnabled)
			}
			_, err = d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !isNoUpstreamCreate(err) {
				t.Fatalf("Dispatch = %v, want a not-created config refusal", err)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%d LinkedIn requests for a refused create", n)
			}
		})
	}
}

// liBoostAPI is a fake LinkedIn API for a boost create: it answers the post read with post and
// records the order of requests and the creative bodies.
type liBoostAPI struct {
	srv       *httptest.Server
	mu        sync.Mutex
	post      string
	calls     []string
	campaign  map[string]any
	creatives []map[string]any
	// existing makes the group and campaign searches find what an earlier create left (group 100
	// with the group POST's name, campaign 200 with the campaign POST's name), so the next create
	// reuses both. finderStatus fails the creatives finder; creativeFound makes it answer with a
	// creative for the post. finderCalls counts the finder requests.
	existing      bool
	group         map[string]any
	finderStatus  int
	creativeFound bool
	finderCalls   int
}

func newLIBoostAPI(t *testing.T, post string) *liBoostAPI {
	t.Helper()
	a := &liBoostAPI{post: post}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		a.calls = append(a.calls, r.Method+" "+r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/posts/"):
			_, _ = io.WriteString(w, a.post)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/creatives"):
			a.finderCalls++
			switch {
			case a.finderStatus != 0:
				w.WriteHeader(a.finderStatus)
				_, _ = io.WriteString(w, `{"status":503,"message":"unavailable"}`)
			case a.creativeFound:
				_, _ = io.WriteString(w, `{"elements":[{"id":"urn:li:sponsoredCreative:777"}],"metadata":{}}`)
			default:
				_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			}
		case r.Method == http.MethodGet && a.existing && strings.HasSuffix(r.URL.Path, "/adCampaignGroups"):
			b, _ := json.Marshal(map[string]any{"elements": []any{map[string]any{"id": 100, "name": a.group["name"], "status": "ACTIVE"}}, "metadata": map[string]any{}})
			_, _ = w.Write(b)
		case r.Method == http.MethodGet && a.existing && strings.HasSuffix(r.URL.Path, "/adCampaignGroups/100"):
			b, _ := json.Marshal(map[string]any{"id": 100, "runSchedule": a.group["runSchedule"]})
			_, _ = w.Write(b)
		case r.Method == http.MethodGet && a.existing && strings.HasSuffix(r.URL.Path, "/adCampaigns"):
			b, _ := json.Marshal(map[string]any{"elements": []any{map[string]any{
				"id": 200, "name": a.campaign["name"], "status": "PAUSED", "type": a.campaign["type"],
				"objectiveType": a.campaign["objectiveType"], "optimizationTargetType": a.campaign["optimizationTargetType"],
				"campaignGroup": "urn:li:sponsoredCampaignGroup:100",
			}}, "metadata": map[string]any{}})
			_, _ = w.Write(b)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_ = json.Unmarshal(raw, &a.group)
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			_ = json.Unmarshal(raw, &a.campaign)
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "creatives"):
			var b map[string]any
			_ = json.Unmarshal(raw, &b)
			a.creatives = append(a.creatives, b)
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func newLIBoostDispatcher(api *liBoostAPI) *LinkedInDispatcher {
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	return NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(api.srv.URL), linkedin.WithClock(clock))
}

// boostPostBody is a Posts API answer for the validBoost post, authored by author.
func boostPostBody(author string) string {
	return `{"id":"urn:li:ugcPost:6856810298419044352","author":"` + author + `","lifecycleState":"PUBLISHED","visibility":"PUBLIC",
		"distribution":{"feedDistribution":"MAIN_FEED"},"content":{"media":{"id":"urn:li:image:C4E22AQ"}}}`
}

// TestLinkedIn_BoostEndToEnd drives Dispatch through the client against a fake API (#673): the
// post is read before the first POST, the campaign is the ordinary SPONSORED_UPDATES one, no dark
// post is sent, the one creative references the post, and the created row records the post in
// its result and snapshot.
func TestLinkedIn_BoostEndToEnd(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsBoostPostEnabled, "true")
	api := newLIBoostAPI(t, boostPostBody("urn:li:organization:987654321"))
	camp, err := newLIBoostDispatcher(api).Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInBoostConfig(validBoost))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated || camp.PlatformCampaignID != "200" {
		t.Errorf("row = %q/%q, want created/200", camp.Status, camp.PlatformCampaignID)
	}
	if !strings.Contains(camp.CampaignName, "| Traffic | Prospecting | Boost |") {
		t.Errorf("campaign name = %q, want the Boost Ad Format segment", camp.CampaignName)
	}
	var result linkedin.CampaignResult
	if err := json.Unmarshal(camp.Result, &result); err != nil || result.BoostedPostURN != "urn:li:ugcPost:6856810298419044352" || result.CreativeCount != 1 {
		t.Errorf("result = %s (%v), want boostedPostUrn and one creative", camp.Result, err)
	}
	var snap linkedinConfig
	if err := json.Unmarshal(camp.ConfigSnapshot, &snap); err != nil || snap.Format != linkedin.FormatBoost || snap.BoostPost != "urn:li:ugcPost:6856810298419044352" {
		t.Errorf("snapshot = %s (%v), want format boost and the post", camp.ConfigSnapshot, err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.calls) == 0 || api.calls[0] != "GET /posts/urn%3Ali%3AugcPost%3A6856810298419044352" {
		t.Errorf("first request = %v, want the post read", api.calls)
	}
	if api.campaign["type"] != "SPONSORED_UPDATES" {
		t.Errorf("campaign body = %v", api.campaign)
	}
	if len(api.creatives) != 1 {
		t.Fatalf("creatives = %d, want 1", len(api.creatives))
	}
	if ref, _ := api.creatives[0]["content"].(map[string]any)["reference"].(string); ref != "urn:li:ugcPost:6856810298419044352" {
		t.Errorf("creative content = %v", api.creatives[0]["content"])
	}
}

// TestLinkedIn_BoostOfAnotherOrganizationsPostCreatesNothing: a post the connection's organization
// did not author fails the job with the post refusal, releases the claim, and sends no POST.
func TestLinkedIn_BoostOfAnotherOrganizationsPostCreatesNothing(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsBoostPostEnabled, "true")
	api := newLIBoostAPI(t, boostPostBody("urn:li:organization:5515715"))
	camp, err := newLIBoostDispatcher(api).Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInBoostConfig(validBoost))
	if camp != nil || !isNoUpstreamCreate(err) || !errors.Is(err, linkedin.ErrBoostPostIneligible) {
		t.Fatalf("Dispatch = %v, %v; want nothing created and the post refusal", camp, err)
	}
	msg, pre := domain.AsPreflightRefusal(err)
	if !pre || !strings.Contains(msg, `is authored by "urn:li:organization:5515715", not by urn:li:organization:987654321`) {
		t.Errorf("refusal = %q (pre-flight %v)", msg, pre)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, c := range api.calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("mutating request for a refused post: %s", c)
		}
	}
}

// TestLinkedIn_BoostThroughAGrantedAccount: with account selection on, a boost naming one of the
// LF system row's grants reads the post against the GRANT's organization. A post by that
// organization is boosted in the granted account; a post by the row's default organization is
// refused with nothing sent, since it is not the granted page's own post.
func TestLinkedIn_BoostThroughAGrantedAccount(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsBoostPostEnabled, "true")
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	cfg := linkedInBoostConfig(validBoost + `,"adAccountId":"500928401"`)
	dispatcher := func(api *liBoostAPI) *LinkedInDispatcher {
		repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}}
		return NewLinkedInDispatcher(repo, identityEncryptor{}, linkedin.WithBaseURL(api.srv.URL),
			linkedin.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	}

	t.Run("the grant's own post", func(t *testing.T) {
		api := newLIBoostAPI(t, boostPostBody("urn:li:organization:12893459"))
		camp, err := dispatcher(api).Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if got := linkedInCreationAccountID(camp); got != "500928401" {
			t.Errorf("created row records account %q, want the granted 500928401", got)
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		if len(api.creatives) != 1 {
			t.Fatalf("creatives = %d, want 1", len(api.creatives))
		}
		for _, c := range api.calls {
			if strings.Contains(c, "538170226") {
				t.Errorf("a request named the default account: %s", c)
			}
		}
	})

	t.Run("the default organization's post", func(t *testing.T) {
		api := newLIBoostAPI(t, boostPostBody("urn:li:organization:208777"))
		camp, err := dispatcher(api).Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
		if camp != nil || !isNoUpstreamCreate(err) || !errors.Is(err, linkedin.ErrBoostPostIneligible) {
			t.Fatalf("Dispatch = %v, %v; want nothing created and the post refusal", camp, err)
		}
		if msg, _ := domain.AsPreflightRefusal(err); !strings.Contains(msg, "not by urn:li:organization:12893459") {
			t.Errorf("refusal = %q, want it to name the grant's organization", msg)
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		for _, c := range api.calls {
			if !strings.HasPrefix(c, "GET ") {
				t.Errorf("mutating request for a refused post: %s", c)
			}
		}
	})
}

// TestLinkedIn_BoostLookupFailureIsRetried (#680 review): a create that reuses a campaign found by
// name and cannot look up its creatives fails as not created (claim released), not as a
// created_degraded row the orchestrator would reuse as complete, nor a pending one it would refuse
// to retry. The next Dispatch of the same brief finds the campaign again and repeats the lookup.
func TestLinkedIn_BoostLookupFailureIsRetried(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsBoostPostEnabled, "true")
	api := newLIBoostAPI(t, boostPostBody("urn:li:organization:987654321"))
	d := newLIBoostDispatcher(api)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInBoostConfig(validBoost)); err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}

	api.mu.Lock()
	api.existing, api.finderStatus, api.creatives = true, http.StatusServiceUnavailable, nil
	api.mu.Unlock()
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInBoostConfig(validBoost))
	if camp != nil || !isNoUpstreamCreate(err) || !strings.Contains(err.Error(), "repeats the lookup") {
		t.Fatalf("Dispatch with the finder down = %+v, %v; want nothing recorded and a not-created error", camp, err)
	}

	api.mu.Lock()
	api.finderStatus, api.creativeFound = 0, true
	api.mu.Unlock()
	camp, err = d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInBoostConfig(validBoost))
	if err != nil {
		t.Fatalf("retry Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated || camp.PlatformCampaignID != "200" {
		t.Errorf("retry row = %q/%q, want created/200", camp.Status, camp.PlatformCampaignID)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.finderCalls != 2 || len(api.creatives) != 0 {
		t.Errorf("finder calls %d, creative POSTs %d; want the lookup on both reuses and no second creative", api.finderCalls, len(api.creatives))
	}
}
