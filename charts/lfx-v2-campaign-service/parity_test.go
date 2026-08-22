// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package charts_test holds chart-level invariants that can't be expressed in the
// templates themselves. The central one is route/rule PARITY: the HTTPRoute selects
// this service's project-nested paths with a single RE2 regex, while the Heimdall
// RuleSet authorizes the SAME path set as an enumerated list of Traefik path
// patterns. If the two drift — a path the route forwards but the RuleSet does not
// authorize — Heimdall is default-deny (a request matching no rule is REJECTED, per
// specs/002-deployment-config/research.md), so that path becomes UNREACHABLE through
// the gateway rather than an unauthenticated bypass: the campaign_manager FGA check
// never gets the chance to run because Heimdall rejects the request first. (The
// inverse drift — a path the RuleSet authorizes but the route does not forward — is
// dead config.) Nothing but this test couples the two
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
	"os"
	"os/exec"
	"path/filepath"
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

// projectAPIRuleID is the Heimdall rule id whose paths must be in parity with the
// route regex. Scoping extraction to THIS rule (not "any /projects/ path anywhere")
// is the security point: the invariant is specifically that each forwarded path is
// gated on campaign_manager for project:{projectId}. A path moved into an allow_all,
// deny_all, or differently-scoped rule must FAIL parity, not silently satisfy it.
const projectAPIRuleID = "rule:lfx:lfx-v2-campaign-service:project-api"

// ruleBlock isolates one rendered Heimdall rule (from its `- id: "<id>"` line up to
// the next `- id:` or EOF) so path/authorizer extraction is scoped to a SINGLE rule.
func ruleBlock(t *testing.T, ruleset, ruleID string) string {
	t.Helper()
	lines := strings.Split(ruleset, "\n")
	start := -1
	for i, line := range lines {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "- id:") && strings.Contains(s, ruleID) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("rule %q not found in rendered RuleSet:\n%s", ruleID, ruleset)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "- id:") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// extractRulePatterns pulls the Traefik path patterns out of ONLY the project-api
// rule block. /campaigns, /_campaigns/, and the openapi passthrough entries live in
// OTHER rules (a deny_all placeholder and an allow_all openapi rule) and the route
// regex deliberately does not cover them, so scoping to project-api both excludes
// them and, crucially, ensures a path is counted as "authorized" only when it is
// under the campaign_manager rule — not any unrelated rule.
func extractRulePatterns(t *testing.T, ruleset string) []string {
	t.Helper()
	block := ruleBlock(t, ruleset, projectAPIRuleID)
	var pats []string
	for _, line := range strings.Split(block, "\n") {
		s := strings.TrimSpace(line)
		if !strings.HasPrefix(s, "- path:") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(s, "- path:"))
		if !strings.HasPrefix(p, "/projects/") {
			continue
		}
		pats = append(pats, p)
	}
	if len(pats) == 0 {
		t.Fatalf("no /projects/ path patterns found in the %s rule:\n%s", projectAPIRuleID, block)
	}
	return pats
}

