// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeFollowerLookalikes(t *testing.T) {
	over := make([]string, MaxCreateFollowerLookalikes+1)
	for i := range over {
		over[i] = fmt.Sprintf("%d", i+1)
	}
	for _, tc := range []struct {
		name string
		in   []string
		want string // the normalized list joined, or the refusal it must contain
		ok   bool
	}{
		{"nothing", nil, "", true},
		{"trimmed and de-duplicated", []string{" 14230524 ", "783214", "14230524"}, "14230524,783214", true},
		{"blank", []string{"1", " "}, "followerLookalikeUserIds: entry 1 is blank", false},
		{"a handle, not an id", []string{"@linuxfoundation"}, "followerLookalikeUserIds: entry 0", false},
		{"leading zero", []string{"0123"}, "followerLookalikeUserIds: entry 0", false},
		{"twenty digits", []string{"12345678901234567890"}, "followerLookalikeUserIds: entry 0", false},
		{"above int64", []string{"9999999999999999999"}, "entry 0 is out of range", false},
		{"too many", over, fmt.Sprintf("followerLookalikeUserIds: at most %d distinct entries", MaxCreateFollowerLookalikes), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeFollowerLookalikes(tc.in)
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

// Look-alikes follow the platforms as SIMILAR_TO_FOLLOWERS_OF_USER criteria with EQ, before the
// included custom audiences and the promoted tweet, and make no lookup of their own.
func TestCreateCampaign_FollowerLookalikeCriteria(t *testing.T) {
	f := newTargetingFakeX()
	f.platformsPages = []string{platformsIOSAndroid}
	c := targetingClient(t, f)
	in := targetingInput(nil, []string{"en"})
	in.Platforms = []string{"0"}
	in.FollowerLookalikeUserIDs = []string{"14230524", " 783214", "14230524"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	writes, lookups := f.snapshot()
	want := []string{
		"POST campaigns",
		"POST line_items",
		"POST targeting_criteria LANGUAGE=en",
		"POST targeting_criteria PLATFORM=0",
		"POST targeting_criteria SIMILAR_TO_FOLLOWERS_OF_USER=14230524",
		"POST targeting_criteria SIMILAR_TO_FOLLOWERS_OF_USER=783214",
		"POST promoted_tweets",
	}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes =\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	for _, l := range lookups {
		if strings.Contains(l, "users") || strings.Contains(l, "followers") {
			t.Errorf("a look-alike made a lookup: %s", l)
		}
	}
	for _, op := range f.ops() {
		if op != OperatorEQ {
			t.Errorf("operator %q, want EQ", op)
		}
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Targeting added to line item li1: follower look-alike 783214") {
		t.Errorf("steps lack the look-alike: %v", res.Steps)
	}
}

// The accepted risk (#564 decision 2026-11-14): an id X refuses fails at ITS criterion POST, after
// the PAUSED campaign and line item exist and before any tweet, as a retained partial carrying
// ErrCreateTargetingIncomplete — the same rule as every other criterion failure.
func TestCreateCampaign_RefusedLookalikeStopsBeforeTheTweet(t *testing.T) {
	f := newTargetingFakeX()
	f.failCriterion, f.failStatus = 2, http.StatusBadRequest
	c := targetingClient(t, f)
	in := targetingInput(nil, nil)
	in.FollowerLookalikeUserIDs = []string{"14230524", "999"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil || res == nil || res.CampaignID != "cmp1" || res.LineItemID != "li1" {
		t.Fatalf("res=%+v err=%v, want the partial result and an error", res, err)
	}
	if !errors.Is(err, ErrCreateTargetingIncomplete) {
		t.Errorf("err does not carry ErrCreateTargetingIncomplete: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "1 of 2 criteria applied") || !strings.Contains(msg, "follower look-alike 999 failed") {
		t.Errorf("error = %q", msg)
	}
	writes, _ := f.snapshot()
	if strings.Contains(strings.Join(writes, " "), "promoted_tweets") {
		t.Fatalf("writes = %v, want no tweet after the refused look-alike", writes)
	}
}

// A malformed id, or one over the combined criteria budget, is refused before any request.
func TestCreateCampaign_MalformedLookalikeIsRefusedBeforeAnyRequest(t *testing.T) {
	over := make([]string, MaxCreateTargetingCriteria)
	for i := range over {
		over[i] = fmt.Sprintf("%d", i+1)
	}
	for name, mut := range map[string]func(*CampaignInput){
		"handle":     func(in *CampaignInput) { in.FollowerLookalikeUserIDs = []string{"lf"} },
		"over total": func(in *CampaignInput) { in.FollowerLookalikeUserIDs = over; in.Languages = []string{"en"} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newTargetingFakeX()
			c := targetingClient(t, f)
			in := targetingInput(nil, nil)
			mut(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if res != nil || !isRefusal(err) {
				t.Fatalf("res=%v err=%v, want a pre-create refusal", res, err)
			}
			if writes, lookups := f.snapshot(); len(writes)+len(lookups) != 0 {
				t.Fatalf("writes=%v lookups=%v, want none", writes, lookups)
			}
		})
	}
}
