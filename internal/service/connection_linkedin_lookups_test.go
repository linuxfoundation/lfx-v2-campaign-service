// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

func linkedInLookupService(d PlatformDispatcher) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: d}})
	return svc
}

func TestListLinkedinAdsAudiences_MapsEveryField(t *testing.T) {
	n := int64(48000)
	d := &mockLookupDispatcher{audiences: &model.AudienceResources{AccountID: "512345678", Kind: linkedInAdSegmentKind, Audiences: []model.AudienceResource{
		{ResourceName: "urn:li:adSegment:1", ID: "1", Name: "Site visitors", Subtype: "RETARGETING", Status: "READY", Facet: "dynamicSegments", MemberCount: &n},
		{ResourceName: "urn:li:adSegment:2", ID: "2", Name: "New kind", Subtype: "LOOKALIKE", Status: "BUILDING"},
	}}}
	res, err := linkedInLookupService(d).ListLinkedinAdsAudiences(context.Background(), &conn.ListLinkedinAdsAudiencesPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListLinkedinAdsAudiences: %v", err)
	}
	if d.gotKind != linkedInAdSegmentKind {
		t.Errorf("kind passed down = %q, want %q", d.gotKind, linkedInAdSegmentKind)
	}
	if res.AccountID != "512345678" || res.AudienceCount != 2 {
		t.Fatalf("result = %+v", res)
	}
	a, b := res.Audiences[0], res.Audiences[1]
	if a.ID != "1" || a.Urn != "urn:li:adSegment:1" || a.Name != "Site visitors" || a.Type != "RETARGETING" || a.Status != "READY" ||
		a.Facet == nil || *a.Facet != "dynamicSegments" || a.ApproximateMemberCount == nil || *a.ApproximateMemberCount != 48000 {
		t.Errorf("segment 0 = %+v", a)
	}
	if b.Facet != nil || b.ApproximateMemberCount != nil {
		t.Errorf("an unclassified segment must omit facet and count: %+v", b)
	}
}

func TestListLinkedinAdsConversions_MapsEveryField(t *testing.T) {
	d := &mockLookupDispatcher{actions: &model.ConversionActions{AccountID: "512345678", Actions: []model.ConversionAction{
		{ID: "104012", ResourceName: "urn:lla:llaPartnerConversion:104012", Name: "KubeCon registration", Status: "ENABLED", Type: "REGISTRATION"},
		{ID: "7", ResourceName: "urn:lla:llaPartnerConversion:7", Name: "old", Status: "DISABLED", Type: "LEAD"},
	}}}
	res, err := linkedInLookupService(d).ListLinkedinAdsConversions(context.Background(), &conn.ListLinkedinAdsConversionsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListLinkedinAdsConversions: %v", err)
	}
	want := []conn.LinkedinAdsConversionRule{
		{ID: "104012", Urn: "urn:lla:llaPartnerConversion:104012", Name: "KubeCon registration", Type: "REGISTRATION", Enabled: true},
		{ID: "7", Urn: "urn:lla:llaPartnerConversion:7", Name: "old", Type: "LEAD", Enabled: false},
	}
	if res.AccountID != "512345678" || res.ConversionCount != 2 {
		t.Fatalf("result = %+v", res)
	}
	for i := range want {
		if *res.Conversions[i] != want[i] {
			t.Errorf("rule %d = %+v, want %+v", i, *res.Conversions[i], want[i])
		}
	}
}

func TestListLinkedinAdsLookups_EmptyIsAnEmptyArray(t *testing.T) {
	d := &mockLookupDispatcher{
		audiences: &model.AudienceResources{AccountID: "1", Audiences: []model.AudienceResource{}},
		actions:   &model.ConversionActions{AccountID: "1", Actions: []model.ConversionAction{}},
	}
	svc := linkedInLookupService(d)
	a, err := svc.ListLinkedinAdsAudiences(context.Background(), &conn.ListLinkedinAdsAudiencesPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.ListLinkedinAdsConversions(context.Background(), &conn.ListLinkedinAdsConversionsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := json.Marshal(a.Audiences)
	cb, _ := json.Marshal(c.Conversions)
	if string(ab) != "[]" || string(cb) != "[]" {
		t.Errorf("audiences %s conversions %s, want [] and []", ab, cb)
	}
}

func TestListLinkedinAdsLookups_SystemScopeAndUpstreamFailure(t *testing.T) {
	d := &mockLookupDispatcher{err: errors.New("linkedin: 500")}
	svc := linkedInLookupService(d)
	if _, err := svc.ListLinkedinAdsAudiences(context.Background(), &conn.ListLinkedinAdsAudiencesPayload{ProjectID: model.SystemProjectID}); err == nil || d.calls != 0 {
		t.Errorf("the reserved LF scope must be refused before the dispatcher: err=%v calls=%d", err, d.calls)
	}
	_, err := svc.ListLinkedinAdsConversions(context.Background(), &conn.ListLinkedinAdsConversionsPayload{ProjectID: "cncf"})
	var unavailable *conn.ConnServiceUnavailableError
	if !errors.As(err, &unavailable) {
		t.Errorf("an upstream failure = %T %v, want a 503", err, err)
	}
}
