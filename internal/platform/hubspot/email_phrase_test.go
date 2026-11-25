// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
)

func TestNormalizePhrase(t *testing.T) {
	cases := map[string]string{
		"KubeCon + CloudNativeCon": "kubecon cloudnativecon",
		"  Open   Source Summit ":  "open source summit",
		"AI_dev":                   "ai dev",
		"MCP Dev Summit -":         "mcp dev summit",
		"München":                  "münchen",
		"OS Summit Japan | Tracks": "os summit japan tracks",
		"---":                      "",
		"":                         "",
	}
	for in, want := range cases {
		if got := normalizePhrase(in); got != want {
			t.Errorf("normalizePhrase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeSearchPhrases_DropsBlanksDuplicatesAndCaps(t *testing.T) {
	in := []string{"KubeCon", " kubecon ", "", "!!", "CloudNativeCon"}
	for i := 0; i < 20; i++ {
		in = append(in, fmt.Sprintf("brand %d", i))
	}
	got := NormalizeSearchPhrases(in)
	if len(got) != maxSearchPhrases {
		t.Fatalf("len = %d, want the cap %d: %v", len(got), maxSearchPhrases, got)
	}
	if got[0] != "kubecon" || got[1] != "cloudnativecon" {
		t.Errorf("order or dedupe wrong: %v", got)
	}
}

// The whole point of matching whole words: a brand name must not match inside a longer word, and
// a multi-word brand must match only as a run of words in order. Each row is a real naming
// pattern from the LF portal.
func TestContainsAnyPhrase_WholeWords(t *testing.T) {
	needles := NormalizeSearchPhrases([]string{"KubeCon", "Open Source Summit", "AI_dev"})
	matches := []string{
		"Last Chance to Join KubeCon + CloudNativeCon North America",
		"KubeCon+CloudNativeCon Europe 2027 | Registration Live",
		"OpenSearch KubeCon NA 2026 TY",
		"Open Source Summit + ELC Europe starts next week",
		"AI_dev Europe 2026 - Final Countdown",
		"ai-dev: schedule",
	}
	for _, text := range matches {
		if !ContainsAnyPhrase(text, needles) {
			t.Errorf("%q should match %v", text, needles)
		}
	}
	misses := []string{
		"KubeConference recap",             // brand inside a longer word
		"Open Source Summitry",             // last word only a prefix
		"Source Summit Open",               // words present, wrong order
		"OS Summit Japan + AutoLinux 2026", // abbreviated brand: a different phrase
		"",
	}
	for _, text := range misses {
		if ContainsAnyPhrase(text, needles) {
			t.Errorf("%q must not match %v", text, needles)
		}
	}
	if ContainsAnyPhrase("KubeCon", nil) {
		t.Error("no needles must match nothing")
	}
}

func TestSearchEmailsAnyPhrase_MatchesNameOrSubjectInOneWalk(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if q := r.URL.Query(); q.Get("q") != "" || q.Get("query") != "" {
			t.Errorf("phrases must never be sent upstream, got %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"results":[
			{"id":"1","name":"OS Summit Japan + AutoLinux + Embedded Linux 2026 | Explore the Tracks","subject":"Explore"},
			{"id":"2","name":"New email","subject":"Save on Open Source Summit Japan"},
			{"id":"3","name":"Linux Foundation Newsletter","subject":"November"},
			{"id":"4","name":"Embedded Linuxes","subject":"x"}
		]}`)
	})
	got, err := c.SearchEmailsAnyPhrase(context.Background(), []string{"Open Source Summit", "AutoLinux", "Embedded Linux"})
	if err != nil {
		t.Fatalf("SearchEmailsAnyPhrase: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	// Order is the walk's newest-first sort, which these undated rows do not exercise.
	sort.Strings(ids)
	if strings.Join(ids, ",") != "1,2" {
		t.Errorf("matched %v, want 1 (name) and 2 (subject) only", ids)
	}
	if calls != 1 {
		t.Errorf("one page should mean one request, got %d", calls)
	}
}

func TestSearchEmailsAnyPhrase_RefusesPhrasesWithNothingToMatch(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request may be made for an empty phrase set")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	if _, err := c.SearchEmailsAnyPhrase(context.Background(), []string{"", " - "}); err == nil {
		t.Fatal("an empty phrase set must be refused, not walked as an unfiltered listing")
	}
}

// The bound contract is the filtered walk's: reaching it with nothing matched is
// ErrSearchIncomplete, never an empty answer.
func TestSearchEmailsAnyPhrase_BoundWithNoMatchIsIncomplete(t *testing.T) {
	page := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		page++
		rows := make([]string, 0, 100)
		for i := 0; i < 100; i++ {
			rows = append(rows, fmt.Sprintf(`{"id":"%d-%d","name":"Unrelated %d","subject":"x"}`, page, i, i))
		}
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(rows, ",")+`],"paging":{"next":{"after":"`+fmt.Sprint(page)+`"}}}`)
	})
	_, err := c.SearchEmailsAnyPhrase(context.Background(), []string{"KubeCon"})
	if !errors.Is(err, ErrSearchIncomplete) {
		t.Fatalf("err = %v, want ErrSearchIncomplete", err)
	}
	if page != maxFilteredPages {
		t.Errorf("walked %d pages, want the filtered bound %d", page, maxFilteredPages)
	}
}

func TestSubscriptionDefinitionNames_AcceptsStringAndNumberIDs(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != subscriptionDefinitionsPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"subscriptionDefinitions":[{"id":"55904781","name":"LF Europe Updates"},{"id":11147681,"name":"LF Events"},{"id":"9","name":" "}]}`)
	})
	names, err := c.SubscriptionDefinitionNames(context.Background())
	if err != nil {
		t.Fatalf("SubscriptionDefinitionNames: %v", err)
	}
	if names["55904781"] != "LF Europe Updates" || names["11147681"] != "LF Events" {
		t.Errorf("names = %v", names)
	}
	if _, ok := names["9"]; ok {
		t.Error("a blank name is not a name")
	}
}
