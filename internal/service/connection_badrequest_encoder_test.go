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

// connectionsErrorEncoders is the generated file this test reads. It is generated from
// design/connection.go by `make apigen`, so what the test really pins is a property of
// that design file — see the comment on the test.
const connectionsErrorEncoders = "../../gen/http/lfx_v2_campaign_service_connections/server/encode_decode.go"

// TestEveryConnectionMethodEncodesBadRequest pins the one thing that makes JWTAuth's 400
// reach the caller as a 400.
//
// JWTAuth (connection_handler.go) returns *conn.BadRequestError for any refused token, on
// EVERY method — the reads and the delete included, since they carry bearerToken() exactly
// as the writes do. But Goa generates a method's error encoder from the errors that method
// DECLARES: a method whose design block omits `Error("BadRequest", ...)` has no
// `case "BadRequest"` in its encoder, so the typed error falls through to the generic
// encoder and the client is told 500. Nothing about that failure is visible from the Go
// types — it compiles, the handler returns the right error, and the wire status is simply
// wrong. This test is the only place it is observable without an HTTP round trip.
//
// It reads the generated source rather than driving each encoder by name because the
// hazard is a NEW method (a new provider's get/delete/test) added without the declaration.
// A hand-maintained list of encoders to exercise would not include the new one, which is
// precisely the case that must fail.
func TestEveryConnectionMethodEncodesBadRequest(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, connectionsErrorEncoders, nil, 0)
	if err != nil {
		t.Fatalf("parsing the generated connection encoders: %v", err)
	}

	var checked int
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		name := fn.Name.Name
		if !strings.HasPrefix(name, "Encode") || !strings.HasSuffix(name, "Error") {
			continue
		}
		checked++

		var handlesBadRequest bool
		ast.Inspect(fn, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				lit, ok := expr.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING && lit.Value == `"BadRequest"` {
					handlesBadRequest = true
					return false
				}
			}
			return true
		})

		if !handlesBadRequest {
			method := strings.TrimSuffix(strings.TrimPrefix(name, "Encode"), "Error")
			t.Errorf("%s has no BadRequest case: the %s method in design/connection.go does not "+
				"declare Error(\"BadRequest\", ...), so a token JWTAuth refuses is encoded as a 500",
				name, method)
		}
	}

	// A parse that silently matched nothing would pass every assertion above. The exact
	// count is deliberately not pinned — new providers are expected — but zero means the
	// file moved or the generated naming changed, and the test is then checking nothing.
	if checked == 0 {
		t.Fatalf("no Encode*Error functions found in %s; the test is not exercising anything",
			connectionsErrorEncoders)
	}
}
