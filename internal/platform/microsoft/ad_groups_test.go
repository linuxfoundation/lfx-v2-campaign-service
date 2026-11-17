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
		{"repeat ignoring spacing", []string{"Group A", "Group  A"}, "entry 2: nameSuffix: repeats entry 1's suffix"},
		{"repeat with a no-break space", []string{"Group A", "group\u00a0a"}, "entry 2: nameSuffix: repeats entry 1's suffix"},
		{"url with a query", []string{"A", "https://x.co/t?key=abc"}, "entry 2: nameSuffix: must not contain a URL or credentials"},
		{"url with credentials", []string{"https://u:p@x.co"}, "entry 1: nameSuffix: must not contain a URL or credentials"},
		{"schemeless url", []string{"x.co/kubecon"}, "entry 1: nameSuffix: must not contain a URL or credentials"},
		// SnapshotText keeps scheme + host, so an origin-only URL is unchanged by it; the
		// explicit URL-shape check refuses these (#623 review).
		{"origin-only https url", []string{"https://x.co"}, "entry 1: nameSuffix: must not contain a URL or credentials"},
		{"origin-only http url", []string{"A", "http://example.org"}, "entry 2: nameSuffix: must not contain a URL or credentials"},
		{"url mid-suffix", []string{"Go to https://x.co"}, "entry 1: nameSuffix: must not contain a URL or credentials"},
		{"other scheme", []string{"ftp://x.co"}, "entry 1: nameSuffix: must not contain a URL or credentials"},
		{"www host", []string{"www.example.com"}, "entry 1: nameSuffix: must not contain a URL or credentials"},
		{"www host mid-suffix", []string{"Brand WWW.Example.com"}, "entry 1: nameSuffix: must not contain a URL or credentials"},
		// A bare host alone is not treated as a URL: it cannot be told apart from a label such
		// as "Node.js", and with a path or query SnapshotText already changes it (above).
		{"labels with dots and digits", []string{"Brand", "Generic 2", "Node.js", "v1.2", "U.S."}, ""},
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

// FindAdGroupByName proves absence only from a list whose bytes say one thing: an entry naming
// the sought ad group under one spelling of Name and another under a second spelling must not
// stream as "absent", and an entry with no readable name blocks the proof.
func TestFindAdGroupByName_TrustedListOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		id      string
		found   bool
		wantErr bool
	}{
		"found":              {`{"AdGroups":[{"Id":656,"Name":"X | B"}]}`, "656", true, false},
		"definitely absent":  {`{"AdGroups":[{"Id":655,"Name":"X | A"}]}`, "", false, false},
		"duplicate name key": {`{"AdGroups":[{"Id":656,"Name":"X | B","name":"Other"}]}`, "", false, true},
		"unnamed entry":      {`{"AdGroups":[{"Id":656}]}`, "", false, true},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := kwRecorder(t, func(string, string) (int, string) { return http.StatusOK, tc.body })
			id, found, err := c.FindAdGroupByName(context.Background(), "321", "X | B")
			if (err != nil) != tc.wantErr || id != tc.id || found != tc.found {
				t.Fatalf("FindAdGroupByName = %q, %v, %v; want %q, %v, err=%v", id, found, err, tc.id, tc.found, tc.wantErr)
			}
		})
	}
}

