// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const targetingAdGroup = `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"geolocations":["US"],"expand_targeting":true,"keywords":["a","b"]}}}`

func TestGetAdGroupTargeting_ReadsKeywordsAndARevisionIndependentOfMemberOrder(t *testing.T) {
	c, seen := budgetTestClient(t, http.StatusOK, targetingAdGroup)
	got, err := c.GetAdGroupTargeting(context.Background(), "t5_ag")
	if err != nil {
		t.Fatalf("GetAdGroupTargeting: %v", err)
	}
	if got.CampaignID != "t3_c" || len(got.Keywords) != 2 || got.Keywords[0] != "a" || got.Keywords[1] != "b" {
		t.Fatalf("unexpected shape: %+v", got)
	}
	if reqs := seen(); len(reqs) != 1 || !strings.HasPrefix(reqs[0], "GET /api/v3/ad_accounts/t2_test/ad_groups/t5_ag") {
		t.Errorf("want one GET of the ad group, got %v", reqs)
	}
	reordered, _ := budgetTestClient(t, http.StatusOK, `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"keywords":["a","b"],"expand_targeting":true,"geolocations":["US"]}}}`)
	got2, err := reordered.GetAdGroupTargeting(context.Background(), "t5_ag")
	if err != nil || got2.Revision != got.Revision {
		t.Errorf("the same targeting in another member order must fingerprint the same: %q vs %q (%v)", got.Revision, got2.Revision, err)
	}
	changed, _ := budgetTestClient(t, http.StatusOK, `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"geolocations":["GB"],"expand_targeting":true,"keywords":["a","b"]}}}`)
	got3, _ := changed.GetAdGroupTargeting(context.Background(), "t5_ag")
	if got3 == nil || got3.Revision == got.Revision {
		t.Error("a change outside keywords must change the revision")
	}
}

func TestGetAdGroupTargeting_Shapes(t *testing.T) {
	t.Run("absent keywords is an empty list", func(t *testing.T) {
		c, _ := budgetTestClient(t, http.StatusOK, `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"geolocations":["US"]}}}`)
		got, err := c.GetAdGroupTargeting(context.Background(), "t5_ag")
		if err != nil || got.Keywords == nil || len(got.Keywords) != 0 {
			t.Fatalf("want an empty list, got %+v, %v", got, err)
		}
	})
	t.Run("404 is no such ad group", func(t *testing.T) {
		c, _ := budgetTestClient(t, http.StatusNotFound, `{}`)
		got, err := c.GetAdGroupTargeting(context.Background(), "t5_ag")
		if err != nil || got != nil {
			t.Fatalf("want (nil, nil), got %+v, %v", got, err)
		}
	})
	for _, body := range []string{
		`{"data":{"id":"t5_ag","campaign_id":"t3_c"}}`,
		`{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":null}}`,
		`{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":["x"]}}`,
		`{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"keywords":"a"}}}`,
		`{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"keywords":[{"k":"a"}]}}}`,
	} {
		c, _ := budgetTestClient(t, http.StatusOK, body)
		if _, err := c.GetAdGroupTargeting(context.Background(), "t5_ag"); !errors.Is(err, ErrTargetingUnreadable) {
			t.Errorf("%s: want ErrTargetingUnreadable, got %v", body, err)
		}
	}
	t.Run("another ad group's answer is an error", func(t *testing.T) {
		c, _ := budgetTestClient(t, http.StatusOK, `{"data":{"id":"t5_other","targeting":{}}}`)
		if _, err := c.GetAdGroupTargeting(context.Background(), "t5_ag"); err == nil {
			t.Fatal("want an error")
		}
	})
}

func readTargeting(t *testing.T) *AdGroupTargeting {
	t.Helper()
	c, _ := budgetTestClient(t, http.StatusOK, targetingAdGroup)
	got, err := c.GetAdGroupTargeting(context.Background(), "t5_ag")
	if err != nil {
		t.Fatalf("GetAdGroupTargeting: %v", err)
	}
	return got
}

