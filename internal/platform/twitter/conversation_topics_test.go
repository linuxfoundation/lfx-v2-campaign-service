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

func TestNormalizeConversationTopics(t *testing.T) {
	over := make([]string, MaxCreateConversationTopics+1)
	for i := range over {
		over[i] = fmt.Sprintf("t%d", i)
	}
	for _, tc := range []struct {
		name string
		in   []string
		want string // the normalized list joined, or the refusal it must contain
		ok   bool
	}{
		{"nothing", nil, "", true},
		{"trimmed, folded and de-duplicated", []string{" A1 ", "b2", "a1"}, "a1,b2", true},
		{"blank", []string{"a1", "  "}, "conversationTopics: entry 1 is blank", false},
		{"punctuation", []string{"a-1"}, "conversationTopics: entry 0", false},
		{"too long", []string{strings.Repeat("a", 33)}, "conversationTopics: entry 0", false},
		{"too many", over, fmt.Sprintf("conversationTopics: at most %d distinct entries", MaxCreateConversationTopics), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeConversationTopics(tc.in)
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

// X's example conversation entry is {"targeting_type":"CONVERSATION","targeting_value":"a1",
// "name":"NFL","conversation_type":"SPORTS"}; the second page proves the walk follows the cursor.
const (
	conversationsPage0 = `{"data":[{"name":"NFL","conversation_type":"SPORTS","targeting_type":"CONVERSATION","targeting_value":"a1"}],"next_cursor":"p1"}`
	conversationsPage1 = `{"data":[{"name":"Kubernetes","conversation_type":"TECHNOLOGY","targeting_type":"CONVERSATION","targeting_value":"k8s"},{"name":"Wrong type","targeting_type":"INTEREST","targeting_value":"zz"}],"next_cursor":null}`
)

// Conversation topics follow the interests as CONVERSATION criteria with EQ, before the promoted
// tweet, each checked against the walked list first and sent as X listed it.
func TestCreateCampaign_ConversationTopicCriteria(t *testing.T) {
	f := newTargetingFakeX()
	f.interestsPages = []string{interestsTechDev}
	f.conversationsPages = []string{conversationsPage0, conversationsPage1}
	c := targetingClient(t, f)
	in := targetingInput(nil, nil)
	in.InterestIDs = []string{"1001"}
	in.ConversationTopics = []string{"K8S", "a1", "k8s"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	writes, lookups := f.snapshot()
	want := []string{
		"POST campaigns",
		"POST line_items",
		"POST targeting_criteria INTEREST=1001",
		"POST targeting_criteria CONVERSATION=k8s",
		"POST targeting_criteria CONVERSATION=a1",
		"POST promoted_tweets",
	}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes =\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	if got := strings.Join(f.ops(), ","); got != "EQ,EQ,EQ" {
		t.Errorf("operators = %s", got)
	}
	var convReads []string
	for _, l := range lookups {
		if strings.HasPrefix(l, "conversations?") {
			convReads = append(convReads, l)
		}
	}
	if len(convReads) != 2 || !strings.Contains(convReads[0], "count=1000") || !strings.Contains(convReads[1], "cursor=p1") {
		t.Errorf("conversation reads = %v, want both pages walked", convReads)
	}
	if steps := strings.Join(res.Steps, "\n"); !strings.Contains(steps, "Targeting added to line item li1: conversation topic k8s") {
		t.Errorf("steps = %s", steps)
	}
}

// A topic X does not list (including one listed under another targeting_type), or a list that
// cannot be read to its end, creates nothing.
func TestCreateCampaign_ConversationTopicRefusalsCreateNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		pages   []string
		topic   string
		refusal bool
	}{
		"unknown topic":           {[]string{conversationsPage0, conversationsPage1}, "b9", true},
		"listed as another type":  {[]string{conversationsPage0, conversationsPage1}, "zz", true},
		"second page unreadable":  {[]string{conversationsPage0}, "k8s", false},
		"no list in the response": {[]string{`{"data":null}`}, "a1", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTargetingFakeX()
			f.conversationsPages = tc.pages
			c := targetingClient(t, f)
			in := targetingInput(nil, nil)
			in.ConversationTopics = []string{tc.topic}
			res, err := c.CreateCampaign(context.Background(), in)
			if res != nil || err == nil || isRefusal(err) != tc.refusal {
				t.Fatalf("CreateCampaign = (%v, %v), want (nil, err) with refusal=%v", res, err, tc.refusal)
			}
			if tc.refusal && !strings.Contains(err.Error(), "invalid twitterConfig.conversationTopics: X offers no conversation topic") {
				t.Errorf("err = %v", err)
			}
			if writes, _ := f.snapshot(); len(writes) != 0 {
				t.Errorf("writes = %v, want none", writes)
			}
		})
	}
}

// Conversation topics count toward the per-create criteria cap, refused before anything is sent.
func TestCreateCampaign_ConversationTopicsCountTowardTheCriteriaCap(t *testing.T) {
	f := newTargetingFakeX()
	c := targetingClient(t, f)
	in := targetingInput([]string{"US"}, nil)
	for i := range MaxCreateTargetingCriteria {
		in.ConversationTopics = append(in.ConversationTopics, fmt.Sprintf("t%d", i))
	}
	res, err := c.CreateCampaign(context.Background(), in)
	if res != nil || !isRefusal(err) || !strings.Contains(err.Error(), fmt.Sprintf("together name %d targeting criteria", MaxCreateTargetingCriteria+1)) ||
		!strings.Contains(err.Error(), FieldConversationTopics) {
		t.Fatalf("CreateCampaign = (%v, %v)", res, err)
	}
	if writes, lookups := f.snapshot(); len(writes) != 0 || len(lookups) != 0 {
		t.Errorf("writes=%v lookups=%v, want nothing sent", writes, lookups)
	}
}
