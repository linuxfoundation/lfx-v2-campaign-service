// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	briefsserver "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/server"
	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// adDispatcher stands in for the Meta adapter's ad read.
type adDispatcher struct {
	err       error
	read      *model.MetaAds
	reached   int
	gotAdSet  string
	gotWindow model.MetricsWindow
}

func (d *adDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *adDispatcher) ReadMetaAds(_ context.Context, _ string, _ model.Provider, _ *model.Campaign, adSetID string, w model.MetricsWindow) (*model.MetaAds, error) {
	d.reached++
	d.gotAdSet, d.gotWindow = adSetID, w
	return d.read, d.err
}

func adsPayload() *briefs.ListMetaAdsPayload {
	return &briefs.ListMetaAdsPayload{ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", AdSetID: "888"}
}

func TestListMetaAds_RendersTheWireBody(t *testing.T) {
	name, creative, creativeName := "Image A", "701", "Creative A"
	d := &adDispatcher{read: &model.MetaAds{
		PlatformCampaignID: "555", AdSetID: "888", Window: model.MetricsWindowLast7Days, Currency: "USD",
		ReadAt: time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC), Truncated: true,
		Ads: []model.MetaAd{
			{ID: "901", Listed: true, Name: &name, CreativeID: &creative, CreativeName: &creativeName, Impressions: 1000, Clicks: 20, CostMicros: 7250000, Ctr: 0.02},
			{ID: "903", Impressions: 10, Clicks: 1, CostMicros: 500000, Ctr: 0.1},
		},
	}}
	s, _ := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
	w := "last_7_days"
	p := adsPayload()
	p.Window = &w
	res, err := s.ListMetaAds(context.Background(), p)
	if err != nil {
		t.Fatalf("ListMetaAds: %v", err)
	}
	body, _ := json.Marshal(briefsserver.NewListMetaAdsResponseBody(res))
	want := `{"campaign_id":"c1","platform_campaign_id":"555","ad_set_id":"888","window":"last_7_days","currency":"USD","read_at":"2026-10-15T09:30:00Z","truncated":true,"ads":[` +
		`{"id":"901","listed":true,"name":"Image A","creative_id":"701","creative_name":"Creative A","impressions":1000,"clicks":20,"cost_micros":7250000,"ctr":0.02},` +
		`{"id":"903","listed":false,"impressions":10,"clicks":1,"cost_micros":500000,"ctr":0.1}],"ad_count":2}`
	if string(body) != want {
		t.Errorf("wire body = %s\nwant        %s", body, want)
	}
	if d.gotWindow != model.MetricsWindowLast7Days || d.gotAdSet != "888" {
		t.Errorf("window = %q, ad set = %q", d.gotWindow, d.gotAdSet)
	}
}

func TestListMetaAds_ValidatesInputBeforeAnything(t *testing.T) {
	d := &adDispatcher{read: &model.MetaAds{}}
	s, _ := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
	if _, err := s.ListMetaAds(context.Background(), adsPayload()); err != nil || d.gotWindow != model.MetricsWindowLast30Days {
		t.Fatalf("err = %v, window = %q", err, d.gotWindow)
	}
	d.reached = 0
	bad := "last_90_days"
	p := adsPayload()
	p.Window = &bad
	if _, err := s.ListMetaAds(context.Background(), p); !isStatus(err, 400) {
		t.Fatalf("err = %T %v, want 400", err, err)
	}
	for _, id := range []string{"", "0888", "88/8", strings.Repeat("9", 33)} {
		p := adsPayload()
		p.AdSetID = id
		if _, err := s.ListMetaAds(context.Background(), p); !isStatus(err, 400) {
			t.Fatalf("ad set %q: err = %T %v, want 400", id, err, err)
		}
	}
	if d.reached != 0 {
		t.Fatalf("refused input reached the adapter %d time(s)", d.reached)
	}
}

