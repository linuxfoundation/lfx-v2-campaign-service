// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package charts_test holds chart-level invariants that can't be expressed in the
// templates themselves. The central one is route/rule PARITY: the HTTPRoute selects
// this service's project-nested paths with a single RE2 regex, while the Heimdall
// RuleSet authorizes the SAME path set as an enumerated list of Traefik path
// patterns. If the two drift — a path the route forwards but the RuleSet does not
// authorize — that path reaches the service WITHOUT the campaign_manager FGA check
// (an unruled, unauthenticated bypass). Nothing but this test couples the two
// hand-maintained matchers, so it renders both with `helm template` and checks them
// two ways: (1) a curated accepted/rejected table both matchers must agree on, and
// (2) a WITNESS derivation that couples the assertions to the matchers' own content —
// concrete example paths enumerated from the route regex's AST must each be ruled,
// and a witness built from each RuleSet pattern must match the route. The witness
// check is what catches a ONE-SIDED matcher edit (e.g. adding `tiktok-ads/metrics`
// to only the route regex): a static table can miss it, but an enumerated witness
// for the new alternative will match the route and not the RuleSet, failing parity.
package charts_test

import (
	"os/exec"
	"regexp"
	"regexp/syntax"
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

// extractRouteRegexRaw pulls the single RegularExpression path-match value out of
// the rendered HTTPRoute as its raw string. The value line looks like:
//
//	value: ^/projects/[^/]+/(...)$
func extractRouteRegexRaw(t *testing.T, httproute string) string {
	t.Helper()
	for _, line := range strings.Split(httproute, "\n") {
		s := strings.TrimSpace(line)
		// The project-nested selector is the only RE2 value anchored at /projects/.
		if strings.HasPrefix(s, "value:") && strings.Contains(s, "^/projects/") {
			return strings.TrimSpace(strings.TrimPrefix(s, "value:"))
		}
	}
	t.Fatalf("no RegularExpression /projects/ value found in rendered HTTPRoute:\n%s", httproute)
	return ""
}

// extractRouteRegex pulls the RegularExpression path-match value and compiles it.
func extractRouteRegex(t *testing.T, httproute string) *regexp.Regexp {
	t.Helper()
	raw := extractRouteRegexRaw(t, httproute)
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

// enumerateMatches returns a bounded set of concrete strings that the compiled
// regex fully matches, by walking the parsed regexp AST. It expands alternations
// (every branch) and concatenations (cartesian across sub-parts), collapses the
// open-ended pieces the route uses — `[^/]+` (a projectId segment) and `.*` (a
// free descendant suffix) — to fixed witness literals, and treats `?`/star/plus as
// "zero or one representative occurrence". The point is not to enumerate the
// infinite language but to emit at least one witness per ALTERNATION LEAF, so a new
// branch added to the regex necessarily yields a new witness path — which the
// parity assertion then requires the RuleSet to also match. The cap guards against
// a combinatorial blow-up if the regex ever grows many independent option groups.
func enumerateMatches(t *testing.T, pattern string) []string {
	t.Helper()
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		t.Fatalf("cannot parse route regex for enumeration: %v", err)
	}
	const cap = 512
	out := expand(re.Simplify())
	if len(out) > cap {
		t.Fatalf("route regex enumerated to %d witnesses (> cap %d) — the regex likely grew independent option groups; raise the cap or curate witnesses", len(out), cap)
	}
	// Drop the anchors that OpLiteral can't carry; MatchString re-applies them.
	for i, s := range out {
		out[i] = strings.TrimSuffix(strings.TrimPrefix(s, "^"), "$")
	}
	return out
}

// expand returns the set of representative match strings for one regexp AST node.
func expand(re *syntax.Regexp) []string {
	switch re.Op {
	case syntax.OpLiteral:
		return []string{string(re.Rune)}
	case syntax.OpCharClass:
		// The only char classes this regex uses are `[^/]` (a path segment char) and
		// implicit ones; a single representative char suffices for a witness segment.
		return []string{"x"}
	case syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return []string{"x"}
	case syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText, syntax.OpEmptyMatch:
		return []string{""}
	case syntax.OpCapture:
		return expand(re.Sub[0])
	case syntax.OpConcat:
		acc := []string{""}
		for _, sub := range re.Sub {
			parts := expand(sub)
			next := make([]string, 0, len(acc)*len(parts))
			for _, a := range acc {
				for _, p := range parts {
					next = append(next, a+p)
				}
			}
			acc = next
		}
		return acc
	case syntax.OpAlternate:
		var out []string
		for _, sub := range re.Sub {
			out = append(out, expand(sub)...)
		}
		return out
	case syntax.OpQuest, syntax.OpStar:
		// zero OR one representative occurrence.
		out := []string{""}
		out = append(out, expand(re.Sub[0])...)
		return out
	case syntax.OpPlus:
		// one representative occurrence (a `[^/]+` segment or a `.*`-derived suffix).
		return expand(re.Sub[0])
	default:
		// Fall back to a single opaque witness so an unexpected op doesn't silently
		// drop a branch; the caller's assertions will surface any mismatch.
		return []string{"x"}
	}
}

// ruleWitness turns a RuleSet path pattern into a single concrete witness path by
// substituting each token with a representative value: `:name`/`*` -> one segment,
// `**` -> a two-segment descendant (so it also proves the "any-depth" intent).
func ruleWitness(pattern string) string {
	segs := strings.Split(pattern, "/")
	for i, seg := range segs {
		switch {
		case seg == "**":
			segs[i] = "w1/w2"
		case seg == "*" || strings.HasPrefix(seg, ":"):
			segs[i] = "p1"
		}
	}
	return strings.Join(segs, "/")
}

// TestRouteRuleSetParityWitnesses couples the parity assertion to the matchers' OWN
// content, defeating a one-sided matcher edit that a static table would miss:
//   - every concrete path enumerated from the ROUTE regex's alternation leaves must
//     be authorized by some RuleSet entry (a route-only new branch fails here);
//   - a witness built from every RULESET pattern must match the route regex (a
//     RuleSet-only new entry fails here).
func TestRouteRuleSetParityWitnesses(t *testing.T) {
	routeValue := extractRouteRegexRaw(t, helmTemplate(t, "templates/httproute.yaml"))
	routeRe := regexp.MustCompile(routeValue)
	rulePats := extractRulePatterns(t, helmTemplate(t, "templates/ruleset.yaml"))
	ruleMatchers := make([]*regexp.Regexp, 0, len(rulePats))
	for _, p := range rulePats {
		ruleMatchers = append(ruleMatchers, ruleMatcher(t, p))
	}

	// Direction 1: every route-regex leaf witness must be ruled.
	witnesses := enumerateMatches(t, routeValue)
	if len(witnesses) == 0 {
		t.Fatal("route regex enumerated to zero witnesses")
	}
	for _, w := range witnesses {
		if !routeRe.MatchString(w) {
			t.Fatalf("internal error: enumerated witness %q does not match its own route regex", w)
		}
		if !anyRuleMatches(ruleMatchers, w) {
			t.Errorf("route regex forwards %q but NO RuleSet entry authorizes it — one-sided route edit (unauthenticated bypass)", w)
		}
	}

	// Direction 2: a witness from every RuleSet pattern must be forwarded by the route.
	for _, p := range rulePats {
		w := ruleWitness(p)
		if !routeRe.MatchString(w) {
			t.Errorf("RuleSet authorizes %q (witness %q) but the route regex does NOT forward it — one-sided RuleSet edit (a dead rule, or a route gap)", p, w)
		}
	}
}