func TestReplaceAdGroupKeywords_SendsTheWholeTargetingWithOnlyKeywordsChanged(t *testing.T) {
	base := readTargeting(t)
	c, seen := budgetTestClient(t, http.StatusOK, `{"data":{"id":"t5_ag"}}`)
	if err := c.ReplaceAdGroupKeywords(context.Background(), "t5_ag", base, []string{"b"}); err != nil {
		t.Fatalf("ReplaceAdGroupKeywords: %v", err)
	}
	reqs := seen()
	want := `PATCH /api/v3/ad_accounts/t2_test/ad_groups/t5_ag {"data":{"targeting":{"expand_targeting":true,"geolocations":["US"],"keywords":["b"]}}}`
	if len(reqs) != 1 || reqs[0] != want {
		t.Errorf("request = %v\nwant      %s", reqs, want)
	}
}

func TestReplaceAdGroupKeywords_Classification(t *testing.T) {
	base := readTargeting(t)
	t.Run("an empty list is refused before any request", func(t *testing.T) {
		c, seen := budgetTestClient(t, http.StatusOK, `{}`)
		if err := c.ReplaceAdGroupKeywords(context.Background(), "t5_ag", base, nil); err == nil || len(seen()) != 0 {
			t.Fatalf("want a local refusal, got %v after %d requests", err, len(seen()))
		}
	})
	t.Run("a read of another ad group is refused", func(t *testing.T) {
		c, seen := budgetTestClient(t, http.StatusOK, `{}`)
		if err := c.ReplaceAdGroupKeywords(context.Background(), "t5_other", base, []string{"a"}); err == nil || len(seen()) != 0 {
			t.Fatalf("want a local refusal, got %v", err)
		}
	})
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		unconfirmed bool
	}{
		{"5xx", http.StatusBadGateway, `{}`, true},
		{"definite 400", http.StatusBadRequest, `{"error":{}}`, false},
		{"echo of another ad group", http.StatusOK, `{"data":{"id":"t5_x"}}`, true},
		{"echo of another keyword list", http.StatusOK, `{"data":{"id":"t5_ag","targeting":{"keywords":["a","b"]}}}`, true},
		{"echo of the same keywords in another order", http.StatusOK, `{"data":{"id":"t5_ag","targeting":{"keywords":["b"]}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := budgetTestClient(t, tc.status, tc.body)
			err := c.ReplaceAdGroupKeywords(context.Background(), "t5_ag", base, []string{"b"})
			if tc.status == http.StatusOK && !tc.unconfirmed {
				if err != nil {
					t.Fatalf("want success, got %v", err)
				}
				return
			}
			if err == nil || IsOutcomeUnconfirmed(err) != tc.unconfirmed {
				t.Fatalf("unconfirmed = %v, want %v (%v)", err != nil && IsOutcomeUnconfirmed(err), tc.unconfirmed, err)
			}
		})
	}
}

func TestSameKeywords(t *testing.T) {
	if !SameKeywords([]string{"a", "b", "a"}, []string{"a", "a", "b"}) || SameKeywords([]string{"a", "b"}, []string{"a", "a"}) || SameKeywords([]string{"a"}, nil) {
		t.Error("SameKeywords must compare as multisets")
	}
}

// A refusal answering a PATCH retried after a 429 cannot speak for the 429'd attempt.
func TestReplaceAdGroupKeywords_RefusalAfterARetried429IsUnconfirmed(t *testing.T) {
	base := readTargeting(t)
	c, calls := throttledThenClient(t, http.StatusBadRequest, `{"error":{}}`)
	err := c.ReplaceAdGroupKeywords(context.Background(), "t5_ag", base, []string{"b"})
	if !IsOutcomeUnconfirmed(err) {
		t.Fatalf("want UNCONFIRMED, got %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("want the 429 retried once (2 attempts), got %d", n)
	}
}
