// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package charts_test holds chart-level invariants that can't be expressed in the
// templates themselves. The central one is route/rule PARITY: the HTTPRoute selects
// this service's project-nested paths with a single RE2 regex, while the Heimdall
// RuleSet authorizes the SAME path set as an enumerated list of Traefik path
// patterns. If the two drift — a path the route forwards but the RuleSet does not
// authorize — that path reaches the service WITHOUT the campaign_manager FGA check
// (an unruled, unauthenticated bypass). Nothing but this test couples the two
// hand-maintained matchers, so it renders both with `helm template` and asserts a
// curated table of accepted/rejected paths matches IDENTICALLY in both matchers.
package charts_test

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// chartDir is the package directory, which IS the chart root (the test lives at the
// chart root so `helm template .` resolves without a repo-relative path.)
const chartDir = "."

// helmTemplate renders one template file of the chart and returns its YAML. It skips
// the test when helm is unavailable (local envs without helm) but FAILS on a real
// render error — a broken template must not be silently skipped.
func helmTemplate(t *testing.T, showOnly string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm not on PATH; skipping chart parity test: %v", err)
	}
	out, err := exec.Command("helm", "template", chartDir, "--show-only", showOnly).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s failed: %v\n%s", showOnly, err, out)
	}
	return string(out)
}

// extractRouteRegex pulls the single RegularExpression path-match value out of the
// rendered HTTPRoute and compiles it. The value line looks like:
//
//	value: ^/projects/[^/]+/(...)$
func extractRouteRegex(t *testing.T, httproute string) *regexp.Regexp {
	t.Helper()
	var raw string
	for _, line := range strings.Split(httproute, "\n") {
		s := strings.TrimSpace(line)
		// The project-nested selector is the only RE2 value anchored at /projects/.
		if strings.HasPrefix(s, "value:") && strings.Contains(s, "^/projects/") {
			raw = strings.TrimSpace(strings.TrimPrefix(s, "value:"))
			break
		}
	}
	if raw == "" {
		t.Fatalf("no RegularExpression /projects/ value found in rendered HTTPRoute:\n%s", httproute)
	}
	re, err := regexp.Compile(raw)
	if err != nil {
		t.Fatalf("route regex %q does not compile: %v", raw, err)
	}
	return re
}

// extractRulePatterns pulls the Traefik path patterns out of the rendered RuleSet.
// Only the project-nested rule matters for parity with the /projects/ route regex,
// so /campaigns, /_campaigns/, and the /_campaigns/openapi passthrough entries
// (which the route regex deliberately does NOT cover) are excluded.
func extractRulePatterns(t *testing.T, ruleset string) []string {
	t.Helper()
	var pats []string
	for _, line := range strings.Split(ruleset, "\n") {
		s := strings.TrimSpace(line)
		if !strings.HasPrefix(s, "- path:") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(s, "- path:"))
		if !strings.HasPrefix(p, "/projects/") {
			continue // /campaigns, /_campaigns/... are not part of the /projects/ regex
		}
		pats = append(pats, p)
	}
	if len(pats) == 0 {
		t.Fatalf("no /projects/ path patterns found in rendered RuleSet:\n%s", ruleset)
	}
	return pats
}

