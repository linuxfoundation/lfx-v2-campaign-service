// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// metaAudienceDimensionFields is which value fields each published dimension may carry.
var metaAudienceDimensionFields = map[string][]string{
	"age_gender": {"age", "gender"},
	"placement":  {"publisher_platform", "platform_position"},
}

// TestPublishedMetaAudienceExamplesArePossible reads the GENERATED OpenAPI, because the defect
// lived in what Goa published: with only attribute-level examples it composed a bucket carrying
// age, gender AND placement values at once, and an envelope whose bucket_count (24) disagreed
// with its two-item buckets array. Every published bucket example must carry exactly its own
// dimension's value fields, and the envelope's bucket_count must equal len(buckets).
func TestPublishedMetaAudienceExamplesArePossible(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("..", "..", "gen", "http", "openapi3.json"),
		filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi3.json"),
	} {
		t.Run(rel, func(t *testing.T) {
			raw, err := os.ReadFile(rel) //nolint:gosec // fixed repo-relative path in a test
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			var doc struct {
				Components struct {
					Schemas map[string]struct {
						Example json.RawMessage `json:"example"`
					} `json:"schemas"`
				} `json:"components"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			var env struct {
				Buckets     []map[string]any `json:"buckets"`
				BucketCount *int             `json:"bucket_count"`
			}
			if err := json.Unmarshal(doc.Components.Schemas["MetaAdsAudience"].Example, &env); err != nil {
				t.Fatalf("MetaAdsAudience example: %v", err)
			}
			switch {
			case env.BucketCount == nil:
				t.Errorf("the envelope example has no bucket_count")
			case *env.BucketCount != len(env.Buckets):
				t.Errorf("bucket_count = %d but buckets has %d entries", *env.BucketCount, len(env.Buckets))
			}
			var bucket map[string]any
			if err := json.Unmarshal(doc.Components.Schemas["MetaAdsAudienceBucket"].Example, &bucket); err != nil {
				t.Fatalf("MetaAdsAudienceBucket example: %v", err)
			}
			for i, b := range append(env.Buckets, bucket) {
				dim, _ := b["dimension"].(string)
				own, ok := metaAudienceDimensionFields[dim]
				if !ok {
					t.Errorf("example bucket %d has dimension %q", i, dim)
					continue
				}
				for d, fields := range metaAudienceDimensionFields {
					for _, f := range fields {
						_, present := b[f]
						if want := d == dim; present != want {
							t.Errorf("example %s bucket %d: %s present=%v, want %v (own fields %v)", dim, i, f, present, want, own)
						}
					}
				}
			}
		})
	}
}