// Every group's composed ad group name is recorded, even for a later group whose step returned
// no result (or one without the name) after an UNCONFIRMED failure: the bid write reconciles that
// group by this name, and FindAdGroupByName refuses an empty one (Cursor Bugbot, #623).
func TestCreateAdGroups_RecordsEveryGroupsComposedName(t *testing.T) {
	in := CampaignInput{EventName: "KubeCon NA", Project: "cncf"}
	groups := []validatedAdGroup{{in: AdGroupInput{NameSuffix: "A"}}, {in: AdGroupInput{NameSuffix: "B"}}, {in: AdGroupInput{NameSuffix: "C"}}, {in: AdGroupInput{NameSuffix: "D"}}}
	want := func(suffix string) string { return "LFX | Ad Group | cncf | KubeCon NA | " + suffix }
	orig := createAdGroupStep
	t.Cleanup(func() { createAdGroupStep = orig })
	createAdGroupStep = func(_ *Client, _ context.Context, gin CampaignInput, _ string, _ bool, _ *[]string, campaignPartial func() *CampaignResult, _ targeting) (*CampaignResult, error) {
		unconfirmed := &hierarchyStepUnconfirmedError{err: errors.New("ad group create UNCONFIRMED")}
		switch gin.adGroupNameSuffix {
		case "B":
			return nil, unconfirmed // no result at all
		case "C":
			return campaignPartial(), unconfirmed // a result without the name
		}
		id := map[string]string{"A": "654", "D": "655"}[gin.adGroupNameSuffix]
		return &CampaignResult{CampaignID: "321", AdGroupName: composeAdGroupName(gin), AdGroupID: id}, nil
	}
	var steps []string
	out, err := (&Client{}).createAdGroups(context.Background(), in, groups, "321", false, &steps,
		func() *CampaignResult { return &CampaignResult{CampaignID: "321"} }, targeting{})
	if err != nil {
		t.Fatalf("createAdGroups: %v", err)
	}
	for i, suffix := range []string{"A", "B", "C", "D"} {
		g := out.AdGroups[i]
		if g.AdGroupName != want(suffix) {
			t.Errorf("group %s adGroupName = %q, want %q", suffix, g.AdGroupName, want(suffix))
		}
		if unconfirmed := suffix == "B" || suffix == "C"; g.Unconfirmed != unconfirmed {
			t.Errorf("group %s unconfirmed = %v, want %v", suffix, g.Unconfirmed, unconfirmed)
		}
	}
}

func TestSiblingAdGroupName(t *testing.T) {
	for _, tc := range []struct {
		name, sibling, siblingSuffix, suffix, want string
		ok                                         bool
	}{
		{"swaps the suffix", "LFX | Ad Group | cncf | KubeCon | A", "A", "Cloud  Native", "LFX | Ad Group | cncf | KubeCon | Cloud Native", true},
		{"sanitized sibling suffix", "LFX | Ad Group | Two Words", " Two\tWords ", "B", "LFX | Ad Group | B", true},
		{"name without the suffix", "LFX | Ad Group | X", "A", "B", "", false},
		{"empty sibling suffix", "LFX | Ad Group", "", "B", "", false},
		{"empty suffix", "LFX | Ad Group | A", "A", " ", "", false},
		{"over the limit", "LFX | Ad Group | A", "A", strings.Repeat("x", maxAdGroupNameRunes), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SiblingAdGroupName(tc.sibling, tc.siblingSuffix, tc.suffix)
			if got != tc.want || ok != tc.ok {
				t.Errorf("SiblingAdGroupName = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// FailedAdGroups names only entries with no ad group of their own; IncompleteAdGroups names the
// failed entries whose own ad group Microsoft confirmed, with the step left to finish.
func TestFailedAndIncompleteAdGroups(t *testing.T) {
	r := &CampaignResult{AdGroups: []AdGroupResult{
		{NameSuffix: "A", AdGroupID: "654", Status: AdGroupStatusCreated},
		{NameSuffix: "B", Status: AdGroupStatusFailed},
		{NameSuffix: "C", AdGroupID: "656", Status: AdGroupStatusFailed},
		{NameSuffix: "D", AdGroupID: "657", AdID: "990", Status: AdGroupStatusFailed},
		{NameSuffix: "E", AdGroupID: "654", Status: AdGroupStatusFailed},
	}}
	if got := strings.Join(r.FailedAdGroups(), "; "); got != "entry 2 (B); entry 5 (E)" {
		t.Errorf("FailedAdGroups = %q", got)
	}
	want := "entry 3 (C): ad group 656 exists but its ad (and so its keywords) was not created; entry 4 (D): ad group 657 exists but its keywords were not all created"
	if got := strings.Join(r.IncompleteAdGroups(), "; "); got != want {
		t.Errorf("IncompleteAdGroups = %q\nwant %q", got, want)
	}
}
