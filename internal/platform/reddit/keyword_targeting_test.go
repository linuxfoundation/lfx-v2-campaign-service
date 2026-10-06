// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"bytes"
	"context"
	"encoding/json"
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

// richTargeting carries the shapes the whole-object rewrite must not disturb: an integer beyond
// float64 and int64 precision, a decimal with a trailing zero, an explicit null, a nested object,
// an unknown member holding HTML-significant characters, and a null keyword element.
const richTargeting = `{"geolocations":["US"],"bid_cap":12345678901234567890,"ratio":1.10,"interests":null,"geo":{"include":[{"id":"US-CA","n":1}]},"x_unknown":"a<b&c","keywords":["a",null,"b"]}`

func richAdGroup(targeting string) string {
	return `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":` + targeting + `}}`
}

func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return buf.String()
}

func TestRemoveAdGroupKeywords_RoundTripsEveryOtherMemberExactly(t *testing.T) {
	rc, _ := budgetTestClient(t, http.StatusOK, richAdGroup(richTargeting))
	base, err := rc.GetAdGroupTargeting(context.Background(), "t5_ag")
	if err != nil {
		t.Fatalf("GetAdGroupTargeting: %v", err)
	}
	if len(base.Keywords) != 2 || base.Keywords[0] != "a" || base.Keywords[1] != "b" {
		t.Fatalf("keywords = %q (a null element is not a keyword)", base.Keywords)
	}
	before, err := base.OtherDimensionsFingerprint()
	if err != nil {
		t.Fatal(err)
	}

	c, seen := budgetTestClient(t, http.StatusOK, `{"data":{"id":"t5_ag"}}`)
	remaining, err := c.RemoveAdGroupKeywords(context.Background(), "t5_ag", base, []string{"a"})
	if err != nil {
		t.Fatalf("RemoveAdGroupKeywords: %v", err)
	}
	if len(remaining) != 1 || remaining[0] != "b" {
		t.Errorf("remaining = %q", remaining)
	}
	reqs := seen()
	prefix := "PATCH /api/v3/ad_accounts/t2_test/ad_groups/t5_ag "
	if len(reqs) != 1 || !strings.HasPrefix(reqs[0], prefix) {
		t.Fatalf("want one PATCH of the ad group, got %v", reqs)
	}
	var sent struct {
		Data struct {
			Targeting map[string]json.RawMessage `json:"targeting"`
		} `json:"data"`
	}
	sentBody := strings.TrimPrefix(reqs[0], prefix)
	if err := json.Unmarshal([]byte(sentBody), &sent); err != nil {
		t.Fatalf("PATCH body: %v", err)
	}
	var read map[string]json.RawMessage
	if err := json.Unmarshal([]byte(richTargeting), &read); err != nil {
		t.Fatal(err)
	}
	for k, v := range read {
		if k == "keywords" {
			continue
		}
		got, ok := sent.Data.Targeting[k]
		if !ok {
			t.Errorf("member %s was read but not sent", k)
			continue
		}
		if g, w := compactJSON(t, got), compactJSON(t, v); g != w {
			t.Errorf("member %s sent as %s, read as %s", k, g, w)
		}
	}
	if string(sent.Data.Targeting["interests"]) != "null" {
		t.Errorf("a null member must stay null, sent %s", sent.Data.Targeting["interests"])
	}
	for _, absent := range []string{"communities", "excluded_keywords"} {
		if _, ok := sent.Data.Targeting[absent]; ok {
			t.Errorf("member %s was absent when read and must stay absent", absent)
		}
	}
	if len(sent.Data.Targeting) != len(read) {
		t.Errorf("sent %d members, read %d", len(sent.Data.Targeting), len(read))
	}
	// The un-removed keyword elements go back as read, the null one included.
	if g := compactJSON(t, sent.Data.Targeting["keywords"]); g != `[null,"b"]` {
		t.Errorf("keywords sent = %s, want [null,\"b\"]", g)
	}

	// read → write → read: re-reading what was written fingerprints every other dimension the same.
	written, _ := json.Marshal(sent.Data.Targeting)
	ac, _ := budgetTestClient(t, http.StatusOK, richAdGroup(string(written)))
	after, err := ac.GetAdGroupTargeting(context.Background(), "t5_ag")
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got, _ := after.OtherDimensionsFingerprint(); got != before {
		t.Errorf("other-dimension fingerprint changed across read → write → read: %s → %s", before, got)
	}
	if !SameKeywords(after.Keywords, remaining) {
		t.Errorf("re-read keywords %q, want %q", after.Keywords, remaining)
	}
}

func TestRemoveAdGroupKeywords_ComparesExactly(t *testing.T) {
	rc, _ := budgetTestClient(t, http.StatusOK, richAdGroup(`{"keywords":["Kubernetes"," ebpf","x"]}`))
	base, err := rc.GetAdGroupTargeting(context.Background(), "t5_ag")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := budgetTestClient(t, http.StatusOK, `{}`)
	// Neither a case-folded nor a trimmed spelling names a keyword.
	remaining, err := c.RemoveAdGroupKeywords(context.Background(), "t5_ag", base, []string{"kubernetes", "ebpf"})
	if err != nil || !SameKeywords(remaining, []string{"Kubernetes", " ebpf", "x"}) {
		t.Errorf("inexact spellings removed something: %q, %v", remaining, err)
	}
	remaining, err = c.RemoveAdGroupKeywords(context.Background(), "t5_ag", base, []string{" ebpf"})
	if err != nil || !SameKeywords(remaining, []string{"Kubernetes", "x"}) {
		t.Errorf("the exact spelling must remove it: %q, %v", remaining, err)
	}
}

func TestRemoveAdGroupKeywords_Classification(t *testing.T) {
	base := readTargeting(t)
	t.Run("removing every keyword is refused before any request", func(t *testing.T) {
		c, seen := budgetTestClient(t, http.StatusOK, `{}`)
		if _, err := c.RemoveAdGroupKeywords(context.Background(), "t5_ag", base, []string{"a", "b"}); err == nil || len(seen()) != 0 {
			t.Fatalf("want a local refusal, got %v after %d requests", err, len(seen()))
		}
	})
	t.Run("a read of another ad group is refused", func(t *testing.T) {
		c, seen := budgetTestClient(t, http.StatusOK, `{}`)
		if _, err := c.RemoveAdGroupKeywords(context.Background(), "t5_other", base, []string{"a"}); err == nil || len(seen()) != 0 {
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
			_, err := c.RemoveAdGroupKeywords(context.Background(), "t5_ag", base, []string{"a"})
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
func TestRemoveAdGroupKeywords_RefusalAfterARetried429IsUnconfirmed(t *testing.T) {
	base := readTargeting(t)
	c, calls := throttledThenClient(t, http.StatusBadRequest, `{"error":{}}`)
	_, err := c.RemoveAdGroupKeywords(context.Background(), "t5_ag", base, []string{"a"})
	if !IsOutcomeUnconfirmed(err) {
		t.Fatalf("want UNCONFIRMED, got %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("want the 429 retried once (2 attempts), got %d", n)
	}
}
