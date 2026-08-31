// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strings"
	"testing"
)

// TestThrottleContractIsDocumentedWhereItIsRead pins a failure mode no behavioural test can
// reach: a comment that states the OPPOSITE of what the code does.
//
// When 429 became ambiguous, three comments in this file went on saying a 429 proves nothing
// was created and that every 4xx means "definitely not applied" — including the doc comment on
// createOutcomeAmbiguous itself and the one on request(), which is where a caller looks first.
// Nothing failed. A reader trusting either one would write exactly the blind retry the change
// exists to prevent, and a later editor would "restore consistency" by deleting the branch.
//
// So the invariant is asserted from both ends: the behaviour, and the prose a caller reads
// before relying on it. Deleting the branch fails the first check; deleting the explanation
// fails the second.
func TestThrottleContractIsDocumentedWhereItIsRead(t *testing.T) {
	// The behaviour. An exhausted 429 is UNCONFIRMED, not a clean rejection.
	if !createOutcomeAmbiguous(&apiError{StatusCode: http.StatusTooManyRequests, Method: http.MethodPost}) {
		t.Error("an exhausted 429 classified as definitely-not-applied: a caller will retry a create that may already have run")
	}
	// And the contrast case, so the check above cannot be satisfied by calling everything ambiguous.
	if createOutcomeAmbiguous(&apiError{StatusCode: http.StatusBadRequest, Method: http.MethodPost}) {
		t.Error("a definite 400 classified as ambiguous: every clean rejection would tell the operator to go check Reddit")
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "client.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing client.go: %v", err)
	}

	// Each entry names a declaration whose doc a caller reads before deciding how to treat an
	// error from it, and the claim that doc must carry.
	for _, want := range []struct {
		decl   string
		phrase string
		why    string
	}{
		{"createOutcomeAmbiguous", "429", "the classifier's own doc must name the 4xx it exempts, or a reader takes 'a definite 4xx means NOT applied' at face value"},
		{"request", "NOT proof", "request() is where the retry policy is documented; without this it reads as 'a 429 means nothing was created'"},
	} {
		doc := docOf(file, want.decl)
		if doc == "" {
			t.Errorf("%s has no doc comment (a declaration inserted above may have taken it)", want.decl)
			continue
		}
		if !strings.Contains(doc, want.phrase) {
			t.Errorf("%s's doc no longer says %q: %s", want.decl, want.phrase, want.why)
		}
	}
}

// docOf returns the doc comment of the named func decl, or "".
func docOf(file *ast.File, name string) string {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Doc == nil {
			continue
		}
		return fn.Doc.Text()
	}
	return ""
}
