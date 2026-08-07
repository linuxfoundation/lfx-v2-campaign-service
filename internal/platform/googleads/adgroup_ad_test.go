// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- adGroupAdID ------------------------------------------------------------

func TestAdGroupAdID(t *testing.T) {
	cases := []struct {
		name                string
		resourceName        string
		wantAdGroup, wantAd string
	}{
		{"valid composite", "customers/1/adGroupAds/111~222", "111", "222"},
		{"empty resource name", "", "", ""},
		{"no tilde", "customers/1/adGroupAds/111", "", ""},
		{"empty adGroup half", "customers/1/adGroupAds/~222", "", ""},
		{"empty ad half", "customers/1/adGroupAds/111~", "", ""},
		{"extra tildes rejected as malformed", "customers/1/adGroupAds/111~222~333", "", ""},
		{"non-numeric adGroup half rejected", "customers/1/adGroupAds/abc~222", "", ""},
		{"non-numeric ad half rejected", "customers/1/adGroupAds/111~abc", "", ""},
		{"wrong resource kind (campaigns instead of adGroupAds) rejected", "customers/1/campaigns/111~222", "", ""},
		{"missing adGroupAds segment rejected", "customers/1/111~222", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotGroup, gotAd := adGroupAdID(tc.resourceName)
			if gotGroup != tc.wantAdGroup || gotAd != tc.wantAd {
				t.Errorf("adGroupAdID(%q) = (%q, %q), want (%q, %q)", tc.resourceName, gotGroup, gotAd, tc.wantAdGroup, tc.wantAd)
			}
		})
	}
}

// ---- isDuplicateAdGroupNameErr ----------------------------------------------

func TestIsDuplicateAdGroupNameErr(t *testing.T) {
	if isDuplicateAdGroupNameErr(errors.New("plain error")) {
		t.Error("a plain error must not be classified as a duplicate ad group name")
	}
}

// ---- precomputeAdGroupAdInputs: name length unit ----------------------------

// AdGroup.name must be measured in CHARACTERS (like Campaign.name), not UTF-8
// bytes (like CampaignBudget.name): a multibyte EventName is the discriminator
// since it produces a different byte count than rune count. A byte-based check
// would reject an ad-group name Google would accept.
func TestPrecomputeAdGroupAdInputs_NameMeasuredInRunesNotBytes(t *testing.T) {
	// "é" is 2 UTF-8 bytes / 1 rune. 200 of them: composed name adds a few ASCII
	// header/delimiter runes on top ("LFX Ad Group | CNCF | " + 200 é's), so byte
	// count is well over 255 while rune count stays under 255.
	multibyte := strings.Repeat("é", 200)
	in := CampaignInput{Project: "CNCF", EventName: multibyte, RegistrationURL: "https://example.com/event"}
	_, _, _, adGroupName, _, _, err := precomputeAdGroupAdInputs(in)
	if err != nil {
		t.Fatalf("a multibyte ad group name under 255 characters must be accepted (rune-measured), got: %v", err)
	}
	if len(adGroupName) <= 255 {
		t.Fatalf("test setup invalid: composed name must exceed 255 UTF-8 bytes to discriminate byte vs rune measurement, got %d bytes", len(adGroupName))
	}
}

// A 256+ character ad-group name (well past any byte-vs-rune ambiguity) must
// still be rejected preflight, so the rune-based fix does not silently drop the
// cap altogether.
func TestPrecomputeAdGroupAdInputs_OversizedNameRejected(t *testing.T) {
	in := CampaignInput{Project: "CNCF", EventName: strings.Repeat("x", 300), RegistrationURL: "https://example.com/event"}
	_, _, _, _, _, _, err := precomputeAdGroupAdInputs(in)
	if err == nil || !strings.Contains(err.Error(), "name exceeds") {
		t.Errorf("an over-length ad group name must be rejected preflight, got: %v", err)
	}
}

// An over-length COMPOSED final URL (registration URL + LFX utm_* tagging) must be
// rejected preflight, before any budget/campaign/ad-group mutate: a registration URL just
// under Google's 2,084-byte Final URL limit can still exceed it once utm_source/
// utm_medium/utm_campaign/utm_content are appended, and that composed length is what
// actually reaches adGroupAds:mutate.
func TestPrecomputeAdGroupAdInputs_OversizedFinalURLRejected(t *testing.T) {
	// A ~2040-byte path plus the appended utm_* query pushes the composed URL over 2084.
	longPath := strings.Repeat("a", 2040)
	in := CampaignInput{
		Project:         "CNCF",
		EventName:       "KubeCon",
		RegistrationURL: "https://example.com/" + longPath,
	}
	finalURL, _, _, _, _, _, err := precomputeAdGroupAdInputs(in)
	if err == nil {
		t.Fatalf("an over-length composed final URL must be rejected preflight, got finalURL=%q", finalURL)
	}
	if !strings.Contains(err.Error(), "final URL") || !strings.Contains(err.Error(), "2084") {
		t.Errorf("error must explain the final-URL length limit, got: %v", err)
	}
}

