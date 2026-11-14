// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func TestValidateCallToAction(t *testing.T) {
	for _, label := range append([]string{""}, CallToActionLabels...) {
		if err := ValidateCallToAction(label); err != nil {
			t.Errorf("ValidateCallToAction(%q) = %v, want nil", label, err)
		}
	}
	for _, label := range []string{"learn_more", " LEARN_MORE", "Learn More", "CLICK_HERE", "VIEW_NOW"} {
		err := ValidateCallToAction(label)
		if err == nil || !strings.Contains(err.Error(), "REGISTER") {
			t.Errorf("ValidateCallToAction(%q) = %v, want a refusal listing the labels", label, err)
		}
	}
}

func TestCreativeName(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"", "KubeCon | variant-2"},
		{"   \t", "KubeCon | variant-2"},
		{"OSSNA 2026 – Skill Up", "OSSNA 2026 – Skill Up"},
		{"  Big\n  Tent  ", "Big Tent"},
		{strings.Repeat("é", 300), strings.Repeat("é", maxCreativeNameLen)},
	} {
		if got := creativeName("KubeCon", 1, CreativeVariant{Name: tc.name}); got != tc.want {
			t.Errorf("creativeName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestValidateVariantFields(t *testing.T) {
	long := strings.Repeat("a", maxCreativeNameLen)
	for _, tc := range []struct {
		name     string
		variants []CreativeVariant
		want     string
	}{
		{"none set", []CreativeVariant{{}, {}}, ""},
		{"distinct names and labels", []CreativeVariant{{Name: "Skill Up", CallToAction: "REGISTER"}, {Name: "Big Tent", CallToAction: "LEARN_MORE"}}, ""},
		{"unknown label", []CreativeVariant{{}, {CallToAction: "CLICK"}}, "variant-2 callToAction"},
		{"repeated name", []CreativeVariant{{Name: "Skill Up"}, {Name: "Big Tent"}, {Name: " Skill  Up "}}, "variant-3 name"},
		{"names equal once cut to 255", []CreativeVariant{{Name: long + "x"}, {Name: long + "y"}}, "repeats variant-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateVariantFields(tc.variants)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ValidateVariantFields = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateVariantFields = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// TestCreateCampaign_RefusesBadVariantFieldsBeforeAnyPOST: an unknown call to action is refused by
// LinkedIn only on the dark post, after the campaign group and campaign exist, so the client
// refuses it (and a repeated name) as a pre-flight refusal before the first POST.
func TestCreateCampaign_RefusesBadVariantFieldsBeforeAnyPOST(t *testing.T) {
	srv := noPOSTServer(t)
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	for _, variants := range [][]CreativeVariant{
		{{IntroText: "join us", Headline: "KubeCon", CallToAction: "CLICK"}},
		{{IntroText: "join us", Headline: "KubeCon", Name: "A"}, {IntroText: "join us", Headline: "KubeCon", Name: "A"}},
		// A caller's name equal to the unnamed first variant's default name.
		{{IntroText: "join us", Headline: "KubeCon"}, {IntroText: "join us", Headline: "KubeCon", Name: "KubeCon | variant-1"}},
	} {
		in := validCampaignInput()
		in.Variants = variants
		res, err := c.CreateCampaign(context.Background(), in)
		if _, pre := domain.AsPreflightRefusal(err); res != nil || !pre {
			t.Errorf("CreateCampaign(%+v) = %v, %v; want a nil result and a pre-flight refusal", variants, res, err)
		}
	}
}

// TestCreateCampaign_SendsVariantNameCallToActionAndDescription pins the wire bodies: a variant's
// call to action and description reach its dark post, its name the creative; a variant without
// them sends the shape sent before (#588, #589): no contentCallToActionLabel, no
// contentLandingPage, an empty description and the "<event> | variant-N" name.
func TestCreateCampaign_SendsVariantNameCallToActionAndDescription(t *testing.T) {
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
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	in := validCampaignInput()
	in.Variants = []CreativeVariant{
		{IntroText: "join us", Headline: "KubeCon", Name: " OSSNA 2026 – Skill Up ", CallToAction: "REGISTER", Description: " Hands-on — labs " + strings.Repeat("x", 400)},
		{IntroText: "join us", Headline: "KubeCon"},
	}
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 2 || len(creatives) != 2 {
		t.Fatalf("%d posts and %d creatives, want 2 of each", len(posts), len(creatives))
	}

	first := posts[0]
	if got := first["contentCallToActionLabel"]; got != "REGISTER" {
		t.Errorf("variant-1 contentCallToActionLabel = %v, want REGISTER", got)
	}
	article := first["content"].(map[string]any)["article"].(map[string]any)
	if got := first["contentLandingPage"]; got == nil || got != article["source"] {
		t.Errorf("variant-1 contentLandingPage = %v, want the article source %v", got, article["source"])
	}
	desc, _ := article["description"].(string)
	if !strings.HasPrefix(desc, "Hands-on, labs x") || len([]rune(desc)) != maxDescriptionLen {
		t.Errorf("variant-1 description = %q (%d runes), want it normalized and cut to %d", desc, len([]rune(desc)), maxDescriptionLen)
	}
	if got := creatives[0]["name"]; got != "OSSNA 2026 – Skill Up" {
		t.Errorf("variant-1 creative name = %v, want the caller's name trimmed", got)
	}

	second := posts[1]
	for _, k := range []string{"contentCallToActionLabel", "contentLandingPage"} {
		if _, ok := second[k]; ok {
			t.Errorf("variant-2 sent %s without a call to action", k)
		}
	}
	if got := second["content"].(map[string]any)["article"].(map[string]any)["description"]; got != "" {
		t.Errorf("variant-2 description = %v, want empty", got)
	}
	if got := creatives[1]["name"]; got != "KubeCon | variant-2" {
		t.Errorf("variant-2 creative name = %v, want the default", got)
	}
}
