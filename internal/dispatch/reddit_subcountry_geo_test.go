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

// geoValidatingRedditAPI is a fake Reddit API that also answers the geolocation validations read
// with validationStatus and validationBody.
type geoValidatingRedditAPI struct {
	mu               sync.Mutex
	paths            []string
	adGroupBodies    []map[string]any
	validationStatus int
	validationBody   string
}

func (f *geoValidatingRedditAPI) start(t *testing.T) *RedditDispatcher {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/targeting/geolocations_validations"):
			if f.validationStatus != 0 {
				w.WriteHeader(f.validationStatus)
			}
			_, _ = io.WriteString(w, f.validationBody)
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

func (f *geoValidatingRedditAPI) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// adGroups copies the captured ad-group bodies under the lock the handler writes them under.
func (f *geoValidatingRedditAPI) adGroups() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.adGroupBodies...)
}

// requireNoRedditWrite fails when any request other than the validations read was a POST.
func requireNoRedditWrite(t *testing.T, paths []string) {
	t.Helper()
	for _, p := range paths {
		if strings.HasPrefix(p, "POST ") && !strings.HasSuffix(p, "/targeting/geolocations_validations") {
			t.Fatalf("something was created: %v", paths)
		}
	}
}

const (
	subCountryExtra   = `,"geoTargets":["CA"],"subCountryGeoTargets":["us-ca","662"]`
	subCountryConfirm = `{"data":[{"geolocation":{"id":"US-CA","country":"US"}},{"geolocation":{"id":"662","country":"US"}}]}`
)

// Off (unset, or anything but the exact "true"): the field is a synchronous 400 naming the flag,
// refused again at dispatch, with no Reddit call of any kind.
func TestReddit_SubCountryGeo_GateOff(t *testing.T) {
	for _, flag := range []string{"", "false", "TRUE", "1", " true"} {
		t.Setenv(constants.EnvRedditSubCountryGeoEnabled, flag)
		f := &geoValidatingRedditAPI{validationBody: subCountryConfirm}
		d := f.start(t)
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAudienceCfg(subCountryExtra))
		if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), "subCountryGeoTargets disabled on this deployment (REDDIT_SUBCOUNTRY_GEO_ENABLED is not \"true\")") {
			t.Fatalf("flag %q: PreflightCreate = %v", flag, err)
		}
		_, derr := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAudienceCfg(subCountryExtra))
		requirePreCreate(t, derr)
		if !errors.Is(derr, domain.ErrCreateConfigInvalid) {
			t.Fatalf("flag %q: Dispatch = %v", flag, derr)
		}
		if p := f.seen(); len(p) != 0 {
			t.Fatalf("flag %q: Reddit was called: %v", flag, p)
		}
	}
}

// Off, an absent, null or empty list is no change: the create is not gated by it.
func TestReddit_SubCountryGeo_AbsentIsNotGated(t *testing.T) {
	t.Setenv(constants.EnvRedditSubCountryGeoEnabled, "")
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{})
	for _, extra := range []string{``, `,"subCountryGeoTargets":null`, `,"subCountryGeoTargets":[]`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAudienceCfg(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Fatalf("%q: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// A value that is not an array of strings is a synchronous 400, whatever the flag says.
func TestReddit_SubCountryGeo_WrongTypeRefused(t *testing.T) {
	for _, flag := range []string{"", "true"} {
		t.Setenv(constants.EnvRedditSubCountryGeoEnabled, flag)
		d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{})
		for _, extra := range []string{`,"subCountryGeoTargets":"US-CA"`, `,"subCountryGeoTargets":[662]`, `,"SubCountryGeoTargets":{"id":"US-CA"}`} {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAudienceCfg(extra))
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), "redditConfig.subCountryGeoTargets: must be an array of strings") {
				t.Fatalf("flag %q, %s: PreflightCreate = %v", flag, extra, err)
			}
			_, derr := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAudienceCfg(extra))
			requirePreCreate(t, derr)
			if !strings.Contains(derr.Error(), "must be an array of strings") {
				t.Fatalf("flag %q, %s: Dispatch = %v", flag, extra, derr)
			}
		}
	}
}

