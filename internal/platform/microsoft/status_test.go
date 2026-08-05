// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

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

// statusRecorder returns a test API server that records each Status PUT (method, path,
// status) in call order, plus a client wired to it and a token server. The returned
// mutex guards got — the caller must hold it before reading got once the exercised
// call returns.
func statusRecorder(t *testing.T, got *[]struct{ method, path, status string }) (*Client, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Campaigns []msStatusUpdate `json:"Campaigns"`
			AdGroups  []msStatusUpdate `json:"AdGroups"`
			Ads       []msStatusUpdate `json:"Ads"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var status string
		switch {
		case len(body.Campaigns) > 0:
			status = body.Campaigns[0].Status
		case len(body.AdGroups) > 0:
			status = body.AdGroups[0].Status
		case len(body.Ads) > 0:
			status = body.Ads[0].Status
		}
		mu.Lock()
		*got = append(*got, struct{ method, path, status string }{r.Method, r.URL.Path, status})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"PartialErrors":[]}`))
	}))
	t.Cleanup(apiSrv.Close)
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	return NewClient(testCreds(), testAccount(), WithBaseURL(apiSrv.URL), WithTokenURL(tokenSrv.URL), WithClock(fixedClock())), &mu
}

// TestUpdateCampaignAndChildrenStatus_ActivateIsChildrenFirst verifies that on ACTIVATE the
// cascade lifts the CHILDREN first (ad, then ad group) and flips the CAMPAIGN GATE LAST, so a
// mid-cascade failure never opens the gate over a not-yet-activated child.
func TestUpdateCampaignAndChildrenStatus_ActivateIsChildrenFirst(t *testing.T) {
	type patch = struct{ method, path, status string }
	var got []patch
	c, mu := statusRecorder(t, &got)

	if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "111", "222", "333", StatusActive); err != nil {
		t.Fatalf("UpdateCampaignAndChildrenStatus: %v", err)
	}
	want := []patch{
		{http.MethodPut, "/CampaignManagement/v13/Ads", StatusActive},
		{http.MethodPut, "/CampaignManagement/v13/AdGroups", StatusActive},
		{http.MethodPut, "/CampaignManagement/v13/Campaigns", StatusActive},
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("issued %d PUTs, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("PUT[%d] = %+v, want %+v (activate must be children-first, campaign-gate last)", i, got[i], w)
		}
	}
}

// TestUpdateCampaignAndChildrenStatus_PauseIsCampaignFirst verifies that on PAUSE the cascade
// flips the CAMPAIGN GATE FIRST (delivery stops immediately) and then the children.
func TestUpdateCampaignAndChildrenStatus_PauseIsCampaignFirst(t *testing.T) {
	type patch = struct{ method, path, status string }
	var got []patch
	c, mu := statusRecorder(t, &got)

	if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "111", "222", "333", StatusPaused); err != nil {
		t.Fatalf("UpdateCampaignAndChildrenStatus: %v", err)
	}
	want := []patch{
		{http.MethodPut, "/CampaignManagement/v13/Campaigns", StatusPaused},
		{http.MethodPut, "/CampaignManagement/v13/AdGroups", StatusPaused},
		{http.MethodPut, "/CampaignManagement/v13/Ads", StatusPaused},
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("issued %d PUTs, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("PUT[%d] = %+v, want %+v (pause must be campaign-gate first)", i, got[i], w)
		}
	}
}

// TestUpdateCampaignAndChildrenStatus_PauseAdFailureIsPartialError verifies that on PAUSE the
// campaign gate and ad group succeed, then the AD PUT fails — the tree is partially applied,
// so the result is a partialCascadeError{stage:"ad"} whose Unconfirmed() is true.
func TestUpdateCampaignAndChildrenStatus_PauseAdFailureIsPartialError(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Ads") {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"PartialErrors":[]}`))
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	c := NewClient(testCreds(), testAccount(), WithBaseURL(apiSrv.URL), WithTokenURL(tokenSrv.URL), WithClock(fixedClock()))

	err := c.UpdateCampaignAndChildrenStatus(context.Background(), "111", "222", "333", StatusPaused)
	if err == nil {
		t.Fatal("an ad PUT failure after the campaign+ad-group pause must return an error")
	}
	var pce *partialCascadeError
	if !errors.As(err, &pce) || pce.stage != "ad" {
		t.Fatalf("want a partialCascadeError{stage:\"ad\"}, got %T: %v", err, err)
	}
	if !IsOutcomeUnconfirmed(err) {
		t.Errorf("the ad partial cascade must be Unconfirmed(), got %T: %v", err, err)
	}
}

