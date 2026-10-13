// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestExcludedKeywords_Shapes(t *testing.T) {
	for _, tc := range []struct {
		body string
		want []string
		bad  bool
	}{
		{`{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"keywords":["a"]}}}`, []string{}, false},
		{`{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"keywords":["a"],"excluded_keywords":null}}}`, []string{}, false},
		{`{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"keywords":["a"],"excluded_keywords":["x",null,"y"]}}}`, []string{"x", "y"}, false},
		{`{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"keywords":["a"],"excluded_keywords":"x"}}}`, nil, true},
		{`{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"keywords":["a"],"excluded_keywords":[1]}}}`, nil, true},
	} {
		c, _ := budgetTestClient(t, http.StatusOK, tc.body)
		got, err := c.GetAdGroupTargeting(context.Background(), "t5_ag")
		if err != nil {
			t.Fatalf("the positive read must not depend on excluded_keywords: %v", err)
		}
		ex, err := got.ExcludedKeywords()
		if tc.bad {
			if err == nil {
				t.Errorf("%s: want ErrTargetingUnreadable", tc.body)
			}
			continue
		}
		if err != nil || strings.Join(ex, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: got %v, %v", tc.body, ex, err)
		}
	}
}

// The client's own guards refuse before any request: nothing to add, a keyword already excluded,
// over Reddit's 2000, and a base that is not a read of this ad group.
func TestAddAdGroupExcludedKeywords_RefusesBeforeAnyRequest(t *testing.T) {
	many := make([]string, MaxExcludedKeywords)
	for i := range many {
		many[i] = fmt.Sprintf("%q", fmt.Sprintf("k%d", i))
	}
	full := `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"keywords":["a"],"excluded_keywords":[` + strings.Join(many, ",") + `]}}}`
	for _, tc := range []struct {
		name, body string
		add        []string
	}{
		{"nothing to add", targetingAdGroup, nil},
		{"already excluded", `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"keywords":["a"],"excluded_keywords":["x"]}}}`, []string{"x"}},
		{"over reddit's 2000", full, []string{"new"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, seen := budgetTestClient(t, http.StatusOK, tc.body)
			base, err := c.GetAdGroupTargeting(context.Background(), "t5_ag")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.AddAdGroupExcludedKeywords(context.Background(), "t5_ag", base, tc.add); err == nil {
				t.Fatal("want a refusal")
			}
			if n := len(seen()); n != 1 {
				t.Fatalf("want only the read, got %d requests", n)
			}
		})
	}
	t.Run("a base read of another ad group", func(t *testing.T) {
		c, seen := budgetTestClient(t, http.StatusOK, targetingAdGroup)
		base, _ := c.GetAdGroupTargeting(context.Background(), "t5_ag")
		if _, err := c.AddAdGroupExcludedKeywords(context.Background(), "t5_other", base, []string{"x"}); err == nil || len(seen()) != 1 {
			t.Fatalf("want a refusal with no write, got %v", err)
		}
	})
}