// On, a rule breach is a config-only 400: no credential lookup, no Reddit call, not marked as
// upstream-consulted. Overlap with a whole-country target, targeted or excluded, is one.
func TestReddit_SubCountryGeo_RuleBreachRefusedBeforeAnyCall(t *testing.T) {
	t.Setenv(constants.EnvRedditSubCountryGeoEnabled, "true")
	t.Setenv(constants.EnvRedditTargetingExclusionsEnabled, "true")
	for extra, want := range map[string]string{
		`,"geoTargets":["US"],"subCountryGeoTargets":["US-CA"]`:        `redditConfig.subCountryGeoTargets: entry 1 ("US-CA") is inside US, which geoTargets already targets`,
		`,"excludedGeoTargets":["GB"],"subCountryGeoTargets":["GB:1"]`: "redditConfig.subCountryGeoTargets: entry 1 (\"GB:1\") is inside GB, which excludedGeoTargets excludes",
		`,"subCountryGeoTargets":["US"]`:                               "is a whole country; put it in geoTargets",
		`,"subCountryGeoTargets":["California"]`:                       "is not a Reddit region",
	} {
		f := &geoValidatingRedditAPI{validationBody: subCountryConfirm}
		d := f.start(t)
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAudienceCfg(extra))
		if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: PreflightCreate = %v, want a 400 containing %q", extra, err, want)
		}
		var consulted interface{ UpstreamConsulted() bool }
		if errors.As(err, &consulted) {
			t.Errorf("%s: a rule breach is decided on the config alone", extra)
		}
		_, derr := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAudienceCfg(extra))
		requirePreCreate(t, derr)
		if p := f.seen(); len(p) != 0 {
			t.Fatalf("%s: Reddit was called: %v", extra, p)
		}
	}
}

// On, sub-country ids only: excluding the US is not refused, because the US default no longer
// applies (the exclusion rule reads the same effective targeting the create sends).
func TestReddit_SubCountryGeo_ExcludingUSWithSubCountryOnly(t *testing.T) {
	t.Setenv(constants.EnvRedditSubCountryGeoEnabled, "true")
	t.Setenv(constants.EnvRedditTargetingExclusionsEnabled, "true")
	f := &geoValidatingRedditAPI{validationBody: `{"data":[{"geolocation":{"id":"GB:2641599","country":"GB"}}]}`}
	d := f.start(t)
	cfg := redditAudienceCfg(`,"excludedGeoTargets":["US"],"subCountryGeoTargets":["GB:2641599"]`)
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, cfg); err != nil {
		t.Fatalf("PreflightCreate = %v, want nil", err)
	}
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	tg := targetingOf(t, f.adGroups()[0])
	if !reflect.DeepEqual(tg["geolocations"], []any{"GB:2641599"}) || !reflect.DeepEqual(tg["excluded_geolocations"], []any{"US"}) {
		t.Errorf("targeting = %v", tg)
	}
}

// On: the pre-check confirms the ids with Reddit (nil = checked and accepted), and Dispatch
// confirms them again before the campaign is created, then sends them after the countries.
func TestReddit_SubCountryGeo_ConfirmedAndSent(t *testing.T) {
	t.Setenv(constants.EnvRedditSubCountryGeoEnabled, "true")
	f := &geoValidatingRedditAPI{validationBody: subCountryConfirm}
	d := f.start(t)
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAudienceCfg(subCountryExtra)); err != nil {
		t.Fatalf("PreflightCreate = %v, want nil (checked and accepted)", err)
	}
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAudienceCfg(subCountryExtra)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	tg := targetingOf(t, f.adGroups()[0])
	if !reflect.DeepEqual(tg["geolocations"], []any{"CA", "US-CA", "662"}) {
		t.Errorf("geolocations = %v", tg["geolocations"])
	}
	// The validation ran in Dispatch too, before the campaign POST.
	paths := f.seen()
	validations, campaignAt := 0, -1
	for i, p := range paths {
		if strings.HasSuffix(p, "/targeting/geolocations_validations") {
			validations++
			if campaignAt >= 0 {
				t.Fatalf("validation after the campaign create: %v", paths)
			}
		}
		if strings.HasPrefix(p, "POST ") && strings.HasSuffix(p, "/campaigns") && campaignAt < 0 {
			campaignAt = i
		}
	}
	if validations != 2 || campaignAt < 0 {
		t.Fatalf("want one validation in each of PreflightCreate and Dispatch, then a create: %v", paths)
	}
}

