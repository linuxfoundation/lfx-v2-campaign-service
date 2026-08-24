// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestPublishedBytesMaxLengthIsTheENCODEDCeiling pins the finding a review round caught: the
// OpenAPI `maxLength` on the bytes attribute constrains the base64 STRING, not the decoded image,
// and publishing the decoded ceiling there rejected uploads the endpoint intends to accept.
//
// The test reads the GENERATED artifact rather than the design source, because the defect lived in
// what Goa published, not in what the design meant. It derives the expected figure from
// maxCreativeStoredBytes via base64's own encoder — asserting a hardcoded 41943040 would restate
// the constant instead of proving the RELATIONSHIP, and would still pass if both moved together
// and became wrong together.
func TestPublishedBytesMaxLengthIsTheENCODEDCeiling(t *testing.T) {
	// What a maximum-size image actually occupies on the wire, per base64 itself.
	wantEncoded := base64.StdEncoding.EncodedLen(maxCreativeStoredBytes)

	// Sanity-check the 4/3 expansion the finding turned on, so a broken assumption here is
	// reported as such rather than silently agreeing with the schema.
	if wantEncoded <= maxCreativeStoredBytes {
		t.Fatalf("base64 encoding of %d bytes is %d characters: expected EXPANSION, so the "+
			"premise of this test is wrong", maxCreativeStoredBytes, wantEncoded)
	}

	for _, rel := range []string{
		filepath.Join("..", "..", "gen", "http", "openapi3.json"),
		filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi3.json"),
	} {
		t.Run(filepath.Base(filepath.Dir(filepath.Dir(rel)))+"/"+filepath.Base(rel), func(t *testing.T) {
			raw, err := os.ReadFile(rel) //nolint:gosec // fixed repo-relative path in a test
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			var doc struct {
				Components struct {
					Schemas map[string]struct {
						Properties map[string]struct {
							Type      string `json:"type"`
							MaxLength *int   `json:"maxLength"`
						} `json:"properties"`
					} `json:"schemas"`
				} `json:"components"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}

			b, ok := doc.Components.Schemas["UploadCreativeAssetRequestBody"].Properties["bytes"]
			if !ok {
				t.Fatalf("%s has no UploadCreativeAssetRequestBody.bytes property", rel)
			}
			// The whole finding rests on this being a string: maxLength on a JSON string counts
			// CHARACTERS. If Goa ever emitted it as something else the reasoning would change.
			if b.Type != "string" {
				t.Fatalf("bytes is published as %q, not \"string\"; maxLength would no longer count "+
					"base64 characters and this test's premise needs revisiting", b.Type)
			}
			if b.MaxLength == nil {
				t.Fatal("bytes has no maxLength: the wire ceiling is unpublished, so a client has " +
					"no declared bound at all")
			}
			if *b.MaxLength != wantEncoded {
				t.Errorf("published maxLength = %d, want %d.\n"+
					"maxLength constrains the BASE64 STRING, so it must be the encoded ceiling. "+
					"Publishing the decoded ceiling (%d) makes standards-compliant validators and "+
					"generated clients reject uploads at ~%d decoded bytes — inside what this "+
					"endpoint accepts.",
					*b.MaxLength, wantEncoded, maxCreativeStoredBytes,
					maxCreativeStoredBytes/4*3)
			}
		})
	}
}
