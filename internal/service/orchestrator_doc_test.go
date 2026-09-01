// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestCapabilityInterfacesKeepTheirDocComments pins a failure mode that is invisible in a diff
// and that this file has now hit twice.
//
// Go binds a doc comment to whatever declaration FOLLOWS it. Inserting a new type between an
// existing comment and its type silently reassigns the comment: the new type inherits prose
// describing something else, and the old one is left undocumented. Nothing fails — not the
// build, not vet, and the diff reads as a pure addition.
//
// That matters here because these comments are where the CAPABILITY BOUNDARIES are written
// down: which dispatcher answers which question, why the interfaces are separate, and what a
// missing one means for the caller. A reader who takes EmailSearcher's prose as a description of
// CampaignSearcher concludes that a mutating create is "a pure read that never mutates platform
// or DB state", which is the opposite of true.
func TestCapabilityInterfacesKeepTheirDocComments(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "orchestrator.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing orchestrator.go: %v", err)
	}

	docs := map[string]string{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			// A grouped `type (...)` block carries its doc on the spec; a lone decl carries it
			// on the GenDecl. Check both, or the test reports a false orphan.
			d := ts.Doc
			if d == nil {
				d = gd.Doc
			}
			if d != nil {
				docs[ts.Name.Name] = d.Text()
			}
		}
	}

	for _, name := range []string{"EmailSearcher", "CampaignSearcher", "AccountLister", "StatusToggler"} {
		doc, ok := docs[name]
		if !ok {
			t.Errorf("%s has no doc comment. If one was written for it, a declaration inserted "+
				"just above has taken it — Go binds a comment to the declaration that FOLLOWS it.", name)
			continue
		}
		// The doc must describe ITS OWN type, which is what a reattached comment fails.
		if !strings.HasPrefix(doc, name+" ") {
			t.Errorf("%s's doc begins %q — it documents something else, so the two have been "+
				"transposed by an insertion", name, firstLineOf(doc))
		}
	}
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
