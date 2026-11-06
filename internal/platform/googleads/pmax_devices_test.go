// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Performance Max device exclusions (#500). Google creates the four device rows on every
// Performance Max campaign itself, and an exclusion is an UPDATE of the existing row to
// bidModifier 0 — see performanceMaxDeviceCriterionIDs for the ids and the probes.

// TestPerformanceMaxDeviceCriterionIDs_AreGooglesPlatformIDs pins the row ids to Google's
// published platform criterion ids (codes-formats#platforms: Desktop 30000, HighEndMobile
// 30001, Tablet 30002, ConnectedTv 30004). A wrong id updates a row that does not exist
// — a NOT_FOUND at the criteria mutate, after the campaign is paid for.
func TestPerformanceMaxDeviceCriterionIDs_AreGooglesPlatformIDs(t *testing.T) {
	want := map[string]string{
		"DESKTOP":      "30000",
		"MOBILE":       "30001",
		"TABLET":       "30002",
		"CONNECTED_TV": "30004",
	}
	if !reflect.DeepEqual(performanceMaxDeviceCriterionIDs, want) {
		t.Errorf("performanceMaxDeviceCriterionIDs = %v, want %v", performanceMaxDeviceCriterionIDs, want)
	}
	if !reflect.DeepEqual(deviceTypesFor(campaignKindPerformanceMax), map[string]struct{}{
		"DESKTOP": {}, "MOBILE": {}, "TABLET": {}, "CONNECTED_TV": {},
	}) {
		t.Errorf("Performance Max vocabulary = %v, want exactly the four rows Google creates", deviceTypesFor(campaignKindPerformanceMax))
	}
}

func TestPreflightPerformanceMax_DeviceExclusionsAccepted(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	cases := map[string][]DeviceBidModifier{
		"connected tv": {{Device: "CONNECTED_TV", BidModifier: 0}},
		"desktop":      {{Device: "DESKTOP", BidModifier: 0}},
		"lowercase":    {{Device: " tablet ", BidModifier: 0}},
		// Each "kept" case leaves exactly one device unexcluded, one per device, so the
		// all-excluded refusal cannot be wrong about any one of the four.
		"desktop kept":      {{Device: "MOBILE", BidModifier: 0}, {Device: "TABLET", BidModifier: 0}, {Device: "connected_tv", BidModifier: 0}},
		"mobile kept":       {{Device: "DESKTOP", BidModifier: 0}, {Device: "TABLET", BidModifier: 0}, {Device: "CONNECTED_TV", BidModifier: 0}},
		"tablet kept":       {{Device: "DESKTOP", BidModifier: 0}, {Device: "MOBILE", BidModifier: 0}, {Device: "CONNECTED_TV", BidModifier: 0}},
		"connected tv kept": {{Device: "DESKTOP", BidModifier: 0}, {Device: "MOBILE", BidModifier: 0}, {Device: "TABLET", BidModifier: 0}},
	}
	for name, devices := range cases {
		t.Run(name, func(t *testing.T) {
			in := demandGenInput()
			in.PerformanceMaxCreative = fullPMaxCreative()
			in.DeviceBidModifiers = devices
			pf, err := c.preflightCampaignKind(campaignKindPerformanceMax, in)
			if err != nil {
				t.Fatalf("Performance Max must accept the device exclusion: %v", err)
			}
			if len(pf.criteria.devices) != len(devices) {
				t.Fatalf("devices = %v, want %d — an accepted exclusion was dropped", pf.criteria.devices, len(devices))
			}
			if !pf.criteria.devicesUpdateExisting {
				t.Error("devicesUpdateExisting = false: the exclusion would be sent as a CREATE of a row Google already made")
			}
		})
	}
}

// The update shape is Performance Max's alone: Search (and every other channel) still
// creates its device criteria.
func TestCriteriaPlan_DevicesUpdateExistingOnlyOnPerformanceMax(t *testing.T) {
	for _, kind := range []string{campaignKindSearch, campaignKindDisplay, campaignKindVideo, campaignKindDemandGen} {
		in := demandGenInput()
		in.DeviceBidModifiers = []DeviceBidModifier{{Device: "TABLET", BidModifier: 0}}
		plan, err := validateCriteriaPlan(kind, in)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if plan.devicesUpdateExisting {
			t.Errorf("%s: devicesUpdateExisting = true; only Performance Max has pre-existing device rows", kind)
		}
	}
	plan, err := validateCriteriaPlan(campaignKindPerformanceMax, demandGenInput())
	if err != nil {
		t.Fatal(err)
	}
	if plan.devicesUpdateExisting {
		t.Error("devicesUpdateExisting = true with no devices asked for")
	}
}

