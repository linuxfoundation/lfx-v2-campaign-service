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

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// bodyRoute is one body-bearing operation read from the OpenAPI document the service serves.
type bodyRoute struct {
	method string
	path   string // concrete: every {param} replaced with the spec's own example value
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
			out = append(out, bodyRoute{method: strings.ToUpper(method), path: concrete})
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
// 42 MiB cap, so a ~2 MiB body on create-brief (whose Any fields accept arbitrary JSON) was read
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

	// Comfortably over the non-upload cap and far under the upload one, so the test fails
	// against a single global 42 MiB cap and passes only when the cap is per-route.
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

	covered := 0
	for _, rt := range routes {
		if strings.HasSuffix(rt.path, "/creative-assets") && rt.method == http.MethodPost {
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
