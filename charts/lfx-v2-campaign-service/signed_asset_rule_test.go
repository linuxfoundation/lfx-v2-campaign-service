// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package charts_test

import (
	"strings"
	"testing"
)

const (
	signedAssetRuleID      = "rule:lfx:lfx-v2-campaign-service:signed-creative-asset:get"
	signedAssetRulePattern = "/_campaigns/creative-assets/:projectId/:briefId/:assetId"
	campaignsDenyRuleID    = "rule:lfx:lfx-v2-campaign-service:campaigns-placeholder:deny"
)

// blockList returns the trimmed `- x` items listed directly under `key:` in a rule block.
func blockList(block, key string) []string {
	var out []string
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != key+":" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		for _, l := range lines[i+1:] {
			s := strings.TrimSpace(l)
			if s == "" || strings.HasPrefix(s, "#") {
				continue
			}
			if len(l)-len(strings.TrimLeft(l, " ")) <= indent || !strings.HasPrefix(s, "- ") {
				break
			}
			out = append(out, strings.TrimSpace(strings.TrimPrefix(s, "- ")))
		}
		break
	}
	return out
}

// TestSignedAssetRuleIsAnonymousAndExactlyScoped pins the ONE anonymous rule this chart adds
// (LFXV2-2665): GET/HEAD only, exactly the service's three-segment route, no wildcard, and an
// execute pipeline with no credential check — so any widening (a ** tail, a POST, a second
// path) fails here rather than quietly opening more of /_campaigns/ to the internet.
func TestSignedAssetRuleIsAnonymousAndExactlyScoped(t *testing.T) {
	ruleset := helmTemplate(t, "templates/ruleset.yaml")
	block := ruleBlock(t, ruleset, signedAssetRuleID)

	if got := blockList(block, "methods"); strings.Join(got, ",") != "GET,HEAD" {
		t.Errorf("methods = %v, want exactly [GET HEAD]", got)
	}
	if got := blockList(block, "routes"); len(got) != 1 || got[0] != "path: "+signedAssetRulePattern {
		t.Errorf("routes = %v, want exactly [path: %s]", got, signedAssetRulePattern)
	}
	if strings.Contains(block, "**") {
		t.Error("the anonymous rule must not carry a wildcard")
	}
	exec := blockList(block, "execute")
	want := []string{"authenticator: anonymous_authenticator", "authorizer: allow_all", "finalizer: create_jwt"}
	if strings.Join(exec, "|") != strings.Join(want, "|") {
		t.Errorf("execute = %v, want %v", exec, want)
	}
	if !strings.Contains(block, "allow_encoded_slashes: 'off'") {
		t.Error("the anonymous rule must refuse encoded slashes")
	}

	// It matches the served route and nothing beside it.
	m := ruleMatcher(t, signedAssetRulePattern)
	const uuidA, uuidB = "11111111-2222-4333-8444-555555555555", "66666666-7777-4888-9999-aaaaaaaaaaaa"
	if !m.MatchString("/_campaigns/creative-assets/cncf/" + uuidA + "/" + uuidB) {
		t.Error("the rule does not admit the served route")
	}
	for _, p := range []string{
		"/_campaigns/creative-assets",
		"/_campaigns/creative-assets/cncf/" + uuidA,
		"/_campaigns/creative-assets/cncf/" + uuidA + "/" + uuidB + "/extra",
		"/_campaigns/openapi.json",
		"/_campaigns/anything",
		"/projects/cncf/briefs",
	} {
		if m.MatchString(p) {
			t.Errorf("the anonymous rule must not admit %s", p)
		}
	}
}

// TestNoOtherRuleWasLoosened: the deny placeholder still covers the rest of /_campaigns/, and
// the signed-asset rule is the only one that runs anonymous_authenticator WITHOUT oidc.
func TestNoOtherRuleWasLoosened(t *testing.T) {
	ruleset := helmTemplate(t, "templates/ruleset.yaml")
	deny := ruleBlock(t, ruleset, campaignsDenyRuleID)
	if !strings.Contains(deny, "path: /_campaigns/**") || !strings.Contains(deny, "authorizer: deny_all") {
		t.Fatalf("the /_campaigns/** deny placeholder changed:\n%s", deny)
	}
	if strings.Contains(deny, "anonymous_authenticator") {
		t.Fatal("the deny placeholder must keep requiring a token")
	}

	var anonymousOnly []string
	for _, chunk := range strings.Split(ruleset, "- id:")[1:] {
		id := strings.TrimSpace(strings.SplitN(chunk, "\n", 2)[0])
		exec := blockList(chunk, "execute")
		hasAnon, hasOIDC := false, false
		for _, e := range exec {
			hasAnon = hasAnon || e == "authenticator: anonymous_authenticator"
			hasOIDC = hasOIDC || e == "authenticator: oidc"
		}
		if hasAnon && !hasOIDC {
			anonymousOnly = append(anonymousOnly, id)
		}
	}
	if len(anonymousOnly) != 1 || !strings.Contains(anonymousOnly[0], signedAssetRuleID) {
		t.Fatalf("anonymous-only rules = %v, want exactly the signed-asset rule", anonymousOnly)
	}
}

// TestSignedAssetRouteIsForwarded: the HTTPRoute forwards the path (through the /_campaigns/
// prefix), or the anonymous rule would be dead config and Reddit could not fetch the image.
func TestSignedAssetRouteIsForwarded(t *testing.T) {
	route := helmTemplate(t, "templates/httproute.yaml")
	if !strings.Contains(route, "type: PathPrefix") || !strings.Contains(route, "value: /_campaigns/") {
		t.Fatal("the HTTPRoute no longer forwards /_campaigns/, so the signed asset route is unreachable")
	}
	// It must NOT be swallowed by the project-nested regex either (wrong rule family).
	if extractRouteRegex(t, route).MatchString("/_campaigns/creative-assets/cncf/a/b") {
		t.Fatal("the project-nested regex must not match the signed asset route")
	}
}
