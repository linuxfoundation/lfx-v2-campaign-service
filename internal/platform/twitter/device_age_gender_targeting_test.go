// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeDevices(t *testing.T) {
	over := make([]string, MaxCreateDevices+1)
	for i := range over {
		over[i] = fmt.Sprintf("d%d", i)
	}
	for _, tc := range []struct {
		name string
		in   []string
		want string // the normalized list joined, or the refusal it must contain
		ok   bool
	}{
		{"nothing", nil, "", true},
		{"trimmed, lower-cased and de-duplicated", []string{" 1Q ", "1r", "1q"}, "1q,1r", true},
		{"blank", []string{"1q", " "}, "devices: entry 1 is blank", false},
		{"a name, not a value", []string{"iPhone 3GS"}, "devices: entry 0", false},
		{"too many", over, fmt.Sprintf("devices: at most %d distinct entries", MaxCreateDevices), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDevices(tc.in)
			if !tc.ok {
				var tErr *TargetingError
				if !errors.As(err, &tErr) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want a TargetingError containing %q", err, tc.want)
				}
				return
			}
			if err != nil || strings.Join(got, ",") != tc.want {
				t.Fatalf("got (%v, %v), want %q", got, err, tc.want)
			}
		})
	}
}

func TestNormalizeAgeBucketAndGender(t *testing.T) {
	for in, want := range map[string]string{"": "", "  ": "", "AGE_21_TO_34": "AGE_21_TO_34", " age_over_50 ": "AGE_OVER_50", "AGE_13_TO_24": "AGE_13_TO_24"} {
		if got, err := NormalizeAgeBucket(in); err != nil || got != want {
			t.Errorf("NormalizeAgeBucket(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}
	// AGE_18_TO_24 and AGE_OVER_65 are not in X's AGE_BUCKET enumeration; 21-34 spelled with
	// hyphens is not a bucket name.
	for _, in := range []string{"AGE_18_TO_24", "AGE_OVER_65", "21-34", "AGE"} {
		var tErr *TargetingError
		if _, err := NormalizeAgeBucket(in); !errors.As(err, &tErr) || tErr.Field != FieldAgeBucket {
			t.Errorf("NormalizeAgeBucket(%q) err = %v, want an ageBucket TargetingError", in, err)
		}
	}
	for in, want := range map[string]string{"": "", "male": "MALE", " Female ": "FEMALE"} {
		if got, err := NormalizeGender(in); err != nil || got != want {
			t.Errorf("NormalizeGender(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"1", "2", "M", "OTHER", "ALL"} {
		var tErr *TargetingError
		if _, err := NormalizeGender(in); !errors.As(err, &tErr) || tErr.Field != FieldGender {
			t.Errorf("NormalizeGender(%q) err = %v, want a gender TargetingError", in, err)
		}
	}
}

// X's example device entry: {"name":"iPhone 3GS","manufacturer":"Apple","os_type":"iOS",
// "targeting_value":"1q","targeting_type":"DEVICE"}.
const devicesIPhones = `{"data":[{"name":"iPhone 3GS","manufacturer":"Apple","os_type":"iOS","targeting_type":"DEVICE","targeting_value":"1q"},{"name":"iPhone 4","manufacturer":"Apple","os_type":"iOS","targeting_type":"DEVICE","targeting_value":"1r"},{"name":"iOS","targeting_type":"PLATFORM","targeting_value":"0"}],"next_cursor":null}`

// Devices follow the platforms as DEVICE criteria, then one AGE and one GENDER criterion, all EQ
// and before the promoted tweet. The gender is sent as X's 1 or 2, and a numeric echo of it
// confirms the criterion.
func TestCreateCampaign_DeviceAgeGenderCriteria(t *testing.T) {
	f := newTargetingFakeX()
	f.platformsPages = []string{platformsIOSAndroid}
	f.devicesPages = []string{devicesIPhones}
	f.numericEcho = true
	c := targetingClient(t, f)
	in := targetingInput(nil, nil)
	in.Platforms = []string{"1"}
	in.Devices = []string{"1R", "1q", "1r"}
	in.AgeBucket = "age_25_to_54"
	in.Gender = "female"
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	writes, lookups := f.snapshot()
	want := []string{
		"POST campaigns",
		"POST line_items",
		"POST targeting_criteria PLATFORM=1",
		"POST targeting_criteria DEVICE=1r",
		"POST targeting_criteria DEVICE=1q",
		"POST targeting_criteria AGE=AGE_25_TO_54",
		"POST targeting_criteria GENDER=2",
		"POST promoted_tweets",
	}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes =\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	if got := strings.Join(f.ops(), ","); got != "EQ,EQ,EQ,EQ,EQ" {
		t.Errorf("operators = %s", got)
	}
	sawDevices := false
	for _, l := range lookups {
		sawDevices = sawDevices || strings.HasPrefix(l, "devices?") && strings.Contains(l, "count=1000")
	}
	if !sawDevices {
		t.Errorf("lookups = %v, want the device list read", lookups)
	}
	steps := strings.Join(res.Steps, "\n")
	for _, s := range []string{"device 1q", "age AGE_25_TO_54", "gender female"} {
		if !strings.Contains(steps, "Targeting added to line item li1: "+s) {
			t.Errorf("steps = %s, want %q", steps, s)
		}
	}
}

// A device X does not list (including a value listed under another targeting_type), a list that
// cannot be read, or an age bucket or gender outside its set, creates nothing.
func TestCreateCampaign_DeviceAgeGenderRefusalsCreateNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		pages      []string
		devices    []string
		age        string
		gender     string
		refusal    bool
		message    string
		noRequests bool
	}{
		"unknown device":         {[]string{devicesIPhones}, []string{"9z"}, "", "", true, "invalid twitterConfig.devices: X offers no device", false},
		"listed as a platform":   {[]string{devicesIPhones}, []string{"0"}, "", "", true, "invalid twitterConfig.devices: X offers no device", false},
		"device read fails":      {nil, []string{"1q"}, "", "", false, "", false},
		"no list in the answer":  {[]string{`{"data":null}`}, []string{"1q"}, "", "", false, "", false},
		"age outside the enum":   {nil, nil, "AGE_18_TO_24", "", true, "invalid twitterConfig.ageBucket", true},
		"gender outside the set": {nil, nil, "", "2", true, "invalid twitterConfig.gender", true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTargetingFakeX()
			f.devicesPages = tc.pages
			c := targetingClient(t, f)
			in := targetingInput(nil, nil)
			in.Devices, in.AgeBucket, in.Gender = tc.devices, tc.age, tc.gender
			res, err := c.CreateCampaign(context.Background(), in)
			if res != nil || err == nil || isRefusal(err) != tc.refusal {
				t.Fatalf("CreateCampaign = (%v, %v), want (nil, err) with refusal=%v", res, err, tc.refusal)
			}
			if tc.message != "" && !strings.Contains(err.Error(), tc.message) {
				t.Errorf("err = %v, want %q", err, tc.message)
			}
			writes, lookups := f.snapshot()
			if len(writes) != 0 {
				t.Errorf("writes = %v, want none", writes)
			}
			if tc.noRequests && len(lookups) != 0 {
				t.Errorf("lookups = %v, want none", lookups)
			}
		})
	}
}

// The devices, the age bucket and the gender count toward the per-create criteria cap, and toward
// the lower cap of a create with a video.
func TestCreateCampaign_DeviceAgeGenderCountTowardTheCriteriaCap(t *testing.T) {
	f := newTargetingFakeX()
	c := targetingClient(t, f)
	in := targetingInput(nil, nil)
	for i := range MaxCreateTargetingCriteria - 2 {
		in.Keywords = append(in.Keywords, fmt.Sprintf("kw%d", i))
	}
	in.Devices = []string{"1q"}
	in.AgeBucket = "AGE_OVER_18"
	in.Gender = "MALE"
	res, err := c.CreateCampaign(context.Background(), in)
	if res != nil || !isRefusal(err) || !strings.Contains(err.Error(), fmt.Sprintf("together name %d targeting criteria", MaxCreateTargetingCriteria+1)) {
		t.Fatalf("CreateCampaign = (%v, %v)", res, err)
	}
	if writes, lookups := f.snapshot(); len(writes) != 0 || len(lookups) != 0 {
		t.Errorf("writes=%v lookups=%v, want nothing sent", writes, lookups)
	}
}
