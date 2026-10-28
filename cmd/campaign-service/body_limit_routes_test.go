// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// bodyRoute is one body-bearing operation read from the OpenAPI document the service serves.
type bodyRoute struct {
	method   string
	template string // as published, e.g. /projects/{project_id}/briefs
	path     string // concrete: every {param} replaced with the spec's own example value
}

// bodyBearingRoutes returns every operation in kodata's openapi3.json that declares a request
// body, with path parameters filled from their documented examples so the request routes to the
// real Goa handler. Reading the spec rather than hand-listing routes is what keeps the test
// honest as endpoints are added: a new POST is covered the moment it is published.
func bodyBearingRoutes(t *testing.T) []bodyRoute {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("kodata", "gen", "http", "openapi3.json"))
	if err != nil {
		t.Fatalf("read openapi3.json: %v", err)
	}
	type param struct {
		Name   string `json:"name"`
		In     string `json:"in"`
		Schema struct {
			Example any `json:"example"`
		} `json:"schema"`
	}
	type op struct {
		RequestBody json.RawMessage `json:"requestBody"`
		Parameters  []param         `json:"parameters"`
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi3.json: %v", err)
	}

	placeholder := regexp.MustCompile(`\{(\w+)\}`)
	var out []bodyRoute
	for path, ops := range doc.Paths {
		for method, rawOp := range ops {
			var o op
			if json.Unmarshal(rawOp, &o) != nil || len(o.RequestBody) == 0 {
				continue
			}
			examples := map[string]string{}
			for _, p := range o.Parameters {
				if p.In == "path" {
					if s, ok := p.Schema.Example.(string); ok {
						examples[p.Name] = s
					}
				}
			}
			concrete := placeholder.ReplaceAllStringFunc(path, func(m string) string {
				name := m[1 : len(m)-1]
				v, ok := examples[name]
				if !ok {
					t.Fatalf("%s %s: path parameter %q has no string example to route with", method, path, name)
				}
				return v
			})
			out = append(out, bodyRoute{method: strings.ToUpper(method), template: path, path: concrete})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].method+out[i].path < out[j].method+out[j].path })
	return out
}