func TestPreflightPerformanceMax_DeviceRefusals(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	cases := map[string]struct {
		devices []DeviceBidModifier
		wantSub string
	}{
		"adjustment up": {[]DeviceBidModifier{{Device: "MOBILE", BidModifier: 1.2}},
			"adjusts the bid by 1.2, which Google refuses on PerformanceMax Campaign (OPERATION_NOT_PERMITTED_FOR_CAMPAIGN_TYPE"},
		// Google ACCEPTED 1 in the probe, so its refusal must not claim Google refuses it.
		"no-op one": {[]DeviceBidModifier{{Device: "TABLET", BidModifier: 1}},
			"device bid modifier 0 is 1, which adjusts nothing, and PerformanceMax Campaign takes only the device exclusion"},
		"second entry adjusts": {[]DeviceBidModifier{{Device: "TABLET", BidModifier: 0}, {Device: "DESKTOP", BidModifier: 0.5}},
			"device bid modifier 1 adjusts the bid by 0.5"},
		"every device excluded": {[]DeviceBidModifier{
			{Device: "DESKTOP", BidModifier: 0}, {Device: "mobile", BidModifier: 0},
			{Device: "TABLET", BidModifier: 0}, {Device: "CONNECTED_TV", BidModifier: 0},
		}, "exclude every device (DESKTOP, MOBILE, TABLET and CONNECTED_TV) on PerformanceMax Campaign, so the campaign could not serve anywhere"},
		"unknown device": {[]DeviceBidModifier{{Device: "OTHER", BidModifier: 0}},
			`device "OTHER" must be one of MOBILE, DESKTOP, TABLET, CONNECTED_TV on PerformanceMax Campaign`},
		"repeated device": {[]DeviceBidModifier{{Device: "TABLET", BidModifier: 0}, {Device: "tablet", BidModifier: 0}},
			"device TABLET appears more than once"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := demandGenInput()
			in.PerformanceMaxCreative = fullPMaxCreative()
			in.DeviceBidModifiers = tc.devices
			_, err := c.preflightCampaignKind(campaignKindPerformanceMax, in)
			if err == nil {
				t.Fatal("accepted; it must be refused before the budget mutate")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("refused by the wrong rule: got %q, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

// pmaxDeviceCriteriaServer records the Step 4 campaignCriteria:mutate body and answers
// each operation: an update with the resource name it named (or answerUpdate's override),
// a create with a fresh criterion id.
func pmaxDeviceCriteriaServer(answerUpdate func(name string) string) (http.HandlerFunc, func() []string) {
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
		mu.Unlock()
		var decoded struct {
			Operations []struct {
				Update *struct {
					ResourceName string `json:"resourceName"`
				} `json:"update"`
			} `json:"operations"`
		}
		_ = json.Unmarshal(raw, &decoded)
		parts := make([]string, 0, len(decoded.Operations))
		for i, op := range decoded.Operations {
			name := "customers/1234567890/campaignCriteria/222~" + strconv.Itoa(500+i)
			if op.Update != nil {
				name = answerUpdate(op.Update.ResourceName)
			}
			parts = append(parts, `{"resourceName":"`+name+`"}`)
		}
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
	}
	return h, read
}

func pmaxDeviceInput(base string) CampaignInput {
	in := demandGenInput()
	in.PerformanceMaxCreative = pmaxCreativeAt(base)
	in.Languages = []string{"EN"}
	in.DeviceBidModifiers = []DeviceBidModifier{
		{Device: "connected_tv", BidModifier: 0},
		{Device: "TABLET", BidModifier: 0},
	}
	return in
}

// TestCreatePerformanceMaxCampaign_DeviceExclusionsUpdateGooglesRows asserts the exact
// Step 4 mutate: the language is a create, and each device is an update of the row Google
// created on campaign 222, with updateMask bid_modifier and an explicit bidModifier 0.
func TestCreatePerformanceMaxCampaign_DeviceExclusionsUpdateGooglesRows(t *testing.T) {
	criteriaH, readCriteria := pmaxDeviceCriteriaServer(func(name string) string { return name })
	srv := demandGenTLSServer(t, criteriaCascade(t, pmaxImages(t), criteriaH))
	c := demandGenClient(t, srv)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), pmaxDeviceInput(srv.URL))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bodies := readCriteria()
	if len(bodies) != 1 {
		t.Fatalf("got %d campaignCriteria:mutate calls, want 1 (no geo asked for; the devices ride with the language)", len(bodies))
	}
	var got any
	if err := json.Unmarshal([]byte(bodies[0]), &got); err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal([]byte(`{"operations":[
		{"create":{"campaign":"customers/1234567890/campaigns/222","language":{"languageConstant":"languageConstants/1000"}}},
		{"update":{"resourceName":"customers/1234567890/campaignCriteria/222~30004","bidModifier":0},"updateMask":"bid_modifier"},
		{"update":{"resourceName":"customers/1234567890/campaignCriteria/222~30002","bidModifier":0},"updateMask":"bid_modifier"}
	]}`), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("campaignCriteria:mutate body =\n%s\nwant the language create and two bid_modifier updates of Google's device rows", bodies[0])
	}

	if wantIDs := []string{"500", "30004", "30002"}; !reflect.DeepEqual(res.TargetingCriterionIDs, wantIDs) {
		t.Errorf("TargetingCriterionIDs = %v, want %v", res.TargetingCriterionIDs, wantIDs)
	}
	steps := strings.Join(res.Steps, "\n")
	if !strings.Contains(steps, "Campaign targeting applied: 3 criteria (1 language(s); 2 device bid modifier(s))") {
		t.Errorf("the audit trail does not record the device step:\n%s", steps)
	}
}

// An update whose result names a different row is not a confirmation of the row asked for.
func TestCreatePerformanceMaxCampaign_DeviceUpdateAnsweredForAnotherRowIsUnconfirmed(t *testing.T) {
	criteriaH, _ := pmaxDeviceCriteriaServer(func(name string) string {
		return strings.Replace(name, "~30002", "~30001", 1)
	})
	srv := demandGenTLSServer(t, criteriaCascade(t, pmaxImages(t), criteriaH))
	c := demandGenClient(t, srv)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), pmaxDeviceInput(srv.URL))
	if err == nil {
		t.Fatal("a result naming a different device row was accepted as confirmation")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") || !strings.Contains(err.Error(), "222~30002") {
		t.Errorf("err = %v, want an UNCONFIRMED naming the row that was asked for", err)
	}
	if res == nil || res.CampaignID != "222" {
		t.Fatalf("res = %+v, want the created campaign returned alongside the error", res)
	}
}

// A refused device update leaves the PAUSED campaign, returned with the error, and says the
// campaign would serve on every device.
func TestCreatePerformanceMaxCampaign_DeviceUpdateFailureKeepsTheCampaign(t *testing.T) {
	srv := demandGenTLSServer(t, criteriaCascade(t, pmaxImages(t),
		gaqlError(http.StatusBadRequest, "operationAccessDeniedError", "OPERATION_NOT_PERMITTED_FOR_CAMPAIGN_TYPE")))
	c := demandGenClient(t, srv)

	res, err := c.CreatePerformanceMaxCampaign(context.Background(), pmaxDeviceInput(srv.URL))
	if err == nil {
		t.Fatal("a refused criteria mutate reported success")
	}
	if res == nil || res.CampaignID != "222" {
		t.Fatalf("res = %+v, want the PAUSED campaign returned alongside the error", res)
	}
	// "device exclusion", not "device": Google's device rows exist regardless; what the
	// failure leaves out is the exclusion.
	for _, want := range []string{"campaign 222 created", "NO language or device exclusion criteria", "on every device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	if res.AssetGroupID != "" {
		t.Errorf("AssetGroupID = %q: the cascade went on past a failed criteria step", res.AssetGroupID)
	}
}
