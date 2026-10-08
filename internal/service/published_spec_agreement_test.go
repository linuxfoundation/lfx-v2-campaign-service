// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPublishedSpecsAgreeOnOperationText pins that all eight published OpenAPI documents — v2 and
// v3, JSON and YAML, in gen/ and in the kodata copy the binary embeds — carry the SAME summary and
// description for every operation. The JSON renderings are single-line files, so a stale one is
// invisible in a diff review (a #296 review thread suspected exactly that drift); this makes it a
// test failure instead. Regenerate with `make apigen`, never by hand.
func TestPublishedSpecsAgreeOnOperationText(t *testing.T) {
	type opText struct{ Summary, Description string }
	read := func(rel string) map[string]opText {
		t.Helper()
		raw, err := os.ReadFile(rel) //nolint:gosec // fixed repo-relative path in a test
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		var doc struct {
			Paths map[string]map[string]any `json:"paths" yaml:"paths"`
		}
		if filepath.Ext(rel) == ".json" {
			err = json.Unmarshal(raw, &doc)
		} else {
			err = yaml.Unmarshal(raw, &doc)
		}
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		out := map[string]opText{}
		for path, ops := range doc.Paths {
			for method, op := range ops {
				m, ok := op.(map[string]any)
				if !ok {
					continue
				}
				s, _ := m["summary"].(string)
				d, _ := m["description"].(string)
				out[method+" "+path] = opText{s, d}
			}
		}
		return out
	}
	for _, pair := range [][2]string{
		{"openapi.json", "openapi.yaml"},
		{"openapi3.json", "openapi3.yaml"},
	} {
		var want map[string]opText
		var wantFrom string
		for _, dir := range []string{filepath.Join("..", "..", "gen", "http"), filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http")} {
			for _, f := range pair {
				rel := filepath.Join(dir, f)
				got := read(rel)
				if want == nil {
					want, wantFrom = got, rel
					if len(want) == 0 {
						t.Fatalf("%s has no operations", rel)
					}
					continue
				}
				if len(got) != len(want) {
					t.Errorf("%s has %d operations, %s has %d", rel, len(got), wantFrom, len(want))
				}
				for op, w := range want {
					if g := got[op]; g != w {
						t.Errorf("%s: %s text differs from %s", rel, op, wantFrom)
					}
				}
			}
		}
	}
}
