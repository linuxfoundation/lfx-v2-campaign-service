// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func isTargetingRefusal(err error) bool {
	var r *domain.PreflightRefusal
	return errors.As(err, &r)
}

func TestNormalizeSubreddits(t *testing.T) {
	got := NormalizeSubreddits([]string{" r/Kubernetes ", "kubernetes", "", "  ", "R/devops", "r/"})
	if strings.Join(got, ",") != "Kubernetes,devops" {
		t.Fatalf("NormalizeSubreddits = %v", got)
	}
}

// The names go in one names= filter (comma-separated, exact), pages are followed, and each name
// comes back in Reddit's spelling; a name Reddit does not list is a refusal naming its entry.
func TestResolveCommunities(t *testing.T) {
	const path = "/api/v3/targeting/communities"
	stub := &discoveryStub{respond: func(s *discoveryStub, key string, _ int) (int, string) {
		switch {
		case strings.HasPrefix(key, path+"?names=") && !strings.Contains(key, "page.token"):
			return 200, `{"data":[{"id":"t5_1","name":"Kubernetes"}],"pagination":` + s.next(path+"?names=x&page.token=p2") + `}`
		case strings.HasPrefix(key, path+"?names=x&page.token=p2"):
			return 200, `{"data":[{"id":"t5_2","name":"devops"}],"pagination":{"next_url":null}}`
		}
		return 404, `{}`
	}}
	c := newCustomAudienceClient(t, stub)
	got, err := c.ResolveCommunities(context.Background(), NormalizeSubredditEntries([]string{"kubernetes", "DevOps"}))
	if err != nil || strings.Join(got, ",") != "Kubernetes,devops" {
		t.Fatalf("ResolveCommunities = %v, %v", got, err)
	}
	first := stub.seen()[0]
	q, _ := url.ParseQuery(first[strings.Index(first, "?")+1:])
	if q.Get("names") != "kubernetes,DevOps" || q.Get("page.size") != "1000" {
		t.Errorf("query = %s", first)
	}
	_, err = c.ResolveCommunities(context.Background(), NormalizeSubredditEntries([]string{"kubernetes", "nosuchsub"}))
	if !isTargetingRefusal(err) || !strings.Contains(err.Error(), `invalid redditConfig.subreddits: entry 2 ("nosuchsub")`) {
		t.Fatalf("missing community: err = %v", err)
	}
}

// More than communityNamesPerLookup names are split across calls.
func TestResolveCommunities_ChunksTheNamesFilter(t *testing.T) {
	names := make([]string, communityNamesPerLookup+1)
	var data []string
	for i := range names {
		names[i] = fmt.Sprintf("sub%d", i)
		data = append(data, fmt.Sprintf(`{"id":"t5_%d","name":"sub%d"}`, i, i))
	}
	stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) {
		return 200, `{"data":[` + strings.Join(data, ",") + `]}`
	}}
	c := newCustomAudienceClient(t, stub)
	if _, err := c.ResolveCommunities(context.Background(), NormalizeSubredditEntries(names)); err != nil {
		t.Fatalf("ResolveCommunities: %v", err)
	}
	if n := len(stub.seen()); n != 2 {
		t.Fatalf("lookups = %d, want 2", n)
	}
}

