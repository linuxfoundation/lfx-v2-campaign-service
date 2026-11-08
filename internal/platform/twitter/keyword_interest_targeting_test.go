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

func TestNormalizeKeywordInterestTargeting(t *testing.T) {
	over := make([]string, MaxCreateKeywords+1)
	for i := range over {
		over[i] = fmt.Sprintf("kw %d", i)
	}
	for _, tc := range []struct {
		name          string
		kws, ints     []string
		wantKws       string
		wantInterests string
		want          string // "" = accepted
	}{
		{"nothing", nil, nil, "", "", ""},
		{"valid", []string{" kubernetes ", "Cloud Native", "KUBERNETES"}, []string{" 1001", "1002", "1001"}, "kubernetes,Cloud Native", "1001,1002", ""},
		{"case-fold duplicates", []string{"ΟΔΟΣ", "οδος", "οδοσ", "Straße", "STRASSE"}, nil, "ΟΔΟΣ,Straße,STRASSE", "", ""},
		{"blank keyword", []string{"ok", " "}, nil, "", "", "keywords: entry 1 is blank"},
		{"long keyword", []string{strings.Repeat("k", 81)}, nil, "", "", "keywords: entry 0 must be at most 80 characters"},
		{"control keyword", []string{"a\tb"}, nil, "", "", "keywords: entry 0 must be at most 80 characters with no control character"},
		{"too many keywords", over, nil, "", "", fmt.Sprintf("keywords: at most %d distinct entries", MaxCreateKeywords)},
		{"letters in an interest id", nil, []string{"10a1"}, "", "", "interestIds: entry 0"},
		{"blank interest", nil, []string{""}, "", "", "interestIds: entry 0 is blank"},
		{"too long an interest id", nil, []string{strings.Repeat("1", 20)}, "", "", "interestIds: entry 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kws, ints, err := NormalizeKeywordInterestTargeting(tc.kws, tc.ints)
			if tc.want != "" {
				var tErr *TargetingError
				if !errors.As(err, &tErr) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want a TargetingError containing %q", err, tc.want)
				}
				return
			}
			if err != nil || strings.Join(kws, ",") != tc.wantKws || strings.Join(ints, ",") != tc.wantInterests {
				t.Fatalf("got (%v, %v, %v)", kws, ints, err)
			}
		})
	}
}

const interestsTechDev = `{"data":[{"name":"Technology","targeting_type":"INTEREST","targeting_value":"1001"},{"name":"Developers","targeting_type":"INTEREST","targeting_value":"1002"}],"next_cursor":null}`

// Keywords and interests follow the languages, as BROAD_KEYWORD and INTEREST criteria with EQ,
// before the promoted tweet; the interests are checked against the walked list first, and the
// step labels a keyword by number, never by its text.
func TestCreateCampaign_KeywordAndInterestCriteria(t *testing.T) {
	f := newTargetingFakeX()
	f.interestsPages = []string{interestsTechDev}
	c := targetingClient(t, f)
	in := targetingInput(nil, []string{"en"})
	in.Keywords = []string{"kubernetes", "Kubernetes", "cloud native"}
	in.InterestIDs = []string{"1002"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	writes, lookups := f.snapshot()
	want := []string{
		"POST campaigns",
		"POST line_items",
		"POST targeting_criteria LANGUAGE=en",
		"POST targeting_criteria BROAD_KEYWORD=kubernetes",
		"POST targeting_criteria BROAD_KEYWORD=cloud native",
		"POST targeting_criteria INTEREST=1002",
		"POST promoted_tweets",
	}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes =\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	if got := strings.Join(f.ops(), ","); got != "EQ,EQ,EQ,EQ" {
		t.Errorf("operators = %s", got)
	}
	sawInterests := false
	for _, l := range lookups {
		sawInterests = sawInterests || strings.HasPrefix(l, "interests?") && strings.Contains(l, "count=1000")
	}
	if !sawInterests {
		t.Errorf("lookups = %v, want the interest list read", lookups)
	}
	steps := strings.Join(res.Steps, "\n")
	if !strings.Contains(steps, "Targeting added to line item li1: keyword 2") || !strings.Contains(steps, "interest 1002") || strings.Contains(steps, "cloud native") {
		t.Errorf("steps = %s", steps)
	}
}

// An interest id X does not list, or an interest list that cannot be read to its end, creates
// nothing.
func TestCreateCampaign_InterestRefusalsCreateNothing(t *testing.T) {
	full := make([]string, listPageSize)
	for i := range full {
		full[i] = fmt.Sprintf(`{"targeting_type":"INTEREST","targeting_value":"%d"}`, 5000+i)
	}
	for name, tc := range map[string]struct {
		pages   []string
		refusal bool
	}{
		"unknown id":                 {[]string{interestsTechDev}, true},
		"full page without a cursor": {[]string{`{"data":[` + strings.Join(full, ",") + `]}`}, false},
		"unreadable":                 {[]string{`{"data":null}`}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTargetingFakeX()
			f.interestsPages = tc.pages
			c := targetingClient(t, f)
			in := targetingInput(nil, nil)
			in.InterestIDs = []string{"9999"}
			res, err := c.CreateCampaign(context.Background(), in)
			if res != nil || err == nil || isRefusal(err) != tc.refusal {
				t.Fatalf("CreateCampaign = (%v, %v), want (nil, err) with refusal=%v", res, err, tc.refusal)
			}
			if tc.refusal && !strings.Contains(err.Error(), `invalid twitterConfig.interestIds: X offers no interest "9999"`) {
				t.Errorf("err = %v", err)
			}
			if writes, _ := f.snapshot(); len(writes) != 0 {
				t.Errorf("writes = %v, want none", writes)
			}
		})
	}
}

// Keywords and interests count toward the per-create criteria cap.
func TestCreateCampaign_KeywordsCountTowardTheCriteriaCap(t *testing.T) {
	f := newTargetingFakeX()
	c := targetingClient(t, f)
	in := targetingInput([]string{"US"}, nil)
	for i := range MaxCreateTargetingCriteria {
		in.Keywords = append(in.Keywords, fmt.Sprintf("kw%d", i))
	}
	res, err := c.CreateCampaign(context.Background(), in)
	if res != nil || !isRefusal(err) || !strings.Contains(err.Error(), fmt.Sprintf("together name %d targeting criteria", MaxCreateTargetingCriteria+1)) {
		t.Fatalf("CreateCampaign = (%v, %v)", res, err)
	}
	if writes, lookups := f.snapshot(); len(writes) != 0 || len(lookups) != 0 {
		t.Errorf("writes=%v lookups=%v, want nothing sent", writes, lookups)
	}
}

// foldKey agrees with strings.EqualFold, the comparison the echo and reuse checks make.
func TestFoldKeyAgreesWithEqualFold(t *testing.T) {
	for _, pair := range [][2]string{{"Σ", "ς"}, {"Σ", "σ"}, {"K", "\u212a"}, {"kubernetes", "KUBERNETES"}, {"ß", "SS"}, {"a", "b"}} {
		if got, want := foldKey(pair[0]) == foldKey(pair[1]), strings.EqualFold(pair[0], pair[1]); got != want {
			t.Errorf("foldKey(%q)==foldKey(%q) is %v, EqualFold says %v", pair[0], pair[1], got, want)
		}
	}
}
