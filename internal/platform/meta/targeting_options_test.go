// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// TestCreateCampaign_LocalesReachTheAdSet: locale keys are sent de-duplicated in order as
// `locales`, and a language alone is not an inclusion, so no Advantage+ flag is sent.
func TestCreateCampaign_LocalesReachTheAdSet(t *testing.T) {
	ts := newTargetingServer(t, nil)
	defer ts.Close()
	in := metaFieldInput()
	in.Targeting = &AudienceTargeting{Locales: []int{6, 1001, 6}}
	if _, err := biddingClient(ts.URL, 100).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	got := ts.adSetTargeting(t)
	if !reflect.DeepEqual(got["locales"], []any{float64(6), float64(1001)}) {
		t.Errorf("targeting.locales = %#v, want [6 1001]", got["locales"])
	}
	if _, has := got["targeting_automation"]; has {
		t.Errorf("targeting_automation = %v, want it absent for a locales-only block", got["targeting_automation"])
	}
}

// TestCreateCampaign_TargetingOptionRefusalsSendNothing: an invalid locale list is a config-invalid
// pre-flight refusal and no request reaches Meta.
func TestCreateCampaign_TargetingOptionRefusalsSendNothing(t *testing.T) {
	many := make([]int, MaxLocales+1)
	for i := range many {
		many[i] = i + 1
	}
	for _, tc := range []struct {
		name string
		tgt  AudienceTargeting
		want string
	}{
		{"zero locale", AudienceTargeting{Locales: []int{6, 0}}, "metaConfig.targeting: locales entry 0 is not a Meta locale key"},
		{"negative locale", AudienceTargeting{Locales: []int{-6}}, "locales entry -6"},
		{"too many locales", AudienceTargeting{Locales: many}, "at most 50 are allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.String())
			}))
			defer srv.Close()
			in := metaFieldInput()
			tgt := tc.tgt
			in.Targeting = &tgt
			res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateCampaign = %+v, %v; want a config-invalid pre-flight refusal containing %q", res, err, tc.want)
			}
		})
	}
	// The cap counts DISTINCT keys: exactly the cap, with repeats, is accepted.
	if _, err := NormalizeAudienceTargeting(&AudienceTargeting{Locales: append(many[:MaxLocales:MaxLocales], 1, 2)}); err != nil {
		t.Fatalf("%d distinct locales with duplicates refused: %v", MaxLocales, err)
	}
}

// TestAudienceTargeting_HasAudienceFields: only the six fields META_AUDIENCE_TARGETING_ENABLED
// gates count; the later fields with their own flags do not.
func TestAudienceTargeting_HasAudienceFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		tgt  *AudienceTargeting
		want bool
	}{
		{"nil", nil, false},
		{"empty", &AudienceTargeting{}, false},
		{"locales only", &AudienceTargeting{Locales: []int{6}}, false},
		{"ageMin", &AudienceTargeting{AgeMin: intp(30)}, true},
		{"ageMax", &AudienceTargeting{AgeMax: intp(30)}, true},
		{"genders", &AudienceTargeting{Genders: []string{"male"}}, true},
		{"customAudiences", &AudienceTargeting{CustomAudiences: []string{"1"}}, true},
		{"excludedCustomAudiences", &AudienceTargeting{ExcludedCustomAudiences: []string{"1"}}, true},
		{"advantageAudience", &AudienceTargeting{AdvantageAudience: boolp(false)}, true},
	} {
		if got := tc.tgt.HasAudienceFields(); got != tc.want {
			t.Errorf("%s: HasAudienceFields = %v, want %v", tc.name, got, tc.want)
		}
	}
}
