// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMetricsIsExcludedFromOpenAPI pins the hard rule that /metrics must not
// appear in the PUBLISHED OpenAPI documents, exactly as /livez and /readyz do not.
//
// Those two are Goa methods kept out of the spec by Meta("swagger:generate",
// "false"). /metrics takes the stronger route — it is not declared in design/ at
// all, so it cannot be published even if someone forgets an annotation. This test
// asserts the OUTCOME rather than the mechanism, so it keeps holding if /metrics
// is ever moved into the design with the annotation instead.
//
// The generated specs are checked in, so this reads them directly. It also
// asserts /livez is absent, which proves the assertion has teeth: if the spec
// files were unreadable or empty, a bare "/metrics is absent" check would pass
// vacuously.
func TestMetricsIsExcludedFromOpenAPI(t *testing.T) {
	specs, err := filepath.Glob(filepath.Join("..", "..", "gen", "http", "openapi*.json"))
	if err != nil {
		t.Fatalf("glob openapi specs: %v", err)
	}
	yamls, err := filepath.Glob(filepath.Join("..", "..", "gen", "http", "openapi*.yaml"))
	if err != nil {
		t.Fatalf("glob openapi specs: %v", err)
	}
	specs = append(specs, yamls...)
	if len(specs) == 0 {
		t.Fatal("no generated OpenAPI documents found; run `make apigen`")
	}

	for _, path := range specs {
		body, rerr := os.ReadFile(path) //nolint:gosec // fixed, repo-relative generated spec path
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		text := string(body)
		if len(strings.TrimSpace(text)) == 0 {
			t.Fatalf("%s is empty; the exclusion assertions below would pass vacuously", path)
		}

		// The control: an endpoint that EXISTS and is published, proving this file
		// really is the served API surface and the absence checks mean something.
		if !strings.Contains(text, "/projects/") {
			t.Fatalf("%s contains no /projects/ paths; it does not look like the published API surface", path)
		}

		// Match the path as a whole KEY, not as a substring. The service legitimately
		// publishes campaign- and brief-scoped metrics reads whose paths END in
		// "/metrics" (…/campaigns/{id}/metrics), so a bare substring search reports
		// those and never fails for the reason this test exists.
		for _, bad := range []string{"/metrics", "/livez", "/readyz"} {
			if publishesPath(text, bad) {
				t.Errorf("%s publishes the top-level path %q; it must be excluded from the OpenAPI documents", path, bad)
			}
		}
	}
}

// publishesPath reports whether the OpenAPI document declares `path` as a
// top-level path item. Both encodings are checked because the generator emits
// JSON (`"/metrics": {`) and YAML (`    /metrics:`) forms, and in each the path
// must be the WHOLE key — `/projects/{id}/briefs/{id}/metrics` must not match
// `/metrics`.
func publishesPath(doc, path string) bool {
	if strings.Contains(doc, `"`+path+`":`) {
		return true
	}
	for _, line := range strings.Split(doc, "\n") {
		if strings.TrimSpace(line) == path+":" {
			return true
		}
	}
	return false
}

// TestScrapeAndProbesAreNotTraced pins that the machine-polled endpoints are
// excluded from tracing.
//
// /metrics belongs with the health probes for the same reason they are excluded: a
// Prometheus collector scrapes on a fixed cadence forever, so tracing it produces a
// steady stream of spans describing no user-visible work, burying the request traces
// someone is actually reading. Leaving it in was the drift this pins.
func TestScrapeAndProbesAreNotTraced(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/metrics", false},
		{"/livez", false},
		{"/readyz", false},
		{"/healthz", false},
		// Real request paths must still be traced.
		{"/projects/p1/campaign-briefs", true},
		{"/projects/p1/campaigns/c1/metrics", true},
		{"/", true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := shouldTrace(tc.path); got != tc.want {
				t.Errorf("shouldTrace(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
