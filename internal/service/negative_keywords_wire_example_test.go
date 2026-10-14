// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The published add-negative-keywords request example must be a request the endpoint can accept.
// It carries `revision`, which only Reddit takes, and Reddit offers the Broad match type only — so
// an example pairing a revision with Exact or Phrase is a guaranteed 400 on every platform
// (PR #321 review).
func TestAddNegativeKeywords_PublishedRequestExampleIsAcceptable(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("..", "..", "gen", "http", "openapi3.json"),
		filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi3.json"),
	} {
		raw, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		var doc struct {
			Paths map[string]map[string]struct {
				RequestBody struct {
					Content map[string]struct {
						Example struct {
							NegativeKeywords []struct {
								MatchType string `json:"match_type"`
							} `json:"negative_keywords"`
							Revision *string `json:"revision"`
						} `json:"example"`
					} `json:"content"`
				} `json:"requestBody"`
			} `json:"paths"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		found := false
		for path, ops := range doc.Paths {
			if !strings.HasSuffix(path, "/negative-keywords") {
				continue
			}
			ex := ops["post"].RequestBody.Content["application/json"].Example
			found = true
			if len(ex.NegativeKeywords) == 0 {
				t.Errorf("%s: the request example carries no negative keyword", rel)
			}
			for i, k := range ex.NegativeKeywords {
				if ex.Revision != nil && k.MatchType != "Broad" {
					t.Errorf("%s: example keyword %d is %q beside a revision — Reddit, the only platform taking a revision, offers Broad only", rel, i, k.MatchType)
				}
			}
		}
		if !found {
			t.Fatalf("%s: no negative-keywords route", rel)
		}
	}
}
