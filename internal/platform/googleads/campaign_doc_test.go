// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestCreateCampaignKeepsItsDocComment pins a failure mode that is invisible in a diff.
//
// Go binds a doc comment to whatever declaration FOLLOWS it, not to the one it was written
// for. Extracting campaignPreflight put a type declaration between CreateCampaign's
// create-cascade contract and CreateCampaign itself, so the whole comment reattached to the
// helper: `go doc Client.CreateCampaign` printed nothing while `go doc campaignPreflight`
// printed the cascade contract. No build error, no vet warning, and a diff that reads as a
// pure addition.
//
// That contract is not decoration — it is where the partial-result rule, the UNCONFIRMED
// classification and "which mutate leaves what committed" are written down, and it is the
// first thing a caller reads before deciding how to handle an error from this method. Losing
// it loses the only statement of the create path's most subtle behaviour.
//
// This asserts one method rather than a package-wide convention on purpose. A broad "every
// exported declaration must be documented" rule fails on pre-existing silence that is
// deliberate, and the wider AST version of it trips over struct-field comments and comments
// inside function bodies — a test that fires on things nobody intends to change gets turned
// off rather than obeyed.
func TestCreateCampaignKeepsItsDocComment(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "campaign.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing campaign.go: %v", err)
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "CreateCampaign" {
			continue
		}
		if fn.Doc == nil {
			t.Fatalf("CreateCampaign has no doc comment. If one was written for it, a "+
				"declaration inserted just above has taken it — Go binds a comment to the "+
				"declaration that FOLLOWS it. Check what sits between %s and the nearest "+
				"comment block above it.", fset.Position(fn.Pos()))
		}
		doc := fn.Doc.Text()
		if !strings.HasPrefix(doc, "CreateCampaign ") {
			t.Errorf("CreateCampaign's doc begins %q — it documents something else", firstLine(doc))
		}
		// The cascade contract specifically, not just any doc: a one-line replacement would
		// satisfy the check above while the part that matters stayed attached to the helper.
		for _, want := range []string{"PARTIAL", "UNCONFIRMED", "PAUSED"} {
			if !strings.Contains(doc, want) {
				t.Errorf("CreateCampaign's doc no longer mentions %s — the create-cascade "+
					"contract has been separated from the method it describes", want)
			}
		}
		return
	}
	t.Fatal("CreateCampaign not found in campaign.go")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