func TestListMetaAds_StatusMapping(t *testing.T) {
	t.Run("non-meta campaign is 400 without reaching any dispatcher", func(t *testing.T) {
		d := &noAdSetDispatcher{}
		row := metaCampaignRow()
		row.Platform = model.ProviderGoogleAds
		row.PlatformCampaignID = ""
		s, _ := adSetService(row, model.ProviderGoogleAds, d)
		if _, err := s.ListMetaAds(context.Background(), adsPayload()); !isStatus(err, 400) || d.reached {
			t.Fatalf("err = %T %v, reached = %v", err, err, d.reached)
		}
	})
	t.Run("unprovisioned row is 409 without reaching the adapter", func(t *testing.T) {
		d := &adDispatcher{}
		row := metaCampaignRow()
		row.PlatformCampaignID = ""
		s, _ := adSetService(row, model.ProviderMetaAds, d)
		_, err := s.ListMetaAds(context.Background(), adsPayload())
		if !isStatus(err, 409) || d.reached != 0 || !strings.Contains(errMessage(err), "no ads to read") {
			t.Fatalf("err = %T %v, reached = %d", err, err, d.reached)
		}
	})
	t.Run("a missing campaign row is the only 404", func(t *testing.T) {
		d := &adDispatcher{}
		s, repo := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
		repo.getErr = domain.ErrNotFound
		if _, err := s.ListMetaAds(context.Background(), adsPayload()); !isStatus(err, 404) || d.reached != 0 {
			t.Fatalf("err = %T %v", err, err)
		}
	})
	const secret = "upstream-secret-fbtrace-XYZ"
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"foreign ad set", fmt.Errorf("%s: %w", secret, domain.ErrMetaAdSetNotInCampaign), 409},
		{"unknown provenance", errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch, errors.New(secret)), 409},
		{"account mismatch", fmt.Errorf("%s: %w", secret, domain.ErrCampaignAccountMismatch), 409},
		{"upstream identity mismatch", fmt.Errorf("%s: %w", secret, domain.ErrCampaignUpstreamIdentityMismatch), 409},
		{"no connection is 404", fmt.Errorf("%s: %w: %w", secret, domain.ErrNotFound, domain.ErrConnectionAbsent), 404},
		{"another not-found source is not the connection answer", fmt.Errorf("%s: %w", secret, domain.ErrNotFound), 503},
		{"invalid stored platform id", fmt.Errorf("%s: %w", secret, domain.ErrStoredPlatformIDInvalid), 409},
		{"invalid ad set id", fmt.Errorf("%s: %w", secret, domain.ErrMetaAdSetInvalid), 400},
		{"unsupported window", fmt.Errorf("%s: %w", secret, domain.ErrMetricsWindowUnsupported), 400},
		{"connection not usable", fmt.Errorf("%s: %w", secret, domain.ErrConnectionNotUsable), 409},
		{"system connection not usable", fmt.Errorf("%w: %w: %s", domain.ErrSystemConnectionNotUsable, domain.ErrConnectionNotUsable, secret), 500},
		{"decryption", fmt.Errorf("%s: %w", secret, domain.ErrCredentialDecryptionFailed), 500},
		{"Graph 100/33 or any upstream failure is 503, never 404", errors.New("meta API GET /888/ads failed (400): " + secret + " (code: 100)"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &adDispatcher{err: tc.err}
			s, _ := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
			_, err := s.ListMetaAds(context.Background(), adsPayload())
			if !isStatus(err, tc.want) {
				t.Fatalf("err = %T %v, want %d", err, err, tc.want)
			}
			if strings.Contains(errMessage(err), secret) {
				t.Fatalf("client message leaks upstream text: %q", errMessage(err))
			}
			if tc.name == "unsupported window" && !strings.Contains(errMessage(err), "Meta ads") {
				t.Fatalf("window wording = %q, want the ad read's own", errMessage(err))
			}
			if tc.name == "upstream identity mismatch" && !strings.Contains(errMessage(err), "an ad of this ad set") {
				t.Fatalf("identity-mismatch wording = %q, want the ad read's own", errMessage(err))
			}
			if tc.want == 503 && !strings.Contains(errMessage(err), "ads could not be read") {
				t.Fatalf("503 wording = %q, want the ad read's own", errMessage(err))
			}
		})
	}
}
