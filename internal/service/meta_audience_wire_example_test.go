// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// metaAudienceDimensionFields is which value fields each published Meta audience dimension may
// carry; metaAudienceDimensionRank is the order the endpoint lists dimensions in.
var (
	metaAudienceDimensionFields = map[string][]string{
		"age_gender": {"age", "gender"},
		"placement":  {"publisher_platform", "platform_position"},
	}
	metaAudienceDimensionRank = map[string]int{"age_gender": 0, "placement": 1}
)

// publishedSpecs are every generated OpenAPI document — v2 and v3, in gen/ and in the kodata copy
// the binary embeds. The JSON renderings carry the same examples as the YAML ones.
var publishedSpecs = []string{
	filepath.Join("..", "..", "gen", "http", "openapi.json"),
	filepath.Join("..", "..", "gen", "http", "openapi3.json"),
	filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi.json"),
	filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi3.json"),
}

// publishedExamples returns every `example` value anywhere in the spec at rel, keyed by its JSON
// path. Walking the whole document (schemas, properties, response bodies, items) rather than
// naming schemas is the point: Goa emits the same example under several generated names, and
// the defects these tests pin lived in PROPERTY-level examples as well as type-level ones.
func publishedExamples(t *testing.T, rel string) map[string]any {
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
					out[path+"/example"] = child
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

// visitValues calls fn on v and on every object and array nested inside it.
func visitValues(v any, fn func(any)) {
	fn(v)
	switch n := v.(type) {
	case map[string]any:
		for _, child := range n {
			visitValues(child, fn)
		}
	case []any:
		for _, child := range n {
			visitValues(child, fn)
		}
	}
}

// TestPublishedMetaAudienceExamplesArePossible pins that no example Goa publishes for the Meta
// audience read describes a response the endpoint cannot return:
//
//   - a bucket carries ONLY its own dimension's value fields (attribute-composed examples once
//     put age, gender and placement on one bucket);
//   - a buckets array lists age_gender before placement and never repeats a segment (the
//     property-level example once showed three identical placement buckets — rows are merged
//     per segment, so a repeat is impossible);
//   - an envelope's bucket_count equals len(buckets) (once 24 beside two buckets).
func TestPublishedMetaAudienceExamplesArePossible(t *testing.T) {
	for _, rel := range publishedSpecs {
		t.Run(rel, func(t *testing.T) {
			sawBuckets, sawEnvelope := false, false
			for path, ex := range publishedExamples(t, rel) {
				visitValues(ex, func(v any) {
					switch n := v.(type) {
					case map[string]any:
						if dim, ok := n["dimension"].(string); ok {
							if _, meta := metaAudienceDimensionFields[dim]; meta {
								checkMetaBucketFields(t, path, dim, n)
							}
						}
						if buckets, ok := n["buckets"].([]any); ok && isMetaBucketList(buckets) {
							sawEnvelope = true
							if count, ok := n["bucket_count"].(float64); !ok || int(count) != len(buckets) {
								t.Errorf("%s: bucket_count %v beside %d buckets", path, n["bucket_count"], len(buckets))
							}
						}
					case []any:
						if isMetaBucketList(n) {
							sawBuckets = true
							checkMetaBucketList(t, path, n)
						}
					}
				})
			}
			// Guard against the walk silently finding nothing (a renamed field, a moved spec).
			if !sawBuckets || !sawEnvelope {
				t.Fatalf("found no Meta audience buckets array (%v) or envelope (%v) example to check", sawBuckets, sawEnvelope)
			}
		})
	}
}

func isMetaBucketList(list []any) bool {
	if len(list) == 0 {
		return false
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return false
		}
		dim, _ := m["dimension"].(string)
		if _, meta := metaAudienceDimensionFields[dim]; !meta {
			return false
		}
	}
	return true
}

func checkMetaBucketFields(t *testing.T, path, dim string, b map[string]any) {
	t.Helper()
	for d, fields := range metaAudienceDimensionFields {
		for _, f := range fields {
			if _, present := b[f]; present != (d == dim) {
				t.Errorf("%s: %s bucket has %s present=%v", path, dim, f, present)
			}
		}
	}
}

func checkMetaBucketList(t *testing.T, path string, list []any) {
	t.Helper()
	seen := map[string]bool{}
	prevRank := -1
	for i, item := range list {
		b := item.(map[string]any)
		dim := b["dimension"].(string)
		if rank := metaAudienceDimensionRank[dim]; rank < prevRank {
			t.Errorf("%s[%d]: %s bucket listed after a later dimension", path, i, dim)
		} else {
			prevRank = rank
		}
		key := fmt.Sprint(dim, b["age"], b["gender"], b["publisher_platform"], b["platform_position"])
		if seen[key] {
			t.Errorf("%s[%d]: segment %s repeated; rows are merged per segment", path, i, key)
		}
		seen[key] = true
	}
}

// TestPublishedLastSentEmailExamplesArePossible pins audience-last-sent-email's invariant on every
// published example: lists_unavailable=true means the two list arrays are empty because they are
// UNKNOWN, so no example may show it beside populated lists.
func TestPublishedLastSentEmailExamplesArePossible(t *testing.T) {
	for _, rel := range publishedSpecs {
		t.Run(rel, func(t *testing.T) {
			saw := false
			for path, ex := range publishedExamples(t, rel) {
				visitValues(ex, func(v any) {
					n, ok := v.(map[string]any)
					if !ok {
						return
					}
					if _, has := n["lists_unavailable"]; !has {
						return
					}
					saw = true
					if n["lists_unavailable"] != true {
						return
					}
					for _, f := range []string{"included_lists", "suppression_lists"} {
						if l, _ := n[f].([]any); len(l) != 0 {
							t.Errorf("%s: lists_unavailable=true beside %d %s", path, len(l), f)
						}
					}
				})
			}
			if !saw {
				t.Fatal("found no audience-last-sent-email example to check")
			}
		})
	}
}