// ---- createAdGroupAndAd integration paths ----------------------------------

func TestCreateAdGroupAndAd_HappyPath(t *testing.T) {
	var mu sync.Mutex
	var adGroupBody, adGroupAdBody map[string]any
	c := newCampaignClientFull(t, okBudget, okCampaign,
		func(w http.ResponseWriter, r *http.Request) {
			body := decode(t, r)
			mu.Lock()
			adGroupBody = body
			mu.Unlock()
			okAdGroup(w, r)
		},
		func(w http.ResponseWriter, r *http.Request) {
			body := decode(t, r)
			mu.Lock()
			adGroupAdBody = body
			mu.Unlock()
			okAdGroupAd(w, r)
		},
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.AdGroupID != "333" {
		t.Errorf("AdGroupID = %q, want 333", res.AdGroupID)
	}
	if res.AdGroupName == "" {
		t.Error("AdGroupName must be populated")
	}
	if res.AdID != "444" {
		t.Errorf("AdID = %q, want 444", res.AdID)
	}

	// Ad group body: PAUSED, SEARCH_STANDARD, references the campaign resourceName.
	mu.Lock()
	agBody := adGroupBody
	mu.Unlock()
	agOp := firstCreate(t, agBody)
	if agOp["status"] != StatusPaused || agOp["type"] != adGroupTypeSearchStandard {
		t.Errorf("ad group status/type = %v / %v, want %s / %s", agOp["status"], agOp["type"], StatusPaused, adGroupTypeSearchStandard)
	}
	if agOp["campaign"] != "customers/1234567890/campaigns/222" {
		t.Errorf("ad group campaign = %v, want customers/1234567890/campaigns/222", agOp["campaign"])
	}
	if agOp["name"] != res.AdGroupName {
		t.Errorf("ad group name = %v, want %q", agOp["name"], res.AdGroupName)
	}

	// Ad body: PAUSED, references the ad group resourceName, carries the final URL
	// and RSA headlines/descriptions (padded up to the platform minimums since
	// sampleInput supplies none).
	mu.Lock()
	adBody := adGroupAdBody
	mu.Unlock()
	adOp := firstCreate(t, adBody)
	if adOp["status"] != StatusPaused {
		t.Errorf("ad status = %v, want %s", adOp["status"], StatusPaused)
	}
	if adOp["adGroup"] != "customers/1234567890/adGroups/333" {
		t.Errorf("ad adGroup = %v, want customers/1234567890/adGroups/333", adOp["adGroup"])
	}
	ad, ok := adOp["ad"].(map[string]any)
	if !ok {
		t.Fatalf("ad create must carry an ad object, got %v", adOp["ad"])
	}
	finalURLs, ok := ad["finalUrls"].([]any)
	if !ok || len(finalURLs) != 1 {
		t.Fatalf("ad.finalUrls = %v, want exactly one URL", ad["finalUrls"])
	}
	if !strings.HasPrefix(finalURLs[0].(string), "https://events.linuxfoundation.org/kubecon") {
		t.Errorf("ad.finalUrls[0] = %v, want it to start with the registration URL", finalURLs[0])
	}
	rsa, ok := ad["responsiveSearchAd"].(map[string]any)
	if !ok {
		t.Fatalf("ad must carry a responsiveSearchAd, got %v", ad["responsiveSearchAd"])
	}
	headlines, ok := rsa["headlines"].([]any)
	if !ok || len(headlines) < minHeadlines {
		t.Errorf("responsiveSearchAd.headlines = %v, want at least %d", rsa["headlines"], minHeadlines)
	}
	descriptions, ok := rsa["descriptions"].([]any)
	if !ok || len(descriptions) < minDescriptions {
		t.Errorf("responsiveSearchAd.descriptions = %v, want at least %d", rsa["descriptions"], minDescriptions)
	}
}

func TestCreateAdGroupAndAd_InvalidRegistrationURL(t *testing.T) {
	c := newCampaignClient(t, okBudget, okCampaign)
	in := sampleInput()
	in.RegistrationURL = ""
	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error for a missing registration URL")
	}
	if !strings.Contains(err.Error(), "invalid destination URL") {
		t.Errorf("error = %v, want it to mention the invalid destination URL", err)
	}
}

