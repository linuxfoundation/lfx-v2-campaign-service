// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The AccountLister roster is prose in four places and code in one. This file is the coupling.
//
// WHY A PARITY TEST RATHER THAN A FOURTH CORRECTION. The comment above
// accountDiscoveryProviders already carries the warning that "an enumeration of members goes
// stale silently — this comment described a Google/Meta-only world for two tickets after that
// stopped being true", and sysacct_test.go records that its own roster sentence has been
// "corrected three times by naming which providers sit on which side, and each correction was
// falsified by the next ticket that moved one." LFXV2-3319 falsified them again: it gave X both
// eligibility halves, which made "the same position Microsoft is in" and "Microsoft is the one
// member worth calling out" wrong, and left docs/knowledge/kubernetes/ruleset.md naming
// twitter-ads among the providers "whose clients have no ListAdAccounts". A fourth correction
// buys exactly one ticket. This test is what makes the fifth ticket fail loudly instead.
//
// WHY NOT DERIVE THE PROSE FROM THE MAP AT RUNTIME — the other option, and it was rejected on
// evidence rather than taste. Two reasons, the second decisive:
//
//  1. The prose that went stale is not IN the map's package. It is in a Helm-chart knowledge doc
//     (ruleset.md), a Goa design comment (design/connection.go) and a test comment. Runtime
//     derivation cannot reach a Markdown file or a comment; it would fix the one restatement
//     that is already adjacent to the map and leave the three that actually drifted.
//
//  2. accountDiscoveryProviders is NOT the AccountLister set, so there is no single map to
//     derive from. AccountLister is {GoogleAds, Meta, LinkedIn, Microsoft, X} — the FIRST
//     eligibility half. accountDiscoveryProviders is {GoogleAds, Meta} — providers holding BOTH
//     halves. The second half is "Dispatch itself calls the validator that tags
//     ErrAccountNotSelected", which is a CALL-GRAPH property: every dispatcher including Reddit
//     mentions that sentinel, so no grep, symbol table or runtime reflection distinguishes
//     LinkedIn (tags it in a resolver Dispatch does not call) from X (whose Dispatch calls the
//     tagging validator itself). A derived sentence would therefore have to hardcode the very
//     judgement that keeps going stale.
//
// So the FIRST half — which is mechanically checkable, and is what LFXV2-3319 actually moved —
// is pinned here against the interface itself, and the second half stays prose because it is a
// human judgement. That split is the point: pin what a compiler can see, and stop restating what
// it cannot.
//
// This test deliberately asserts the DOCS, not just the code. A provider that gains
// ListAccounts without its route, rule and knowledge docs being updated is the exact failure
// LFXV2-3319 shipped.

// accountListerProviders derives the CURRENT first-half roster from the type system: every
// dispatcher constructed here is type-asserted against service.AccountLister, so adding or
// removing the method moves this set with no edit to this file.
//
// Constructed with nil dependencies on purpose — the assertion is about the METHOD SET, which is
// a compile-time property of the type. Nothing is called, so nothing is dereferenced.
func accountListerProviders(t *testing.T) map[model.Provider]bool {
	t.Helper()
	candidates := map[model.Provider]any{
		model.ProviderGoogleAds:    NewGoogleAdsDispatcher(nil, nil),
		model.ProviderMetaAds:      NewMetaDispatcher(nil, nil),
		model.ProviderLinkedInAds:  NewLinkedInDispatcher(nil, nil),
		model.ProviderMicrosoftAds: NewMicrosoftDispatcher(nil, nil),
		model.ProviderTwitterAds:   NewTwitterDispatcher(nil, nil),
		model.ProviderRedditAds:    NewRedditDispatcher(nil, nil),
	}
	got := map[model.Provider]bool{}
	for p, d := range candidates {
		if _, ok := d.(service.AccountLister); ok {
			got[p] = true
		}
	}
	if len(got) == 0 {
		t.Fatal("no dispatcher satisfied service.AccountLister — the assertion itself is broken, " +
			"which would make every check below vacuously pass")
	}
	return got
}

