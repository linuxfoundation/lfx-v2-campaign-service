// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/accesscheck"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
)

type stubRequester struct{}

func (stubRequester) RequestWithContext(context.Context, string, []byte) (*nats.Msg, error) {
	return nil, nil
}

// The relay's resolver exists only with the flag on AND a connection (#590).
func TestIndexRelayResolver(t *testing.T) {
	if r := indexRelayResolver(true, stubRequester{}); r == nil {
		t.Error("flag on with a connection must give the relay a resolver")
	} else if _, ok := r.(*accesscheck.Checker); !ok {
		t.Errorf("resolver is %T, want the access checker", r)
	}
	if r := indexRelayResolver(false, stubRequester{}); r != nil {
		t.Error("flag off must give no resolver")
	}
	if r := indexRelayResolver(true, nil); r != nil {
		t.Error("no connection must give no resolver")
	}
}

// newIndexRelay with no NATS connection, or with the flag off, builds a relay that keeps the
// slug.
func TestNewIndexRelay_NoResolverWithoutAConnectionOrTheFlag(t *testing.T) {
	c := &Container{indexPublisher: &indexer.NATSPublisher{}}
	for _, on := range []bool{true, false} {
		if c.newIndexRelay(&config.Config{IndexerResolveProjectUID: on}, nil).ResolvesProjectUID() {
			t.Errorf("flag %v with no connection: relay resolves project UIDs", on)
		}
	}
}

// The live boot path needs PostgreSQL and NATS, so its wiring is pinned in the source, as
// wiring_source_test.go does for the services: every indexer.NewRelay is inside
// newIndexRelay, every setIndexRelay is fed by newIndexRelay, and newIndexRelay passes
// indexRelayResolver's answer to WithProjectUIDResolver.
func TestIndexRelayIsOnlyBuiltByNewIndexRelay(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "container.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]map[string]int{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			case *ast.Ident:
				name = fun.Name
			}
			if calls[fn.Name.Name] == nil {
				calls[fn.Name.Name] = map[string]int{}
			}
			calls[fn.Name.Name][name]++
			if name == "setIndexRelay" {
				arg, ok := call.Args[0].(*ast.CallExpr)
				sel, sok := func() (*ast.SelectorExpr, bool) {
					if !ok {
						return nil, false
					}
					s, k := arg.Fun.(*ast.SelectorExpr)
					return s, k
				}()
				if !sok || sel.Sel.Name != "newIndexRelay" {
					t.Errorf("%s: setIndexRelay is not fed by newIndexRelay", fset.Position(call.Pos()))
				}
			}
			return true
		})
	}
	for fn, names := range calls {
		if names["NewRelay"] > 0 && fn != "newIndexRelay" {
			t.Errorf("%s builds an index relay directly; use newIndexRelay", fn)
		}
	}
	if calls["newIndexRelay"]["indexRelayResolver"] == 0 || calls["newIndexRelay"]["WithProjectUIDResolver"] == 0 {
		t.Error("newIndexRelay must pass indexRelayResolver's answer to WithProjectUIDResolver")
	}
	if calls["wireLiveBackends"]["setIndexRelay"] == 0 {
		t.Error("wireLiveBackends no longer sets the index relay")
	}
}
