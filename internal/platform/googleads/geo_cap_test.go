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

// The geo target cap (#515). Each assertion spells the number out rather than reading
// maxGeoTargets, so lowering the constant again fails here instead of moving the goalposts.

// geoCapIDs returns n distinct canonical geo target constant ids.
func geoCapIDs(start, n int) []string {
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, strconv.Itoa(start+i))
	}
	return ids
}

// The live KubeCon NA 2026 Demand Gen YouTube campaign targets 63 locations, and a DMA-level
// US campaign about 210. Both must pass, as must exactly 500 in each list; 501 is refused,
// naming the list and the cap.
func TestResolveGeoList_CapIsFiveHundredPerList(t *testing.T) {
	for _, n := range []int{61, 63, 210, 500} {
		if _, err := validateGeoTargets(geoCapIDs(1000000, n)); err != nil {
			t.Errorf("%d geo targets must be accepted, got %v", n, err)
		}
		if _, err := validateExcludedGeoTargets(geoCapIDs(2000000, n)); err != nil {
			t.Errorf("%d excluded geo targets must be accepted, got %v", n, err)
		}
	}
	_, err := validateGeoTargets(geoCapIDs(1000000, 501))
	if err == nil || !strings.Contains(err.Error(), "at most 500 geo targets") {
		t.Errorf("501 geo targets: want the 500 cap refusal, got %v", err)
	}
	_, err = validateExcludedGeoTargets(geoCapIDs(2000000, 501))
	if err == nil || !strings.Contains(err.Error(), "at most 500 excluded geo targets") {
		t.Errorf("501 excluded geo targets: want the 500 cap refusal, got %v", err)
	}
}

// countingMutate answers every call with count well-formed results and records how many
// calls arrived and the operation count of the last body.
func countingMutate(count int, name func(i int) string) (http.HandlerFunc, func() (calls, ops int)) {
	var (
		mu       sync.Mutex
		calls    int
		lastOpsN int
	)
	h := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Operations []json.RawMessage `json:"operations"`
		}
		_ = json.Unmarshal(b, &req)
		mu.Lock()
		calls++
		lastOpsN = len(req.Operations)
		mu.Unlock()
		parts := make([]string, 0, count)
		for i := 0; i < count; i++ {
			parts = append(parts, `{"resourceName":"`+name(i)+`"}`)
		}
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
	}
	return h, func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return calls, lastOpsN
	}
}

// At the cap the Search location criteria still go in ONE atomic campaignCriteria:mutate:
// 500 inclusions, 500 exclusions and 20 radius targets, the shape a validateOnly probe
// accepted on 2026-11-06. Splitting it would reopen the half-targeted ordering geo.go warns
// about.
func TestCreateCampaign_FullGeoListsStayInOneMutate(t *testing.T) {
	const want = 500 + 500 + maxProximityTargets
	h, seen := countingMutate(want, campaignCriterionName)
	c := newGeoClient(t, h, failHandler(t, "adGroupCriteria:mutate (Search geo attaches at CAMPAIGN level)"))

	in := sampleInput()
	in.GeoTargets = geoCapIDs(1000000, 500)
	in.ExcludedGeoTargets = geoCapIDs(2000000, 500)
	for i := 0; i < maxProximityTargets; i++ {
		in.ProximityTargets = append(in.ProximityTargets, ProximityTarget{
			Latitude: 40 + float64(i)/10, Longitude: -111, Radius: 10, RadiusUnit: "MILES",
		})
	}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls, ops := seen(); calls != 1 || ops != want {
		t.Fatalf("campaignCriteria:mutate calls=%d ops=%d, want 1 call of %d operations", calls, ops, want)
	}
	if len(res.GeoCriterionIDs) != want {
		t.Errorf("GeoCriterionIDs = %d ids, want %d", len(res.GeoCriterionIDs), want)
	}
}

// The Demand Gen ad group takes both full lists in ONE adGroupCriteria:mutate, as the
// validateOnly probe of 1000 operations on a Demand Gen ad group accepted.
func TestCreateDemandGenCampaign_FullGeoListsStayInOneMutate(t *testing.T) {
	const want = 500 + 500
	h, seen := countingMutate(want, adGroupCriterionName)
	c := newGeoClient(t,
		failHandler(t, "campaignCriteria:mutate (Demand Gen REJECTS campaign-level location criteria)"),
		h)

	in := sampleInput()
	in.GeoTargets = geoCapIDs(1000000, 500)
	in.ExcludedGeoTargets = geoCapIDs(2000000, 500)

	if _, err := c.CreateDemandGenCampaign(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls, ops := seen(); calls != 1 || ops != want {
		t.Fatalf("adGroupCriteria:mutate calls=%d ops=%d, want 1 call of %d operations", calls, ops, want)
	}
}