func TestCreateAdGroupAndAd_DuplicateAdGroupName(t *testing.T) {
	c := newCampaignClientFull(t, okBudget, okCampaign,
		gaqlError(http.StatusBadRequest, "adGroupError", "DUPLICATE_ADGROUP_NAME"),
		okAdGroupAd,
	)
	_, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error on a duplicate ad group name")
	}
	if !strings.Contains(err.Error(), "DUPLICATE_ADGROUP_NAME") {
		t.Errorf("error = %v, want it to mention DUPLICATE_ADGROUP_NAME", err)
	}
}

func TestCreateAdGroupAndAd_AdGroupAmbiguous5xx(t *testing.T) {
	c := newCampaignClientFull(t, okBudget, okCampaign,
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		okAdGroupAd,
	)
	_, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error on a 5xx ad group mutate")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error = %v, want it classified as UNCONFIRMED", err)
	}
}

func TestCreateAdGroupAndAd_AdCreationFails(t *testing.T) {
	c := newCampaignClientFull(t, okBudget, okCampaign,
		okAdGroup,
		gaqlError(http.StatusBadRequest, "adError", "INVALID_AD_TYPE"),
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error on ad creation failure")
	}
	if !strings.Contains(err.Error(), "ad group 333 created") {
		t.Errorf("error = %v, want it to acknowledge the ad group was created", err)
	}
	// The load-bearing contract on this path is the PARTIAL result, not the error
	// text: the campaign and ad group really exist upstream, so the dispatcher must
	// receive their ids to reconcile. Returning (nil, err) here — or dropping
	// AdGroupID — would strand a created hierarchy the service can never find again.
	assertPartialAdGroupResult(t, res)
}

// assertPartialAdGroupResult pins the shape CreateCampaign must return when the ad group
// was created but the ad was not: everything up to the ad group is populated, AdID is
// empty. AdID being empty is what tells the caller the cascade stopped short.
func assertPartialAdGroupResult(t *testing.T, res *CampaignResult) {
	t.Helper()
	if res == nil {
		t.Fatal("result must not be nil: the campaign and ad group exist upstream and the dispatcher needs their ids to reconcile")
	}
	if res.CampaignID == "" {
		t.Error("CampaignID must be populated: the campaign was created before the ad failed")
	}
	if res.AdGroupID == "" {
		t.Error("AdGroupID must be populated: the ad group was created before the ad failed")
	}
	if res.AdGroupName == "" {
		t.Error("AdGroupName must be populated so the ad group is findable by name on retry")
	}
	if res.AdID != "" {
		t.Errorf("AdID must be empty — the ad mutate failed — got %q", res.AdID)
	}
}

func TestCreateAdGroupAndAd_AdAmbiguous5xx(t *testing.T) {
	c := newCampaignClientFull(t, okBudget, okCampaign,
		okAdGroup,
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error on a 5xx ad mutate")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error = %v, want it classified as UNCONFIRMED", err)
	}
	// An ambiguous ad mutate is the case where the partial result matters MOST: the ad
	// may or may not exist, so reconciliation is the only way to find out, and it needs
	// the ad group id to look under.
	assertPartialAdGroupResult(t, res)
}

func TestCreateAdGroupAndAd_MalformedAdGroupAdResourceName(t *testing.T) {
	c := newCampaignClientFull(t, okBudget, okCampaign,
		okAdGroup,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/333"}]}`)
		},
	)
	_, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error on a malformed adGroupAd resource name")
	}
	if !strings.Contains(err.Error(), "malformed adGroupAd resource name") {
		t.Errorf("error = %v, want it to mention the malformed resource name", err)
	}
}

func TestCreateAdGroupAndAd_AdGroupAdResourceNameReportsWrongAdGroup(t *testing.T) {
	c := newCampaignClientFull(t, okBudget, okCampaign,
		okAdGroup,
		func(w http.ResponseWriter, _ *http.Request) {
			// A different ad-group id (999) than the one just created (333) — the
			// response does not describe the ad this call created, so it must be
			// rejected rather than persisted under the wrong ad group.
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/999~444"}]}`)
		},
	)
	_, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error when the adGroupAd resource name reports a different ad group id")
	}
	if !strings.Contains(err.Error(), "different ad group id") {
		t.Errorf("error = %v, want it to mention the ad-group-id mismatch", err)
	}
}

// ---- UpdateAdGroupAndAdStatus ------------------------------------------------

