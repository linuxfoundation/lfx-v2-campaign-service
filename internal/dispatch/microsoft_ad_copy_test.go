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
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// msAdCopyCapture records the raw AddAds body and the number of API requests.
type msAdCopyCapture struct {
	mu     sync.Mutex
	adBody string
	calls  int
}

func (c *msAdCopyCapture) snapshot() (string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.adBody, c.calls
}

// microsoftAdCopyServers serves a full create and captures the AddAds body.
func microsoftAdCopyServers(t *testing.T) ([]microsoft.Option, *msAdCopyCapture) {
	t.Helper()
	capture := &msAdCopyCapture{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		capture.mu.Lock()
		capture.calls++
		capture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case strings.HasSuffix(p, "/Ads/QueryByAdGroupId"):
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case strings.HasSuffix(p, "/Campaigns"):
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/AdGroups"):
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Ads"):
			b, _ := io.ReadAll(r.Body)
			capture.mu.Lock()
			capture.adBody = string(b)
			capture.mu.Unlock()
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return []microsoft.Option{microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL)}, capture
}

// noUpstreamCreate reports whether err says nothing was created upstream (the claim is released).
func noUpstreamCreate(err error) bool {
	var nuc interface{ NoUpstreamCreate() bool }
	return errors.As(err, &nuc) && nuc.NoUpstreamCreate()
}

const msAdCopyConfig = `{"microsoftConfig":{"budget":50,` +
	`"headlines":[" KubeCon NA ","Register Today","Atlanta, Nov 10-13"],` +
	`"descriptions":["Join cloud native practitioners.","Early bird pricing ends soon."],` +
	`"path1":"kubecon","path2":"register"}}`

// TestMicrosoftAdCopy_GateOffRefusesOnBothPaths: with the flag unset (or not exactly "true"),
// each field is a synchronous 400 naming the flag from PreflightCreate, and a pre-create Dispatch
// failure with no Microsoft request.
func TestMicrosoftAdCopy_GateOffRefusesOnBothPaths(t *testing.T) {
	for _, val := range []string{"", "TRUE", "1"} {
		for _, cfg := range []string{
			`{"microsoftConfig":{"budget":50,"headlines":["a","b","c"]}}`,
			`{"microsoftConfig":{"budget":50,"descriptions":["a","b"]}}`,
			`{"microsoftConfig":{"budget":50,"path1":"x"}}`,
			`{"microsoftConfig":{"budget":50,"path2":"x"}}`,
		} {
			t.Run(val+cfg, func(t *testing.T) {
				t.Setenv(constants.EnvMicrosoftAdCopyEnabled, val)
				opts, capture := microsoftAdCopyServers(t)
				d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
				err := d.PreflightCreate(context.Background(), "p1", model.ProviderMicrosoftAds, json.RawMessage(cfg))
				if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), constants.EnvMicrosoftAdCopyEnabled) {
					t.Fatalf("PreflightCreate = %v; want a 400 naming %s", err, constants.EnvMicrosoftAdCopyEnabled)
				}
				camp, derr := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, json.RawMessage(cfg))
				if camp != nil || derr == nil || !noUpstreamCreate(derr) {
					t.Fatalf("Dispatch = (%v, %v); want a pre-create failure", camp, derr)
				}
				if !strings.Contains(derr.Error(), constants.EnvMicrosoftAdCopyEnabled) {
					t.Errorf("Dispatch error %q does not name the flag", derr)
				}
				if _, calls := capture.snapshot(); calls != 0 {
					t.Errorf("%d Microsoft request(s) with the gate off", calls)
				}
			})
		}
	}
}

// TestMicrosoftAdCopy_GateOffIgnoresAbsentAndNull: a config without copy, or with explicit
// nulls / empty values, is not refused by the gate.
func TestMicrosoftAdCopy_GateOffIgnoresAbsentAndNull(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdCopyEnabled, "")
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{})
	for _, cfg := range []string{
		`{"microsoftConfig":{"budget":50}}`,
		`{"microsoftConfig":{"budget":50,"headlines":null,"descriptions":[],"path1":null,"path2":""}}`,
	} {
		if err := d.PreflightCreate(context.Background(), "p1", model.ProviderMicrosoftAds, json.RawMessage(cfg)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("PreflightCreate(%s) = %v; want not applicable", cfg, err)
		}
	}
}

