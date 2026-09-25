// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// wantProbeConnectionDispatchers names every dispatcher the connection tests route through.
// Listed explicitly rather than derived, so DELETING a ProbeConnection method fails this test
// too — a derived list would simply stop checking the method that went away, and the endpoint
// would quietly fall back to answering "a credential blob exists in the row".
var wantProbeConnectionDispatchers = []string{
	"GoogleAdsDispatcher",
	"MetaDispatcher",
	"RedditDispatcher",
	"TwitterDispatcher",
	"MicrosoftDispatcher",
	"HubSpotDispatcher",
}

// The same six must SATISFY service.ConnectionProber, and that is a different claim from the
// one the AST guard below makes. The guard reads source: it proves a method named
// ProbeConnection exists and resolves owned credentials, and it would keep passing if the
// receiver or the signature drifted. Orchestrator.ProbeConnection reaches these through a type
// assertion (internal/service/orchestrator.go), so a drifted signature misses silently and
// turns all six connection tests into typed 500s logged reason=probe_unwired — the endpoint
// exists and always fails, which is the exact hazard the AccountLister assertions in
// account_discovery_test.go were added for.
//
// Unlike that block, this one IS the full roster: ConnectionProber has six implementations and
// wantProbeConnectionDispatchers above names the same six. LinkedIn is deliberately absent —
// TestLinkedin does not call ProbeConnection at all.
var (
	_ service.ConnectionProber = (*GoogleAdsDispatcher)(nil)
	_ service.ConnectionProber = (*MetaDispatcher)(nil)
	_ service.ConnectionProber = (*RedditDispatcher)(nil)
	_ service.ConnectionProber = (*TwitterDispatcher)(nil)
	_ service.ConnectionProber = (*MicrosoftDispatcher)(nil)
	_ service.ConnectionProber = (*HubSpotDispatcher)(nil)
)

// TestProbeConnection_ResolvesOwnedCredentialsOnly is a source-derived guard on the single
// design constraint this whole feature rests on (LFXV2-2665).
//
// Every other dispatch path may fall back to the shared LF SYSTEM connection row when a project
// has none of its own — that fallback is deliberate and load-bearing for campaign creation. A
// connection TEST must never take it. "Is this project's connection good?" answered from a
// borrowed row reports a connection the project does not have as healthy, and the operator has
// no way to see that from the response: the test passes, and campaign creation later fails for
// a project that was told it was fine.
//
// The check is on source rather than behavior because the failure is silent by construction —
// a resolveOwned swapped for resolve still compiles, still passes every functional test that
// uses a project WITH its own connection, and only misbehaves for the projects that do not.
func TestProbeConnection_ResolvesOwnedCredentialsOnly(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing dispatch sources: %v", err)
	}

	found := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("reading %s: %v", path, rerr)
		}
		file, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", path, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "ProbeConnection" || fn.Recv == nil || fn.Body == nil {
				continue
			}
			recv := receiverTypeName(fn.Recv)
			found[recv] = true

			// The body of the method, plus the body of any resolve helper it delegates to.
			bodies := []ast.Node{fn.Body}
			for _, helper := range calledResolveHelpers(fn.Body) {
				if h := findFunc(file, helper); h != nil && h.Body != nil {
					bodies = append(bodies, h.Body)
				}
			}

			usesOwned := false
			for _, body := range bodies {
				ast.Inspect(body, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					inner, ok := sel.X.(*ast.SelectorExpr)
					if !ok || inner.Sel.Name != "creds" {
						return true
					}
					switch sel.Sel.Name {
					case "resolveOwned":
						usesOwned = true
					case "resolve":
						// The forced-system resolver, reached directly from a probe path.
						t.Errorf("%s.ProbeConnection resolves credentials through d.creds.resolve at %s: "+
							"the forced-system fallback would let a connection test on a project with no "+
							"connection of its own silently verify the LF SYSTEM row's pairing instead of "+
							"reporting that the project has nothing to test",
							recv, fset.Position(sel.Pos()))
					}
					return true
				})
			}
			if !usesOwned {
				t.Errorf("%s.ProbeConnection never reaches d.creds.resolveOwned, directly or through a "+
					"resolve helper it calls; either it resolves nothing (so it verifies nothing) or it "+
					"reaches credentials by a route this guard cannot see", recv)
			}
		}
	}

	for _, want := range wantProbeConnectionDispatchers {
		if !found[want] {
			t.Errorf("%s has no ProbeConnection method; its connection test answers OK the moment a "+
				"credential blob exists in the row, which is the defect LFXV2-2665 removed", want)
		}
	}
}

// receiverTypeName returns the bare type name of a method receiver, pointer or not.
func receiverTypeName(recv *ast.FieldList) string {
	if len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// calledResolveHelpers names the methods a probe body calls whose names suggest they resolve a
// client or credential — the delegation shape every one of these probes uses.
func calledResolveHelpers(body *ast.BlockStmt) []string {
	var names []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if strings.HasPrefix(sel.Sel.Name, "resolve") {
			names = append(names, sel.Sel.Name)
		}
		return true
	})
	return names
}

// findFunc locates a method declaration by name within one file.
func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}
