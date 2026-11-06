// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package log

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
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

// kvStart is, for each log/slog function or *slog.Logger method that takes alternating
// key/value arguments, the index of the first of them: Info(msg, args...),
// InfoContext(ctx, msg, args...), Log(ctx, level, msg, args...), With(args...). LogAttrs
// takes ...Attr only, so it has no entry: its attributes are slog.Attr values, read where
// they are built.
var kvStart = map[string]int{
	"Debug": 1, "Info": 1, "Warn": 1, "Error": 1,
	"DebugContext": 2, "InfoContext": 2, "WarnContext": 2, "ErrorContext": 2,
	"Log": 3, "With": 0,
}

// attrCtors are the log/slog functions whose first argument is an attribute key.
var attrCtors = map[string]bool{
	"String": true, "Int": true, "Int64": true, "Uint64": true, "Float64": true,
	"Bool": true, "Time": true, "Duration": true, "Any": true, "Group": true,
}

// keyHit is one constant log attribute key and where it is passed.
type keyHit struct {
	pos token.Pos
	key string
}

// TestProjectIDLogKeyIsUniform pins issue #229's decision so it does not erode: every
// constant log attribute key that names a project is spelled "project_id". A new log line
// logging the project as "project", "projectID" or a hashed "project_id_hash" fails here,
// rather than leaving the repo answering the question two ways.
//
// The source is type-checked (export data from `go list -export`), so a key is read by
// what slog does with each argument's TYPE, not by its spelling: see logKeys. A key built
// at runtime is not a constant and is not seen; keep project keys constant.
//
// The floor on how many project_id keys were seen is the positive control: a walk that
// silently checked nothing (a moved directory, a failed type-check) would otherwise pass
// vacuously.
func TestProjectIDLogKeyIsUniform(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	pkgs := goListExport(t, root, "./internal/...", "./cmd/...", "./pkg/...")
	exports := map[string]string{}
	for _, p := range pkgs {
		exports[p.ImportPath] = p.Export
	}
	imp := importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, errors.New("no export data for " + path)
		}
		return os.Open(f)
	})

	fset := token.NewFileSet()
	var canonical int
	var drift []string
	for _, p := range pkgs {
		if p.DepOnly {
			continue
		}
		if len(p.CgoFiles) > 0 {
			t.Fatalf("%s has cgo files; this guard type-checks GoFiles only", p.ImportPath)
		}
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, perr := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
			if perr != nil {
				t.Fatalf("parse: %v", perr)
			}
			files = append(files, f)
		}
		info := newInfo()
		if _, cerr := (&types.Config{Importer: imp}).Check(p.ImportPath, fset, files, info); cerr != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, cerr)
		}
		for _, f := range files {
			for _, h := range logKeys(f, info) {
				switch {
				case h.key == projectIDLogKey:
					canonical++
				case projectKeyVariant.MatchString(h.key):
					drift = append(drift, fset.Position(h.pos).String()+": "+strconv.Quote(h.key))
				}
			}
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
// escape through a slice, a spread, an Attr-valued argument or an elided slog.Attr.
func TestLogKeys_ReadsKeyPositionsOnly(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"message and value are not keys", `slog.Info("project", "kind", "project")`, []string{"kind"}},
		{"ctor value is not a key", `_ = slog.String("project_id", "project")`, []string{"project_id"}},
		{"context method", `slog.InfoContext(ctx, "project", "projectID", x)`, []string{"projectID"}},
		{"Log skips ctx, level and message", `slog.Log(ctx, lvl, "project", "project_slug", x)`, []string{"project_slug"}},
		{"logger method with an attr then a pair", `logger.Warn("m", slog.String("projectID", q), "k", "project")`, []string{"projectID", "k"}},
		{"With is all pairs", `_ = logger.With("projectId", x)`, []string{"projectId"}},
		{"group key and its pairs", `_ = slog.Group("g", "project_uid", 1)`, []string{"g", "project_uid"}},
		{"named constant key", `slog.Info("m", keyConst, x)`, []string{"projectID"}},
		{"non-string non-attr argument takes one slot", `slog.Info("m", 42, "project_slug", x)`, []string{"project_slug"}},
		{"attr variable takes one slot, key after it is read", `a := slog.Int("attempt", 1)
			slog.Info("m", a, "projectID", id)`, []string{"attempt", "projectID"}},
		{"attr variable takes one slot, value after it is not a key", `a := slog.Int("attempt", 1)
			slog.Info("m", a, "project_id", "project")`, []string{"attempt", "project_id"}},
		{"attr from a helper takes one slot", `slog.Info("m", errAttr(e), "project", id)`, []string{"project"}},
		{"slice spread through append", `fields := []any{"projectID", id, "brief_id", "project"}
			slog.WarnContext(ctx, "m", append(fields, "err", e)...)`, []string{"projectID", "brief_id", "err"}},
		{"slice grown by append then spread", `attrs := []any{"a", 1}
			attrs = append(attrs, "projectId", 2)
			slog.Info("m", attrs...)`, []string{"a", "projectId"}},
		{"shadowed slice: only the logged one is read", `args := []any{"project", 1}
			_, _ = db.Query(q, args...)
			{
				args := []any{"project_id", id}
				slog.Info("m", args...)
			}`, []string{"project_id"}},
		{"pair split across a slice and its append", `fields := []any{"kind"}
			slog.Info("m", append(fields, "ok", "projectID", id)...)`, []string{"kind", "projectID"}},
		{"pair split across a slice and a later append", `fields := []any{"a", 1, "b"}
			fields = append(fields, "ok", "projectID", id)
			slog.Info("m", fields...)`, []string{"a", "b", "projectID"}},
		{"conditionally replaced tail spread through append", `tail := []any{"kind", 1}
			if c {
				tail = []any{"projectID", id}
			}
			slog.Info("m", append([]any{}, tail...)...)`, []string{"kind", "projectID"}},
		{"conditionally replaced tail after a base", `extra := []any{"project_id", id}
			if c {
				extra = []any{"projectID", id}
			}
			slog.Info("m", append([]any{"event", "x"}, extra...)...)`, []string{"project_id", "projectID", "event"}},
		{"conditional self-append forks rather than extends", `fields := []any{"a", 1}
			if c {
				fields = append(fields, "projectId", 2)
			}
			slog.Info("m", fields...)`, []string{"a", "projectId"}},
		{"replacement discards earlier appends", `fields := []any{"a", 1}
			fields = append(fields, "unused")
			fields = []any{"kind", 1}
			fields = append(fields, "projectID", id)
			slog.Info("m", fields...)`, []string{"kind", "projectID"}},
		{"an append after the call is not part of it", `fields := []any{"a", 1}
			slog.Info("m", fields...)
			fields = append(fields, "projectID", id)
			_ = fields`, []string{"a"}},
		{"interface conversion is looked through", `slog.Info("m", any("project_id"), "project")`, []string{"project_id"}},
		{"elided slog.Attr in a slice", `slog.LogAttrs(ctx, lvl, "m", []slog.Attr{{Key: "projectID", Value: slog.AnyValue(v)}}...)`, []string{"projectID"}},
		{"explicit slog.Attr", `_ = slog.Attr{Key: "project", Value: slog.AnyValue(v)}`, []string{"project"}},
		{"another package's Error is not a log call", `http.Error(w, "project", 400)`, nil},
		{"an unrelated slice is not log fields", `args := []any{"project", 1}
			_, _ = db.Query(q, args...)`, nil},
	}
	const prelude = `package p
import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
)
var (
	ctx     context.Context
	lvl     slog.Level
	x, id, v any
	e       error
	w       http.ResponseWriter
	q       string
	db      *sql.DB
	logger  *slog.Logger
	errAttr func(error) slog.Attr
	c       bool
)
const keyConst = "projectID"
func f() {
`
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "p.go", prelude+tc.body+"\n}\n", 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			info := newInfo()
			if _, err := (&types.Config{Importer: importer.Default()}).Check("p", fset, []*ast.File{f}, info); err != nil {
				t.Fatalf("type-check: %v", err)
			}
			var got []string
			for _, h := range logKeys(f, info) {
				got = append(got, h.key)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("keys = %q, want %q", got, tc.want)
			}
		})
	}
}

