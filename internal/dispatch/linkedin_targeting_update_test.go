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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// ─── targeting update lever (#666) ───

const liTargetingSnapshot = `{"budgetUsd":100,"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],` +
	`"employerExclusions":["urn:li:organization:208777"],"targetingProfile":"cloud-native","jobFunctions":["urn:li:function:8"],` +
	`"targetingProfiles":[{"id":"cloud-native"}],"variants":[{"headline":"KubeCon"}]}`

func liTargetingRow() *model.Campaign {
	row := liRow()
	row.ConfigSnapshot = json.RawMessage(liTargetingSnapshot)
	return row
}

func liTargetingChange() model.TargetingChange {
	return model.TargetingChange{
		Include:    []map[string][]string{{"skills": {"urn:li:skill:17"}}},
		GeoTargets: []string{"urn:li:geo:103644278", "urn:li:geo:101282230"},
	}
}

func writeLITargeting(d *LinkedInDispatcher, row *model.Campaign, change model.TargetingChange) error {
	return d.WriteTargeting(context.Background(), "proj", model.ProviderLinkedInAds, row, change)
}

func TestLinkedIn_WriteTargeting_ReadsThenWritesTheCriteriaAndRecordsIt(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsTargetingUpdateEnabled, "true")
	d, api := liLeverDispatcher(t, map[string]settingsRoute{liCampaignPath: {status: 200, body: liCampaignJSON()}})
	row := liTargetingRow()
	if err := writeLITargeting(d, row, liTargetingChange()); err != nil {
		t.Fatalf("WriteTargeting: %v", err)
	}
	reqs := api.requests()
	posts := liPosts(reqs)
	if len(reqs) != 2 || reqs[0].Method != http.MethodGet || len(posts) != 1 {
		t.Fatalf("requests = %+v, want one read then one write", reqs)
	}
	body := posts[0].Body
	for _, want := range []string{`"targetingCriteria"`, `urn:li:skill:17`, `urn:li:locale:en_US`, `urn:li:organization:208777`, `urn:li:geo:101282230`} {
		if !strings.Contains(body, want) {
			t.Errorf("write body lacks %s: %s", want, body)
		}
	}
	var snap map[string]json.RawMessage
	if err := json.Unmarshal(row.ConfigSnapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snap["targeting"]), "urn:li:locale:en_US") {
		t.Errorf("snapshot targeting = %s, want what was sent", snap["targeting"])
	}
	if string(snap["geoTargets"]) != `[{"label":"United States","urn":"urn:li:geo:103644278"},{"label":"","urn":"urn:li:geo:101282230"}]` {
		t.Errorf("snapshot geoTargets = %s, want known labels kept", snap["geoTargets"])
	}
	for _, gone := range []string{"targetingProfile", "targetingProfiles", "jobFunctions"} {
		if _, ok := snap[gone]; ok {
			t.Errorf("snapshot still names %s, which a custom targeting leaves unread", gone)
		}
	}
	if snap["variants"] == nil || string(snap["budgetUsd"]) != "100" || !strings.Contains(string(snap["employerExclusions"]), "208777") {
		t.Errorf("the rest of the snapshot must be kept: %s", row.ConfigSnapshot)
	}
}

// Every refusal decided by the gate, the row or the request alone sends nothing.
func TestLinkedIn_WriteTargeting_RefusesBeforeAnyRequest(t *testing.T) {
	adopted := liTargetingRow()
	adopted.Result = []byte(`{"accountId":"123456789","adopted":true}`)
	unknown := liTargetingRow()
	unknown.Result = []byte(`{}`)
	padded := liTargetingRow()
	padded.PlatformCampaignID = " 777"
	noSnapshot := liRow()
	badSnapshot := liTargetingRow()
	badSnapshot.ConfigSnapshot = json.RawMessage(`[1]`)
	badTargeting := liTargetingChange()
	badTargeting.Include = []map[string][]string{{"titles": {"urn:li:title:1"}, "seniorities": {"urn:li:seniority:3"}}}
	noGeo := liTargetingChange()
	noGeo.GeoTargets = nil
	// The row's employerExclusions are merged: including one of them is refused, as at create.
	includesExcluded := liTargetingChange()
	includesExcluded.Include = []map[string][]string{{"employers": {"urn:li:company:208777"}}}

	cases := []struct {
		name   string
		flag   string
		row    *model.Campaign
		change model.TargetingChange
		check  func(error) bool
	}{
		{"flag off", "", liTargetingRow(), liTargetingChange(), func(e error) bool {
			return errors.Is(e, domain.ErrTargetingRejected) && strings.Contains(rejectedReason(e), constants.EnvLinkedInAdsTargetingUpdateEnabled)
		}},
		{"flag not exactly true", "TRUE", liTargetingRow(), liTargetingChange(), func(e error) bool { return errors.Is(e, domain.ErrTargetingRejected) }},
		{"adopted", "true", adopted, liTargetingChange(), func(e error) bool { return errors.Is(e, domain.ErrTargetingUnwritable) }},
		{"unknown provenance", "true", unknown, liTargetingChange(), func(e error) bool {
			return errors.Is(e, domain.ErrCampaignProvenanceUnknown) && errors.Is(e, domain.ErrCampaignAccountMismatch)
		}},
		{"padded stored id", "true", padded, liTargetingChange(), func(e error) bool { return errors.Is(e, domain.ErrTargetingUnwritable) }},
		{"no snapshot", "true", noSnapshot, liTargetingChange(), func(e error) bool { return errors.Is(e, domain.ErrTargetingUnwritable) }},
		{"snapshot not an object", "true", badSnapshot, liTargetingChange(), func(e error) bool { return errors.Is(e, domain.ErrTargetingUnwritable) }},
		{"invalid combination", "true", liTargetingRow(), badTargeting, func(e error) bool {
			return errors.Is(e, domain.ErrTargetingRejected) && strings.HasPrefix(rejectedReason(e), "targeting: ")
		}},
		{"no locations", "true", liTargetingRow(), noGeo, func(e error) bool { return errors.Is(e, domain.ErrTargetingRejected) }},
		{"includes a configured exclusion", "true", liTargetingRow(), includesExcluded, func(e error) bool {
			return errors.Is(e, domain.ErrTargetingRejected) && strings.Contains(rejectedReason(e), "employerExclusions")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsTargetingUpdateEnabled, tc.flag)
			d, api := liLeverDispatcher(t, nil)
			row := tc.row
			before := string(row.ConfigSnapshot)
			err := writeLITargeting(d, row, tc.change)
			if !tc.check(err) {
				t.Fatalf("err = %v", err)
			}
			if n := len(api.requests()); n != 0 {
				t.Fatalf("%d request(s) sent for a refusal decided locally", n)
			}
			if string(row.ConfigSnapshot) != before {
				t.Error("a refusal rewrote the snapshot")
			}
		})
	}
}

