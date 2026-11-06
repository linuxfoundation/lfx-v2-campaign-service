// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package container

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/dispatch"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// bindRedditReportLedger binds the shared ledger into the Reddit dispatcher registerDispatchers
// builds; without a pool it binds nothing, and the dispatcher's budget stays fail-closed.
func TestBindRedditReportLedger_BindsTheRedditDispatcher(t *testing.T) {
	m := registerDispatchers(nil, nil, nil, nil, nil)
	rd, ok := m[model.ProviderRedditAds].(*dispatch.RedditDispatcher)
	if !ok {
		t.Fatalf("registerDispatchers has no *dispatch.RedditDispatcher for reddit: %T", m[model.ProviderRedditAds])
	}
	// The production budget, not this binary's: container tests do not replace it.
	if rd.RedditReportLedgerBound() {
		t.Fatal("a freshly built dispatcher already has a ledger bound")
	}
	bindRedditReportLedger(m, nil)
	if rd.RedditReportLedgerBound() {
		t.Fatal("bindRedditReportLedger bound a ledger with no pool")
	}
	bindRedditReportLedger(m, &postgres.Pool{})
	if !rd.RedditReportLedgerBound() {
		t.Fatal("bindRedditReportLedger did not bind the ledger into the Reddit dispatcher")
	}
	if reddit.NewLocalReportBudgets().LedgerBound() {
		t.Error("a process-local budget reports a shared ledger bound")
	}
}

// Both live wiring paths call bindRedditReportLedger. Neither can be booted in a unit test (each
// needs a reachable Postgres), and a path that skips the call fails every Reddit report closed in
// production while every test stays green — so the call sites are pinned in the source.
func TestBothLiveWiringPathsBindTheRedditReportLedger(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "container.go", nil, 0)
	if err != nil {
		t.Fatalf("parse container.go: %v", err)
	}
	want := map[string]bool{"wireLiveBackends": false, "retryDatabaseInit": false}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if _, tracked := want[fn.Name.Name]; !tracked {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "bindRedditReportLedger" {
					want[fn.Name.Name] = true
				}
			}
			return true
		})
	}
	for name, found := range want {
		if !found {
			t.Errorf("%s does not call bindRedditReportLedger: that wiring path's Reddit reports would all fail closed", name)
		}
	}
}
