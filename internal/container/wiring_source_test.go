// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package container

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// TestNoServiceIsConstructedOutsideItsVerifierInjectingHelper is the half of the wiring
// guarantee that TestNewContainer_AllPathsInjectTheTokenVerifier cannot reach.
//
// That test boots the container and asserts every service carries a verifier, but it can
// only exercise the paths a test can boot: no-database, and 503-mode. The LIVE path
// (wireLiveBackends) needs a reachable PostgreSQL, so it is not covered — and it
// constructs all three services independently. The claim "every boot path is pinned" was
// therefore not actually backed by a test, only by having read the code.
//
// Reachability is a SOURCE property, so this checks the source. Each service has exactly
// one constructor helper on *Container whose job is to attach the shared token verifier;
// a path that calls service.NewXService directly compiles, runs, serves traffic, and
// rejects every request with "verifier is not configured". This test fails if such a call
// site is ever added, on any path, bootable in a unit test or not.
func TestNoServiceIsConstructedOutsideItsVerifierInjectingHelper(t *testing.T) {
	// constructor -> the single helper permitted to call it.
	permitted := map[string]string{
		"NewBriefService":      "newBriefService",
		"NewConnectionService": "newConnectionService",
		"NewAudienceService":   "newAudienceService",
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		// Non-test files only: a test may legitimately construct a service directly to
		// exercise the service itself, which is not a boot path.
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the container package: %v", err)
	}

	seen := map[string]bool{}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			// enclosing tracks the function each call sits in, so the helper can call its
			// own constructor while nothing else can.
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				enclosing := fn.Name.Name
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					pkgIdent, ok := sel.X.(*ast.Ident)
					if !ok || pkgIdent.Name != "service" {
						return true
					}
					helper, guarded := permitted[sel.Sel.Name]
					if !guarded {
						return true
					}
					seen[sel.Sel.Name] = true
					if enclosing != helper {
						t.Errorf("%s calls service.%s directly, in %s.\n"+
							"Every construction must go through (*Container).%s, which injects the "+
							"shared token verifier. A service built without one compiles and serves "+
							"traffic, rejecting every request as unauthenticated.",
							fset.Position(call.Pos()), sel.Sel.Name, enclosing, helper)
					}
					return true
				})
			}
			_ = path
		}
	}

	// Without this the test passes vacuously the day a constructor is renamed: no call
	// sites match, nothing is flagged, and the guarantee quietly stops being checked.
	for ctor := range permitted {
		if !seen[ctor] {
			t.Errorf("no call to service.%s was found in the container package -- this test is "+
				"matching on a name that no longer exists, so it is no longer guarding anything. "+
				"Update the permitted map.", ctor)
		}
	}
}
