// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package log

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
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

// kvStart is, for each slog.Logger method and package-level slog function that takes a
// message and alternating key/value arguments, the index of its first key/value argument:
// Info(msg, args...), InfoContext(ctx, msg, args...), Log(ctx, level, msg, args...),
// With(args...). LogAttrs takes ...Attr only, so it has no entry: its attributes are
// slog.Attr expressions, which attrKeys reads where they are built.
var kvStart = map[string]int{
	"Debug": 1, "Info": 1, "Warn": 1, "Error": 1,
	"DebugContext": 2, "InfoContext": 2, "WarnContext": 2, "ErrorContext": 2,
	"Log": 3, "With": 0,
}

// attrCtors are the slog functions whose first argument is an attribute key.
var attrCtors = map[string]bool{
	"String": true, "Int": true, "Int64": true, "Uint64": true, "Float64": true,
	"Bool": true, "Time": true, "Duration": true, "Any": true, "Group": true,
}

// keyHit is one string-literal log attribute key and where it sits.
type keyHit struct {
	pos token.Pos
	key string
}

// TestProjectIDLogKeyIsUniform pins issue #229's decision so it does not erode: every
// string-literal log attribute key that names a project is spelled "project_id". A new
// log line logging the project as "project", "projectID" or a hashed "project_id_hash"
// fails here, rather than leaving the repo answering the question two ways.
//
// Only KEY positions are read (see logKeys): a message or a value that happens to be the
// word "project" is not a key and is not checked. Only string-literal keys are seen: a key
// held in a named constant or built at runtime is not. None exists today; keep project
// keys literal so this guard can see them.
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
			for _, h := range logKeys(f) {
				switch {
				case h.key == projectIDLogKey:
					canonical++
				case projectKeyVariant.MatchString(h.key):
					drift = append(drift, fset.Position(h.pos).String()+": "+strconv.Quote(h.key))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	for _, d := range drift {
		t.Errorf("log attribute key names the project but is not %q: %s", projectIDLogKey, d)
	}
	// 212 distinct project_id keys when #229 was settled; the floor only has to prove the
	// walk ran.
	if canonical < 100 {
		t.Fatalf("saw only %d %q log keys; the source walk is not reaching the log calls", canonical, projectIDLogKey)
	}
}

// TestLogKeys_ReadsKeyPositionsOnly pins the extractor itself, so the repo-wide guard
// neither rejects valid logging (a message or value that reads "project") nor lets a key
// escape through a slice, a spread or an elided slog.Attr literal.
func TestLogKeys_ReadsKeyPositionsOnly(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"message and value are not keys", `slog.Info("project", "kind", "project")`, []string{"kind"}},
		{"ctor value is not a key", `slog.String("project_id", "project")`, []string{"project_id"}},
		{"context method", `slog.InfoContext(ctx, "project", "projectID", x)`, []string{"projectID"}},
		{"Log skips ctx, level and message", `slog.Log(ctx, lvl, "project", "project_slug", x)`, []string{"project_slug"}},
		{"logger method with an attr then a pair", `logger.Warn("m", slog.String("projectID", x), "k", "project")`, []string{"projectID", "k"}},
		{"With is all pairs", `logger.With("projectId", x)`, []string{"projectId"}},
		{"group key and its pairs", `slog.Group("g", "project_uid", 1)`, []string{"g", "project_uid"}},
		{"slice spread through append", `fields := []any{"projectID", id, "brief_id", "project"}
			slog.WarnContext(ctx, "m", append(fields, "err", e)...)`, []string{"projectID", "brief_id", "err"}},
		{"slice grown by append then spread", `attrs := []any{"a", 1}
			attrs = append(attrs, "projectId", 2)
			slog.Info("m", attrs...)`, []string{"a", "projectId"}},
		{"elided slog.Attr in a slice", `slog.LogAttrs(ctx, lvl, "m", []slog.Attr{{Key: "projectID", Value: v}}...)`, []string{"projectID"}},
		{"explicit slog.Attr", `_ = slog.Attr{Key: "project", Value: v}`, []string{"project"}},
		{"another package's Error is not a log call", `http.Error(w, "project", 400)`, nil},
		{"an unrelated slice is not log fields", `args := []any{"project", 1}
			db.Query(q, args...)`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package p\nimport (\n\"log/slog\"\n\"net/http\"\n)\nvar _ = http.Error\nfunc f() {\n" + tc.body + "\n}\n"
			f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var got []string
			for _, h := range logKeys(f) {
				got = append(got, h.key)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("keys = %q, want %q", got, tc.want)
			}
		})
	}
}

