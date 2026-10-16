// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestCreateNegativeKeywordCriterion_PostsANegatedKeyword(t *testing.T) {
	c, seen := targetingServer(t, tcReply{http.StatusOK, `{"data":{"id":"n9","line_item_id":"li1","targeting_type":"PHRASE_KEYWORD","targeting_value":"Free Download","operator_type":"NE","deleted":false}}`})
	id, err := c.CreateNegativeKeywordCriterion(context.Background(), "li1", TargetingPhraseKeyword, "free download")
	if err != nil || id != "n9" {
		t.Fatalf("CreateNegativeKeywordCriterion = %q, %v (X's case-normalised echo is still this keyword)", id, err)
	}
	reqs := seen()
	if len(reqs) != 1 || !strings.HasPrefix(reqs[0], "POST /12/accounts/account123/targeting_criteria?") ||
		!strings.Contains(reqs[0], "operator_type=NE") || !strings.Contains(reqs[0], "targeting_type=PHRASE_KEYWORD") ||
		!strings.Contains(reqs[0], "line_item_id=li1") || !strings.Contains(reqs[0], "targeting_value=free+download") {
		t.Errorf("requests = %v", reqs)
	}
}

func TestCreateNegativeKeywordCriterion_RefusesBeforeSending(t *testing.T) {
	for _, tc := range []struct{ lineItem, typ, kw string }{
		{"li1", TargetingUnorderedKeyword, "free"},
		{"li1", "NEGATIVE_PHRASE_KEYWORD", "free"},
		{"li/1", TargetingBroadKeyword, "free"},
		{"li1", TargetingBroadKeyword, "  "},
	} {
		c, seen := targetingServer(t, tcReply{http.StatusOK, `{}`})
		if _, err := c.CreateNegativeKeywordCriterion(context.Background(), tc.lineItem, tc.typ, tc.kw); err == nil {
			t.Errorf("%+v: want a refusal", tc)
		}
		if n := len(seen()); n != 0 {
			t.Errorf("%+v: %d requests sent", tc, n)
		}
	}
}

func TestCreateNegativeKeywordCriterion_UnprovenEchoIsUnconfirmed(t *testing.T) {
	for _, body := range []string{
		`{"data":null}`,
		`{"data":{"line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"free","operator_type":"NE"}}`,
		`{"data":{"id":"n1","line_item_id":"li2","targeting_type":"BROAD_KEYWORD","targeting_value":"free","operator_type":"NE"}}`,
		`{"data":{"id":"n1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"free","operator_type":"EQ"}}`,
		`{"data":{"id":"n1","line_item_id":"li1","targeting_type":"EXACT_KEYWORD","targeting_value":"free","operator_type":"NE"}}`,
		`{"data":{"id":"n1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"other","operator_type":"NE"}}`,
	} {
		c, _ := targetingServer(t, tcReply{http.StatusOK, body})
		_, err := c.CreateNegativeKeywordCriterion(context.Background(), "li1", TargetingBroadKeyword, "free")
		if !IsOutcomeUnconfirmed(err) {
			t.Errorf("%s: want UNCONFIRMED, got %v", body, err)
		}
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway} {
		c, seen := targetingServer(t, tcReply{status, `{}`})
		_, err := c.CreateNegativeKeywordCriterion(context.Background(), "li1", TargetingBroadKeyword, "free")
		if !IsOutcomeUnconfirmed(err) || len(seen()) != 1 {
			t.Errorf("%d: want UNCONFIRMED after exactly one request, got %v after %d", status, err, len(seen()))
		}
	}
	c, _ := targetingServer(t, tcReply{http.StatusBadRequest, `{"errors":[{"code":"INVALID_PARAMETER"}]}`})
	if _, err := c.CreateNegativeKeywordCriterion(context.Background(), "li1", TargetingBroadKeyword, "free"); err == nil || IsOutcomeUnconfirmed(err) {
		t.Errorf("a 400 is a definite refusal, got %v", err)
	}
}

// The POST was sent, so an echo identityjson refuses is UNCONFIRMED — never a definite refusal,
// and never retried. Each echo's last-wins reading would be a confirmed negative keyword.
func TestCreateNegativeKeywordCriterion_UntrustworthyEchoIsUnconfirmed(t *testing.T) {
	const tail = `"targeting_type":"BROAD_KEYWORD","targeting_value":"free","operator_type":"NE"}}`
	for name, body := range map[string]string{
		"duplicate id":           `{"data":{"id":"n0","id":"n1","line_item_id":"li1",` + tail,
		"duplicate line_item_id": `{"data":{"id":"n1","line_item_id":"li2","line_item_id":"li1",` + tail,
		"case-folded operator":   `{"data":{"id":"n1","line_item_id":"li1","OPERATOR_TYPE":"EQ",` + tail,
		"unpaired surrogate":     `{"data":{"id":"n1","line_item_id":"li1","note":"\uDBFF",` + tail,
		"invalid UTF-8":          "{\"data\":{\"id\":\"n1\",\"line_item_id\":\"li1\",\"note\":\"\xc3\"," + tail,
	} {
		t.Run(name, func(t *testing.T) {
			c, seen := targetingServer(t, tcReply{http.StatusOK, body})
			id, err := c.CreateNegativeKeywordCriterion(context.Background(), "li1", TargetingBroadKeyword, "free")
			if id != "" || !IsOutcomeUnconfirmed(err) {
				t.Fatalf("want UNCONFIRMED and no id, got %q, %v", id, err)
			}
			if n := len(seen()); n != 1 {
				t.Errorf("want exactly one POST (no retry), got %d", n)
			}
		})
	}
}
