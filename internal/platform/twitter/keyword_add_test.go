// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// #564 gap 12: a positive keyword add is one BROAD_KEYWORD criterion with operator_type EQ on the
// line item; an echo without operator_type confirms EQ (X's default), as on the create path.
func TestCreateKeywordCriterion_PostsABroadPositiveKeyword(t *testing.T) {
	for _, op := range []string{`,"operator_type":"EQ"`, ``} {
		c, seen := targetingServer(t, tcReply{http.StatusOK, `{"data":{"id":"k9","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"Cloud Native"` + op + `,"deleted":false}}`})
		id, err := c.CreateKeywordCriterion(context.Background(), "li1", "cloud native")
		if err != nil || id != "k9" {
			t.Fatalf("echo %q: CreateKeywordCriterion = %q, %v", op, id, err)
		}
		reqs := seen()
		if len(reqs) != 1 || !strings.HasPrefix(reqs[0], "POST /12/accounts/account123/targeting_criteria?") ||
			!strings.Contains(reqs[0], "operator_type=EQ") || !strings.Contains(reqs[0], "targeting_type=BROAD_KEYWORD") ||
			!strings.Contains(reqs[0], "line_item_id=li1") || !strings.Contains(reqs[0], "targeting_value=cloud+native") {
			t.Errorf("requests = %v", reqs)
		}
	}
}

// A 2xx that reports a negated, other-typed, other-line-item or other-text criterion does not
// confirm the add; a 429 or 5xx is UNCONFIRMED after one request; a 400 is a definite refusal.
func TestCreateKeywordCriterion_UnprovenEchoIsUnconfirmed(t *testing.T) {
	for _, body := range []string{
		`{"data":null}`,
		`{"data":{"id":"k1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"free","operator_type":"NE"}}`,
		`{"data":{"id":"k1","line_item_id":"li1","targeting_type":"PHRASE_KEYWORD","targeting_value":"free","operator_type":"EQ"}}`,
		`{"data":{"id":"k1","line_item_id":"li2","targeting_type":"BROAD_KEYWORD","targeting_value":"free","operator_type":"EQ"}}`,
		`{"data":{"id":"k1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"other","operator_type":"EQ"}}`,
		`{"data":{"id":"k1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"free","operator_type":"EQ","deleted":true}}`,
	} {
		c, _ := targetingServer(t, tcReply{http.StatusOK, body})
		if _, err := c.CreateKeywordCriterion(context.Background(), "li1", "free"); !IsOutcomeUnconfirmed(err) {
			t.Errorf("%s: want UNCONFIRMED, got %v", body, err)
		}
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway} {
		c, seen := targetingServer(t, tcReply{status, `{}`})
		_, err := c.CreateKeywordCriterion(context.Background(), "li1", "free")
		if !IsOutcomeUnconfirmed(err) || len(seen()) != 1 {
			t.Errorf("%d: want UNCONFIRMED after exactly one request, got %v after %d", status, err, len(seen()))
		}
	}
	c, _ := targetingServer(t, tcReply{http.StatusBadRequest, `{"errors":[{"code":"INVALID_PARAMETER"}]}`})
	if _, err := c.CreateKeywordCriterion(context.Background(), "li1", "free"); err == nil || IsOutcomeUnconfirmed(err) {
		t.Errorf("a 400 is a definite refusal, got %v", err)
	}
	c, seen := targetingServer(t, tcReply{http.StatusOK, `{}`})
	if _, err := c.CreateKeywordCriterion(context.Background(), "li/1", "free"); err == nil || len(seen()) != 0 {
		t.Errorf("a malformed line item id must be refused before sending: %v, %d sent", err, len(seen()))
	}
}

// The add takes the create-time keyword rules (#574) and its own per-request bound.
func TestNormalizeKeywordAdditions(t *testing.T) {
	got, err := NormalizeKeywordAdditions([]string{"  Kubernetes ", "kubernetes", "cloud native"})
	if err != nil || len(got) != 2 || got[0] != "Kubernetes" || got[1] != "cloud native" {
		t.Fatalf("NormalizeKeywordAdditions = %q, %v; want trimmed, case-folded duplicates dropped", got, err)
	}
	many := make([]string, MaxKeywordAdditions+1)
	for i := range many {
		many[i] = "kw" + strings.Repeat("x", i+1)
	}
	for name, in := range map[string][]string{
		"empty":    nil,
		"blank":    {"  "},
		"too long": {strings.Repeat("a", 81)},
		"control":  {"a\u0007b"},
		"too many": many,
	} {
		if _, err := NormalizeKeywordAdditions(in); !errors.Is(err, domain.ErrKeywordTargetingInvalid) {
			t.Errorf("%s: want ErrKeywordTargetingInvalid, got %v", name, err)
		}
	}
	if _, err := NormalizeKeywordAdditions(many[:MaxKeywordAdditions]); err != nil {
		t.Errorf("%d distinct keywords must be accepted: %v", MaxKeywordAdditions, err)
	}
}