// TestUpdateCampaignAndChildrenStatus_ActivateChildFailureDoesNotOpenGate verifies that when a
// child PUT fails during ACTIVATE, the campaign gate is NEVER flipped.
func TestUpdateCampaignAndChildrenStatus_ActivateChildFailureDoesNotOpenGate(t *testing.T) {
	var mu sync.Mutex
	var campaignPatched bool
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Campaigns") {
			mu.Lock()
			campaignPatched = true
			mu.Unlock()
		}
		if strings.HasSuffix(r.URL.Path, "/AdGroups") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"PartialErrors":[]}`))
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	c := NewClient(testCreds(), testAccount(), WithBaseURL(apiSrv.URL), WithTokenURL(tokenSrv.URL), WithClock(fixedClock()))

	err := c.UpdateCampaignAndChildrenStatus(context.Background(), "111", "222", "333", StatusActive)
	if err == nil {
		t.Fatal("a child failure during activate must return an error")
	}
	mu.Lock()
	defer mu.Unlock()
	if campaignPatched {
		t.Error("the campaign gate must NOT be flipped Active when a child activate fails")
	}
}

// TestUpdateCampaignAndChildrenStatus_ActivateRequiresBothChildIDs verifies the method refuses
// to ACTIVATE when either child id is missing, and that PAUSE with no child ids is fine.
func TestUpdateCampaignAndChildrenStatus_ActivateRequiresBothChildIDs(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no API call should happen when a child id is missing on activate: %s %s", r.Method, r.URL.Path)
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	c := NewClient(testCreds(), testAccount(), WithBaseURL(apiSrv.URL), WithTokenURL(tokenSrv.URL), WithClock(fixedClock()))

	for name, tc := range map[string]struct{ adGroupID, adID string }{
		"missing ad id":       {adGroupID: "222", adID: ""},
		"missing ad group id": {adGroupID: "", adID: "333"},
		"missing both":        {adGroupID: "", adID: ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := c.UpdateCampaignAndChildrenStatus(context.Background(), "111", tc.adGroupID, tc.adID, StatusActive)
			if err == nil {
				t.Fatalf("%s: activate must be refused when a child id is missing", name)
			}
			if !strings.Contains(err.Error(), "servable") {
				t.Errorf("%s: error should explain the tree cannot be made servable, got: %v", name, err)
			}
		})
	}
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"PartialErrors":[]}`))
	}))
	defer okSrv.Close()
	pc := NewClient(testCreds(), testAccount(), WithBaseURL(okSrv.URL), WithTokenURL(tokenSrv.URL), WithClock(fixedClock()))
	if err := pc.UpdateCampaignAndChildrenStatus(context.Background(), "111", "", "", StatusPaused); err != nil {
		t.Errorf("pause with no child ids must be allowed, got: %v", err)
	}
}

// TestUpdateCampaignAndChildrenStatus_ValidatesInput rejects bad input BEFORE any API call.
func TestUpdateCampaignAndChildrenStatus_ValidatesInput(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("validation failure must not send any request: %s %s", r.Method, r.URL.Path)
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	c := NewClient(testCreds(), testAccount(), WithBaseURL(apiSrv.URL), WithTokenURL(tokenSrv.URL), WithClock(fixedClock()))

	if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "111", "222", "333", "Bogus"); err == nil {
		t.Fatal("an unsupported status must be rejected")
	}
	if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "", "222", "333", StatusPaused); err == nil {
		t.Fatal("an empty campaign id must be rejected")
	}
}

// TestPartialErrors_On200IsDefiniteFailure verifies a PartialErrors entry on an otherwise-200
// response is a definite rejection (not wrapped as unconfirmed).
func TestUpdateCampaignAndChildrenStatus_PartialErrorOn200IsDefiniteFailure(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"PartialErrors":[{"Code":123,"ErrorCode":"CampaignServiceInvalidCampaignId"}]}`))
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	defer tokenSrv.Close()
	c := NewClient(testCreds(), testAccount(), WithBaseURL(apiSrv.URL), WithTokenURL(tokenSrv.URL), WithClock(fixedClock()))

	err := c.UpdateCampaignAndChildrenStatus(context.Background(), "111", "", "", StatusPaused)
	if err == nil {
		t.Fatal("a PartialErrors entry must be surfaced as an error")
	}
	if IsOutcomeUnconfirmed(err) {
		t.Errorf("a definite PartialErrors rejection must NOT be Unconfirmed(), got: %v", err)
	}
}

func TestParseEntityID(t *testing.T) {
	if _, err := parseEntityID("campaign", ""); err == nil {
		t.Error("empty id must be rejected")
	}
	if _, err := parseEntityID("campaign", "abc"); err == nil {
		t.Error("non-numeric id must be rejected")
	}
	id, err := parseEntityID("campaign", " 123 ")
	if err != nil {
		t.Fatalf("parseEntityID: %v", err)
	}
	if id.String() != "123" {
		t.Errorf("id = %q, want 123 (trimmed)", id.String())
	}
}

func TestIsOutcomeUnconfirmed_NilIsFalse(t *testing.T) {
	if IsOutcomeUnconfirmed(nil) {
		t.Error("nil error must not be Unconfirmed")
	}
}