// assertProjectAPIAuthz verifies the project-api rule actually enforces the claimed
// security invariant: an openfga_check authorizer with relation campaign_manager on
// object project:{projectId}. Without this, the path-parity checks could pass on a
// rule that was silently downgraded to allow_all/deny_all or re-scoped to a different
// relation/object — the exact regression the parity test exists to catch.
//
// LFXV2-3324 introduced project_slug_resolver_contextualizer as an upstream
// contextualizer that resolves project slug to UID; the object field now reads the
// resolved .Outputs.project_slug_resolver_contextualizer.uid, not the raw capture.
// This function must assert the contextualizer runs AND that the object field
// specifically uses the resolved UID, not merely that the raw capture appears
// somewhere in the block (which would still pass even if someone accidentally reverted
// the object field back to the slug).
//
// It also pins the second, UUID-branch openfga_check (raw Captures.projectId object,
// no contextualizer/resolver involved) and the exact-negation relationship between the
// two branches' if: guards. Without this, a future edit could drop or mis-guard the
// UUID-branch check entirely — every UUID-form :projectId (the historical
// migration-000003 rows) would then fall through both slug-branch guards and reach no
// openfga_check at all, an authz bypass that the slug-only assertions above would not
// catch.
func assertProjectAPIAuthz(t *testing.T, ruleset string) {
	t.Helper()
	block := ruleBlock(t, ruleset, projectAPIRuleID)
	if got := strings.Count(block, "authorizer: openfga_check"); got != 2 {
		t.Errorf("%s rule must have exactly 2 openfga_check authorizers (one per UUID/slug branch), got %d:\n%s", projectAPIRuleID, got, block)
	}
	if got := strings.Count(block, "relation: campaign_manager"); got != 2 {
		t.Errorf("%s rule must gate both branches on relation campaign_manager, got %d occurrences:\n%s", projectAPIRuleID, got, block)
	}
	// The contextualizer must be present to resolve the project slug to a UID.
	if !strings.Contains(block, "contextualizer: project_slug_resolver_contextualizer") {
		t.Errorf("%s rule must include project_slug_resolver_contextualizer to resolve project slug to UID:\n%s", projectAPIRuleID, block)
	}

	// Pin PAIRING, not just presence: an inverted-branch edit (resolved-UID object
	// under the positive/UUID guard, raw-capture object under the negative/slug guard)
	// is a total lockout, but would still satisfy independent substring checks for
	// "two openfga_check", "two campaign_manager", both objects, and both guards. Each
	// openfga_check ENTRY must carry its own matching guard+object pair.
	entries := openfgaCheckEntries(t, block)
	if len(entries) != 2 {
		t.Fatalf("%s rule must have exactly 2 openfga_check entries to pin, got %d:\n%s", projectAPIRuleID, len(entries), block)
	}
	var sawSlugBranch, sawUUIDBranch bool
	for _, e := range entries {
		negative := strings.Contains(e, `if: '!Request.URL.Captures.projectId.matches(`)
		positive := !negative && strings.Contains(e, `if: 'Request.URL.Captures.projectId.matches(`)
		resolvedObject := strings.Contains(e, "object: \"project:") && strings.Contains(e, ".Outputs.project_slug_resolver_contextualizer.uid")
		rawObject := strings.Contains(e, "object: \"project:{{- .Request.URL.Captures.projectId -}}\"")
		switch {
		case negative:
			sawSlugBranch = true
			if !resolvedObject {
				t.Errorf("%s rule's negative-guard (slug branch) openfga_check must scope object to the resolved .Outputs.project_slug_resolver_contextualizer.uid, not the raw capture:\n%s", projectAPIRuleID, e)
			}
			if rawObject {
				t.Errorf("%s rule's negative-guard (slug branch) openfga_check must NOT use the raw Captures.projectId object (that belongs to the UUID branch):\n%s", projectAPIRuleID, e)
			}
		case positive:
			sawUUIDBranch = true
			if !rawObject {
				t.Errorf("%s rule's positive-guard (UUID branch) openfga_check must scope object to the raw project:{{- .Request.URL.Captures.projectId -}} capture, not the resolver's Outputs:\n%s", projectAPIRuleID, e)
			}
			if resolvedObject {
				t.Errorf("%s rule's positive-guard (UUID branch) openfga_check must NOT reference the resolver's Outputs (undocumented/unsafe when the contextualizer is skipped):\n%s", projectAPIRuleID, e)
			}
		default:
			t.Errorf("%s rule's openfga_check entry must have an if: guard that is either the negative (slug) or positive (UUID) Captures.projectId match:\n%s", projectAPIRuleID, e)
		}
	}
	if !sawSlugBranch {
		t.Errorf("%s rule must have a negative if: guard (slug branch) on Captures.projectId:\n%s", projectAPIRuleID, block)
	}
	if !sawUUIDBranch {
		t.Errorf("%s rule must have a positive if: guard (UUID branch) on Captures.projectId:\n%s", projectAPIRuleID, block)
	}

	// The resolver contextualizer itself must carry the negative (slug) guard — a
	// contextualizer left unguarded, or guarded on the wrong condition, would run the
	// slug-only resolver against a UUID capture (404, fail-closed lockout) or skip
	// resolution for an actual slug (empty Outputs feeding the slug-branch object).
	if !strings.Contains(block, `contextualizer: project_slug_resolver_contextualizer`) ||
		!strings.Contains(block, "contextualizer: project_slug_resolver_contextualizer\n          if: '!Request.URL.Captures.projectId.matches(") {
		t.Errorf("%s rule's project_slug_resolver_contextualizer must carry the negative (slug) if: guard on Captures.projectId:\n%s", projectAPIRuleID, block)
	}
}

