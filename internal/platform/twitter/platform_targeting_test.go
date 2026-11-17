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

func TestNormalizePlatforms(t *testing.T) {
	over := make([]string, MaxCreatePlatforms+1)
	for i := range over {
		over[i] = fmt.Sprintf("%d", i)
	}
	for _, tc := range []struct {
		name string
		in   []string
		want string // the normalized list joined, or the refusal it must contain
		ok   bool
	}{
		{"nothing", nil, "", true},
		{"trimmed and de-duplicated", []string{" 0 ", "1", "0"}, "0,1", true},
		{"blank", []string{"0", ""}, "platforms: entry 1 is blank", false},
		{"a name, not a value", []string{"i OS"}, "platforms: entry 0", false},
		{"too many", over, fmt.Sprintf("platforms: at most %d distinct entries", MaxCreatePlatforms), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePlatforms(tc.in)
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

// X's example platform entry is {"name":"iOS","targeting_type":"PLATFORM","targeting_value":"0"}.
const platformsIOSAndroid = `{"data":[{"name":"iOS","targeting_type":"PLATFORM","targeting_value":"0"},{"name":"Android","targeting_type":"PLATFORM","targeting_value":"1"},{"name":"Not a platform","targeting_type":"DEVICE","targeting_value":"1q"}],"next_cursor":null}`

// Platforms follow the conversation topics as PLATFORM criteria with EQ, before the promoted tweet,
// each checked against the walked list first.
func TestCreateCampaign_PlatformCriteria(t *testing.T) {
	f := newTargetingFakeX()
	f.conversationsPages = []string{conversationsPage1}
	f.platformsPages = []string{platformsIOSAndroid}
	c := targetingClient(t, f)
	in := targetingInput(nil, nil)
	in.ConversationTopics = []string{"k8s"}
	in.Platforms = []string{"1", "0", " 1"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	writes, lookups := f.snapshot()
	want := []string{
		"POST campaigns",
		"POST line_items",
		"POST targeting_criteria CONVERSATION=k8s",
		"POST targeting_criteria PLATFORM=1",
		"POST targeting_criteria PLATFORM=0",
		"POST promoted_tweets",
	}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes =\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	if got := strings.Join(f.ops(), ","); got != "EQ,EQ,EQ" {
		t.Errorf("operators = %s", got)
	}
	sawPlatforms := false
	for _, l := range lookups {
		sawPlatforms = sawPlatforms || strings.HasPrefix(l, "platforms?") && strings.Contains(l, "count=1000")
	}
	if !sawPlatforms {
		t.Errorf("lookups = %v, want the platform list read", lookups)
	}
	if steps := strings.Join(res.Steps, "\n"); !strings.Contains(steps, "Targeting added to line item li1: platform 0") {
		t.Errorf("steps = %s", steps)
	}
}

// A platform X does not list (including a value listed under another targeting_type), or a list
// that cannot be read, creates nothing.
func TestCreateCampaign_PlatformRefusalsCreateNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		pages    []string
		platform string
		refusal  bool
	}{
		"unknown platform":      {[]string{platformsIOSAndroid}, "7", true},
		"listed as a device":    {[]string{platformsIOSAndroid}, "1q", true},
		"platform read fails":   {nil, "0", false},
		"no list in the answer": {[]string{`{"data":null}`}, "0", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTargetingFakeX()
			f.platformsPages = tc.pages
			c := targetingClient(t, f)
			in := targetingInput(nil, nil)
			in.Platforms = []string{tc.platform}
			res, err := c.CreateCampaign(context.Background(), in)
			if res != nil || err == nil || isRefusal(err) != tc.refusal {
				t.Fatalf("CreateCampaign = (%v, %v), want (nil, err) with refusal=%v", res, err, tc.refusal)
			}
			if tc.refusal && !strings.Contains(err.Error(), "invalid twitterConfig.platforms: X offers no platform") {
				t.Errorf("err = %v", err)
			}
			if writes, _ := f.snapshot(); len(writes) != 0 {
				t.Errorf("writes = %v, want none", writes)
			}
		})
	}
}

// Platforms count toward the per-create criteria cap.
func TestCreateCampaign_PlatformsCountTowardTheCriteriaCap(t *testing.T) {
	f := newTargetingFakeX()
	c := targetingClient(t, f)
	in := targetingInput(nil, nil)
	for i := range MaxCreateTargetingCriteria - 1 {
		in.Keywords = append(in.Keywords, fmt.Sprintf("kw%d", i))
	}
	in.Platforms = []string{"0", "1"}
	res, err := c.CreateCampaign(context.Background(), in)
	if res != nil || !isRefusal(err) || !strings.Contains(err.Error(), fmt.Sprintf("together name %d targeting criteria", MaxCreateTargetingCriteria+1)) {
		t.Fatalf("CreateCampaign = (%v, %v)", res, err)
	}
	if writes, lookups := f.snapshot(); len(writes) != 0 || len(lookups) != 0 {
		t.Errorf("writes=%v lookups=%v, want nothing sent", writes, lookups)
	}
}