// An unreadable lookup is a plain error, never a refusal of the caller's value.
func TestResolveTargeting_UnreadableLookupIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"no data":        `{"pagination":{}}`,
		"duplicated key": `{"data":[{"id":"t5_1","name":"Kubernetes"}],"data":[]}`,
		"blank name":     `{"data":[{"id":"t5_1","name":" "}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) { return 200, body }}
			c := newCustomAudienceClient(t, stub)
			if _, err := c.ResolveCommunities(context.Background(), NormalizeSubredditEntries([]string{"kubernetes"})); err == nil || isTargetingRefusal(err) {
				t.Fatalf("err = %v, want a plain error", err)
			}
		})
	}
	stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) { return 500, `{}` }}
	c := newCustomAudienceClient(t, stub)
	if _, err := c.ResolveInterests(context.Background(), []string{"Technology"}); err == nil || isTargetingRefusal(err) {
		t.Fatalf("500: err = %v, want a plain error", err)
	}
	stub2 := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) {
		return 200, `{"data":[{"id":" ","name":"Technology"}]}`
	}}
	if _, err := newCustomAudienceClient(t, stub2).ResolveInterests(context.Background(), []string{"Technology"}); err == nil || isTargetingRefusal(err) {
		t.Fatalf("blank id: err = %v, want a plain error", err)
	}
}

// Interests: an id (ignoring case) is used as is; a label matching exactly one name becomes that
// id; duplicates after resolution are dropped; unknown, ambiguous and blank entries are refused.
func TestResolveInterests(t *testing.T) {
	stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) {
		return 200, `{"data":[{"id":"technology_v3","name":"Technology","category":"Tech"},` +
			`{"id":"programming_v3","name":"Programming","category":"Tech"},` +
			`{"id":"ai_v3","name":"AI","category":"Tech"},{"id":"ai_v4","name":"ai","category":"Other"}]}`
	}}
	c := newCustomAudienceClient(t, stub)
	got, err := c.ResolveInterests(context.Background(), []string{"TECHNOLOGY_V3", " programming ", "technology"})
	if err != nil || strings.Join(got, ",") != "technology_v3,programming_v3" {
		t.Fatalf("ResolveInterests = %v, %v", got, err)
	}
	for entries, want := range map[string]string{
		"Technology|Knitting": `invalid redditConfig.interests: entry 2 ("Knitting") is not a Reddit interest id or name`,
		"AI":                  "entry 1 (\"AI\") names 2 Reddit interests; send the interest id instead",
		"Technology| ":        "entry 2 is blank",
	} {
		_, err := c.ResolveInterests(context.Background(), strings.Split(entries, "|"))
		if !isTargetingRefusal(err) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want a refusal containing %q", entries, err, want)
		}
	}
}

// A malformed interest catalogue (a repeated id, a row with no name) is an unreadable lookup,
// never a refusal blaming the caller.
func TestResolveInterests_MalformedCatalogueIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"repeated id":  `{"data":[{"id":"technology_v3","name":"Technology"},{"id":"TECHNOLOGY_V3","name":"Technology"}]}`,
		"missing name": `{"data":[{"id":"technology_v3"}]}`,
		"blank name":   `{"data":[{"id":"technology_v3","name":"  "}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) { return 200, body }}
			if _, err := newCustomAudienceClient(t, stub).ResolveInterests(context.Background(), []string{"Technology"}); err == nil || isTargetingRefusal(err) {
				t.Fatalf("err = %v, want a plain error", err)
			}
		})
	}
}

// A refusal names the entry the caller sent, counting blanks and duplicates the normalization
// dropped; the caps are refused before any request.
func TestResolveCommunities_ReportsTheCallersEntryAndCaps(t *testing.T) {
	stub := &discoveryStub{respond: func(*discoveryStub, string, int) (int, string) {
		return 200, `{"data":[{"id":"t5_1","name":"Kubernetes"}]}`
	}}
	c := newCustomAudienceClient(t, stub)
	_, err := c.ResolveCommunities(context.Background(), NormalizeSubredditEntries([]string{"kubernetes", " ", "Kubernetes", "nosuchsub"}))
	if !isTargetingRefusal(err) || !strings.Contains(err.Error(), `entry 4 ("nosuchsub")`) {
		t.Fatalf("err = %v, want entry 4", err)
	}
	many := make([]string, MaxResolvedSubreddits+1)
	for i := range many {
		many[i] = fmt.Sprintf("s%d", i)
	}
	before := len(stub.seen())
	if _, err := c.ResolveCommunities(context.Background(), NormalizeSubredditEntries(many)); !isTargetingRefusal(err) || !strings.Contains(err.Error(), "at most 100") {
		t.Fatalf("over the cap: err = %v", err)
	}
	if _, err := c.ResolveInterests(context.Background(), make([]string, MaxResolvedInterests+1)); !isTargetingRefusal(err) || !strings.Contains(err.Error(), "at most 200") {
		t.Fatalf("interests over the cap: err = %v", err)
	}
	if len(stub.seen()) != before {
		t.Error("a request was sent for a list over its cap")
	}
}