// providerSlug maps a provider to the path segment the route, the RuleSet and the knowledge docs
// all spell it with (connection-<slug>/accounts).
var providerSlug = map[model.Provider]string{
	model.ProviderGoogleAds:    "google-ads",
	model.ProviderMetaAds:      "meta-ads",
	model.ProviderLinkedInAds:  "linkedin-ads",
	model.ProviderMicrosoftAds: "microsoft-ads",
	model.ProviderTwitterAds:   "twitter-ads",
	model.ProviderRedditAds:    "reddit-ads",
}

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// routeRegexp compiles the project-nested RegularExpression path match out of the raw
// HTTPRoute template. The value carries no Helm templating (verified: the line is literal),
// so it compiles directly without rendering the chart — which keeps this test runnable in
// `go test ./...` without helm on PATH.
//
// Compiling it is the point. Asking whether a slug appears ANYWHERE in the template is not a
// test that the provider's discovery route exists: the same slug appears in the /metrics
// alternation and in the explanatory comments, so a substring check survives the deletion of
// the /accounts path it claims to pin.
func routeRegexp(t *testing.T) *regexp.Regexp {
	t.Helper()
	route := repoFile(t, "charts/lfx-v2-campaign-service/templates/httproute.yaml")
	for _, line := range strings.Split(route, "\n") {
		s := strings.TrimSpace(line)
		// The project-nested selector is the only RE2 value anchored at /projects/.
		if !strings.HasPrefix(s, "value:") || !strings.Contains(s, "^/projects/") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(s, "value:"))
		if strings.Contains(raw, "{{") {
			t.Fatalf("the HTTPRoute project regex now contains Helm templating (%q); this test "+
				"compiles the raw value, so it must be rendered instead of re-pointed silently", raw)
		}
		re, err := regexp.Compile(raw)
		if err != nil {
			t.Fatalf("HTTPRoute project regex %q does not compile: %v", raw, err)
		}
		return re
	}
	t.Fatal("no RegularExpression /projects/ value found in the HTTPRoute template; if the chart " +
		"was restructured, re-point this assertion — an unbindable check is one that passes forever.")
	return nil
}

// projectAPIRuleID is the Heimdall rule whose paths gate on campaign_manager. Scoping to THIS
// rule matters: a path moved into an allow_all or differently-scoped rule must not count as
// authorized. charts/parity_test.go pins the authorizer content itself; here we only need the
// path list of that same rule.
const projectAPIRuleID = "rule:lfx:lfx-v2-campaign-service:project-api"

// ruleSetMatchers compiles the Traefik path patterns of the project-api rule out of the raw
// RuleSet template. Only `:name` placeholders appear on the /accounts entries, so the token
// handling here is deliberately small: `:name` and `*` become one segment, `**` any suffix.
func ruleSetMatchers(t *testing.T) []*regexp.Regexp {
	t.Helper()
	ruleset := repoFile(t, "charts/lfx-v2-campaign-service/templates/ruleset.yaml")
	lines := strings.Split(ruleset, "\n")

	start := -1
	for i, line := range lines {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "- id:") && strings.Contains(s, projectAPIRuleID) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("rule %q not found in the RuleSet template; re-point this assertion rather than "+
			"letting it stop checking anything", projectAPIRuleID)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "- id:") {
			end = i
			break
		}
	}

	var matchers []*regexp.Regexp
	for _, line := range lines[start:end] {
		s := strings.TrimSpace(line)
		if !strings.HasPrefix(s, "- path:") {
			continue
		}
		pat := strings.TrimSpace(strings.TrimPrefix(s, "- path:"))
		if !strings.HasPrefix(pat, "/projects/") {
			continue
		}
		matchers = append(matchers, ruleMatcher(t, pat))
	}
	if len(matchers) == 0 {
		t.Fatalf("no /projects/ path patterns found in the %s rule", projectAPIRuleID)
	}
	return matchers
}

