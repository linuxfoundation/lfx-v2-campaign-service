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

func linkedInVariantsConfig(variants string) json.RawMessage {
	return json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":` + variants + `
	}}`)
}

func TestLinkedIn_CallToActionRefusedWhileTheFlagIsOff(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsCallToActionEnabled, "")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
	cfg := linkedInVariantsConfig(`[{"introText":"Join us","headline":"KubeCon 2099","callToAction":"REGISTER"}]`)

	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, cfg)
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), constants.EnvLinkedInAdsCallToActionEnabled) {
		t.Fatalf("PreflightCreate = %v, want a 400 naming %s", err, constants.EnvLinkedInAdsCallToActionEnabled)
	}
	_, err = d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Fatalf("Dispatch = %v, want a not-created config refusal", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d LinkedIn requests for a refused create", n)
	}
}

func TestLinkedIn_VariantFieldsPreflight(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsCallToActionEnabled, "true")
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	const v = `"introText":"Join us","headline":"KubeCon 2099"`
	for _, tc := range []struct{ name, variants, want string }{
		{"valid", `[{` + v + `,"name":"Skill Up","callToAction":"REGISTER","description":"Labs"},{` + v + `,"name":"Big Tent"}]`, ""},
		{"none set", `[{` + v + `}]`, ""},
		{"empty strings are none", `[{` + v + `,"name":"","callToAction":"","description":""}]`, ""},
		{"unknown label", `[{` + v + `,"callToAction":"learn_more"}]`, "is not a LinkedIn call to action"},
		{"repeated name", `[{` + v + `,"name":"Skill Up"},{` + v + `,"name":"Skill Up"}]`, "repeats variant-1"},
		{"number label", `[{` + v + `,"callToAction":1}]`, "variants[0].callToAction must be a string"},
		{"null name", `[{` + v + `},{` + v + `,"name":null}]`, "variants[1].name must be a string"},
		{"object description", `[{` + v + `,"description":{"text":"x"}}]`, "variants[0].description must be a string"},
		{"key case follows the decoder", `[{` + v + `,"CallToAction":true}]`, "callToAction must be a string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, linkedInVariantsConfig(tc.variants))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, "linkedInConfig.variants") || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// TestLinkedIn_VariantFieldsReachTheWire is the end-to-end pin (#588, #589): Dispatch sends a
// variant's call to action and description on its dark post and its name on its creative, a
// variant without them keeps the earlier shape, and the stored snapshot redacts a link pasted
// into a name or description.
func TestLinkedIn_VariantFieldsReachTheWire(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsCallToActionEnabled, "true")
	var mu sync.Mutex
	var posts, creatives []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			mu.Lock()
			posts = append(posts, body)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			mu.Lock()
			creatives = append(creatives, body)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock))

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInVariantsConfig(`[
		{"introText":"Join us","headline":"KubeCon 2099","name":"OSSNA 2026 – Skill Up https://x.example/reset/s3cr3t","callToAction":"REGISTER","description":"Labs at https://x.example/magic/t0ken"},
		{"introText":"Join us","headline":"KubeCon 2099"}
	]`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %s, want %s", camp.Status, campaignStatusCreated)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 2 || len(creatives) != 2 {
		t.Fatalf("%d posts and %d creatives, want 2 of each", len(posts), len(creatives))
	}
	article := posts[0]["content"].(map[string]any)["article"].(map[string]any)
	if posts[0]["contentCallToActionLabel"] != "REGISTER" || posts[0]["contentLandingPage"] != article["source"] {
		t.Errorf("variant-1 post call to action = %v / %v, want REGISTER opening %v", posts[0]["contentCallToActionLabel"], posts[0]["contentLandingPage"], article["source"])
	}
	// The FULL text reaches LinkedIn; only the stored copy is redacted.
	if article["description"] != "Labs at https://x.example/magic/t0ken" {
		t.Errorf("variant-1 description = %v", article["description"])
	}
	if creatives[0]["name"] != "OSSNA 2026 – Skill Up https://x.example/reset/s3cr3t" {
		t.Errorf("variant-1 creative name = %v", creatives[0]["name"])
	}
	if _, ok := posts[1]["contentCallToActionLabel"]; ok {
		t.Errorf("variant-2 sent a call to action it was not given: %v", posts[1])
	}
	if got := posts[1]["content"].(map[string]any)["article"].(map[string]any)["description"]; got != "" {
		t.Errorf("variant-2 description = %v, want empty", got)
	}
	if !strings.HasSuffix(creatives[1]["name"].(string), " | variant-2") {
		t.Errorf("variant-2 creative name = %v, want the default", creatives[1]["name"])
	}
	snap := string(camp.ConfigSnapshot)
	if strings.Contains(snap, "s3cr3t") || strings.Contains(snap, "t0ken") {
		t.Errorf("snapshot keeps a pasted link's path: %s", snap)
	}
	if !strings.Contains(snap, `"CallToAction":"REGISTER"`) {
		t.Errorf("snapshot does not record the call to action: %s", snap)
	}
}