// logKeys returns the distinct string-literal attribute keys a file passes to slog, in
// source order. A key is read only where slog reads one:
//   - in a log call's key/value arguments (after the message, per kvStart), a string
//     literal in key position; a slog.X(...) call or slog.Attr literal there is one
//     attribute and takes one slot; any other expression is taken as a non-literal key
//     and takes two (slog's own rule for a string-typed argument);
//   - the first argument of an attribute constructor (attrCtors), and slog.Group's pairs;
//   - the Key of a slog.Attr literal, including an element of a []slog.Attr literal whose
//     type is elided;
//   - the elements of a []any slice that reaches a log call through a spread (fields... or
//     append(fields, ...)...), resolved within the enclosing function.
func logKeys(f *ast.File) []keyHit {
	imports := map[string]string{} // local name -> import path
	for _, im := range f.Imports {
		path, _ := strconv.Unquote(im.Path.Value)
		name := path[strings.LastIndex(path, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		imports[name] = path
	}
	isSlog := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && imports[id.Name] == "log/slog"
	}
	isPkg := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && imports[id.Name] != ""
	}

	seen := map[token.Pos]bool{}
	var out []keyHit
	addKey := func(e ast.Expr) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || seen[lit.Pos()] {
			return
		}
		if key, err := strconv.Unquote(lit.Value); err == nil {
			seen[lit.Pos()] = true
			out = append(out, keyHit{lit.Pos(), key})
		}
	}
	isAttrExpr := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			return ok && isSlog(sel.X)
		case *ast.CompositeLit:
			sel, ok := x.Type.(*ast.SelectorExpr)
			return ok && isSlog(sel.X) && sel.Sel.Name == "Attr"
		}
		return false
	}
	attrLitKey := func(cl *ast.CompositeLit) {
		for i, e := range cl.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Key" {
					addKey(kv.Value)
				}
			} else if i == 0 {
				addKey(e) // positional slog.Attr{"key", value}
			}
		}
	}

	walkFunc := func(body ast.Node) {
		// sources: every expression assigned to a local identifier, so a spread can be
		// traced back to the []any literal and appends that built it.
		sources := map[string][]ast.Expr{}
		ast.Inspect(body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				if len(x.Lhs) == len(x.Rhs) {
					for i, l := range x.Lhs {
						if id, ok := l.(*ast.Ident); ok {
							sources[id.Name] = append(sources[id.Name], x.Rhs[i])
						}
					}
				}
			case *ast.ValueSpec:
				if len(x.Names) == len(x.Values) {
					for i, id := range x.Names {
						sources[id.Name] = append(sources[id.Name], x.Values[i])
					}
				}
			}
			return true
		})

		var kvWalk func(args []ast.Expr, spreadLast bool, visiting map[string]bool)
		var spread func(e ast.Expr, visiting map[string]bool)
		spread = func(e ast.Expr, visiting map[string]bool) {
			switch x := e.(type) {
			case *ast.Ident:
				if visiting[x.Name] {
					return
				}
				visiting[x.Name] = true
				for _, src := range sources[x.Name] {
					spread(src, visiting)
				}
				delete(visiting, x.Name)
			case *ast.CompositeLit:
				if at, ok := x.Type.(*ast.ArrayType); ok && isAnyType(at.Elt) {
					kvWalk(x.Elts, false, visiting)
				}
			case *ast.CallExpr:
				if fn, ok := x.Fun.(*ast.Ident); ok && fn.Name == "append" && len(x.Args) > 0 {
					spread(x.Args[0], visiting)
					kvWalk(x.Args[1:], x.Ellipsis.IsValid(), visiting)
				}
			}
		}
		kvWalk = func(args []ast.Expr, spreadLast bool, visiting map[string]bool) {
			for i := 0; i < len(args); {
				a := args[i]
				if spreadLast && i == len(args)-1 {
					spread(a, visiting)
					return
				}
				if isAttrExpr(a) {
					i++
					continue
				}
				addKey(a)
				i += 2
			}
		}

		ast.Inspect(body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := sel.Sel.Name
				switch {
				case isSlog(sel.X) && attrCtors[name]:
					if len(x.Args) > 0 {
						addKey(x.Args[0])
					}
					if name == "Group" && len(x.Args) > 1 {
						kvWalk(x.Args[1:], x.Ellipsis.IsValid(), map[string]bool{})
					}
				case isSlog(sel.X) || !isPkg(sel.X):
					if start, ok := kvStart[name]; ok && len(x.Args) > start {
						kvWalk(x.Args[start:], x.Ellipsis.IsValid(), map[string]bool{})
					}
				}
			case *ast.CompositeLit:
				switch t := x.Type.(type) {
				case *ast.SelectorExpr:
					if isSlog(t.X) && t.Sel.Name == "Attr" {
						attrLitKey(x)
					}
				case *ast.ArrayType:
					if sel, ok := t.Elt.(*ast.SelectorExpr); ok && isSlog(sel.X) && sel.Sel.Name == "Attr" {
						for _, e := range x.Elts {
							if cl, ok := e.(*ast.CompositeLit); ok && cl.Type == nil {
								attrLitKey(cl)
							}
						}
					}
				}
			}
			return true
		})
	}

	for _, d := range f.Decls {
		walkFunc(d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pos < out[j].pos })
	return out
}

// isAnyType reports whether e spells the empty interface: any or interface{}.
func isAnyType(e ast.Expr) bool {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name == "any"
	}
	it, ok := e.(*ast.InterfaceType)
	return ok && (it.Methods == nil || len(it.Methods.List) == 0)
}
