// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// twitterAudienceSpecs are every generated OpenAPI document — v2 and v3, in gen/ and in the kodata
// copy the binary embeds. The JSON renderings carry the same examples as the YAML ones.
var twitterAudienceSpecs = []string{
	filepath.Join("..", "..", "gen", "http", "openapi.json"),
	filepath.Join("..", "..", "gen", "http", "openapi3.json"),
	filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi.json"),
	filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi3.json"),
}

var (
	// twitterAudienceDimensionRank is the order the endpoint lists dimensions in.
	twitterAudienceDimensionRank = map[string]int{"age": 0, "gender": 1, "platform": 2}
	// twitterAudienceValuePattern is twitter.audienceValueRE (and the design's Pattern).
	twitterAudienceValuePattern    = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9 _+.\-]{0,62}[A-Za-z0-9+])?$`)
	twitterAudienceCurrencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	twitterAudienceWindowValues    = map[string]bool{"today": true, "yesterday": true, "last_7_days": true}
)

// twitterAudienceSpecExamples returns every `example` value in the spec at rel whose JSON path
// belongs to the X audience read — its schemas (TwitterAdsAudience*) or its route — keyed by path.
// The whole document is walked rather than named schemas, because Goa emits the same example under
// several generated names and both type-level and property-level examples can be impossible.
func twitterAudienceSpecExamples(t *testing.T, rel string) map[string]any {
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
					if strings.Contains(path, "TwitterAdsAudience") || strings.Contains(path, "twitter-ads/audience") {
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

// walkTwitterAudienceValues calls fn on v and on every object and array nested inside it.
func walkTwitterAudienceValues(v any, fn func(any)) {
	fn(v)
	switch n := v.(type) {
	case map[string]any:
		for _, child := range n {
			walkTwitterAudienceValues(child, fn)
		}
	case []any:
		for _, child := range n {
			walkTwitterAudienceValues(child, fn)
		}
	}
}

// TestPublishedTwitterAudienceExamplesArePossible pins that no example Goa publishes for the X
// audience read describes a response the endpoint cannot return:
//
//   - a bucket names one of the three dimensions, carries a value inside the read's charset,
//     non-negative counters, and ctr = clicks/impressions (0 with no impressions);
//   - a buckets array lists dimensions in order (age, gender, platform), impressions descending
//     within one, and never repeats a segment (rows are summed per segment);
//   - an envelope's bucket_count equals len(buckets), its window is one X serves, and its
//     account_currency is an ISO 4217 code.
func TestPublishedTwitterAudienceExamplesArePossible(t *testing.T) {
	for _, rel := range twitterAudienceSpecs {
		t.Run(rel, func(t *testing.T) {
			sawBucket, sawList, sawEnvelope := false, false, false
			for path, ex := range twitterAudienceSpecExamples(t, rel) {
				walkTwitterAudienceValues(ex, func(v any) {
					switch n := v.(type) {
					case map[string]any:
						if _, ok := n["dimension"]; ok {
							if _, hasValue := n["value"]; hasValue {
								sawBucket = true
								checkTwitterAudienceBucket(t, path, n)
							}
						}
						if buckets, ok := n["buckets"].([]any); ok {
							sawEnvelope = true
							if count, ok := n["bucket_count"].(float64); !ok || int(count) != len(buckets) {
								t.Errorf("%s: bucket_count %v beside %d buckets", path, n["bucket_count"], len(buckets))
							}
							if w, _ := n["window"].(string); !twitterAudienceWindowValues[w] {
								t.Errorf("%s: window %q is not one X serves", path, w)
							}
							if c, has := n["account_currency"]; has {
								if s, _ := c.(string); !twitterAudienceCurrencyPattern.MatchString(s) {
									t.Errorf("%s: account_currency %v", path, c)
								}
							}
						}
					case []any:
						if isTwitterAudienceBucketList(n) {
							sawList = true
							checkTwitterAudienceBucketList(t, path, n)
						}
					}
				})
			}
			// Guard against the walk silently finding nothing (a renamed type, a moved spec).
			if !sawBucket || !sawList || !sawEnvelope {
				t.Fatalf("found no X audience bucket (%v), buckets array (%v) or envelope (%v) example", sawBucket, sawList, sawEnvelope)
			}
		})
	}
}

func isTwitterAudienceBucketList(list []any) bool {
	if len(list) == 0 {
		return false
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return false
		}
		if _, ok := m["dimension"]; !ok {
			return false
		}
	}
	return true
}

func checkTwitterAudienceBucket(t *testing.T, path string, b map[string]any) {
	t.Helper()
	dim, _ := b["dimension"].(string)
	if _, ok := twitterAudienceDimensionRank[dim]; !ok {
		t.Errorf("%s: dimension %q is not age, gender or platform", path, dim)
	}
	if v, _ := b["value"].(string); !twitterAudienceValuePattern.MatchString(v) {
		t.Errorf("%s: value %q is outside the read's charset", path, v)
	}
	for k := range b {
		switch k {
		case "dimension", "value", "impressions", "clicks", "cost_micros", "ctr":
		default:
			t.Errorf("%s: bucket carries %q, which the X read never sets", path, k)
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

func checkTwitterAudienceBucketList(t *testing.T, path string, list []any) {
	t.Helper()
	seen := map[string]bool{}
	prevRank, prevImpressions := -1, math.Inf(1)
	for i, item := range list {
		b := item.(map[string]any)
		dim, _ := b["dimension"].(string)
		impressions, _ := b["impressions"].(float64)
		rank := twitterAudienceDimensionRank[dim]
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
			t.Errorf("%s[%d]: segment %s/%v repeated; rows are summed per segment", path, i, dim, b["value"])
		}
		seen[key] = true
	}
}