func rejectedReason(err error) string {
	var r interface{ TargetingRejectedReason() string }
	if errors.As(err, &r) {
		return r.TargetingRejectedReason()
	}
	return ""
}

// What LinkedIn reports about the campaign is checked before the write.
func TestLinkedIn_WriteTargeting_CampaignGuardsRefuseWithoutWriting(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsTargetingUpdateEnabled, "true")
	cases := []struct {
		name string
		body string
		want error
	}{
		{"another account", liCampaignJSON(replace("sponsoredAccount:123456789", "sponsoredAccount:999")), domain.ErrCampaignAccountMismatch},
		{"unreported account", liCampaignJSON(replace(`"account":"urn:li:sponsoredAccount:123456789",`, ``)), domain.ErrTargetingUnwritable},
		{"removed upstream", liCampaignJSON(replace(`"status":"ACTIVE"`, `"status":"REMOVED"`)), domain.ErrTargetingUnwritable},
		{"unreported status", liCampaignJSON(replace(`"status":"ACTIVE",`, ``)), domain.ErrTargetingUnwritable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, api := liLeverDispatcher(t, map[string]settingsRoute{liCampaignPath: {status: 200, body: tc.body}})
			if err := writeLITargeting(d, liTargetingRow(), liTargetingChange()); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if p := liPosts(api.requests()); len(p) != 0 {
				t.Fatalf("a refusal still wrote: %+v", p)
			}
		})
	}
}

func TestLinkedIn_WriteTargeting_WriteOutcomeClassification(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsTargetingUpdateEnabled, "true")
	cases := []struct {
		name            string
		status          int
		wantNil         bool
		wantUnconfirmed bool
		wantRejected    bool
	}{
		{"204 confirmed", 204, true, false, false},
		{"5xx unconfirmed", 503, false, true, false},
		{"429 unconfirmed", 429, false, true, false},
		{"400 refused", 400, false, false, true},
		{"422 refused", 422, false, false, true},
		{"403 definite", 403, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newLIWriteServer(t, liCampaignJSON(), tc.status, `{"status":400,"message":"upstream text"}`)
			d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.url))
			row := liTargetingRow()
			err := writeLITargeting(d, row, liTargetingChange())
			if srv.posts() != 1 {
				t.Errorf("the write was sent %d times, want exactly once", srv.posts())
			}
			if tc.wantNil {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if string(row.ConfigSnapshot) != liTargetingSnapshot {
				t.Error("an unconfirmed or failed write rewrote the snapshot")
			}
			var u interface{ Unconfirmed() bool }
			if got := errors.As(err, &u) && u.Unconfirmed(); got != tc.wantUnconfirmed {
				t.Errorf("unconfirmed = %v (%v)", got, err)
			}
			if got := errors.Is(err, domain.ErrTargetingRejected); got != tc.wantRejected {
				t.Errorf("rejected = %v (%v)", got, err)
			}
			if tc.wantRejected && strings.Contains(rejectedReason(err), "upstream text") {
				t.Errorf("LinkedIn's text reached the reason: %q", rejectedReason(err))
			}
		})
	}
}

func TestLinkedIn_TargetingLeverIsReachableThroughTheOrchestrator(t *testing.T) {
	d, _ := liLeverDispatcher(t, nil)
	o := service.NewOrchestrator(nil, nil, map[model.Provider]service.PlatformDispatcher{model.ProviderLinkedInAds: d})
	err := o.WriteCampaignTargeting(context.Background(), "proj", model.ProviderLinkedInAds, liTargetingRow(), liTargetingChange())
	if errors.Is(err, service.ErrTargetingUnsupported) {
		t.Fatalf("LinkedIn is targeting-unsupported: %v", err)
	}
}
