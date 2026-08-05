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
		{"extra tildes kept in second half", "customers/1/adGroupAds/111~222~333", "111", "222~333"},
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

// ---- createAdGroupAndAd integration paths ----------------------------------

func TestCreateAdGroupAndAd_HappyPath(t *testing.T) {
	c := newCampaignClient(t, okBudget, okCampaign)
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
	_, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error on ad creation failure")
	}
	if !strings.Contains(err.Error(), "ad group 333 created") {
		t.Errorf("error = %v, want it to acknowledge the ad group was created", err)
	}
}

func TestCreateAdGroupAndAd_AdAmbiguous5xx(t *testing.T) {
	c := newCampaignClientFull(t, okBudget, okCampaign,
		okAdGroup,
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
	)
	_, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error on a 5xx ad mutate")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error = %v, want it classified as UNCONFIRMED", err)
	}
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
