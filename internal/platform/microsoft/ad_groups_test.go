// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestValidateAdGroupNameSuffixes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		suffixes []string
		want     string
	}{
		{"valid", []string{"Cloud Native", "Kubernetes 2026"}, ""},
		{"blank", []string{""}, "entry 1: nameSuffix: is required"},
		{"padded", []string{"A", " B"}, "entry 2: nameSuffix: is required, without leading or trailing spaces"},
		{"pipe", []string{"A|B"}, `must not contain a control character or "|"`},
		{"control", []string{"A\tB"}, "must not contain a control character"},
		{"no word", []string{"---"}, "must contain a letter or digit"},
		{"long", []string{strings.Repeat("a", MaxAdGroupNameSuffixRunes+1)}, "is 41 characters; at most 40"},
		{"repeat ignoring case", []string{"Alpha", "ALPHA"}, "entry 2: nameSuffix: repeats entry 1's suffix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAdGroupNameSuffixes(tc.suffixes)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			var gErr *AdGroupError
			if !errors.As(err, &gErr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want an AdGroupError containing %q", err, tc.want)
			}
		})
	}
}

// Every group is checked before anything exists: count, keywords (at least one), copy, and the
// display paths against the registration URL's domain.
func TestValidateAdGroups(t *testing.T) {
	kw := []Keyword{{Text: "kubecon", MatchType: "Exact"}}
	base := CampaignInput{Project: "cncf", EventName: "KubeCon", NameSuffix: "brief-1"}
	ok := base
	ok.AdGroups = []AdGroupInput{{NameSuffix: "A", Keywords: kw}, {NameSuffix: "B", Keywords: kw, Path1: " reg "}}
	got, err := validateAdGroups(ok, "events.example")
	if err != nil || len(got) != 2 || got[1].in.Path1 != "reg" {
		t.Fatalf("validateAdGroups = %+v, %v", got, err)
	}
	for _, tc := range []struct {
		name   string
		groups []AdGroupInput
		want   string
	}{
		{"too many", make([]AdGroupInput, MaxAdGroups+1), "holds 6 ad groups; at most 5"},
		{"no keywords", []AdGroupInput{{NameSuffix: "A"}}, "entry 1: keywords: at least one keyword is required"},
		{"bad keyword", []AdGroupInput{{NameSuffix: "A", Keywords: []Keyword{{Text: "k", MatchType: "Fuzzy"}}}}, "entry 1: keywords:"},
		{"bad copy", []AdGroupInput{{NameSuffix: "A", Keywords: kw, Headlines: []string{"one"}}}, "entry 1: ad copy:"},
		{"display overflow", []AdGroupInput{{NameSuffix: "A", Keywords: kw}, {NameSuffix: "B", Keywords: kw, Path1: "abcdefghijklmno", Path2: "abcdefghijklmno"}}, "entry 2: path1: the display URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.AdGroups = tc.groups
			if _, err := validateAdGroups(in, strings.Repeat("h", 40)+".example"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// A further ad group's name is the single-group name plus its suffix; with no suffix the name is
// unchanged.
func TestComposeAdGroupName_Suffix(t *testing.T) {
	in := CampaignInput{Project: "cncf", EventName: "KubeCon", NameSuffix: "brief-1"}
	if got := composeAdGroupName(in); got != "LFX | Ad Group | cncf | KubeCon | brief-1" {
		t.Fatalf("single = %q", got)
	}
	in.adGroupNameSuffix = "Cloud Native"
	if got := composeAdGroupName(in); got != "LFX | Ad Group | cncf | KubeCon | brief-1 | Cloud Native" {
		t.Fatalf("suffixed = %q", got)
	}
}

func TestAddAdGroupNegativeKeywords_EntityTypeAdGroup(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) {
		return http.StatusOK, `{"NegativeKeywordIds":[{"Ids":[4401]}],"NestedPartialErrors":[]}`
	})
	out, err := c.AddAdGroupNegativeKeywords(context.Background(), "702", []NegativeKeyword{{Text: "jobs", MatchType: "Phrase"}})
	if err != nil || len(out) != 1 || out[0].Outcome != OutcomeApplied {
		t.Fatalf("outcomes = %+v, %v", out, err)
	}
	if cs := calls(); len(cs) != 1 || !strings.Contains(cs[0].body, `"EntityId":702,"EntityType":"AdGroup"`) {
		t.Fatalf("calls = %+v", cs)
	}
	if _, err := c.AddAdGroupNegativeKeywords(context.Background(), "x", []NegativeKeyword{{Text: "jobs", MatchType: "Phrase"}}); err == nil || !strings.Contains(err.Error(), "ad group id") {
		t.Fatalf("err = %v", err)
	}
}

// One further ad group's ad group, ad and keywords are set without the campaign; once the ad group
// changed, a later failure is unconfirmed.
func TestUpdateAdGroupChildrenStatus(t *testing.T) {
	c, calls := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, `{"PartialErrors":[]}` })
	if err := c.UpdateAdGroupChildrenStatus(context.Background(), "321", "655", "988", []string{"702", " "}, StatusActive); err != nil {
		t.Fatalf("UpdateAdGroupChildrenStatus: %v", err)
	}
	cs := calls()
	if len(cs) != 3 || cs[0].path != "AdGroups" || cs[1].path != "Ads" || cs[2].path != "Keywords" {
		t.Fatalf("calls = %+v", cs)
	}
	for _, call := range cs {
		if strings.Contains(call.path, "Campaigns") {
			t.Errorf("the campaign was addressed: %+v", call)
		}
	}
	if err := c.UpdateAdGroupChildrenStatus(context.Background(), "321", "655", "", nil, StatusActive); err == nil {
		t.Error("an ad group with no ad id was accepted")
	}
	n := 0
	c2, _ := kwRecorder(t, func(string, string) (int, string) {
		n++
		if n == 2 {
			return http.StatusBadRequest, `{"Errors":[{"Code":1,"ErrorCode":"X"}]}`
		}
		return http.StatusOK, `{"PartialErrors":[]}`
	})
	err := c2.UpdateAdGroupChildrenStatus(context.Background(), "321", "655", "988", nil, StatusPaused)
	if !IsOutcomeUnconfirmed(err) {
		t.Fatalf("err = %v, want unconfirmed after the ad group changed", err)
	}
}
