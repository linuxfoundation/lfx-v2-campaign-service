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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// resolvingRedditAPI is a fake Reddit API that also serves the targeting lookups: communities
// "kubernetes" and "devops" (Reddit spelling "Kubernetes"/"devops"), interests technology_v3
// ("Technology") and programming_v3 ("Programming"). lookupStatus makes both lookups answer it.
type resolvingRedditAPI struct {
	mu            sync.Mutex
	paths         []string
	adGroupBodies []map[string]any
	lookupStatus  int
}

func (f *resolvingRedditAPI) start(t *testing.T) *RedditDispatcher {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/targeting/communities"), strings.HasSuffix(r.URL.Path, "/targeting/interests"):
			if f.lookupStatus != 0 {
				w.WriteHeader(f.lookupStatus)
				_, _ = io.WriteString(w, `{}`)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/targeting/communities") {
				_, _ = io.WriteString(w, `{"data":[{"id":"t5_1","name":"Kubernetes"},{"id":"t5_2","name":"devops"}],"pagination":{"next_url":null}}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"technology_v3","name":"Technology","category":"Tech"},{"id":"programming_v3","name":"Programming","category":"Tech"}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ad_groups"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			f.adGroupBodies = append(f.adGroupBodies, body)
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
	return NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }))
}

func (f *resolvingRedditAPI) saw(suffix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.paths {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

const redditResolveExtra = `,"subreddits":["r/kubernetes","DevOps"],"interests":["technology","Programming"]`

// Off (the default): no lookup is made, and the values go to the ad group as given.
func TestReddit_TargetingResolution_OffSendsValuesAsGiven(t *testing.T) {
	t.Setenv(constants.EnvRedditTargetingResolutionEnabled, "")
	f := &resolvingRedditAPI{}
	d := f.start(t)
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAudienceCfg(redditResolveExtra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("PreflightCreate = %v, want not applicable", err)
	}
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAudienceCfg(redditResolveExtra)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if f.saw("/targeting/communities") || f.saw("/targeting/interests") {
		t.Fatalf("a lookup was made with the flag off: %v", f.paths)
	}
	tg := targetingOf(t, f.adGroupBodies[0])
	if !reflect.DeepEqual(tg["communities"], []any{"kubernetes", "DevOps"}) || !reflect.DeepEqual(tg["interests"], []any{"technology", "Programming"}) {
		t.Errorf("targeting = %v", tg)
	}
}

// On: the ad group gets Reddit's community spelling and interest ids (an id matched ignoring
// case, a label matched by name).
func TestReddit_TargetingResolution_DispatchSendsResolvedValues(t *testing.T) {
	t.Setenv(constants.EnvRedditTargetingResolutionEnabled, "true")
	f := &resolvingRedditAPI{}
	d := f.start(t)
	cfg := redditAudienceCfg(`,"subreddits":["r/kubernetes","DevOps"],"interests":["TECHNOLOGY_V3","Programming"]`)
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, cfg); err != nil {
		t.Fatalf("PreflightCreate = %v, want nil (checked and accepted)", err)
	}
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	tg := targetingOf(t, f.adGroupBodies[0])
	if !reflect.DeepEqual(tg["communities"], []any{"Kubernetes", "devops"}) || !reflect.DeepEqual(tg["interests"], []any{"technology_v3", "programming_v3"}) {
		t.Errorf("targeting = %v", tg)
	}
}

// On: an unknown community or interest is a synchronous 400 naming the entry, and Dispatch refuses
// it again before the campaign is created.
func TestReddit_TargetingResolution_UnknownValueRefusedBeforeCreate(t *testing.T) {
	t.Setenv(constants.EnvRedditTargetingResolutionEnabled, "true")
	for extra, want := range map[string]string{
		`,"subreddits":["kubernetes","nosuchsub"]`: "invalid campaign config: redditConfig.subreddits: entry 2",
		`,"interests":["Technology","Knitting"]`:   "invalid campaign config: redditConfig.interests: entry 2",
	} {
		f := &resolvingRedditAPI{}
		d := f.start(t)
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAudienceCfg(extra))
		if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: PreflightCreate = %v, want a 400 containing %q", extra, err, want)
		}
		var consulted interface{ UpstreamConsulted() bool }
		if !errors.As(err, &consulted) || !consulted.UpstreamConsulted() {
			t.Errorf("%s: the refusal must say a platform read produced it", extra)
		}
		_, derr := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAudienceCfg(extra))
		requirePreCreate(t, derr)
		for _, p := range f.paths {
			if strings.HasPrefix(p, "POST ") {
				t.Fatalf("%s: something was created: %v", extra, f.paths)
			}
		}
	}
}

// On, a lookup that cannot run: the pre-check fails open (a plain error the orchestrator logs and
// ignores), and Dispatch fails closed, before anything is created.
func TestReddit_TargetingResolution_LookupFailure(t *testing.T) {
	t.Setenv(constants.EnvRedditTargetingResolutionEnabled, "true")
	f := &resolvingRedditAPI{lookupStatus: http.StatusInternalServerError}
	d := f.start(t)
	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAudienceCfg(redditResolveExtra))
	if err == nil || errors.Is(err, domain.ErrCreateConfigInvalid) || errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("PreflightCreate = %v, want a plain (fail-open) error", err)
	}
	_, derr := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAudienceCfg(redditResolveExtra))
	requirePreCreate(t, derr)
	if !strings.Contains(derr.Error(), "nothing was created") {
		t.Errorf("Dispatch error = %v", derr)
	}
	for _, p := range f.paths {
		if strings.HasPrefix(p, "POST ") {
			t.Fatalf("a write was made after a failed lookup: %v", f.paths)
		}
	}
}
