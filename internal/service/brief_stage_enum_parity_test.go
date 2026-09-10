// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/emailstage"
)

// TestPublishedStageEnumMatchesEmailStageNames pins the design's `stage` enum to
// `emailstage.Names()`, which owns the stage taxonomy.
//
// The list is spelled out by hand in the design DSL because `design/` imports only the Goa DSL --
// depending on a service package there would invert the contract/implementation direction. That
// hand copy is the drift risk this test removes.
//
// Drift is ASYMMETRIC and silent, which is why a test rather than a comment is warranted. Add a
// seventh stage to `emailstage` and copy generation accepts it immediately (generate-email
// deliberately resolves an unknown stage rather than rejecting it), while a brief carrying that
// stage cannot be created OR looked up -- both ends reject it against a six-value enum. Nothing
// fails to compile and no other test notices; the stage simply has no storable brief.
//
// Like TestPublishedBytesMaxLengthIsTheENCODEDCeiling, this reads the GENERATED artifact rather
// than the design source: what a client is held to is what Goa published, and a design edit that
// was never regenerated is exactly the state this must fail on. Both published copies are checked
// -- `gen/` and the `kodata/` copy the Makefile makes for ko embedding -- because a `goa gen` run
// that skips `make apigen`'s copy step leaves the SERVED spec disagreeing with the enforced one.
func TestPublishedStageEnumMatchesEmailStageNames(t *testing.T) {
	// "" is the paid brief's stage: paid has no series, so its stage is empty rather than absent.
	// It is a storage concept and not an email stage, which is why it is added here instead of
	// living in emailstage.Names().
	want := append([]string{""}, emailstage.Names()...)

	if len(want) != 7 {
		t.Fatalf("expected 6 email stages plus the empty paid stage, got %d (%v): this test's "+
			"premise moved and the design enum needs a deliberate review, not a mechanical update",
			len(want), want)
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
							Enum []string `json:"enum"`
						} `json:"properties"`
					} `json:"schemas"`
				} `json:"components"`
				Paths map[string]map[string]struct {
					Parameters []struct {
						Name   string `json:"name"`
						In     string `json:"in"`
						Schema struct {
							Enum []string `json:"enum"`
						} `json:"schema"`
					} `json:"parameters"`
				} `json:"paths"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}

			// BriefInput is the WRITE payload, and it is the one that matters most: a stage this
			// type accepts but find-brief's enum rejects writes a row no lookup can name -- the
			// typo returns 400 and the correct spelling 404, leaving it reachable only by id.
			schema, ok := doc.Components.Schemas["BriefInput"]
			if !ok {
				t.Fatalf("%s has no BriefInput schema", rel)
			}
			stage, ok := schema.Properties["stage"]
			if !ok {
				t.Fatalf("%s: BriefInput has no stage property", rel)
			}
			if len(stage.Enum) == 0 {
				t.Fatalf("%s: BriefInput.stage publishes NO enum. The write path would accept any "+
					"string while find-brief enforces a fixed set, so a misspelled stage writes a "+
					"brief that can never be read back.", rel)
			}
			if !equalStrings(stage.Enum, want) {
				t.Errorf("%s: BriefInput.stage enum = %v, want %v (emailstage.Names() plus the "+
					"empty paid stage)", rel, stage.Enum, want)
			}

			// The READ enum too, which is a SEPARATE hand-written list in the find-brief query
			// payload. Pinning only the write side would let someone update `BriefInput` from
			// `emailstage.Names()` and forget this one -- recreating exactly the asymmetry this
			// whole feature exists to close: a stage accepted on write that no lookup can name.
			var readEnum []string
			for _, ops := range doc.Paths {
				for _, op := range ops {
					for _, prm := range op.Parameters {
						if prm.Name == "stage" && prm.In == "query" && len(prm.Schema.Enum) > 0 {
							readEnum = prm.Schema.Enum
						}
					}
				}
			}
			if len(readEnum) == 0 {
				t.Fatalf("%s: no find-brief `stage` QUERY parameter publishes an enum. The read "+
					"path would accept any string while the write path enforces a fixed set.", rel)
			}
			if !equalStrings(readEnum, want) {
				t.Errorf("%s: find-brief stage query enum = %v, want %v — the read and write "+
					"enums have drifted, so a stage valid on one is unaddressable on the other",
					rel, readEnum, want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
