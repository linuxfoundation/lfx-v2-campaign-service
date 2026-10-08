// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestPublishedAudienceBuilderExamplesArePossible walks EVERY example in the four generated
// OpenAPI JSON documents (gen/ and the kodata copy the service serves, v2 and v3) and checks each
// AudienceListBrief- and AudiencePreviewCount-shaped object against its own descriptions. Goa's
// synthesised examples drew attributes independently and published impossible pairings
// (`missing: true` with a resolved name and link; `exact: false` with a count that is neither the
// estimate nor 0); the type-level Example()s in design/audience_builder.go replace them.
func TestPublishedAudienceBuilderExamplesArePossible(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("..", "..", "gen", "http", "openapi.json"),
		filepath.Join("..", "..", "gen", "http", "openapi3.json"),
		filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi.json"),
		filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi3.json"),
	} {
		t.Run(rel, func(t *testing.T) {
			raw, err := os.ReadFile(rel) //nolint:gosec // fixed repo-relative path in a test
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var doc any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse: %v", err)
			}
			briefs, counts := 0, 0
			var walk func(v any, inExample bool)
			walk = func(v any, inExample bool) {
				switch x := v.(type) {
				case map[string]any:
					if inExample {
						if _, ok := x["missing"]; ok {
							if _, ok := x["list_id"]; ok {
								briefs++
								checkListBriefExample(t, x)
							}
						}
						if _, ok := x["exact"]; ok {
							if _, ok := x["estimate"]; ok {
								counts++
								checkPreviewCountExample(t, x)
							}
						}
					}
					for k, c := range x {
						walk(c, inExample || k == "example" || k == "examples" || k == "x-example")
					}
				case []any:
					for _, c := range x {
						walk(c, inExample)
					}
				}
			}
			walk(doc, false)
			if briefs == 0 || counts == 0 {
				t.Fatalf("found %d list-brief and %d preview-count examples; the walk no longer reaches them", briefs, counts)
			}
		})
	}
}

func checkListBriefExample(t *testing.T, x map[string]any) {
	t.Helper()
	if missing, _ := x["missing"].(bool); missing {
		if name, _ := x["name"].(string); name != "" {
			t.Errorf("list-brief example %v: missing=true with a non-empty name", x)
		}
		if _, ok := x["hubspot_url"]; ok {
			t.Errorf("list-brief example %v: missing=true with a hubspot_url", x)
		}
	}
}

func checkPreviewCountExample(t *testing.T, x map[string]any) {
	t.Helper()
	exact, _ := x["exact"].(bool)
	count, _ := x["count"].(float64)
	estimate, _ := x["estimate"].(float64)
	if !exact && count != estimate && count != 0 {
		t.Errorf("preview-count example %v: exact=false with count neither the estimate nor 0", x)
	}
	if exact && count > estimate {
		t.Errorf("preview-count example %v: a counted union larger than the sum of the list sizes", x)
	}
}