func TestUpdateAdGroupAndAdStatus(t *testing.T) {
	t.Run("rejects unsupported status", func(t *testing.T) {
		c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))
		if err := c.UpdateAdGroupAndAdStatus(context.Background(), "1", "2", "BOGUS"); err == nil {
			t.Error("expected an error for an unsupported status")
		}
	})

	t.Run("rejects missing ids", func(t *testing.T) {
		c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))
		if err := c.UpdateAdGroupAndAdStatus(context.Background(), "", "2", StatusPaused); err == nil {
			t.Error("expected an error for a missing ad group id")
		}
		if err := c.UpdateAdGroupAndAdStatus(context.Background(), "1", "", StatusPaused); err == nil {
			t.Error("expected an error for a missing ad id")
		}
	})

	t.Run("rejects non-numeric ids", func(t *testing.T) {
		c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))
		if err := c.UpdateAdGroupAndAdStatus(context.Background(), "abc", "2", StatusPaused); err == nil {
			t.Error("expected an error for a non-numeric ad group id")
		}
		if err := c.UpdateAdGroupAndAdStatus(context.Background(), "1", "abc", StatusPaused); err == nil {
			t.Error("expected an error for a non-numeric ad id")
		}
	})

	t.Run("mutates ad group then ad, stopping on the first failure", func(t *testing.T) {
		var mu sync.Mutex
		var paths []string
		tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
		t.Cleanup(tokenSrv.Close)
		apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(apiSrv.Close)
		c := NewClient(testCreds(), testAccount(),
			WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
		err := c.UpdateAdGroupAndAdStatus(context.Background(), "111", "222", StatusPaused)
		if err == nil {
			t.Fatal("expected an error when the ad group mutate fails")
		}
		mu.Lock()
		gotPaths := append([]string(nil), paths...)
		mu.Unlock()
		if len(gotPaths) == 0 || !strings.HasSuffix(gotPaths[0], "adGroups:mutate") {
			t.Errorf("first call = %v, want an adGroups:mutate", gotPaths)
		}
		for _, p := range gotPaths {
			if strings.HasSuffix(p, "adGroupAds:mutate") {
				t.Errorf("must not attempt the ad mutate after the ad group mutate failed, got %v", gotPaths)
			}
		}
	})

	// The partial-cascade branch: the ad group really was paused upstream, but the ad
	// was not. adgroup_ad.go wraps that in partialCascadeError precisely so the
	// dispatcher learns the toggle was PARTIALLY applied rather than not applied at
	// all — the two demand different recovery. Driving both mutates to 5xx (as the
	// failure sub-test above does) never reaches this branch, so a regression that
	// dropped the wrapping would leave the suite green.
	t.Run("ad group succeeds and ad fails is UNCONFIRMED, not a clean failure", func(t *testing.T) {
		var mu sync.Mutex
		var paths []string
		tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
		t.Cleanup(tokenSrv.Close)
		apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			if strings.HasSuffix(r.URL.Path, "adGroupAds:mutate") {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"ok"}]}`)
		}))
		t.Cleanup(apiSrv.Close)
		c := NewClient(testCreds(), testAccount(),
			WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))

		err := c.UpdateAdGroupAndAdStatus(context.Background(), "111", "222", StatusPaused)
		if err == nil {
			t.Fatal("expected an error when the ad mutate fails after the ad group mutate succeeded")
		}
		if !IsOutcomeUnconfirmed(err) {
			t.Errorf("err must satisfy IsOutcomeUnconfirmed so the dispatcher treats the toggle as partially applied, got: %v", err)
		}
		var pce *partialCascadeError
		if !errors.As(err, &pce) {
			t.Errorf("err must be a *partialCascadeError so the stage is recoverable, got %T: %v", err, err)
		} else if pce.stage != "ad" {
			t.Errorf("partialCascadeError.stage = %q, want \"ad\"", pce.stage)
		}

		mu.Lock()
		gotPaths := append([]string(nil), paths...)
		mu.Unlock()
		// The ad group mutate must have actually happened — that is what makes the
		// outcome partial rather than a no-op.
		if len(gotPaths) == 0 || !strings.HasSuffix(gotPaths[0], "adGroups:mutate") {
			t.Errorf("first call = %v, want the ad group mutate to have been applied", gotPaths)
		}
	})

	t.Run("happy path mutates both", func(t *testing.T) {
		var mu sync.Mutex
		var paths []string
		tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
		t.Cleanup(tokenSrv.Close)
		apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"ok"}]}`)
		}))
		t.Cleanup(apiSrv.Close)
		c := NewClient(testCreds(), testAccount(),
			WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
		if err := c.UpdateAdGroupAndAdStatus(context.Background(), "111", "222", StatusEnabled); err != nil {
			t.Fatalf("UpdateAdGroupAndAdStatus: %v", err)
		}
		mu.Lock()
		gotPaths := append([]string(nil), paths...)
		mu.Unlock()
		if len(gotPaths) != 2 {
			t.Fatalf("issued %d calls, want 2 (ad group then ad): %v", len(gotPaths), gotPaths)
		}
		if !strings.HasSuffix(gotPaths[0], "adGroups:mutate") || !strings.HasSuffix(gotPaths[1], "adGroupAds:mutate") {
			t.Errorf("paths = %v, want [adGroups:mutate, adGroupAds:mutate]", gotPaths)
		}
	})
}