// ruleMatcher compiles one Traefik path pattern into an anchored Go regexp, mirroring how
// Heimdall evaluates a rule entry.
func ruleMatcher(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	var b strings.Builder
	b.WriteString("^")
	for i, seg := range strings.Split(pattern, "/") {
		if i > 0 {
			b.WriteString("/")
		}
		switch {
		case seg == "**":
			b.WriteString(".*")
		case seg == "*" || strings.HasPrefix(seg, ":"):
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

// anyRulePathMatches reports whether ANY project-api entry authorizes the path (Heimdall
// authorizes when any rule entry matches).
func anyRulePathMatches(matchers []*regexp.Regexp, path string) bool {
	for _, m := range matchers {
		if m.MatchString(path) {
			return true
		}
	}
	return false
}

func sortedSlugs(set map[model.Provider]bool) []string {
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, providerSlug[p])
	}
	sort.Strings(out)
	return out
}

// TestAccountListerProseMatchesTheInterface fails when a provider gains or loses ListAccounts
// without the documents that enumerate the roster being updated in the same change.
//
// It is the structural replacement for correcting those documents by hand a fourth time.
func TestAccountListerProseMatchesTheInterface(t *testing.T) {
	listers := accountListerProviders(t)

	// The route regex in the chart is the machine-readable statement of the same roster, and
	// charts/parity_test already couples it to the Heimdall RuleSet. Coupling it to the
	// INTERFACE here closes the remaining side: chart-vs-chart parity cannot notice that both
	// sides agree on a set the code has moved past.
	//
	// COMPILE the matchers and probe a CONCRETE /accounts path per provider rather than
	// asking whether the slug appears somewhere in the file. A substring check does not
	// verify the discovery route exists: every slug already occurs elsewhere — in the
	// per-provider `/metrics` alternation and in the surrounding prose comments — so
	// deleting a provider from the `connection-(...)` discovery branch still satisfies
	// Contains, and the check passes while the endpoint is unreachable. Mutation-verified:
	// removing twitter-ads' /accounts from the route AND the RuleSet together left this
	// test, and the whole charts parity suite, green.
	routeRe := routeRegexp(t)
	ruleMatchers := ruleSetMatchers(t)
	for p := range listers {
		slug := providerSlug[p]
		accountsPath := "/projects/p1/connection-" + slug + "/accounts"

		if !routeRe.MatchString(accountsPath) {
			t.Errorf("%s implements service.AccountLister but the HTTPRoute regex does not forward %q: "+
				"ad-account discovery is unreachable through the gateway", p, accountsPath)
		}

		// Heimdall is default-deny, so a forwarded path with no rule is rejected before the
		// FGA check runs. Assert the RuleSet side against the SAME concrete path.
		if !anyRulePathMatches(ruleMatchers, accountsPath) {
			t.Errorf("%s implements service.AccountLister but no Heimdall RuleSet entry authorizes %q: "+
				"Heimdall default-denies an unruled path, so discovery is unreachable", p, accountsPath)
		}
	}

	// Guard the negative direction too: a provider that does NOT implement AccountLister must
	// not have its /accounts path routed or ruled, or the chart admits an endpoint the service
	// answers with a 400 by construction.
	for p, slug := range providerSlug {
		if listers[p] {
			continue
		}
		accountsPath := "/projects/p1/connection-" + slug + "/accounts"
		if routeRe.MatchString(accountsPath) {
			t.Errorf("%s does NOT implement service.AccountLister but the HTTPRoute regex forwards %q; "+
				"the chart admits a path the service does not serve", p, accountsPath)
		}
		if anyRulePathMatches(ruleMatchers, accountsPath) {
			t.Errorf("%s does NOT implement service.AccountLister but a RuleSet entry authorizes %q; "+
				"that rules a path the service does not serve", p, accountsPath)
		}
	}

	// docs/knowledge/kubernetes/ruleset.md restates the roster in prose and is the file
	// LFXV2-3319 missed while updating its companion httproute.md. Two directions, because both
	// have gone wrong: a provider that HAS discovery must not be described as lacking it, and
	// the doc must not claim a provider has it when the interface says otherwise.
	ruleset := repoFile(t, "docs/knowledge/kubernetes/ruleset.md")
	// The parenthetical that enumerates the providers WITHOUT discovery is the exact sentence
	// LFXV2-3319 falsified: "the providers with neither (reddit-ads and twitter-ads, whose clients
	// have no `ListAdAccounts`)". Match that group precisely rather than a character window around
	// the phrase — a loose window spans the whole paragraph, which also names the providers that DO
	// have discovery, so every provider would appear in it and the check would flag correct prose.
	noListerRE := regexp.MustCompile(`the providers with neither \(([^)]*)\)`)
	if m := noListerRE.FindStringSubmatch(ruleset); m != nil {
		noDiscovery := m[1]
		if !strings.Contains(noDiscovery, "ListAdAccounts") {
			t.Fatalf("the \"providers with neither\" parenthetical no longer contains the "+
				"ListAdAccounts phrase this test keys on (%q); re-point the assertion rather than "+
				"letting it silently stop checking anything", noDiscovery)
		}
		for p := range listers {
			slug := providerSlug[p]
			if strings.Contains(noDiscovery, slug) {
				t.Errorf("docs/knowledge/kubernetes/ruleset.md lists %q among \"the providers with "+
					"neither ... whose clients have no ListAdAccounts\", but %s implements "+
					"service.AccountLister. That list must move with the interface — this is the drift "+
					"LFXV2-3319 shipped when it updated httproute.md and not its companion.", slug, p)
			}
		}
	} else {
		t.Error("docs/knowledge/kubernetes/ruleset.md no longer contains the \"the providers with " +
			"neither (...)\" enumeration this test binds to. If the doc was restructured, re-point " +
			"this assertion at whatever now states the roster — an unbindable check is one that " +
			"passes forever.")
	}

	// And the roster COUNT: the doc spells the set out, so a provider added to the interface
	// without being added there leaves a short list that reads as authoritative.
	for p := range listers {
		slug := providerSlug[p]
		if !strings.Contains(ruleset, slug) {
			t.Errorf("%s implements service.AccountLister but docs/knowledge/kubernetes/ruleset.md "+
				"never mentions %q; the RuleSet doc enumerates the discovery providers, so a missing "+
				"one reads as a provider without discovery", p, slug)
		}
	}

	t.Logf("AccountLister roster derived from the interface: %v", sortedSlugs(listers))
}