// TestNonUploadRoutes_RejectOversizeBodyBeforeDecoding pins #177: every route OTHER than the
// creative-asset upload refuses a body over MaxNonUploadRequestBodyBytes before it is decoded.
//
// Goa decodes a request body before the endpoint authenticates the caller, so whatever the
// per-request cap admits is allocated pre-auth. Before #177 every route shared the upload's
// 42 MiB cap, so a body of several MiB on create-brief (whose Any fields accept arbitrary JSON) was read
// and decoded in full, then refused for an unrelated reason (400/401). This test sends exactly
// that and requires 413 — the only status that proves the size gate, not a later check, refused it.
//
// Both arms are driven per route, for the same reason the upload tests drive both: a declared
// Content-Length is refused from the header alone, while an undeclared (chunked) body can only be
// stopped by the read cap, and it must be WELL-FORMED JSON so the decoder keeps reading past the
// cap instead of aborting on a syntax error and producing a 400 whether or not the cap exists.
func TestNonUploadRoutes_RejectOversizeBodyBeforeDecoding(t *testing.T) {
	routes := bodyBearingRoutes(t)
	h := realChain(t)

	// One byte over the non-upload cap and far under the upload one, so the test fails against
	// a single global 42 MiB cap and pins the exact boundary.
	oversize := constants.MaxNonUploadRequestBodyBytes + 1
	if oversize >= constants.MaxRequestBodyBytes {
		t.Fatalf("premise: non-upload cap %d must be below the upload cap %d", constants.MaxNonUploadRequestBodyBytes, constants.MaxRequestBodyBytes)
	}
	wellFormed := func() io.Reader {
		return io.MultiReader(
			strings.NewReader(`{"pad":"`),
			repeatReader('A', oversize),
			strings.NewReader(`"}`),
		)
	}

	covered, skipped := 0, 0
	for _, rt := range routes {
		// isUploadRequest grants the 42 MiB cap to any POST whose path ENDS in /creative-assets.
		// That is safe only while no POST route ends in a path parameter: one that did would let
		// a caller set the parameter to "creative-assets" and buy the upload's cap on a route that
		// is not the upload. Fail here the moment such a route is published.
		if rt.method == http.MethodPost && strings.HasSuffix(rt.template, "}") {
			t.Errorf("POST %s ends in a path parameter, so a caller could make it match the upload "+
				"predicate's /creative-assets suffix; match the upload by full template instead", rt.template)
		}
		if strings.HasSuffix(rt.path, "/creative-assets") && rt.method == http.MethodPost {
			skipped++
			continue // the upload keeps its own, larger cap: see TestUploadRoute_AdmitsMaximumLegalUpload
		}
		covered++
		for _, arm := range []struct {
			name     string
			declared bool
		}{{"declared", true}, {"chunked", false}} {
			t.Run(rt.method+" "+rt.path+"/"+arm.name, func(t *testing.T) {
				req := httptest.NewRequest(rt.method, rt.path, wellFormed())
				req.Header.Set("Content-Type", "application/json")
				if arm.declared {
					req.ContentLength = oversize + int64(len(`{"pad":""}`))
				} else {
					req.ContentLength = -1
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusRequestEntityTooLarge {
					t.Fatalf("status = %d, want 413: a %d-byte body on a non-upload route must be refused by the pre-decode cap",
						rec.Code, oversize)
				}
			})
		}
	}
	if covered == 0 {
		t.Fatal("no non-upload body-bearing routes found in openapi3.json; the test is asserting nothing")
	}
	// Exactly the one upload route is exempt. More would mean the skip condition is wider than
	// the upload, silently leaving a route unasserted.
	if skipped != 1 {
		t.Errorf("skipped %d routes as the upload, want exactly 1", skipped)
	}
}

// TestNonUploadRoutes_AdmitMaximalWizardSections pins the SIZING of the non-upload cap to the
// largest legitimate body, so the cap cannot be tightened past what the service itself accepts.
//
// update-sections takes `sections` as ArrayOf(Any): Goa declares no bound, and the real one is
// in the service — 120 blocks, 20,000 runes per rich_text block and a 200,000-rune assembled
// body (internal/service/email_wizard_sections.go). This sends a body at that limit, encoded at
// the worst case JSON permits: every rune an emoji written as a UTF-16 surrogate-pair escape,
// 12 bytes per rune, which is what Python's default encoder produces. It is over 2 MiB, which is
// why the cap is 4 MiB. The body is assembled by hand because encoding/json never escapes a
// printable non-BMP rune, so marshalling could not reach this worst case.
func TestNonUploadRoutes_AdmitMaximalWizardSections(t *testing.T) {
	const blocks, richBlocks, runesPerRich = 120, 10, 19900 // 10 x 19,900 + wrappers stays within 200,000
	const emojiEscaped = `\ud83d\ude00`                     // one rune (U+1F600), 12 bytes on the wire
	var b strings.Builder
	b.WriteString(`{"session_id":"48b76cd3-4f8f-4644-9310-a6c9548e5726","sections":[`)
	for i := 0; i < blocks; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		if i < richBlocks {
			b.WriteString(`{"type":"rich_text","html":"` + strings.Repeat(emojiEscaped, runesPerRich) + `"}`)
		} else {
			b.WriteString(`{"type":"divider"}`)
		}
	}
	b.WriteString(`]}`)
	body := []byte(b.String())

	// Pin the premise rather than trusting the arithmetic: the fixture must decode to a block of
	// exactly runesPerRich runes (so it is within the service's per-block limit, i.e. legitimate)
	// and its wire size must exceed 2 MiB (so it shows why the cap is not 2 MiB).
	var decoded struct {
		Sections []struct {
			HTML string `json:"html"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if n := utf8.RuneCountInString(decoded.Sections[0].HTML); n != runesPerRich {
		t.Fatalf("premise: a rich_text block decodes to %d runes, want %d", n, runesPerRich)
	}
	if int64(len(body)) <= 2<<20 {
		t.Fatalf("premise: fixture is %d bytes; it must exceed 2 MiB to show why the cap is not 2 MiB", len(body))
	}
	if int64(len(body)) > constants.MaxNonUploadRequestBodyBytes {
		t.Fatalf("a maximal legitimate update-sections body (%d bytes) exceeds MaxNonUploadRequestBodyBytes (%d)",
			len(body), constants.MaxNonUploadRequestBodyBytes)
	}

	h := realChain(t)
	req := httptest.NewRequest(http.MethodPost,
		"/projects/cncf/briefs/6ba7b810-9dad-11d1-80b4-00c04fd430c8/wizard/update-sections", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("a maximal legitimate update-sections body (%d bytes) was refused with 413", len(body))
	}
}

// TestNonUploadRoutes_AdmitBodyAtTheCap is the other side of #177: the smaller cap must not refuse
// a body that fits. A body of exactly MaxNonUploadRequestBodyBytes is sent to create-brief and
// must get anything BUT 413 (it is then refused further in, with no token and no database, which
// is fine — the size gate let it through).
func TestNonUploadRoutes_AdmitBodyAtTheCap(t *testing.T) {
	h := realChain(t)
	envelope := int64(len(`{"pad":""}`))
	body := io.MultiReader(
		strings.NewReader(`{"pad":"`),
		repeatReader('A', constants.MaxNonUploadRequestBodyBytes-envelope),
		strings.NewReader(`"}`),
	)
	req := httptest.NewRequest(http.MethodPost, "/projects/cncf/briefs", body)
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = constants.MaxNonUploadRequestBodyBytes
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("a body of exactly the non-upload cap (%d bytes) was refused with 413", constants.MaxNonUploadRequestBodyBytes)
	}
}
