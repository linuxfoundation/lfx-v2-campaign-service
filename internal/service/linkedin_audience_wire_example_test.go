// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	// linkedInAudienceDimensionRank is the order the endpoint lists dimensions in.
	linkedInAudienceDimensionRank = map[string]int{"job_function": 0, "seniority": 1, "industry": 2, "company_size": 3, "country": 4}
	// linkedInAudienceValuePattern is linkedin.audienceValueRE (and the design's Pattern).
	linkedInAudienceValuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.\-]{0,127}$`)
	linkedInAudienceWindowValues = map[string]bool{"today": true, "last_7_days": true, "last_30_days": true, "this_month": true, "last_month": true}
)

// linkedInAudienceSpecExamples returns every `example` in the spec at rel whose JSON path belongs
// to the LinkedIn audience read (its schemas or its route), keyed by path.
func linkedInAudienceSpecExamples(t *testing.T, rel string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(rel) //nolint:gosec // fixed repo-relative path in a test
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	out := map[string]any{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch n := v.(type) {
		case map[string]any:
			for k, child := range n {
				if k == "example" {
					if strings.Contains(path, "LinkedinAdsAudience") || strings.Contains(path, "linkedin-ads/audience") {
						out[path+"/example"] = child
					}
					continue
				}
				walk(path+"/"+k, child)
			}
		case []any:
			for i, child := range n {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		}
	}
	walk("", doc)
	return out
}

// TestPublishedLinkedInAudienceExamplesArePossible pins that no example Goa publishes for the
// LinkedIn audience read describes a response the endpoint cannot return: a bucket names one of the
// five dimensions, a value inside the read's charset, non-negative counters and ctr =
// clicks/impressions; a buckets array is ordered by dimension then impressions and never repeats a
// value; an envelope's bucket_count equals len(buckets), its window is one LinkedIn serves, and
// approximate is true.
func TestPublishedLinkedInAudienceExamplesArePossible(t *testing.T) {
	for _, rel := range twitterAudienceSpecs {
		t.Run(rel, func(t *testing.T) {
			sawBucket, sawList, sawEnvelope := false, false, false
			for path, ex := range linkedInAudienceSpecExamples(t, rel) {
				walkTwitterAudienceValues(ex, func(v any) {
					switch n := v.(type) {
					case map[string]any:
						if _, ok := n["dimension"]; ok {
							sawBucket = true
							checkLinkedInAudienceBucket(t, path, n)
						}
						if buckets, ok := n["buckets"].([]any); ok {
							sawEnvelope = true
							if count, ok := n["bucket_count"].(float64); !ok || int(count) != len(buckets) {
								t.Errorf("%s: bucket_count %v beside %d buckets", path, n["bucket_count"], len(buckets))
							}
							if a, ok := n["approximate"].(bool); !ok || !a {
								t.Errorf("%s: approximate %v, want true", path, n["approximate"])
							}
							if w, _ := n["window"].(string); !linkedInAudienceWindowValues[w] {
								t.Errorf("%s: window %q is not one LinkedIn serves", path, w)
							}
						}
					case []any:
						if isTwitterAudienceBucketList(n) {
							sawList = true
							checkLinkedInAudienceBucketList(t, path, n)
						}
					}
				})
			}
			if !sawBucket || !sawList || !sawEnvelope {
				t.Fatalf("found no LinkedIn audience bucket (%v), buckets array (%v) or envelope (%v) example", sawBucket, sawList, sawEnvelope)
			}
		})
	}
}

func checkLinkedInAudienceBucket(t *testing.T, path string, b map[string]any) {
	t.Helper()
	dim, _ := b["dimension"].(string)
	if _, ok := linkedInAudienceDimensionRank[dim]; !ok {
		t.Errorf("%s: dimension %q is not one of the five pivots", path, dim)
	}
	if v, _ := b["value"].(string); !linkedInAudienceValuePattern.MatchString(v) {
		t.Errorf("%s: value %q is outside the read's charset", path, v)
	}
	for k := range b {
		switch k {
		case "dimension", "value", "impressions", "clicks", "cost_micros", "ctr":
		default:
			t.Errorf("%s: bucket carries %q, which the LinkedIn read never sets", path, k)
		}
	}
	impressions, _ := b["impressions"].(float64)
	clicks, _ := b["clicks"].(float64)
	cost, _ := b["cost_micros"].(float64)
	ctr, _ := b["ctr"].(float64)
	if impressions < 0 || clicks < 0 || cost < 0 {
		t.Errorf("%s: negative counter", path)
	}
	want := 0.0
	if impressions > 0 {
		want = clicks / impressions
	}
	if math.Abs(ctr-want) > 1e-9 {
		t.Errorf("%s: ctr %v, but clicks/impressions is %v", path, ctr, want)
	}
}

func checkLinkedInAudienceBucketList(t *testing.T, path string, list []any) {
	t.Helper()
	seen := map[string]bool{}
	prevRank, prevImpressions := -1, math.Inf(1)
	for i, item := range list {
		b := item.(map[string]any)
		dim, _ := b["dimension"].(string)
		impressions, _ := b["impressions"].(float64)
		rank := linkedInAudienceDimensionRank[dim]
		switch {
		case rank < prevRank:
			t.Errorf("%s[%d]: %s bucket listed after a later dimension", path, i, dim)
		case rank > prevRank:
			prevRank, prevImpressions = rank, impressions
		default:
			if impressions > prevImpressions {
				t.Errorf("%s[%d]: impressions rise within dimension %s", path, i, dim)
			}
			prevImpressions = impressions
		}
		key := fmt.Sprint(dim, "\x00", b["value"])
		if seen[key] {
			t.Errorf("%s[%d]: value %s/%v repeated; elements are summed per value", path, i, dim, b["value"])
		}
		seen[key] = true
	}
}