// ruleMatcher compiles a Traefik-style path pattern into a Go regexp. Traefik's
// matcher tokens used here:
//   - :name         a single path segment placeholder (no slash) — e.g. :projectId
//   - **            the free wildcard: ANY suffix, including zero segments and slashes
//   - *             a single path segment (no slash)
//
// Everything else is matched literally. The result is anchored (^…$) so a pattern
// matches a whole path, mirroring how Heimdall evaluates a rule entry.
func ruleMatcher(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	var b strings.Builder
	b.WriteString("^")
	segments := strings.Split(pattern, "/")
	for i, seg := range segments {
		if i > 0 {
			b.WriteString("/")
		}
		switch {
		case seg == "**":
			// Free wildcard: any suffix. Because a leading "/" was already written for
			// this position, allow it to also consume that slash+everything (so
			// "/briefs/**" matches "/briefs" itself, matching the enumerated bare-base
			// entry's intent — but we keep bare bases as their own patterns too).
			b.WriteString(".*")
		case seg == "*":
			b.WriteString("[^/]+")
		case strings.HasPrefix(seg, ":"):
			b.WriteString("[^/]+")
		default:
			b.WriteString(regexp.QuoteMeta(seg))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		t.Fatalf("rule pattern %q compiled to invalid regex %q: %v", pattern, b.String(), err)
	}
	return re
}

// anyRuleMatches reports whether ANY RuleSet entry authorizes the path (Heimdall
// authorizes a request if any rule entry matches).
func anyRuleMatches(matchers []*regexp.Regexp, path string) bool {
	for _, m := range matchers {
		if m.MatchString(path) {
			return true
		}
	}
	return false
}

// TestRouteRuleSetParity asserts every path the HTTPRoute regex forwards is also
// authorized by a RuleSet entry, and vice versa — the chart↔route parity invariant.
// A drift here is a security bug: a forwarded-but-unruled path skips the FGA check.
func TestRouteRuleSetParity(t *testing.T) {
	routeRe := extractRouteRegex(t, helmTemplate(t, "templates/httproute.yaml"))
	rulePats := extractRulePatterns(t, helmTemplate(t, "templates/ruleset.yaml"))
	ruleMatchers := make([]*regexp.Regexp, 0, len(rulePats))
	for _, p := range rulePats {
		ruleMatchers = append(ruleMatchers, ruleMatcher(t, p))
	}

	// Curated table: accepted paths MUST match both matchers; rejected paths MUST
	// match neither. The point is the equality routeMatch == ruleMatch on every row,
	// PLUS confirming accepted rows are genuinely matched (not both-false).
	cases := []struct {
		path   string
		accept bool
	}{
		// --- accepted: connection CRUD + test + set-credential, every provider ---
		{"/projects/p1/connection-google-ads", true},
		{"/projects/p1/connection-google-ads/test", true},
		{"/projects/p1/connection-google-ads/set-credential", true},
		{"/projects/abc-123/connection-linkedin-ads", true},
		{"/projects/p1/connection-meta-ads/test", true},
		{"/projects/p1/connection-reddit-ads/set-credential", true},
		{"/projects/p1/connection-twitter-ads", true},
		{"/projects/p1/connection-microsoft-ads/test", true},
		{"/projects/p1/connection-hubspot/set-credential", true},
		// --- accepted: briefs/jobs base + descendants ---
		{"/projects/p1/briefs", true},
		{"/projects/p1/briefs/b-42", true},
		{"/projects/p1/briefs/b-42/campaigns/c-9", true},
		{"/projects/p1/jobs", true},
		{"/projects/p1/jobs/j-1/status", true},
		// --- accepted: hubspot base + descendants ---
		{"/projects/p1/hubspot", true},
		{"/projects/p1/hubspot/utm", true},
		// --- accepted: per-provider metrics + google-ads keywords/audience ---
		{"/projects/p1/google-ads/metrics", true},
		{"/projects/p1/twitter-ads/metrics", true},
		{"/projects/p1/google-ads/keywords", true},
		{"/projects/p1/google-ads/audience", true},

		// --- rejected: another service's project subpaths (project-service owns these) ---
		{"/projects/p1", false},
		{"/projects/p1/committees", false},
		{"/projects/p1/meetings/m-1", false},
		// --- rejected: unknown provider / unknown connection action ---
		{"/projects/p1/connection-tiktok-ads", false},
		{"/projects/p1/connection-google-ads/delete", false},
		// --- rejected: metrics/keywords on the wrong provider ---
		{"/projects/p1/meta-ads/keywords", false},
		{"/projects/p1/linkedin-ads/audience", false},
		{"/projects/p1/hubspot-ads/metrics", false},
		// --- rejected: missing projectId segment / not project-nested ---
		{"/projects//briefs", false},
		{"/briefs/b-1", false},
		{"/campaigns", false}, // routed, but by the /campaigns rule, not the /projects/ regex
	}

	for _, tc := range cases {
		routeMatch := routeRe.MatchString(tc.path)
		ruleMatch := anyRuleMatches(ruleMatchers, tc.path)
		if routeMatch != ruleMatch {
			t.Errorf("PARITY VIOLATION for %q: HTTPRoute match=%v but RuleSet match=%v — a forwarded path that is (un)authorized inconsistently",
				tc.path, routeMatch, ruleMatch)
		}
		if routeMatch != tc.accept {
			t.Errorf("HTTPRoute match for %q = %v, want %v", tc.path, routeMatch, tc.accept)
		}
		if ruleMatch != tc.accept {
			t.Errorf("RuleSet match for %q = %v, want %v", tc.path, ruleMatch, tc.accept)
		}
	}
}