// openfgaCheckEntries splits a rule block into its individual `- authorizer:
// openfga_check` execute-step entries, each running from its `- authorizer:` line up
// to (but not including) the next execute-step line (`- authorizer:` or
// `- contextualizer:`) or the end of the block. Scoping assertions to a SINGLE entry
// (rather than the whole block) is what lets pairing — not just presence — be pinned:
// each entry's own if: guard must match its own object.
func openfgaCheckEntries(t *testing.T, block string) []string {
	t.Helper()
	lines := strings.Split(block, "\n")
	var starts []int
	for i, line := range lines {
		s := strings.TrimSpace(line)
		if s == "- authorizer: openfga_check" {
			starts = append(starts, i)
		}
	}
	var entries []string
	for _, start := range starts {
		end := len(lines)
		for i := start + 1; i < len(lines); i++ {
			s := strings.TrimSpace(lines[i])
			if strings.HasPrefix(s, "- authorizer:") || strings.HasPrefix(s, "- contextualizer:") || strings.HasPrefix(s, "- authenticator:") {
				end = i
				break
			}
		}
		entries = append(entries, strings.Join(lines[start:end], "\n"))
	}
	return entries
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

// TestProjectAPIRuleEnforcesCampaignManager asserts the project-api rule enforces the
// exact security invariant the parity tests assume: two openfga_check entries on
// relation campaign_manager, each paired with the correct object (the resolved UID
// for the slug branch, the raw capture for the UUID branch). Named separately so a
// downgrade of the rule to allow_all/deny_all — or a re-scope to a different
// relation/object — fails loudly even if the path lists still line up.
func TestProjectAPIRuleEnforcesCampaignManager(t *testing.T) {
	assertProjectAPIAuthz(t, helmTemplate(t, "templates/ruleset.yaml"))
}

// TestRouteRuleSetParity asserts every path the HTTPRoute regex forwards is also
// authorized by a RuleSet entry, and vice versa — the chart↔route parity invariant.
// A drift here is a security bug: a forwarded-but-unruled path skips the FGA check.
func TestRouteRuleSetParity(t *testing.T) {
	routeRe := extractRouteRegex(t, helmTemplate(t, "templates/httproute.yaml"))
	ruleset := helmTemplate(t, "templates/ruleset.yaml")
	// The paths are only meaningfully "authorized" if the project-api rule still gates
	// on campaign_manager for the correct object per branch (resolved UID for slug,
	// raw capture for UUID); assert that before trusting parity.
	assertProjectAPIAuthz(t, ruleset)
	rulePats := extractRulePatterns(t, ruleset)
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
		// Account discovery. Unlike /test and /set-credential this is NOT shared by the
		// whole connection-* family — only the providers whose dispatcher implements
		// AccountLister have it, so each needs its own route/rule entry on both sides.
		// The reddit/twitter rows below pin that the alternation was not widened to every
		// provider by accident.
		{"/projects/p1/connection-google-ads/accounts", true},
		{"/projects/p1/connection-meta-ads/accounts", true},
		{"/projects/p1/connection-linkedin-ads/accounts", true},
		{"/projects/p1/connection-microsoft-ads/accounts", true},
		// HubSpot's extra sub-path is /emails, not /accounts (LFXV2-3197): the connection is
		// already portal-scoped by its token, so there is no account to discover — the choice
		// is which marketing email a campaign clones. It gets its own branch for that reason,
		// and the two negative rows below pin that neither path leaked into the other's
		// providers.
		{"/projects/p1/connection-hubspot/emails", true},
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
		// The campaign status-toggle (LFXV2-2806) is a deeper subroute under a campaign; it
		// inherits the `briefs(/.*)?` HTTPRoute match and the `/briefs/**` campaign_manager
		// rule. This row pins that coverage so a future narrowing of the briefs match/rule
		// can't leave the /status endpoint routed-but-unauthorized (or unreachable).
		{"/projects/p1/briefs/b-42/campaigns/c-9/status", true},
		// The campaign metrics read (LFXV2-3001) is the same shape as /status above: a
		// deeper subroute under a campaign, inheriting the `briefs(/.*)?` HTTPRoute match
		// and the `/briefs/**` campaign_manager rule rather than a separate route/rule
		// entry. This row pins that coverage the same way.
		{"/projects/p1/briefs/b-42/campaigns/c-9/metrics", true},
		// The campaign settings readback (LFXV2-3067) is the same shape again — a deeper
		// subroute under a campaign, inheriting the same match and rule rather than adding
		// its own. Pinned here for the same reason: a narrowing that unroutes it would
		// otherwise be caught by nothing, and an unroutable read is indistinguishable from
		// a platform that cannot be reached.
		{"/projects/p1/briefs/b-42/campaigns/c-9/settings", true},
		// campaign_audiences (LFXV2-2783) is subordinate to a brief, so it inherits both
		// the HTTPRoute `briefs(/.*)?` match and the Heimdall `/briefs/**` campaign_manager
		// rule — no separate route/rule entry. These rows pin that coverage so a future
		// narrowing of the briefs match/rule can't silently unroute or de-authorize it.
		{"/projects/p1/briefs/b-42/audiences", true},
		{"/projects/p1/briefs/b-42/audiences/a-9", true},
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
		// --- accepted: event-page pre-fill (LFXV2-3043) ---
		// A SIBLING of /briefs, not a descendant, so unlike /status and /metrics above it
		// inherits nothing: it needs its own alternation branch in the HTTPRoute regex AND
		// its own RuleSet entry. This row is what fails if a future edit adds only one.
		{"/projects/p1/fetch-event-url", true},

		// --- rejected: another service's project subpaths (project-service owns these) ---
		{"/projects/p1", false},
		{"/projects/p1/committees", false},
		{"/projects/p1/meetings/m-1", false},
		// --- rejected: unknown provider / unknown connection action ---
		{"/projects/p1/connection-tiktok-ads", false},
		{"/projects/p1/connection-google-ads/delete", false},
		// reddit-ads and twitter-ads have NO account discovery: neither platform client has
		// a ListAdAccounts, so their dispatchers do not implement AccountLister and the
		// endpoint does not exist. Admitting the path would route a request the service
		// answers with a 400 by construction. linkedin-ads and microsoft-ads used to sit
		// here for the same reason and now have it -- these two rows are what fails if a
		// future edit collapses the discovery branch back into the shared alternation
		// before their clients grow one.
		{"/projects/p1/connection-reddit-ads/accounts", false},
		{"/projects/p1/connection-twitter-ads/accounts", false},
		{"/projects/p1/connection-hubspot/accounts", false},
		{"/projects/p1/connection-google-ads/emails", false},
		// --- rejected: metrics/keywords on the wrong provider ---
		{"/projects/p1/meta-ads/keywords", false},
		{"/projects/p1/linkedin-ads/audience", false},
		{"/projects/p1/hubspot-ads/metrics", false},
		// The branch is an exact alternative, not a prefix: nothing hangs off it.
		{"/projects/p1/fetch-event-url/anything", false},
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
	ruleset := helmTemplate(t, "templates/ruleset.yaml")
	assertProjectAPIAuthz(t, ruleset)
	rulePats := extractRulePatterns(t, ruleset)
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
			t.Errorf("route regex forwards %q but NO RuleSet entry authorizes it — one-sided route edit (Heimdall default-deny makes this path UNREACHABLE through the gateway)", w)
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

// TestDeploymentUsesRecreateStrategy pins the rollout strategy, because this service runs
// its own schema migrations at boot and migration 000014 is BACKWARD-INCOMPATIBLE: it drops
// UNIQUE (brief_id, platform), after which the previous release's bare
// `ON CONFLICT (brief_id, platform)` matches no index and errors on every dispatch claim.
//
// Kubernetes' default RollingUpdate surges the new pod BEFORE terminating the old one
// (maxSurge 25% rounds up to 1), so the new pod would migrate the shared database while the
// old pod still serves writes against it. Recreate orders it the other way, so the
// incompatible schema is never live under the old code.
//
// Why this is pinned rather than left to convention: 000013 and 000014 cannot be staged
// apart to remove the need for it. Verified on PostgreSQL 16.10 — with the drop deferred,
// the old full constraint still covers soft-deleted rows, so a re-dispatch after delete is
// SILENTLY swallowed by ON CONFLICT DO NOTHING (RowsAffected 0, read back as "already
// claimed"). So the ordering guarantee has to come from the rollout strategy, and a future
// edit that "restores the default" would quietly reopen the window.
func TestDeploymentUsesRecreateStrategy(t *testing.T) {
	deployment := helmTemplate(t, "templates/deployment.yaml")

	// Strip comment lines before asserting. The template's own comments explain WHY
	// RollingUpdate is wrong here, so a naive substring search over the raw render would
	// match the prose rather than the setting.
	var yamlOnly strings.Builder
	for _, line := range strings.Split(deployment, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			yamlOnly.WriteString(line)
			yamlOnly.WriteString("\n")
		}
	}
	rendered := yamlOnly.String()

	if !strings.Contains(rendered, "type: Recreate") {
		t.Errorf("deployment must set `strategy.type: Recreate`; rendered chart does not.\n"+
			"RollingUpdate surges the new pod before terminating the old one, letting the previous "+
			"release run against the post-000014 schema where its bare ON CONFLICT (brief_id, platform) "+
			"fails on every dispatch claim.\nrendered:\n%s", rendered)
	}
	if strings.Contains(rendered, "RollingUpdate") {
		t.Errorf("deployment must NOT use RollingUpdate while a backward-incompatible migration "+
			"(000014, DROP CONSTRAINT) ships in this release.\nrendered:\n%s", rendered)
	}

	// The Recreate flip is only deployable via ArgoCD if the Deployment carries
	// Replace=true: an existing object flipped from the RollingUpdate default keeps an
	// API-server-defaulted strategy.rollingUpdate block that server-side apply will not
	// strip, and rollingUpdate may not coexist with type: Recreate, so a merge/SSA sync is
	// rejected. A full replace discards the orphaned field. Pinning it here keeps the two
	// settings from drifting apart -- Recreate without Replace=true reintroduces the
	// "may not be specified when strategy type is 'Recreate'" cutover failure.
	if !strings.Contains(rendered, "argocd.argoproj.io/sync-options: Replace=true") {
		t.Errorf("deployment must set annotation `argocd.argoproj.io/sync-options: Replace=true` "+
			"so ArgoCD replaces rather than merge-patches the object; otherwise flipping an "+
			"existing RollingUpdate Deployment to Recreate is rejected because the defaulted "+
			"rollingUpdate block cannot coexist with type: Recreate.\nrendered:\n%s", rendered)
	}
}

// TestDeploymentMergesReplaceIntoOperatorSyncOptions pins the hardening half of the
// Replace=true annotation: it is MERGED into an operator-supplied
// argocd.argoproj.io/sync-options rather than emitted as a bare literal. The default render
// in TestDeploymentUsesRecreateStrategy only proves Replace=true is present when
// .Values.annotations is empty; it never exercises the collision path, which is the whole
// point of the merge. Without this a refactor back to a bare literal would keep that test
// green while silently reintroducing the duplicate-map-key drop (last-wins) that reopens the
// forbidden-cutover failure.
func TestDeploymentMergesReplaceIntoOperatorSyncOptions(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm not on PATH; skipping chart guard test: %v", err)
	}

	// An operator setting sync-options for another reason must keep BOTH their option and
	// Replace=true under ONE key -- a duplicate map key is last-wins and would drop one.
	out, err := exec.Command("helm", "template", chartDir,
		"--show-only", "templates/deployment.yaml",
		"--set", `annotations.argocd\.argoproj\.io/sync-options=Prune=false`,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed with an operator sync-options override: %v\n%s", err, out)
	}
	rendered := string(out)
	if !strings.Contains(rendered, "argocd.argoproj.io/sync-options: Prune=false,Replace=true") {
		t.Errorf("operator sync-options must be preserved AND carry Replace=true; want "+
			"`Prune=false,Replace=true`.\nrendered:\n%s", rendered)
	}
	if n := strings.Count(rendered, "argocd.argoproj.io/sync-options:"); n != 1 {
		t.Errorf("sync-options must render as ONE merged map key, got %d occurrences "+
			"(a duplicate key is last-wins and drops Replace=true).\nrendered:\n%s", n, rendered)
	}

	// Idempotent: an operator who already set Replace=true gets no duplicated token.
	out, err = exec.Command("helm", "template", chartDir,
		"--show-only", "templates/deployment.yaml",
		"--set", `annotations.argocd\.argoproj\.io/sync-options=Replace=true`,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed with a Replace=true override: %v\n%s", err, out)
	}
	if got := string(out); !strings.Contains(got, "argocd.argoproj.io/sync-options: Replace=true") ||
		strings.Contains(got, "Replace=true,Replace=true") {
		t.Errorf("Replace=true merge must be idempotent; want a single Replace=true token.\nrendered:\n%s", got)
	}
}

