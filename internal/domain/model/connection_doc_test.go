// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestConnectionTypesKeepTheirDocComments is the model-side twin of
// TestCapabilityInterfacesKeepTheirDocComments, and exists for the same reason: adding
// HubSpotCampaignPage between MarketingEmail's doc and MarketingEmail itself silently
// reassigned the comment. Go binds a doc comment to the declaration that FOLLOWS it, so the new
// type inherited an explanation of why marketing emails are not AccessibleAccounts, and the
// email type was left undocumented. No build error, no vet warning, a diff reading as a pure
// addition.
//
// These particular comments carry tenancy facts — whose portal a campaign is visible in, and
// why an email id is not an account id. A reader who takes one type's prose for another's draws
// the wrong conclusion about who can see a campaign, which is exactly the class of mistake this
// package's comments exist to prevent.
func TestConnectionTypesKeepTheirDocComments(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "connection.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing connection.go: %v", err)
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
			// Grouped `type (...)` blocks carry the doc on the spec, lone decls on the GenDecl.
			d := ts.Doc
			if d == nil {
				d = gd.Doc
			}
			if d != nil {
				docs[ts.Name.Name] = d.Text()
			}
		}
	}

	for _, name := range []string{"MarketingEmail", "HubSpotCampaign", "HubSpotCampaignPage", "AccessibleAccount"} {
		doc, ok := docs[name]
		if !ok {
			t.Errorf("%s has no doc comment. If one was written for it, a declaration inserted "+
				"just above has taken it — Go binds a comment to the declaration that FOLLOWS it.", name)
			continue
		}
		if !strings.HasPrefix(doc, name+" ") {
			line := doc
			if i := strings.IndexByte(line, '\n'); i >= 0 {
				line = line[:i]
			}
			t.Errorf("%s's doc begins %q — it documents something else, so the two have been "+
				"transposed by an insertion", name, line)
		}
	}
}
