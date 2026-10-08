// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPublishedSpecsAgreeOnOperationText pins that all eight published OpenAPI documents — v2 and
// v3, JSON and YAML, in gen/ and in the kodata copy the binary embeds — carry the SAME summary and
// description for every operation, keyed by operationId (identical across v2 and v3). ONE
// baseline is compared against every file, so a drift confined to v2 or to v3 fails as surely as
// one in a single file. The JSON renderings are single-line files, so a stale one is invisible in
// a diff review (a #296 review thread suspected exactly that drift); this makes it a test failure
// instead. Regenerate with `make apigen`, never by hand.
func TestPublishedSpecsAgreeOnOperationText(t *testing.T) {
	var files []string
	for _, dir := range []string{filepath.Join("..", "..", "gen", "http"), filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http")} {
		for _, f := range []string{"openapi.json", "openapi.yaml", "openapi3.json", "openapi3.yaml"} {
			files = append(files, filepath.Join(dir, f))
		}
	}
	for _, d := range specDisagreements(t, files) {
		t.Error(d)
	}
}

type specOpText struct{ Summary, Description string }

// readSpecOperationText returns every operation's summary and description in the spec at rel,
// keyed by operationId.
func readSpecOperationText(t *testing.T, rel string) map[string]specOpText {
	t.Helper()
	raw, err := os.ReadFile(rel) //nolint:gosec // repo-relative or temp path in a test
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
	out := map[string]specOpText{}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			m, ok := op.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["operationId"].(string)
			if id == "" {
				id = method + " " + path
			}
			s, _ := m["summary"].(string)
			d, _ := m["description"].(string)
			out[id] = specOpText{s, d}
		}
	}
	return out
}

// specDisagreements compares every file against the FIRST one's operations and describes each
// difference.
func specDisagreements(t *testing.T, files []string) []string {
	t.Helper()
	want := readSpecOperationText(t, files[0])
	if len(want) == 0 {
		t.Fatalf("%s has no operations", files[0])
	}
	var out []string
	for _, rel := range files[1:] {
		got := readSpecOperationText(t, rel)
		if len(got) != len(want) {
			out = append(out, fmt.Sprintf("%s has %d operations, %s has %d", rel, len(got), files[0], len(want)))
		}
		for id, w := range want {
			if g, ok := got[id]; !ok {
				out = append(out, fmt.Sprintf("%s: operation %s is missing", rel, id))
			} else if g != w {
				out = append(out, fmt.Sprintf("%s: operation %s text differs from %s", rel, id, files[0]))
			}
		}
	}
	return out
}

// The baseline spans v2 and v3: a description changed ONLY in a v3 document is caught. A copy of
// the published files is perturbed in a temp dir; the real files are untouched.
func TestPublishedSpecsAgree_CatchesAV3OnlyDrift(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join("..", "..", "gen", "http")
	var files []string
	for _, f := range []string{"openapi.json", "openapi.yaml", "openapi3.json", "openapi3.yaml"} {
		raw, err := os.ReadFile(filepath.Join(src, f)) //nolint:gosec // fixed repo-relative path
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.HasPrefix(f, "openapi3") {
			const from, to = "limits are per service process", "limits are per pod"
			if !strings.Contains(string(raw), from) {
				t.Fatalf("%s does not carry the sentence this test perturbs", f)
			}
			raw = []byte(strings.Replace(string(raw), from, to, 1))
		}
		p := filepath.Join(dir, f)
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		files = append(files, p)
	}
	if len(specDisagreements(t, files)) == 0 {
		t.Error("a v3-only description change was not detected")
	}
}
