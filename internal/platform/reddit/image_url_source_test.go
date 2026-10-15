// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func lateSourceInput() CampaignInput {
	return CampaignInput{
		EventName:         "Open Source Summit",
		Project:           "tlf",
		EventSlug:         "oss-2026",
		RegistrationURL:   "https://events.linuxfoundation.org/oss/",
		BudgetUSD:         500,
		StartDate:         "2026-08-01",
		EndDate:           "2026-08-31",
		GeoTargets:        []string{"us"},
		Objective:         "conversions",
		ConversionPixelID: "pixel_abc",
	}
}

// TestCreateCampaign_ImageURLSourceIsCalledJustBeforeThePost pins the ORDER the #310 review
// fix depends on: the late image URL is obtained after account verification and before the
// post request, and that post carries exactly the URL the source returned.
func TestCreateCampaign_ImageURLSourceIsCalledJustBeforeThePost(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()
	var gotMedia string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/ad_accounts/"):
			record("verify")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_acct", "name": "acct"}})
		case strings.HasSuffix(r.URL.Path, "/posts"):
			record("post")
			var body struct {
				Data struct {
					Content []struct {
						MediaURL string `json:"media_url"`
					} `json:"content"`
				} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Data.Content) > 0 {
				gotMedia = body.Data.Content[0].MediaURL
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t3_post"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "x_1"}})
		}
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
	in := lateSourceInput()
	const late = "https://lfx-api.example.org/_campaigns/creative-assets/tlf/b/a?exp=1&sig=s"
	in.ImageURLSource = func(context.Context) (string, error) { record("mint"); return late, nil }
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(events[:3], ","); got != "verify,mint,post" {
		t.Fatalf("event order = %v, want verify,mint,post first", events)
	}
	if gotMedia != late {
		t.Fatalf("media_url = %q, want the late URL", gotMedia)
	}
}

// A source failure happens before anything is created, so it is (nil, err) — the claim is
// released — and no post is attempted. Supplying both ImageURL and a source is refused up front.
func TestCreateCampaign_ImageURLSourceFailuresArePreCreate(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()
	var mutated bool
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutated = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_acct", "name": "acct"}})
	}))
	defer apiSrv.Close()
	c := NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	cases := map[string]func(*CampaignInput){
		"source error": func(in *CampaignInput) {
			in.ImageURLSource = func(context.Context) (string, error) { return "", errors.New("mint failed") }
		},
		"source returns a bad URL": func(in *CampaignInput) {
			in.ImageURLSource = func(context.Context) (string, error) { return "ftp://x/y", nil }
		},
		"both ImageURL and source": func(in *CampaignInput) {
			in.ImageURL = "https://example.com/x.jpg"
			in.ImageURLSource = func(context.Context) (string, error) { return "https://example.com/y.jpg", nil }
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			mutated = false
			in := lateSourceInput()
			mut(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil || res != nil {
				t.Fatalf("want (nil, err), got (%+v, %v)", res, err)
			}
			if mutated {
				t.Fatal("a mutating request was made")
			}
		})
	}
}
