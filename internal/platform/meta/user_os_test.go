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

// TestCreateCampaign_UserOSReachesTheAdSet: the values are canonicalized, de-duplicated and sent as
// user_os; an OS is not an inclusion, so no Advantage+ flag is sent.
func TestCreateCampaign_UserOSReachesTheAdSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []any
	}{
		{"both families", []string{"ios", " Android ", "iOS"}, []any{"iOS", "Android"}},
		{"one family with versions", []string{"ios_ver_8.0_and_above", "iOS_VER_4.0_TO_9.0", "iOS"}, []any{"iOS_ver_8.0_and_above", "iOS_ver_4.0_to_9.0", "iOS"}},
		{"android range", []string{"Android_ver_4.2_to_8.0"}, []any{"Android_ver_4.2_to_8.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTargetingServer(t, nil)
			defer ts.Close()
			in := metaFieldInput()
			in.Targeting = &AudienceTargeting{UserOS: tc.in}
			if _, err := biddingClient(ts.URL, 100).CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			got := ts.adSetTargeting(t)
			if !reflect.DeepEqual(got["user_os"], tc.want) {
				t.Errorf("targeting.user_os = %#v, want %#v", got["user_os"], tc.want)
			}
			if _, has := got["targeting_automation"]; has {
				t.Errorf("targeting_automation = %v, want it absent for an OS-only block", got["targeting_automation"])
			}
		})
	}
}

// TestCreateCampaign_UserOSRefusalsSendNothing: a value outside Meta's formats or version lists, a
// version of one family with the other family, desktop only, a version on awareness and a list over
// the cap are config-invalid refusals before any request.
func TestCreateCampaign_UserOSRefusalsSendNothing(t *testing.T) {
	many := []string{"iOS_ver_2.0_and_above", "iOS_ver_3.0_and_above", "iOS_ver_4.0_and_above", "iOS_ver_4.3_and_above",
		"iOS_ver_5.0_and_above", "iOS_ver_6.0_and_above", "iOS_ver_7.0_and_above", "iOS_ver_8.0_and_above",
		"iOS_ver_9.0_and_above", "iOS_ver_2.0_to_3.0", "iOS"}
	for _, tc := range []struct {
		name, objective string
		tgt             AudienceTargeting
		want            string
	}{
		{"unknown os", "", AudienceTargeting{UserOS: []string{"Windows"}}, `userOs entry "Windows" is not a Meta user_os value`},
		{"unlisted version", "", AudienceTargeting{UserOS: []string{"iOS_ver_14.0_and_above"}}, `userOs entry "iOS_ver_14.0_and_above" is not`},
		{"android version iOS lacks", "", AudienceTargeting{UserOS: []string{"iOS_ver_4.4_and_above"}}, `userOs entry "iOS_ver_4.4_and_above" is not`},
		{"inverted range", "", AudienceTargeting{UserOS: []string{"Android_ver_8.0_to_4.2"}}, `userOs entry "Android_ver_8.0_to_4.2" is not`},
		{"empty range", "", AudienceTargeting{UserOS: []string{"Android_ver_4.2_to_4.2"}}, `userOs entry "Android_ver_4.2_to_4.2" is not`},
		{"trailing junk", "", AudienceTargeting{UserOS: []string{"iOS_ver_8.0"}}, `userOs entry "iOS_ver_8.0" is not`},
		{"android with an iOS version", "", AudienceTargeting{UserOS: []string{"Android", "iOS_ver_8.0_and_above"}}, "userOs iOS_ver_8.0_and_above names a version of iOS, so it cannot be combined with Android"},
		{"iOS with an android version", "", AudienceTargeting{UserOS: []string{"iOS", "Android_ver_4.0_and_above"}}, "userOs Android_ver_4.0_and_above names a version of Android, so it cannot be combined with iOS"},
		{"too many", "", AudienceTargeting{UserOS: many}, "userOs names 11 values; at most 10 are allowed"},
		{"desktop only", "", AudienceTargeting{UserOS: []string{"iOS"}, DevicePlatforms: []string{"desktop"}}, "cannot be combined with devicePlatforms [desktop]"},
		{"a version on awareness", "awareness", AudienceTargeting{UserOS: []string{"Android_ver_5.0_and_above"}}, `userOs "Android_ver_5.0_and_above" names an OS version, which the awareness objective cannot target`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.String())
			}))
			defer srv.Close()
			in := metaFieldInput()
			in.Objective = tc.objective
			tgt := tc.tgt
			in.Targeting = &tgt
			res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateCampaign = %+v, %v; want a config-invalid pre-flight refusal containing %q", res, err, tc.want)
			}
		})
	}
}

// TestCheckUserOS: both families unversioned are fine on awareness, mobile with an OS is fine, and
// an OS list alone is neither an audience field, zero targeting nor an inclusion.
func TestCheckUserOS(t *testing.T) {
	for _, tgt := range []*AudienceTargeting{
		nil,
		{UserOS: []string{"iOS", "Android"}},
		{UserOS: []string{"iOS"}, DevicePlatforms: []string{"mobile"}},
		{DevicePlatforms: []string{"desktop"}},
	} {
		if err := CheckUserOS(tgt, "awareness"); err != nil {
			t.Errorf("CheckUserOS(%+v, awareness) = %v, want nil", tgt, err)
		}
	}
	if err := CheckUserOS(&AudienceTargeting{UserOS: []string{"iOS_ver_8.0_and_above"}}, "traffic"); err != nil {
		t.Errorf("a version on traffic: %v", err)
	}
	os := &AudienceTargeting{UserOS: []string{"iOS"}}
	if os.HasAudienceFields() || os.IsZero() || os.inclusion() {
		t.Errorf("userOs alone: HasAudienceFields %v, IsZero %v, inclusion %v; want all false", os.HasAudienceFields(), os.IsZero(), os.inclusion())
	}
}
