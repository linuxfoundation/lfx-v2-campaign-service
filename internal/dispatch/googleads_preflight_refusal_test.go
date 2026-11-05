// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// gaPMaxConfig is a Performance Max dispatch config whose creative passes every local rule
// except what the caller changes, with its images taken from stored asset ids.
func gaPMaxConfig(t *testing.T, mutate func(map[string]any)) json.RawMessage {
	t.Helper()
	ref := []map[string]string{{"imageAssetId": gaAssetID}}
	creative := map[string]any{
		"marketingImages":       ref,
		"squareMarketingImages": ref,
		"logoImages":            ref,
		"headlines":             []string{"Join us at KubeCon", "Three days in Amsterdam", "Meet the maintainers"},
		"longHeadlines":         []string{"KubeCon + CloudNativeCon Europe 2026, Amsterdam"},
		"descriptions":          []string{"Talks, workshops and hallway track.", "Register now and save."},
		"businessName":          "Linux Foundation",
	}
	if mutate != nil {
		mutate(creative)
	}
	b, err := json.Marshal(map[string]any{"googleAdsConfig": map[string]any{
		"budget": 50, "channel": "performance-max", "performanceMaxCreative": creative,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// gaStoredImageConfig is a channel's create config whose image slots all name the stored asset
// gaAssetID, so the image fetch is what judges them; the copy passes every local rule.
func gaStoredImageConfig(t *testing.T, channel string) json.RawMessage {
	t.Helper()
	ref := []map[string]string{{"imageAssetId": gaAssetID}}
	inner := map[string]any{"budget": 50, "channel": channel}
	switch channel {
	case "search":
		inner["searchAssets"] = map[string]any{"adImages": ref}
	case "demand-gen":
		inner["demandGenCreative"] = map[string]any{
			"marketingImages": ref,
			"logoImages":      ref,
			"headlines":       []string{"Join us at KubeCon"},
			"descriptions":    []string{"Talks, workshops and hallway track."},
			"businessName":    "Linux Foundation",
		}
	case "display":
		inner["displayCreative"] = map[string]any{
			"marketingImages":       ref,
			"squareMarketingImages": ref,
			"headlines":             []string{"Join us at KubeCon"},
			"longHeadline":          "KubeCon + CloudNativeCon Europe 2026, Amsterdam",
			"descriptions":          []string{"Talks, workshops and hallway track."},
			"businessName":          "Linux Foundation",
		}
	default:
		t.Fatalf("no stored-image config for channel %q", channel)
	}
	b, err := json.Marshal(map[string]any{"googleAdsConfig": inner})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGoogleAds_PreflightRefusalsReachTheJobResult is #494's end-to-end pin, the Google sibling
// of TestDispatch_PreflightRefusalsAreOperatorSafe: a REAL Google dispatch refused by the client
// before anything is sent reaches service.DispatchFailureMessage as the client's own sentence,
// not "platform campaign creation failed". It covers the prod case (a Performance Max create with
// no description of 60 or less), a flight window, and an image refused during the pre-budget
// fetch rather than the pure checks.
func TestGoogleAds_PreflightRefusalsReachTheJobResult(t *testing.T) {
	small := msTestPNG(t, 100, 100)
	cases := []struct {
		name   string
		config json.RawMessage
		image  []byte
		want   string
	}{
		{
			name: "performance max with no description of 60 or less",
			config: gaPMaxConfig(t, func(c map[string]any) {
				c["descriptions"] = []string{strings.Repeat("d", 61), strings.Repeat("e", 70)}
			}),
			want: "google-ads Performance Max asset group needs at least one description with a display width of 60 or less",
		},
		{
			name:   "end date before start date",
			config: json.RawMessage(`{"googleAdsConfig":{"budget":50,"startDate":"2099-03-10","endDate":"2099-03-01"}}`),
			want:   "google-ads campaign end date 2099-03-01 must not be before start date 2099-03-10",
		},
		{
			// Not an input rule but a channel Google cannot create; the operator who picked
			// it needs to read why rather than the generic string.
			name:   "video, which the Google Ads API cannot create",
			config: videoWireConfig(t, nil),
			want:   "google-ads cannot create Video (YouTube) campaigns",
		},
		{
			name:   "stored image below the slot minimum",
			config: gaPMaxConfig(t, nil),
			image:  small,
			want:   "is 100x100 (png), below the 600x314 minimum",
		},
		// The same fetch refusal on each other channel that fetches images before its budget,
		// so every Create*Campaign's call to the refusal marker is pinned, not only Performance
		// Max's.
		{
			name:   "search ad image below the slot minimum",
			config: gaStoredImageConfig(t, "search"),
			image:  small,
			want:   "is 100x100 (png), below the 300x300 minimum",
		},
		{
			name:   "demand gen image below the slot minimum",
			config: gaStoredImageConfig(t, "demand-gen"),
			image:  small,
			want:   "is 100x100 (png), below the 600x314 minimum",
		},
		{
			name:   "display image below the slot minimum",
			config: gaStoredImageConfig(t, "display"),
			image:  small,
			want:   "is 100x100 (png), below the 600x314 minimum",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &noSendTransport{}
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
				googleads.WithHTTPClient(&http.Client{Transport: tr}))
			image := tc.image
			if image == nil {
				image = msTestPNG(t, 1200, 628)
			}
			d.SetCreativeAssetRepo(gaStoredAsset(image))
			d.SetAssetReserver(NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second))

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, tc.config)
			if err == nil {
				t.Fatalf("expected a pre-flight refusal, got campaign %+v", camp)
			}
			if camp != nil {
				t.Errorf("a pre-flight refusal must return no campaign, got %+v", camp)
			}
			if _, ok := domain.AsPreflightRefusal(err); !ok {
				t.Fatalf("the refusal is not marked operator-safe, so the job reports the generic string; err = %v", err)
			}
			var nuc interface{ NoUpstreamCreate() bool }
			if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("the claim contract changed: a pre-flight refusal must stay NoUpstreamCreate, got %T", err)
			}
			if n := tr.sent.Load(); n != 0 {
				t.Errorf("%d request(s) were sent before the refusal; a pre-flight refusal must send nothing", n)
			}
			got := service.DispatchFailureMessage(model.ProviderGoogleAds, err, true)
			if !strings.Contains(got, tc.want) {
				t.Errorf("job result error = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestGoogleAds_PreflightRefusalsNeverEchoHostileCallerInput is the Google sibling of
// TestDispatch_RefusalsNeverEchoHostileCallerInput: a refusal that quotes a secret-bearing
// caller value carries "(value withheld)" in the job result, and the rule it names survives.
func TestGoogleAds_PreflightRefusalsNeverEchoHostileCallerInput(t *testing.T) {
	const hostileURL = "https://x.example/reset/SECRET-494?token=1"
	for _, tc := range []struct {
		name   string
		config json.RawMessage
		want   string
	}{
		{
			name: "a headline holding a secret-bearing URL",
			config: gaPMaxConfig(t, func(c map[string]any) {
				c["headlines"] = []string{hostileURL, "Three days in Amsterdam", "Meet the maintainers"}
			}),
			want: "google-ads Performance Max asset group headline (value withheld) has a display width of",
		},
		{
			name: "a sitelink whose text holds a secret",
			config: json.RawMessage(`{"googleAdsConfig":{"budget":50,"sitelinks":[` +
				`{"text":"reset/SECRET-494","finalUrl":"https://events.example/kc"},` +
				`{"text":"reset/SECRET-494","finalUrl":"https://events.example/kc"}]}}`),
			want: "google-ads sitelink (value withheld) is listed more than once",
		},
		{
			// The placement refusal formats a redacted URL; only its quotes keep the scheme,
			// host and path out of the job result.
			name: "a display excluded placement with a scheme",
			config: json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"display",` +
				`"excludedPlacements":["https://example.com/SECRET-494-path"]}}`),
			want: "excluded placement (value withheld) carries a scheme",
		},
		{
			// net/url quotes the bad port, and the 100-byte cap on the redacted cause cuts that
			// quote's closing mark, so only the unterminated-quote rule keeps the port text out.
			name: "a display image URL whose malformed port is longer than the error cap",
			config: json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"display","displayCreative":{` +
				`"marketingImages":[{"url":"https://example.com:SECRET-494-` + strings.Repeat("x", 180) + `/image.png"}],` +
				`"squareMarketingImages":[{"imageAssetId":"` + gaAssetID + `"}],` +
				`"headlines":["Join us at KubeCon"],"longHeadline":"KubeCon + CloudNativeCon Europe 2026, Amsterdam",` +
				`"descriptions":["Talks, workshops and hallway track."],"businessName":"Linux Foundation"}}}`),
			want: "(value withheld)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &noSendTransport{}
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
				googleads.WithHTTPClient(&http.Client{Transport: tr}))
			d.SetCreativeAssetRepo(gaStoredAsset(msTestPNG(t, 1200, 628)))
			d.SetAssetReserver(NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second))

			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, tc.config)
			if err == nil {
				t.Fatal("expected a pre-flight refusal")
			}
			if !strings.Contains(err.Error(), "SECRET-494") {
				t.Fatalf("the refusal no longer quotes the value, so the leak check proves nothing: %v", err)
			}
			got := service.DispatchFailureMessage(model.ProviderGoogleAds, err, true)
			if !strings.Contains(got, tc.want) {
				t.Errorf("job result error = %q, want it to contain %q", got, tc.want)
			}
			for _, leak := range []string{"SECRET", "://", "token="} {
				if strings.Contains(got, leak) {
					t.Errorf("job result error %q contains %q from the caller's input", got, leak)
				}
			}
			if n := tr.sent.Load(); n != 0 {
				t.Errorf("%d request(s) sent before a pre-flight refusal", n)
			}
		})
	}
}
