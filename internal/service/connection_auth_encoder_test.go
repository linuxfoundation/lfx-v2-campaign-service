// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// connectionsErrorEncoders is the generated file this test reads. It is generated from
// design/connection.go by `make apigen`, so what the test really pins is a property of
// that design file — see the comment on the test.
const connectionsErrorEncoders = "../../gen/http/lfx_v2_campaign_service_connections/server/encode_decode.go"

// authErrorEncoders is every generated server encoder file that carries a 401. The
// connections service maps its Unauthorized responses through connectionAuthErrorResponses
// (design/connection.go); briefs and audiences do so through briefErrorResponses
// (design/brief.go). Those are INDEPENDENT transport mappings, so a challenge-header
// regression in one is invisible to a test that reads only the other.
var authErrorEncoders = []string{
	connectionsErrorEncoders,
	"../../gen/http/lfx_v2_campaign_service_briefs/server/encode_decode.go",
	"../../gen/http/lfx_v2_campaign_service_audiences/server/encode_decode.go",
}

// TestEveryConnectionMethodEncodesBadRequest pins the one thing that makes JWTAuth's 401
// reach the caller as a 401.
//
// JWTAuth (connection_handler.go) returns *conn.UnauthorizedError for any refused token, on
// EVERY method — the reads and the delete included, since they carry bearerToken() exactly
// as the writes do. But Goa generates a method's error encoder from the errors that method
// DECLARES: a method whose design block omits `Error("Unauthorized", ...)` has no
// `case "Unauthorized"` in its encoder, so the typed error falls through to the generic
// encoder and the client is told 500. Nothing about that failure is visible from the Go
// types — it compiles, the handler returns the right error, and the wire status is simply
// wrong. This test is the only place it is observable without an HTTP round trip.
//
// It reads the generated source rather than driving each encoder by name because the
// hazard is a NEW method (a new provider's get/delete/test) added without the declaration.
// A hand-maintained list of encoders to exercise would not include the new one, which is
// precisely the case that must fail.
func TestEveryConnectionMethodEncodesBadRequest(t *testing.T) {
	// Unauthorized is the one JWTAuth returns; BadRequest still carries payload and
	// path-parameter validation (create-* constrains project_id to a slug Pattern, so
	// the generated decoder can reject before any handler runs). Both are checked because
	// either one missing is the same invisible 500.
	for _, errName := range []string{"BadRequest", "Unauthorized"} {
		t.Run(errName, func(t *testing.T) {
			assertEveryConnectionEncoderHandles(t, errName)
		})
	}
}

// assertEveryConnectionEncoderHandles fails for each generated Encode*Error function whose
// switch has no case for errName.
func assertEveryConnectionEncoderHandles(t *testing.T, errName string) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, connectionsErrorEncoders, nil, 0)
	if err != nil {
		t.Fatalf("parsing the generated connection encoders: %v", err)
	}

	want := `"` + errName + `"`

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

		var handled bool
		ast.Inspect(fn, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				lit, ok := expr.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING && lit.Value == want {
					handled = true
					return false
				}
			}
			return true
		})

		if !handled {
			method := strings.TrimSuffix(strings.TrimPrefix(name, "Encode"), "Error")
			t.Errorf("%s has no %s case: the %s method in design/connection.go does not "+
				"declare Error(%s, ...) via authErrors(), so that rejection is encoded as a 500",
				name, errName, method, want)
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

// TestEveryConnectionUnauthorizedEncoderSetsTheChallenge pins the half a case-presence
// check cannot see. A 401 must carry a WWW-Authenticate challenge (RFC 9110 §15.5.2), and
// it reaches the wire only because design/connection.go maps the error type's
// www_authenticate attribute onto the header inside connectionAuthErrorResponses. Drop
// that Header() line and the field silently moves into the JSON body: the status is still
// 401, every encoder still has its Unauthorized case, and the response is spec-violating.
func TestEveryConnectionUnauthorizedEncoderSetsTheChallenge(t *testing.T) {
	// Go canonicalises the header name at generation time, hence "Www-Authenticate".
	const setChallenge = `w.Header().Set("Www-Authenticate", res.WwwAuthenticate)`

	// Every service that answers 401, not just connections. briefs and audiences take their
	// mapping from briefErrorResponses (design/brief.go), which is a SEPARATE Header call from
	// the connections one: deleting it would leave every brief and audience 401 a bare status
	// with no challenge, and a connections-only test would still pass. The service-layer tests
	// cannot see this either — they assert UnauthorizedError.WwwAuthenticate, the struct field,
	// which is populated before transport and stays correct while the header vanishes.
	for _, path := range authErrorEncoders {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the generated encoders %s: %v", path, err)
		}
		got := strings.Count(string(src), setChallenge)
		want := strings.Count(string(src), `case "Unauthorized":`)
		if want == 0 {
			t.Errorf("no Unauthorized cases in %s; the test is not exercising anything", path)
			continue
		}
		if got != want {
			t.Errorf("%s: %d encoders set the WWW-Authenticate challenge but %d have an Unauthorized "+
				"case: a 401 without a challenge violates RFC 9110 §15.5.2 — check the Header mapping "+
				"in connectionAuthErrorResponses (design/connection.go) and briefErrorResponses "+
				"(design/brief.go)", path, got, want)
		}
	}
}
