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

// projectIDLogKey is the one structured-log attribute key for the project a log line is
// about. Issue #229 settled it: project_id stays in log attributes, under exactly this
// name, everywhere. docs/knowledge/architecture/logging-conventions.md records why.
const projectIDLogKey = "project_id"

// projectKeyVariant matches the spellings a project identifier could drift into as a log
// key: project, projectID, projectId, project-id, project_uid, project_slug, proj_id, and
// a hashed project_id_hash. Only projectIDLogKey itself is allowed.
var projectKeyVariant = regexp.MustCompile(`(?i)^proj(ect)?[_-]?(id|uid|uuid|slug|key)?([_-]?hash)?$`)

// logMethods are the slog.Logger methods (and package-level slog functions) whose
// arguments carry attribute keys. Any call through package slog itself counts as well,
// which covers slog.String, slog.Any, slog.Group and friends.
var logMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
	"Log": true, "LogAttrs": true, "With": true,
}

// TestProjectIDLogKeyIsUniform pins issue #229's decision so it does not erode: every
// string-literal log attribute key that names a project is spelled "project_id". A new
// log line logging the project as "project", "projectID" or a hashed "project_id_hash"
// fails here, rather than leaving the repo answering the question two ways.
//
// The floor on how many project_id keys were seen is the positive control: a walker that
// silently scanned nothing (a moved directory, a broken selector match) would otherwise
// pass vacuously.
func TestProjectIDLogKeyIsUniform(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var canonical int
	var drift []string

	for _, dir := range []string{"internal", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				for _, lit := range logKeyLiterals(n) {
					key, uerr := strconv.Unquote(lit.Value)
					if uerr != nil {
						continue
					}
					switch {
					case key == projectIDLogKey:
						canonical++
					case projectKeyVariant.MatchString(key):
						drift = append(drift, fset.Position(lit.Pos()).String()+": "+lit.Value)
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

	for _, d := range drift {
		t.Errorf("log attribute key names the project but is not %q: %s", projectIDLogKey, d)
	}
	// 131 non-test sites when #229 was filed; the floor only has to prove the walk ran.
	if canonical < 100 {
		t.Fatalf("saw only %d %q log keys; the source walk is not reaching the log calls", canonical, projectIDLogKey)
	}
}

// logKeyLiterals returns the string literals n passes where a log attribute key can sit:
// the arguments of a log call, or the Key of an slog.Attr composite literal.
func logKeyLiterals(n ast.Node) []*ast.BasicLit {
	var out []*ast.BasicLit
	switch x := n.(type) {
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok {
			return nil
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		if !logMethods[sel.Sel.Name] && (!isIdent || pkg.Name != "slog") {
			return nil
		}
		for _, a := range x.Args {
			if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				out = append(out, lit)
			}
		}
	case *ast.CompositeLit:
		sel, ok := x.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Attr" {
			return nil
		}
		for _, e := range x.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Key" {
				if lit, ok := kv.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					out = append(out, lit)
				}
			}
		}
	}
	return out
}
