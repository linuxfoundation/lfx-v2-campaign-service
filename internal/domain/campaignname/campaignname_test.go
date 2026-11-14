// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package campaignname

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

var errTestInvalid = fmt.Errorf("test-ads: %w", ErrInvalid)

func TestValidate_CanonicalisesAndWrapsThePlatformSentinel(t *testing.T) {
	got, err := Validate(Naming{Region: " latam ", AudienceIntent: "intent", BusinessUnit: "pytorch project"}, errTestInvalid)
	if err != nil || got != (Naming{Region: "LATAM", AudienceIntent: "Intent", BusinessUnit: "PyTorch Project"}) {
		t.Fatalf("Validate = %+v, %v; want the canonical spellings", got, err)
	}
	_, err = Validate(Naming{BusinessUnit: "cncf"}, errTestInvalid)
	if !errors.Is(err, errTestInvalid) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate(cncf) = %v; want it to wrap the platform sentinel and ErrInvalid", err)
	}
	if want := `test-ads: invalid naming: businessUnit "cncf" is not one of `; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Validate(cncf) = %q; want prefix %q", err, want)
	}
}

func TestRequireForCreate(t *testing.T) {
	if _, err := RequireForCreate(Naming{Region: "NA"}, errTestInvalid); !isRefusal(err) || !strings.Contains(err.Error(), "requires a business unit") {
		t.Errorf("RequireForCreate(no BU) = %v; want a pre-flight refusal asking for a business unit", err)
	}
	if _, err := RequireForCreate(Naming{Region: "Mars", BusinessUnit: "LF Events"}, errTestInvalid); !isRefusal(err) || !errors.Is(err, errTestInvalid) {
		t.Errorf("RequireForCreate(bad region) = %v; want a pre-flight refusal wrapping the sentinel", err)
	}
	if got, err := RequireForCreate(Naming{BusinessUnit: "valkey"}, errTestInvalid); err != nil || got.BusinessUnit != "Valkey" {
		t.Errorf("RequireForCreate(valkey) = %+v, %v; want the canonical copy", got, err)
	}
}

func TestCheckShape(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool // refused
	}{
		{`{"metaConfig":{}}`, false},
		{`{"metaConfig":{"naming":{"region":"NA"}}}`, false},
		{`{"metaConfig":{"naming":null}}`, true},
		{`{"metaConfig":{"Naming":"NA"}}`, true},
		{`{"metaConfig":{"naming":{"regoin":"NA"}}}`, true},
		{`{"metaConfig":{"naming":{"region":1}}}`, true},
		{`{"redditConfig":{"naming":null}}`, false}, // another platform's key is not this one's
		{`not json`, false},
	} {
		err := CheckShape(json.RawMessage(tc.raw), "metaConfig")
		if refused := err != nil; refused != tc.want {
			t.Errorf("CheckShape(%s) = %v; want refused=%v", tc.raw, err, tc.want)
			continue
		}
		if err != nil && (!errors.Is(err, domain.ErrCreateConfigInvalid) || !isRefusal(err) || !strings.Contains(err.Error(), "metaConfig.naming must be an object")) {
			t.Errorf("CheckShape(%s) = %v; want an ErrCreateConfigInvalid pre-flight refusal naming metaConfig.naming", tc.raw, err)
		}
	}
}

func TestResolveRequest(t *testing.T) {
	const flag = "TEST_NAMING_V2_ENABLED"
	if got, err := ResolveRequest("twitterConfig", &Naming{Region: "EMEA"}, errTestInvalid, false, flag); got != nil || err != nil {
		t.Errorf("flag off = %+v, %v; want nil, nil (the legacy name)", got, err)
	}
	if _, err := ResolveRequest("twitterConfig", &Naming{Region: "X"}, errTestInvalid, false, flag); !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), "twitterConfig.naming: test-ads: invalid naming: region") {
		t.Errorf("flag off, bad region = %v; want a refusal whatever the flag", err)
	}
	if _, err := ResolveRequest("twitterConfig", nil, errTestInvalid, true, flag); !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), "twitterConfig.naming.businessUnit is required on this deployment ("+flag+` is "true")`) {
		t.Errorf("flag on, absent = %v; want the business unit required", err)
	}
	got, err := ResolveRequest("twitterConfig", &Naming{BusinessUnit: "lf events"}, errTestInvalid, true, flag)
	if err != nil || got == nil || *got != (Naming{BusinessUnit: "LF Events"}) {
		t.Errorf("flag on, valid = %+v, %v; want the canonical copy", got, err)
	}
}

func TestUnanimousRegionAndCountriesRegion(t *testing.T) {
	for _, tc := range []struct {
		codes []string
		want  string
	}{
		{nil, "Global"},
		{[]string{"US"}, "NA"},
		{[]string{"us", " CA "}, "NA"},
		{[]string{"US", "DE"}, "Global"},
		{[]string{"IN"}, "India"},
		{[]string{"BR"}, "LATAM"},
		{[]string{"JP", "AU"}, "APAC"},
		{[]string{"JP", "ZZ"}, "Global"},
	} {
		if got := CountriesRegion(tc.codes); got != tc.want {
			t.Errorf("CountriesRegion(%v) = %q, want %q", tc.codes, got, tc.want)
		}
	}
}

func TestCountryRegions_AreInTheRegionList(t *testing.T) {
	for c, r := range CountryRegions {
		if !slices.Contains(Regions, r) {
			t.Errorf("CountryRegions[%s] = %q, not in Regions", c, r)
		}
	}
}

func TestIntentSegmentAndCompose(t *testing.T) {
	if got := IntentSegment("Retargeting", " retargeting ", "", "#2"); got != "Retargeting retargeting #2" {
		t.Errorf("IntentSegment = %q", got)
	}
	if got := IntentSegment("Intent", "a|b"); got != "Intent a-b" {
		t.Errorf("IntentSegment pipe = %q; want the pipe stripped", got)
	}
	got := Compose("KubeCon", "NA", "Traffic", "Intent", "Static", "LF Events", "ToFU")
	if want := "Events | KubeCon | NA | Traffic | Intent | Static | LF Events | ToFU"; got != want {
		t.Errorf("Compose = %q, want %q", got, want)
	}
	if n := len(strings.Split(got, " | ")); n != 8 {
		t.Errorf("Compose has %d segments, want 8", n)
	}
}

func isRefusal(err error) bool {
	_, ok := domain.AsPreflightRefusal(err)
	return ok
}