// listedPkg is the part of `go list -json` output the guard reads.
type listedPkg struct {
	ImportPath, Export, Dir string
	GoFiles, CgoFiles       []string
	DepOnly                 bool
}

// goListExport lists the packages matching patterns plus their dependencies, with the
// path of each one's compiler export data, so the guard can type-check against it.
func goListExport(t *testing.T, root string, patterns ...string) []listedPkg {
	t.Helper()
	args := append([]string{"list", "-export", "-deps", "-json=ImportPath,Export,Dir,GoFiles,CgoFiles,DepOnly"}, patterns...)
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	var pkgs []listedPkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p listedPkg
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

func newInfo() *types.Info {
	return &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
}

// logKeys returns the distinct constant attribute keys a type-checked file passes to
// log/slog, in source order. A key is read where slog reads one, by argument type:
//   - in a log call's key/value arguments (from kvStart on), a string argument is a key
//     and takes two slots, a slog.Attr takes one, and anything else takes one (slog logs
//     it under !BADKEY);
//   - the first argument of an attribute constructor (attrCtors), and slog.Group's pairs;
//   - the Key of a slog.Attr composite literal, including one whose type is elided;
//   - the elements of a []any slice spread into a log call (fields... or
//     append(fields, ...)...), traced through every assignment to that same variable
//     (by object identity, so a shadowing variable is a different one).
//
// Calls are matched by the function they resolve to, so a same-named function or method
// of another package (http.Error, a builder's With) is not a log call.
func logKeys(f *ast.File, info *types.Info) []keyHit {
	seen := map[token.Pos]bool{}
	var out []keyHit
	addKey := func(e ast.Expr) {
		tv, ok := info.Types[e]
		if !ok || tv.Value == nil || tv.Value.Kind() != constant.String || seen[e.Pos()] {
			return
		}
		seen[e.Pos()] = true
		out = append(out, keyHit{e.Pos(), constant.StringVal(tv.Value)})
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
	objOf := func(id *ast.Ident) types.Object {
		if o := info.Defs[id]; o != nil {
			return o
		}
		return info.Uses[id]
	}

	// parents maps every node to its parent, so an assignment can be placed relative to
	// the log call it feeds.
	parents := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			parents[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})
	// dominates reports whether stmt sits directly in a statement list that encloses at,
	// so it runs on every path to at (straight-line code; an assignment in a nested
	// branch does not). It is what makes a reassignment replace the earlier contents.
	dominates := func(stmt, at ast.Node) bool {
		var list ast.Node
		for n := parents[stmt]; n != nil; n = parents[n] {
			switch n.(type) {
			case *ast.BlockStmt, *ast.CaseClause, *ast.CommClause:
				list = n
			}
			if list != nil {
				break
			}
		}
		for n := at; n != nil; n = parents[n] {
			if n == list {
				return true
			}
		}
		return false
	}

	// sources: every assignment to each variable, in source order, so a spread can be
	// traced back to the []any literals and appends that built it.
	type source struct {
		stmt ast.Node
		rhs  ast.Expr
	}
	sources := map[types.Object][]source{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == len(x.Rhs) {
				for i, l := range x.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						if o := objOf(id); o != nil {
							sources[o] = append(sources[o], source{x, x.Rhs[i]})
						}
					}
				}
			}
		case *ast.ValueSpec:
			if len(x.Names) == len(x.Values) {
				for i, id := range x.Names {
					if o := objOf(id); o != nil {
						sources[o] = append(sources[o], source{x, x.Values[i]})
					}
				}
			}
		}
		return true
	})

	// maxAlternatives bounds the sequences one spread can expand to; past it the extra
	// alternatives are dropped rather than letting a long chain of branches explode.
	const maxAlternatives = 1024
	cartesian := func(as, bs [][]ast.Expr) [][]ast.Expr {
		var out [][]ast.Expr
		for _, a := range as {
			for _, b := range bs {
				if len(out) == maxAlternatives {
					return out
				}
				out = append(out, append(append([]ast.Expr{}, a...), b...))
			}
		}
		return out
	}

	// flatten resolves an argument list whose last element may be a spread into every
	// sequence slog could see at node at. The whole sequence is resolved before anything
	// is classified, so a pair that straddles a slice and its append is paired as slog
	// pairs it, and every alternative a branch can produce is kept.
	var flatten func(args []ast.Expr, spreadLast bool, at ast.Node, visiting map[types.Object]bool) [][]ast.Expr
	var flattenSlice func(e ast.Expr, at ast.Node, visiting map[types.Object]bool) [][]ast.Expr
	flatten = func(args []ast.Expr, spreadLast bool, at ast.Node, visiting map[types.Object]bool) [][]ast.Expr {
		if !spreadLast || len(args) == 0 {
			return [][]ast.Expr{args}
		}
		return cartesian([][]ast.Expr{args[:len(args)-1]}, flattenSlice(args[len(args)-1], at, visiting))
	}
	// selfAppend reports whether e is append(o, ...), returning the call.
	selfAppend := func(e ast.Expr, o types.Object) (*ast.CallExpr, bool) {
		c, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok || len(c.Args) == 0 || !isAppend(c, info) {
			return nil, false
		}
		id, ok := ast.Unparen(c.Args[0]).(*ast.Ident)
		return c, ok && objOf(id) == o
	}
	flattenSlice = func(e ast.Expr, at ast.Node, visiting map[types.Object]bool) [][]ast.Expr {
		switch x := ast.Unparen(e).(type) {
		case *ast.Ident:
			o := objOf(x)
			if o == nil || visiting[o] {
				return [][]ast.Expr{nil}
			}
			visiting[o] = true
			defer delete(visiting, o)
			// Replay the assignments that precede at, in source order. One that dominates
			// at replaces (or, for an append, extends) every version so far; one in a
			// nested branch adds the result as an alternative beside them.
			versions := [][]ast.Expr{nil} // a parameter, or declared without a value
			for _, src := range sources[o] {
				if src.stmt.Pos() >= at.Pos() {
					continue
				}
				var next [][]ast.Expr
				if c, ok := selfAppend(src.rhs, o); ok {
					next = cartesian(versions, flatten(c.Args[1:], c.Ellipsis.IsValid(), src.stmt, visiting))
				} else {
					next = flattenSlice(src.rhs, src.stmt, visiting)
				}
				if dominates(src.stmt, at) {
					versions = next
				} else {
					versions = append(versions, next...)
					if len(versions) > maxAlternatives {
						versions = versions[:maxAlternatives]
					}
				}
			}
			return versions
		case *ast.CompositeLit:
			if isAnySlice(info.TypeOf(x)) {
				return [][]ast.Expr{x.Elts}
			}
		case *ast.CallExpr:
			if isAppend(x, info) && len(x.Args) > 0 {
				return cartesian(flattenSlice(x.Args[0], at, visiting), flatten(x.Args[1:], x.Ellipsis.IsValid(), at, visiting))
			}
		}
		return [][]ast.Expr{nil}
	}
	// classify pairs one flattened sequence as slog does: a string is a key and takes two
	// slots; a slog.Attr, or any other value (logged under !BADKEY), takes one. An explicit
	// conversion to an interface (any("k")) is looked through, because slog classifies
	// the dynamic value, not the static type.
	classify := func(seq []ast.Expr) {
		for i := 0; i < len(seq); {
			a := unwrapIfaceConv(seq[i], info)
			if b, ok := info.TypeOf(a).Underlying().(*types.Basic); ok && b.Info()&types.IsString != 0 {
				addKey(a)
				i += 2
				continue
			}
			i++
		}
	}
	kvWalk := func(call *ast.CallExpr, args []ast.Expr) {
		for _, seq := range flatten(args, call.Ellipsis.IsValid(), call, map[types.Object]bool{}) {
			classify(seq)
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			fn := slogFunc(x, info)
			if fn == nil {
				return true
			}
			name := fn.Name()
			isMethod := fn.Type().(*types.Signature).Recv() != nil
			if !isMethod && attrCtors[name] {
				if len(x.Args) > 0 {
					addKey(x.Args[0])
				}
				if name == "Group" && len(x.Args) > 1 {
					kvWalk(x, x.Args[1:])
				}
			} else if start, ok := kvStart[name]; ok && len(x.Args) > start {
				kvWalk(x, x.Args[start:])
			}
		case *ast.CompositeLit:
			if isSlogAttr(info.TypeOf(x)) {
				attrLitKey(x)
			}
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].pos < out[j].pos })
	return out
}