// TestMicrosoftAdCopy_PreflightRules: with the gate on, each rule breach and each wrong JSON type
// (case-variant keys included) is a synchronous 400 naming the field.
func TestMicrosoftAdCopy_PreflightRules(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdCopyEnabled, "true")
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{})
	cases := map[string]struct{ cfg, want string }{
		"two headlines":         {`{"headlines":["a","b"]}`, "microsoftConfig.headlines: must have 3 to 15 headlines, got 2"},
		"one description":       {`{"descriptions":["a"]}`, "microsoftConfig.descriptions: must have 2 to 4 descriptions, got 1"},
		"blank headline":        {`{"headlines":["a"," ","c"]}`, "microsoftConfig.headlines: headline 2 must contain at least one word"},
		"long headline":         {`{"headlines":["` + strings.Repeat("a", 31) + `","b","c"]}`, "microsoftConfig.headlines: headline 1 exceeds 30"},
		"duplicate description": {`{"descriptions":["Same.","same."]}`, "microsoftConfig.descriptions: description 2 is a duplicate"},
		"path2 alone":           {`{"path2":"x"}`, "microsoftConfig.path2: requires path1"},
		"path slash":            {`{"path1":"a/b"}`, "microsoftConfig.path1: must not contain a forward slash"},
		"headlines not array":   {`{"headlines":"Alpha"}`, "microsoftConfig.headlines: must be an array of strings"},
		"headlines of numbers":  {`{"headlines":[1,2,3]}`, "microsoftConfig.headlines: must be an array of strings"},
		"path1 not string":      {`{"path1":7}`, "microsoftConfig.path1: must be a string"},
		"case-variant key":      {`{"Descriptions":{"x":1}}`, "microsoftConfig.descriptions: must be an array of strings"},
		"case-variant path":     {`{"PATH2":true}`, "microsoftConfig.path2: must be a string"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := `{"microsoftConfig":` + strings.Replace(tc.cfg, "{", `{"budget":50,`, 1) + `}`
			err := d.PreflightCreate(context.Background(), "p1", model.ProviderMicrosoftAds, json.RawMessage(cfg))
			if !errors.Is(err, domain.ErrCreateConfigInvalid) {
				t.Fatalf("PreflightCreate = %v; want ErrCreateConfigInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q lacks %q", err, tc.want)
			}
			var pr *domain.PreflightRefusal
			if !errors.As(err, &pr) {
				t.Errorf("error is not a PreflightRefusal: %v", err)
			}
		})
	}
}

// TestMicrosoftAdCopy_DispatchRechecks: a bad value that skipped preflight still fails Dispatch
// pre-create, with no Microsoft request.
func TestMicrosoftAdCopy_DispatchRechecks(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdCopyEnabled, "true")
	for _, cfg := range []string{
		`{"microsoftConfig":{"budget":50,"headlines":["a","b"]}}`,
		`{"microsoftConfig":{"budget":50,"path1":7}}`,
	} {
		opts, capture := microsoftAdCopyServers(t)
		d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
		camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, json.RawMessage(cfg))
		if camp != nil || !noUpstreamCreate(err) || !errors.Is(err, domain.ErrCreateConfigInvalid) {
			t.Fatalf("Dispatch(%s) = (%v, %v); want a pre-create config refusal", cfg, camp, err)
		}
		if _, calls := capture.snapshot(); calls != 0 {
			t.Errorf("%d Microsoft request(s) for an invalid config", calls)
		}
	}
}

// TestMicrosoftAdCopy_DispatchSendsCopyAndPaths: with the gate on, the caller copy (trimmed) and
// the paths reach the AddAds body, exactly and in order, and the snapshot records them.
func TestMicrosoftAdCopy_DispatchSendsCopyAndPaths(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdCopyEnabled, "true")
	opts, capture := microsoftAdCopyServers(t)
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
	if err := d.PreflightCreate(context.Background(), "p1", model.ProviderMicrosoftAds, json.RawMessage(msAdCopyConfig)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("PreflightCreate on a valid config = %v", err)
	}
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, json.RawMessage(msAdCopyConfig))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	body, _ := capture.snapshot()
	var ad struct {
		Ads []struct {
			Headlines, Descriptions []struct {
				Asset struct{ Text string }
			}
			Path1, Path2 string
		}
	}
	if err := json.Unmarshal([]byte(body), &ad); err != nil || len(ad.Ads) != 1 {
		t.Fatalf("AddAds body %q: %v", body, err)
	}
	texts := func(links []struct{ Asset struct{ Text string } }) []string {
		out := make([]string, len(links))
		for i, l := range links {
			out[i] = l.Asset.Text
		}
		return out
	}
	if got, want := strings.Join(texts(ad.Ads[0].Headlines), "|"), "KubeCon NA|Register Today|Atlanta, Nov 10-13"; got != want {
		t.Errorf("headlines = %q, want %q", got, want)
	}
	if got, want := strings.Join(texts(ad.Ads[0].Descriptions), "|"), "Join cloud native practitioners.|Early bird pricing ends soon."; got != want {
		t.Errorf("descriptions = %q, want %q", got, want)
	}
	if ad.Ads[0].Path1 != "kubecon" || ad.Ads[0].Path2 != "register" {
		t.Errorf("paths = %q/%q, want kubecon/register", ad.Ads[0].Path1, ad.Ads[0].Path2)
	}
	var snap map[string]any
	if err := json.Unmarshal(camp.ConfigSnapshot, &snap); err != nil {
		t.Fatalf("config snapshot: %v", err)
	}
	if snap["path1"] != "kubecon" || snap["path2"] != "register" {
		t.Errorf("snapshot paths = %v/%v", snap["path1"], snap["path2"])
	}
	if hs, ok := snap["headlines"].([]any); !ok || len(hs) != 3 {
		t.Errorf("snapshot headlines = %v", snap["headlines"])
	}
}

// TestMicrosoftAdCopy_SnapshotRedactsLinks: copy is caller free text bound for the unencrypted
// snapshot, so a link inside it is reduced there (what Microsoft receives is not).
func TestMicrosoftAdCopy_SnapshotRedactsLinks(t *testing.T) {
	cfg := microsoftConfig{Headlines: []string{"see https://example.test/reset/SECRET?token=V"}, Descriptions: []string{"x example.org/reset/SECRET"}}
	snap := microsoftSnapshotConfig(cfg)
	if strings.Contains(snap.Headlines[0], "SECRET") || strings.Contains(snap.Descriptions[0], "SECRET") {
		t.Errorf("snapshot kept a link path: %q / %q", snap.Headlines[0], snap.Descriptions[0])
	}
	if !strings.Contains(cfg.Headlines[0], "SECRET") {
		t.Error("snapshot rewrite mutated the config Dispatch sends")
	}
}
