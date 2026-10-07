// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// geo and campaign criteria on the three channels that are not Search
// ---------------------------------------------------------------------------
//
// Display, Performance Max and Video each carry the same two cascade steps Search
// carries — a campaign-level geo mutate and a campaign-level criteria mutate — and none
// of the three had a test that reached either one. The channel fixtures all build on
// demandGenInput(), which asks for no geo and no criteria, so every existing cascade
// test walks past both blocks with the conditions false. Deleting either block from any
// of the three files left the whole suite green.
//
// What is asserted here is what distinguishes the block's presence from its absence:
// that the mutate is SENT, that the ids come back on the result where the activation
// gate and the audit trail read them, and that the two mutates carry the geo and the
// language separately — the split the Search path already pins at
// TestCreateCampaign_GeoAndTargetingCriteriaAreSeparateMutates. A test that only checked
// "no error" would stay green with both blocks deleted, which is the failure mode this
// file exists to close.

// perOperationResults answers a mutate with one result per operation in the request,
// naming each by index. The count-agnostic form matters here because one handler serves
// three channels whose asset counts differ, and a fixed count would answer the wrong
// number of results to two of them.
func perOperationResults(name func(i int) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded struct {
			Operations []json.RawMessage `json:"operations"`
		}
		_ = json.Unmarshal(raw, &decoded)
		parts := make([]string, 0, len(decoded.Operations))
		for i := range decoded.Operations {
			parts = append(parts, `{"resourceName":"`+name(i)+`"}`)
		}
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
	}
}

// criteriaCapture records every campaignCriteria:mutate body IN ORDER and answers each
// with one criterion per operation. Order is the point: geo is sent before the rest, and
// a capture that lost the order could not tell the two mutates apart.
func criteriaCapture() (http.HandlerFunc, func() []string) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	read := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
	h := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		nth := len(bodies)
		mu.Unlock()
		var decoded struct {
			Operations []json.RawMessage `json:"operations"`
		}
		_ = json.Unmarshal(raw, &decoded)
		parts := make([]string, 0, len(decoded.Operations))
		for i := range decoded.Operations {
			// Distinct ids across the two mutates, so a result that mixed them up would
			// show as a duplicate rather than passing.
			parts = append(parts, `{"resourceName":"customers/1234567890/campaignCriteria/222~`+strconv.Itoa(nth*100+i)+`"}`)
		}
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
	}
	return h, read
}

// criteriaCascade is a happy handler for every mutate any of the three channels sends,
// with the campaignCriteria endpoint left to the caller. An unknown path is a failure
// rather than a default, as in each channel's own cascade helper.
func criteriaCascade(t *testing.T, images map[string][]byte, criteriaH http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if body, ok := images[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "campaignCriteria:mutate"):
			criteriaH(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			okAdGroup(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			okAdGroupAd(w, r)
		case strings.HasSuffix(r.URL.Path, "assetGroupAssets:mutate"):
			perOperationResults(func(i int) string {
				return "customers/1234567890/assetGroupAssets/555~" + strconv.Itoa(900+i) + "~HEADLINE"
			})(w, r)
		case strings.HasSuffix(r.URL.Path, "assetGroups:mutate"):
			perOperationResults(func(int) string { return "customers/1234567890/assetGroups/555" })(w, r)
		case strings.HasSuffix(r.URL.Path, "assets:mutate"):
			perOperationResults(func(i int) string {
				return "customers/1234567890/assets/" + strconv.Itoa(900+i)
			})(w, r)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestCascade_GeoAndCriteriaStepsRunOnEveryNonSearchChannel(t *testing.T) {
	cases := []struct {
		name   string
		images func(t *testing.T) map[string][]byte
		input  func(base string) CampaignInput
		create func(c *Client, ctx context.Context, in CampaignInput) (*CampaignResult, error)
	}{
		{
			name:   "display",
			images: displayImages,
			input:  displayInput,
			create: func(c *Client, ctx context.Context, in CampaignInput) (*CampaignResult, error) {
				return c.CreateDisplayCampaign(ctx, in)
			},
		},
		{
			name:   "performance max",
			images: pmaxImages,
			input: func(base string) CampaignInput {
				in := demandGenInput()
				in.PerformanceMaxCreative = pmaxCreativeAt(base)
				return in
			},
			create: func(c *Client, ctx context.Context, in CampaignInput) (*CampaignResult, error) {
				return c.CreatePerformanceMaxCampaign(ctx, in)
			},
		},
		{
			// The unreachable cascade, called directly for the same reason the rest of
			// video_test.go calls it directly: CreateVideoCampaign refuses before step 1.
			name:   "video",
			images: func(*testing.T) map[string][]byte { return nil },
			input:  func(string) CampaignInput { return videoInput() },
			create: func(c *Client, ctx context.Context, in CampaignInput) (*CampaignResult, error) {
				return c.createVideoCampaignCascade(ctx, in)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			criteriaH, readCriteria := criteriaCapture()
			srv := demandGenTLSServer(t, criteriaCascade(t, tc.images(t), criteriaH))
			c := demandGenClient(t, srv)

			in := tc.input(srv.URL)
			in.GeoTargets = []string{"US"}
			in.Languages = []string{"EN"}

			res, err := tc.create(c, context.Background(), in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res == nil {
				t.Fatal("nil result on success")
			}

			bodies := readCriteria()
			if len(bodies) != 2 {
				t.Fatalf("got %d campaignCriteria:mutate calls, want 2 (geo then the rest) — a missing cascade step", len(bodies))
			}
			if !strings.Contains(bodies[0], "geoTargetConstant") || strings.Contains(bodies[0], "languageConstant") {
				t.Errorf("the first mutate must carry only the geo criteria, got %s", bodies[0])
			}
			if !strings.Contains(bodies[1], "languageConstant") || strings.Contains(bodies[1], "geoTargetConstant") {
				t.Errorf("the second mutate must carry only the targeting criteria, got %s", bodies[1])
			}

			if len(res.GeoCriterionIDs) != 1 {
				t.Errorf("GeoCriterionIDs = %v, want one id — the geo step's ids never reached the result", res.GeoCriterionIDs)
			}
			if len(res.TargetingCriterionIDs) != 1 {
				t.Errorf("TargetingCriterionIDs = %v, want one id — the criteria step's ids never reached the result", res.TargetingCriterionIDs)
			}

			steps := strings.Join(res.Steps, "\n")
			if !strings.Contains(steps, "Geo targeting applied: 1 campaign location criteria") {
				t.Errorf("the audit trail does not record the geo step:\n%s", steps)
			}
			if !strings.Contains(steps, "Campaign targeting applied: 1 criteria (1 language(s))") {
				t.Errorf("the audit trail does not record the criteria step:\n%s", steps)
			}
		})
	}
}