// On: an id Reddit refuses is a synchronous 400 marked as upstream-consulted, and Dispatch refuses
// it again with nothing created.
func TestReddit_SubCountryGeo_UntargetableIDRefusedBeforeCreate(t *testing.T) {
	t.Setenv(constants.EnvRedditSubCountryGeoEnabled, "true")
	f := &geoValidatingRedditAPI{validationBody: `{"data":[{"geolocation":{"id":"US-CA","country":"US"}},{"geolocation":{"id":"662"},"error_message":"not targetable"}]}`}
	d := f.start(t)
	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAudienceCfg(subCountryExtra))
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), `invalid campaign config: redditConfig.subCountryGeoTargets: entry 2 ("662") is not a targetable Reddit geolocation`) {
		t.Fatalf("PreflightCreate = %v", err)
	}
	var consulted interface{ UpstreamConsulted() bool }
	if !errors.As(err, &consulted) || !consulted.UpstreamConsulted() {
		t.Error("the refusal must say a platform read produced it")
	}
	_, derr := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAudienceCfg(subCountryExtra))
	requirePreCreate(t, derr)
	requireNoRedditWrite(t, f.seen())
}

// On, a read that cannot be completed: the pre-check fails open (a plain error the orchestrator
// logs), and Dispatch fails closed with nothing created.
func TestReddit_SubCountryGeo_ValidationFailure(t *testing.T) {
	t.Setenv(constants.EnvRedditSubCountryGeoEnabled, "true")
	f := &geoValidatingRedditAPI{validationStatus: http.StatusInternalServerError, validationBody: `{}`}
	d := f.start(t)
	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAudienceCfg(subCountryExtra))
	if err == nil || errors.Is(err, domain.ErrCreateConfigInvalid) || errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("PreflightCreate = %v, want a plain (fail-open) error", err)
	}
	_, derr := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, redditAudienceCfg(subCountryExtra))
	requirePreCreate(t, derr)
	if !strings.Contains(derr.Error(), "reddit geolocations could not be confirmed (nothing was created)") {
		t.Errorf("Dispatch error = %v", derr)
	}
	requireNoRedditWrite(t, f.seen())
}

// With only the geo flag on, the community/interest caps of the resolution pre-check (its own
// flag) do not apply: the geo validation runs alone.
func TestReddit_SubCountryGeo_DoesNotTurnOnTargetingResolution(t *testing.T) {
	t.Setenv(constants.EnvRedditSubCountryGeoEnabled, "true")
	t.Setenv(constants.EnvRedditTargetingResolutionEnabled, "")
	subs := make([]string, reddit.MaxResolvedSubreddits+1)
	for i := range subs {
		subs[i] = `"sub` + strings.Repeat("x", i/26) + string(rune('a'+i%26)) + `"`
	}
	f := &geoValidatingRedditAPI{validationBody: subCountryConfirm}
	d := f.start(t)
	cfg := redditAudienceCfg(subCountryExtra + `,"subreddits":[` + strings.Join(subs, ",") + `]`)
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, cfg); err != nil {
		t.Fatalf("PreflightCreate = %v, want nil", err)
	}
	for _, p := range f.seen() {
		if strings.HasSuffix(p, "/targeting/communities") || strings.HasSuffix(p, "/targeting/interests") {
			t.Fatalf("a resolution lookup ran with its flag off: %v", f.seen())
		}
	}
}

// The persisted snapshot keeps the list (sanitized like geoTargets) and leaves an omitted one
// absent, so a stored snapshot replays through the gate exactly as the request did.
func TestReddit_SubCountryGeo_Snapshot(t *testing.T) {
	raw, _ := json.Marshal(redditSnapshotConfig(redditConfig{GeoTargets: []string{"CA"}}))
	if strings.Contains(string(raw), "subCountryGeoTargets") {
		t.Fatalf("an omitted list must stay absent: %s", raw)
	}
	snap := redditSnapshotConfig(redditConfig{SubCountryGeoTargets: []string{"US-CA", "https://user:pw@evil.example/x"}})
	if snap.SubCountryGeoTargets[0] != "US-CA" || strings.Contains(snap.SubCountryGeoTargets[1], "pw") {
		t.Fatalf("snapshot = %v", snap.SubCountryGeoTargets)
	}
}