// isAppend reports whether call is the append builtin.
func isAppend(call *ast.CallExpr, info *types.Info) bool {
	fn, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if !ok {
		return false
	}
	b, ok := info.Uses[fn].(*types.Builtin)
	return ok && b.Name() == "append"
}

// unwrapIfaceConv looks through explicit conversions to an interface type, such as
// any("project_id"), returning the converted operand.
func unwrapIfaceConv(e ast.Expr, info *types.Info) ast.Expr {
	for {
		c, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok || len(c.Args) != 1 {
			return e
		}
		tv, ok := info.Types[c.Fun]
		if !ok || !tv.IsType() || !types.IsInterface(tv.Type) {
			return e
		}
		e = c.Args[0]
	}
}

// slogFunc returns the log/slog function or method call resolves to, or nil.
func slogFunc(call *ast.CallExpr, info *types.Info) *types.Func {
	var id *ast.Ident
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = fun
	case *ast.SelectorExpr:
		id = fun.Sel
	default:
		return nil
	}
	fn, ok := info.Uses[id].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "log/slog" {
		return nil
	}
	return fn
}

// isSlogAttr reports whether t is log/slog.Attr.
func isSlogAttr(t types.Type) bool {
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "log/slog" && n.Obj().Name() == "Attr"
}

// isAnySlice reports whether t is a slice of the empty interface ([]any).
func isAnySlice(t types.Type) bool {
	if t == nil {
		return false
	}
	s, ok := t.Underlying().(*types.Slice)
	if !ok {
		return false
	}
	it, ok := s.Elem().Underlying().(*types.Interface)
	return ok && it.Empty()
}
