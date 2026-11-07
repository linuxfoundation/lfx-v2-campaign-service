// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package log

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// projectKeyVariant matches spellings a project identifier could drift into as a log key
// (project, projectID, projectId, project-id, project_uid, project_slug, project_id_hash,
// ...). Issue #229 settled the key as exactly "project_id"; see
// docs/knowledge/architecture/logging-conventions.md.
var projectKeyVariant = regexp.MustCompile(`(?i)^proj(ect)?[_-]?(id|uid|uuid|slug|key)?([_-]?hash)?$`)

// logMethods are slog logger methods (any receiver); attrCtors are slog.X attribute
// constructors, matched only when called as slog.X so that flag.String and the like are not.
var (
	logMethods = map[string]bool{
		"Debug": true, "Info": true, "Warn": true, "Error": true, "Log": true, "With": true, "Add": true,
		"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true, "WithGroup": true,
	}
	attrCtors = map[string]bool{
		"String": true, "Int": true, "Int64": true, "Uint64": true, "Float64": true, "Bool": true,
		"Time": true, "Duration": true, "Any": true, "Group": true, "GroupAttrs": true,
	}
)

// TestProjectIDLogKeyIsUniform fails, with file:line, on any string literal passed directly
// to a slog-style call (logger methods by name, slog.X constructors) that spells the project
// key other than "project_id".
//
// Known blind spots, by design: keys built at run time, held in constants or variables, or
// placed in a slice ([]any{...}, []slog.Attr{...}) and spread into the call are not
// checked, and a literal VALUE spelled like a project key would be reported. The rule is
// enforced by review for those cases.
func TestProjectIDLogKeyIsUniform(t *testing.T) {
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join("..", "..", dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, _ := sel.X.(*ast.Ident)
				if !logMethods[sel.Sel.Name] && (!attrCtors[sel.Sel.Name] || pkg == nil || pkg.Name != "slog") {
					return true
				}
				for _, a := range call.Args {
					if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if k, _ := strconv.Unquote(lit.Value); k != "project_id" && projectKeyVariant.MatchString(k) {
							t.Errorf("%s: log key %q should be \"project_id\" (issue #229)", fset.Position(lit.Pos()), k)
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}