// TestEveryConfiguredEnvVarIsWiredInTheChart is a second parity invariant: an environment
// variable the SERVICE reads must be one the CHART actually injects.
//
// The failure mode is silent and total. Snowflake shipped this way: five SNOWFLAKE_* vars were
// read by config.go, none appeared in values.yaml, so on every deployed environment the
// warehouse lookup was disabled, audience groups 5 and 7 never built, and each audience recorded
// "no past editions resolved" — which reads as "this is a first-time event" rather than "this
// feature is not wired". Nothing failed; the feature was simply absent.
//
// Deliberately one-directional. It asserts code ⊆ chart, not the reverse: the chart legitimately
// sets OTEL_* and other vars consumed by libraries rather than by pkg/constants, so requiring
// chart ⊆ code would fail on correct config.
func TestEveryConfiguredEnvVarIsWiredInTheChart(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "pkg", "constants", "constants.go"))
	if err != nil {
		t.Fatalf("read constants: %v", err)
	}
	// Every `EnvSomething = "NAME"` constant is a var the service reads from the environment.
	names := regexp.MustCompile(`Env[A-Za-z0-9_]*\s*=\s*"([A-Z0-9_]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(names) == 0 {
		t.Fatal("found no Env* constants; the extraction regex is stale")
	}

	rendered := helmTemplate(t, "templates/deployment.yaml")

	// Vars that are deliberately NOT chart-injected, each with the reason it is exempt.
	exempt := map[string]string{
		// Local-development escape hatches. Wiring these would make it possible to disable auth
		// in a deployed environment by editing values.yaml, which is exactly what must not be
		// one edit away.
		"JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL": "local dev only; deliberately not deployable",

		// Injecting this would DEFEAT it. It is one discriminator that lets auth.New refuse
		// the mock principal in a deployment: the same override that would enable the bypass
		// must not also be able to conceal the cluster. A chart entry — even one rendering
		// the right value — would put "unset it" back within reach of whoever set the bypass.
		//
		// Absence from the DEFAULT values is not the guarantee, though; an operator override
		// renders just as readily. TestDeploymentRejectsReservedAndBypassEnv pins the
		// template-time guard that refuses the name outright.
		"KUBERNETES_SERVICE_HOST": "kubelet-injected; chart-settable would defeat the auth-bypass guard",

		// Have working in-code defaults, and the chart's own values (service.port, the
		// container's listen address) are the source of truth. Injecting them would create two
		// places to change one setting.
		"PORT":  "defaulted in code; chart sets service.port instead",
		"HOST":  "defaulted in code (listen on all interfaces in a container)",
		"DEBUG": "defaulted off; LOG_LEVEL is the deployed knob",

		// Superseded by the PG* vars, which the chart DOES inject from the ExternalSecret. The
		// app composes the DSN in-process so the password never lands in the pod spec; a
		// DATABASE_URL would defeat that.
		"DATABASE_URL": "superseded by PGHOST/PGUSER/PGPASSWORD/PGDATABASE from the secret",

		// Defaulted in code to the platform issuer ("heimdall"), which is the only issuer
		// this service accepts. Empty does NOT mean "skip the issuer check" — the check is
		// unconditional; leaving the var unset simply selects the default. Injecting it
		// would create a second place to change one value, and a wrong value there would
		// reject every real token.
		"JWT_ISSUER": "defaulted in code to the platform issuer; the check is unconditional",
	}

	for _, m := range names {
		name := m[1]
		if reason, ok := exempt[name]; ok {
			t.Logf("skipping %s: %s", name, reason)
			continue
		}
		if !strings.Contains(rendered, "name: "+name) {
			t.Errorf("%s is read by the service (pkg/constants) but never injected by the chart, "+
				"so the feature behind it is silently disabled in every deployed environment. "+
				"Add it to app.environment in values.yaml, or add it to the exempt map with a reason.",
				name)
		}
	}
}

// TestDeploymentRejectsReservedAndBypassEnv pins the template-time guard.
//
// The runtime refuses to boot with JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL set only when it
// also detects a cluster, and one of the signals it uses for that is
// KUBERNETES_SERVICE_HOST. Because app.environment renders every key it is handed and
// app.extraEnv is appended verbatim, an override could otherwise supply BOTH — the bypass
// plus an explicit empty KUBERNETES_SERVICE_HOST, which takes precedence over the
// kubelet's — and get a pod that accepts any token as the named principal. The render has
// to fail instead, in every input that reaches the container's env.
func TestDeploymentRejectsReservedAndBypassEnv(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm not on PATH; skipping chart guard test: %v", err)
	}

	const bypassKey = "JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL"
	cases := []struct {
		name string
		set  string
		want string
	}{
		{
			name: "environment declares the kubelet variable",
			set:  "app.environment.KUBERNETES_SERVICE_HOST.value=",
			want: "is reserved",
		},
		{
			// The empty value is the dangerous one, so it gets its own case: a guard keyed on
			// truthiness rather than on the NAME would let this through.
			name: "environment declares a sibling KUBERNETES_ variable",
			set:  "app.environment.KUBERNETES_SERVICE_PORT.value=443",
			want: "is reserved",
		},
		{
			name: "environment sets the auth bypass",
			set:  "app.environment." + bypassKey + ".value=someone@example.com",
			want: "must stay empty",
		},
		{
			name: "extraEnv declares the kubelet variable",
			set:  "app.extraEnv[0].name=KUBERNETES_SERVICE_HOST,app.extraEnv[0].value=",
			want: "is reserved",
		},
		{
			name: "extraEnv sets the auth bypass",
			set:  "app.extraEnv[0].name=" + bypassKey + ",app.extraEnv[0].value=someone@example.com",
			want: "may not set",
		},
		{
			// valueFrom is the same hole through a different door. Both env inputs support
			// it (see the `else if $config.valueFrom` branch in the container env loop), and
			// the value lives in a Secret the template cannot read -- so a guard that only
			// inspects `.value` sees nothing and renders a Deployment whose principal is
			// sourced at runtime. The whole form is refused for this key rather than
			// inspected, because "the template cannot see it" must not read as "it is empty".
			name: "environment sources the auth bypass from a secret",
			set: "app.environment." + bypassKey + ".valueFrom.secretKeyRef.name=creds," +
				"app.environment." + bypassKey + ".valueFrom.secretKeyRef.key=principal",
			want: "may not use valueFrom",
		},
		{
			name: "extraEnv sources the auth bypass from a secret",
			set: "app.extraEnv[0].name=" + bypassKey + "," +
				"app.extraEnv[0].valueFrom.secretKeyRef.name=creds," +
				"app.extraEnv[0].valueFrom.secretKeyRef.key=principal",
			want: "may not set",
		},
		// Helm's scalar typing is the third door. `--set ...value=false` is parsed as a
		// BOOLEAN, and Helm's `default ""` treats boolean false and numeric 0 as empty — so
		// a guard written as `default "" .value` saw nothing while the renderer went on to
		// emit `value: "false"`, a perfectly non-empty container env var. These four cases
		// exist because that bypass was real: it rendered a Deployment carrying the bypass
		// key, which the runtime then refuses to boot with, turning a template-time
		// rejection into a CrashLoopBackOff. Both scalars, both env inputs.
		{
			name: "environment sets the auth bypass to boolean false",
			set:  "app.environment." + bypassKey + ".value=false",
			want: "must stay empty",
		},
		{
			name: "environment sets the auth bypass to numeric zero",
			set:  "app.environment." + bypassKey + ".value=0",
			want: "must stay empty",
		},
		{
			name: "extraEnv sets the auth bypass to boolean false",
			set:  "app.extraEnv[0].name=" + bypassKey + ",app.extraEnv[0].value=false",
			want: "may not set",
		},
		{
			name: "extraEnv sets the auth bypass to numeric zero",
			set:  "app.extraEnv[0].name=" + bypassKey + ",app.extraEnv[0].value=0",
			want: "may not set",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command("helm", "template", chartDir,
				"--show-only", "templates/deployment.yaml", "--set", tc.set).CombinedOutput()
			if err == nil {
				t.Fatalf("helm template SUCCEEDED with --set %s; the deployment renders an env "+
					"block that can disable authentication:\n%s", tc.set, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("render failed, but not on the guard: want a message containing %q, got:\n%s",
					tc.want, out)
			}
		})
	}
}

// TestDeploymentStillRendersWithOrdinaryOverrides is the other half: a guard that rejected
// everything would pass the test above while breaking every real deploy.
func TestDeploymentStillRendersWithOrdinaryOverrides(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm not on PATH; skipping chart guard test: %v", err)
	}
	out, err := exec.Command("helm", "template", chartDir,
		"--show-only", "templates/deployment.yaml",
		"--set", "app.environment.LOG_LEVEL.value=debug",
		"--set", "app.extraEnv[0].name=MY_EXTRA,app.extraEnv[0].value=x",
		// valueFrom is refused only for the bypass key. An ordinary secret-sourced variable
		// is the normal way to inject a credential, so rejecting the FORM rather than the
		// key would break every real deploy — the failure mode a one-sided guard invites.
		"--set", "app.environment.DB_PASSWORD.valueFrom.secretKeyRef.name=creds",
		"--set", "app.environment.DB_PASSWORD.valueFrom.secretKeyRef.key=password",
		"--set", "app.extraEnv[1].name=MY_SECRET",
		"--set", "app.extraEnv[1].valueFrom.secretKeyRef.name=creds",
		"--set", "app.extraEnv[1].valueFrom.secretKeyRef.key=other",
		// The empty default must keep rendering — it is declared so the key is discoverable.
		"--set", "app.environment.JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL.value=",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed on ordinary overrides: %v\n%s", err, out)
	}
	for _, want := range []string{"name: LOG_LEVEL", "name: MY_EXTRA", "name: DB_PASSWORD", "name: MY_SECRET"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rendered deployment is missing %q:\n%s", want, out)
		}
	}
}

// TestWhitespaceOnlyBypassValueStillRenders is a SEPARATE render invocation on purpose.
// Helm's --set is last-write-wins, so setting the same key twice in one command silently
// discards the earlier value: folded into the test above, the whitespace case would never
// have reached the guard and the trim would have gone untested while appearing covered.
//
// A whitespace-only bypass value belongs with the acceptances rather than the rejections,
// because it is NOT a bypass: config.LoadConfig applies strings.TrimSpace, so " " leaves
// MockLocalPrincipal empty and verification fully on. The guard trims for exactly that
// reason — it has to judge the value the same way the service will, and failing the render
// for a value the service treats as unset would block a deploy over nothing.
func TestWhitespaceOnlyBypassValueStillRenders(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm not on PATH; skipping chart guard test: %v", err)
	}
	out, err := exec.Command("helm", "template", chartDir,
		"--show-only", "templates/deployment.yaml",
		"--set", "app.environment.JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL.value= ",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed on a whitespace-only bypass value: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "name: JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL") {
		t.Errorf("rendered deployment is missing the bypass key entirely:\n%s", out)
	}
}

// helmTemplateWithSet renders one template file with extra --set arguments, so a
// test can render the chart the way a HOSTILE or careless operator would configure
// it rather than only at its defaults.
func helmTemplateWithSet(t *testing.T, showOnly string, sets ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm not on PATH; skipping chart test: %v", err)
	}
	args := []string{"template", chartDir, "--show-only", showOnly}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s %v failed: %v\n%s", showOnly, sets, err, out)
	}
	return string(out)
}

// podAnnotationValues returns every value rendered for the given annotation key in
// the pod template. It returns a SLICE rather than one value on purpose: the bug
// this guards against renders the key TWICE, and a helper that returned only the
// first (or last) match would hide exactly that.
func podAnnotationValues(manifest, key string) []string {
	var out []string
	for _, line := range strings.Split(manifest, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+":") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(trimmed, key+":")))
		}
	}
	return out
}

// TestScrapePortCannotDriftFromServicePort pins the invariant the values.yaml and
// README both assert: prometheus.io/port is DERIVED from service.port and cannot be
// overridden through podAnnotations.
//
// The template renders the derived key and then merges the user's podAnnotations. If
// that map is merged verbatim, a user-set prometheus.io/port renders a SECOND copy of
// the key whose value wins under YAML's last-key-wins rule, silently pointing the
// scraper at a port the container is not listening on. `omit` is what makes the
// documented invariant true rather than merely intended.
func TestScrapePortCannotDriftFromServicePort(t *testing.T) {
	const deployment = "templates/deployment.yaml"

	t.Run("default render derives the port exactly once", func(t *testing.T) {
		got := podAnnotationValues(helmTemplate(t, deployment), "prometheus.io/port")
		if len(got) != 1 {
			t.Fatalf("prometheus.io/port rendered %d times, want exactly 1: %v", len(got), got)
		}
		if got[0] != `"8080"` {
			t.Errorf("prometheus.io/port = %s, want \"8080\" (the default service.port)", got[0])
		}
	})

	t.Run("a user override cannot change or duplicate the key", func(t *testing.T) {
		manifest := helmTemplateWithSet(t, deployment, `podAnnotations.prometheus\.io/port=9999`)
		got := podAnnotationValues(manifest, "prometheus.io/port")
		if len(got) != 1 {
			t.Fatalf("a podAnnotations override rendered prometheus.io/port %d times, want exactly 1: %v", len(got), got)
		}
		if strings.Contains(got[0], "9999") {
			t.Errorf("the scrape port drifted to %s via podAnnotations; it must stay derived from service.port", got[0])
		}
	})

	t.Run("the derived port tracks service.port", func(t *testing.T) {
		manifest := helmTemplateWithSet(t, deployment, "service.port=9090")
		got := podAnnotationValues(manifest, "prometheus.io/port")
		if len(got) != 1 || got[0] != `"9090"` {
			t.Errorf("prometheus.io/port = %v, want [\"9090\"] after setting service.port", got)
		}
	})

	t.Run("unrelated podAnnotations still pass through", func(t *testing.T) {
		manifest := helmTemplateWithSet(t, deployment, `podAnnotations.example\.com/owner=marketing`)
		if got := podAnnotationValues(manifest, "example.com/owner"); len(got) != 1 || got[0] != "marketing" {
			t.Errorf("a non-reserved podAnnotation was dropped: %v", got)
		}
		// The scrape keys set in values.yaml must survive the omit.
		if got := podAnnotationValues(manifest, "prometheus.io/scrape"); len(got) != 1 {
			t.Errorf("prometheus.io/scrape rendered %d times, want 1: %v", len(got), got)
		}
	})
}
