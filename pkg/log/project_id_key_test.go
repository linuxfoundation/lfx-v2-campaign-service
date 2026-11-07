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

// attrCtors are the log/slog functions whose first argument is an attribute key (a group's
// name included). Logger.WithGroup, a method, is handled beside them.
var attrCtors = map[string]bool{
	"String": true, "Int": true, "Int64": true, "Uint64": true, "Float64": true,
	"Bool": true, "Time": true, "Duration": true, "Any": true, "Group": true, "GroupAttrs": true,
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
		decls := funcDecls(files, info)
		for _, f := range files {
			hits, over := logKeys(f, info, decls)
			if over {
				t.Errorf("%s: a log call's spread slice cannot be fully traced (more than %d paths, or a goto or an in-place []any element write in its function); simplify how the slice is built",
					fset.Position(f.Package).Filename, 1024)
			}
			for _, h := range hits {
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
		{"an alias of the slice is followed", `fields := []any{"projectID", id}
			copied := fields
			fields = append(copied, "err", e)
			slog.Info("m", fields...)`, []string{"projectID", "err"}},
		{"appends in one branch run in sequence", `fields := []any{"event", "x"}
			if c {
				fields = append(fields, "kind")
				fields = append(fields, "project")
			}
			slog.Info("m", fields...)`, []string{"event", "kind"}},
		{"a switch clause's appends join the other paths", `fields := []any{"a", 1}
			switch {
			case c:
				fields = append(fields, "projectID", id)
			default:
				fields = append(fields, "b", 2)
			}
			slog.Info("m", fields...)`, []string{"a", "projectID", "b"}},
		{"a log call inside a closure sees the state where the closure is", `fields := []any{"a", 1}
			func() {
				fields = append(fields, "projectID", id)
				slog.Info("m", fields...)
			}()`, []string{"a", "projectID"}},
		{"group names are keys", `_ = logger.WithGroup("project")
			_ = slog.GroupAttrs("projectID", slog.Int("n", 1))`, []string{"project", "projectID", "n"}},
		{"a for loop's initializer is traced", `for fields := []any{"projectID", id}; c; {
				slog.Info("m", fields...)
				break
			}`, []string{"projectID"}},
		{"a for initializer assigning an outer slice reaches a later call", `fields := []any{"a", 1}
			for fields = []any{"projectID", id}; c; {
				break
			}
			slog.Info("m", fields...)`, []string{"projectID"}},
		{"a break skips the loop's post statement", `fields := []any{"kind", 1}
			for ; c; fields = []any{"kind", 1} {
				fields = []any{"projectID", id}
				break
			}
			slog.Info("m", fields...)`, []string{"kind", "projectID"}},
		{"a break before the call in the body ends that path", `fields := []any{"a", 1}
			for c {
				if c {
					fields = []any{"project", id}
					break
				}
				slog.Info("m", fields...)
			}`, []string{"a"}},
		{"a panic ends its path", `fields := []any{"project_id", id}
			if c {
				fields = []any{"project", id}
				panic(fields)
			}
			slog.Info("m", fields...)`, []string{"project_id"}},
		{"fallthrough carries a clause's paths into the next", `fields := []any{"kind"}
			switch {
			case c:
				fields = append(fields, "ok")
				fallthrough
			default:
				fields = append(fields, "projectID", id)
			}
			slog.Info("m", fields...)`, []string{"kind", "projectID"}},
		{"a call in the clause after a fallthrough sees its paths", `fields := []any{"kind"}
			switch {
			case c:
				fields = append(fields, "ok")
				fallthrough
			default:
				slog.Info("m", append(fields, "projectID", id)...)
			}`, []string{"kind", "projectID"}},
		{"a labelled break leaves the outer loop", `fields := []any{"a", 1}
		Outer:
			for c {
				for c {
					fields = []any{"projectID", id}
					break Outer
				}
				fields = []any{"kind", 1}
			}
			slog.Info("m", fields...)`, []string{"a", "projectID", "kind"}},
		{"a labelled continue skips the rest of the outer body", `fields := []any{"a", 1}
		Outer:
			for c {
				for c {
					fields = []any{"projectID", id}
					continue Outer
				}
				fields = []any{"kind", 1}
			}
			slog.Info("m", fields...)`, []string{"a", "projectID", "kind"}},
		{"a same-package helper's result is traced", `slog.Info("m", helperFields(c)...)`, []string{"step", "projectID"}},
		{"make with length zero is empty", `byClass := make([]any, 0, 2)
			byClass = append(byClass, slog.Int("projectID", 1))
			_ = slog.Group("g", byClass...)`, []string{"projectID", "g"}},
		{"a full reslice is the slice", `fields := []any{"projectID", id}
			slog.Info("m", fields[:]...)`, []string{"projectID"}},
		{"a constant reslice keeps its window", `fields := []any{"x", 1, "projectID", id}
			slog.Info("m", fields[2:]...)`, []string{"projectID"}},
		{"a method expression's receiver is not an argument", `(*slog.Logger).Info(logger, "m", "projectID", id)`, []string{"projectID"}},
		{"a method expression's value is not a key", `(*slog.Logger).Info(logger, "m", "project_id", "project")`, []string{"project_id"}},
		{"a method expression WithGroup", `_ = (*slog.Logger).WithGroup(logger, "project")`, []string{"project"}},
		{"slices assigned together stay together", `fields := []any{"project_id"}
			tail := []any{"project"}
			if c {
				fields = []any{"event", "x"}
				tail = []any{"kind", 1}
			}
			slog.Info("m", append(fields, tail...)...)`, []string{"project_id", "event", "kind"}},
		{"a path that returned does not reach the call", `fields := []any{"project_id", id}
			if c {
				fields = []any{"project", id}
				_, _ = db.Query(q, fields...)
				return
			}
			slog.Info("m", fields...)`, []string{"project_id"}},
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
	mk      func() ([]any, error)
)
const keyConst = "projectID"
func helperFields(c bool) []any {
	attrs := []any{"step", 1}
	if c {
		attrs = append(attrs, "projectID", 2)
	}
	return attrs
}
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
			hits, over := logKeys(f, info, funcDecls([]*ast.File{f}, info))
			if over {
				t.Fatalf("spread could not be fully traced")
			}
			for _, h := range hits {
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
// log/slog, in source order, and whether a spread slice could not be fully traced (more
// paths than maxPaths, or a goto or in-place []any element write in its function), in which case the keys are incomplete
// and the caller must fail. A key is read where slog reads one, by argument type:
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
func logKeys(f *ast.File, info *types.Info, decls map[*types.Func]*ast.FuncDecl) ([]keyHit, bool) {
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

	// Spread slices are resolved by interpreting the statements of the enclosing function
	// up to the log call. The interpreter tracks whole PATHS: each path holds one concrete
	// sequence per []any variable, so slices assigned together on one branch stay
	// together, and an alias is followed. Statements run in order; an if, switch or select
	// runs each branch from the paths on entry and keeps the union of the paths that fall
	// through (a return ends a path); a loop body runs zero or one times, after a for
	// loop's initializer.
	const maxPaths = 1024
	// overflow: the keys are incomplete, because a distinct path was dropped at maxPaths
	// or a spread sits in a function with a goto or an in-place []any element write,
	// neither of which is modelled.
	overflow := false
	type path = map[types.Object][]ast.Expr
	clonePath := func(p path) path {
		out := make(path, len(p))
		for k, v := range p {
			out[k] = v
		}
		return out
	}
	clonePaths := func(ps []path) []path {
		out := make([]path, len(ps))
		for i, p := range ps {
			out[i] = clonePath(p)
		}
		return out
	}
	// union keeps distinct paths, bounded by maxPaths.
	union := func(groups ...[]path) []path {
		seen := map[string]bool{}
		var out []path
		for _, g := range groups {
			for _, p := range g {
				keys := make([]string, 0, len(p))
				for o, seq := range p {
					var k strings.Builder
					k.WriteString(strconv.Itoa(int(o.Pos())) + ":")
					for _, e := range seq {
						k.WriteString(strconv.Itoa(int(e.Pos())) + ",")
					}
					keys = append(keys, k.String())
				}
				sort.Strings(keys)
				key := strings.Join(keys, ";")
				if seen[key] {
					continue
				}
				if len(out) == maxPaths {
					overflow = true
					continue
				}
				seen[key] = true
				out = append(out, p)
			}
		}
		return out
	}

	type seqs = [][]ast.Expr
	cartesian := func(as, bs seqs) seqs {
		var out seqs
		for _, x := range as {
			for _, y := range bs {
				if len(out) == maxPaths {
					overflow = true
					return out
				}
				out = append(out, append(append([]ast.Expr{}, x...), y...))
			}
		}
		return out
	}
	// calleeResults is defined below, once pathsAt exists: the []any sequences a
	// same-package function can return.
	var calleeResults func(fn *types.Func) seqs

	// evalSlice returns the sequences a []any expression can hold on path p. It is one
	// sequence except where a call to a same-package function can return several.
	var evalSlice func(e ast.Expr, p path) seqs
	evalArgs := func(args []ast.Expr, spreadLast bool, p path) seqs {
		if !spreadLast || len(args) == 0 {
			return seqs{args}
		}
		return cartesian(seqs{args[:len(args)-1]}, evalSlice(args[len(args)-1], p))
	}
	evalSlice = func(e ast.Expr, p path) seqs {
		switch x := ast.Unparen(e).(type) {
		case *ast.Ident:
			return seqs{p[objOf(x)]}
		case *ast.CompositeLit:
			if isAnySlice(info.TypeOf(x)) {
				return seqs{x.Elts}
			}
		case *ast.CallExpr:
			if isAppend(x, info) && len(x.Args) > 0 {
				return cartesian(evalSlice(x.Args[0], p), evalArgs(x.Args[1:], x.Ellipsis.IsValid(), p))
			}
			if isBuiltin(x, info, "make") && isAnySlice(info.TypeOf(x)) && len(x.Args) >= 2 {
				if n, ok := constInt(x.Args[1], info); ok && n == 0 {
					return seqs{nil} // make([]any, 0, capacity): empty
				}
			}
			if fn := calledFunc(x, info); fn != nil && isAnySlice(info.TypeOf(x)) {
				if alts := calleeResults(fn); alts != nil {
					return alts
				}
			}
		case *ast.SliceExpr:
			// fields[lo:hi] with constant bounds is resolved exactly; anything else is not.
			var out seqs
			for _, seq := range evalSlice(x.X, p) {
				lo, hi := 0, len(seq)
				okLo, okHi := true, true
				if x.Low != nil {
					lo, okLo = constInt(x.Low, info)
				}
				if x.High != nil {
					hi, okHi = constInt(x.High, info)
				}
				if okLo && okHi && !x.Slice3 && 0 <= lo && lo <= hi && hi <= len(seq) && !hasUntraced(seq) {
					out = append(out, seq[lo:hi])
				} else {
					out = append(out, []ast.Expr{untraced})
				}
			}
			return out
		}
		if isAnySlice(info.TypeOf(e)) {
			// A call outside the package, a field, an element of [][]any, ...: not modelled.
			return seqs{{untraced}}
		}
		return seqs{nil}
	}

	// assign returns the paths after lhs = rhs. A right-hand side with several possible
	// sequences splits the path.
	assign := func(lhs []ast.Expr, rhs []ast.Expr, ps []path) []path {
		var out []path
		for _, p := range ps {
			vals := make([]seqs, len(lhs))
			for i := range vals {
				vals[i] = seqs{{untraced}} // a multi-value call's results are not modelled
			}
			if len(lhs) == len(rhs) {
				for i := range lhs {
					vals[i] = evalSlice(rhs[i], p)
				}
			}
			next := []path{clonePath(p)}
			for i, l := range lhs {
				id, ok := l.(*ast.Ident)
				if !ok {
					continue
				}
				o := objOf(id)
				if o == nil || !isAnySlice(o.Type()) {
					continue
				}
				var split []path
				for _, np := range next {
					for _, v := range vals[i] {
						c := clonePath(np)
						c[o] = v
						split = append(split, c)
					}
				}
				next = split
			}
			out = append(out, next...)
		}
		return union(out)
	}
	// flow is where the paths leaving a statement go: on to the next statement, out of a
	// loop or switch (break), or to a loop's next iteration (continue). brk and cont are
	// keyed by label, "" for an unlabeled break or continue, which targets the innermost
	// loop (or, for break, switch). A return or panic sends a path nowhere.
	type flow struct {
		next      []path
		brk, cont map[string][]path
	}
	mergeInto := func(dst *map[string][]path, src map[string][]path) {
		for k, v := range src {
			if *dst == nil {
				*dst = map[string][]path{}
			}
			(*dst)[k] = union((*dst)[k], v)
		}
	}
	// take removes and returns the paths aimed at "" and at label.
	take := func(m map[string][]path, label string) []path {
		out := m[""]
		delete(m, "")
		if label != "" {
			out = union(out, m[label])
			delete(m, label)
		}
		return out
	}
	var execL func(s ast.Stmt, ps []path, label string) flow
	execF := func(s ast.Stmt, ps []path) flow { return execL(s, ps, "") }
	execListF := func(list []ast.Stmt, ps []path) flow {
		var out flow
		for _, s := range list {
			f := execF(s, ps)
			mergeInto(&out.brk, f.brk)
			mergeInto(&out.cont, f.cont)
			ps = f.next
		}
		out.next = ps
		return out
	}
	exec := func(s ast.Stmt, ps []path) []path { return execF(s, ps).next }
	// iterate runs one iteration of a loop (labelled label, or "") from ps. It returns the
	// paths that break out of this loop, the paths that start another iteration (those
	// that fall through or continue this loop, after the post statement; a break skips
	// it), and the breaks and continues aimed at an outer loop, which pass through.
	iterate := func(body *ast.BlockStmt, post ast.Stmt, ps []path, label string) (brk, again []path, outer flow) {
		it := execListF(body.List, clonePaths(ps))
		brk = take(it.brk, label)
		again = union(it.next, take(it.cont, label))
		if post != nil {
			again = exec(post, again)
		}
		return brk, again, flow{brk: it.brk, cont: it.cont}
	}
	// clauses returns a switch or select statement's init and clause bodies, and whether
	// one clause always runs (a default, or any select).
	clauses := func(s ast.Stmt) (init ast.Stmt, bodies [][]ast.Stmt, nodes []ast.Node, always bool) {
		var body *ast.BlockStmt
		switch y := s.(type) {
		case *ast.SwitchStmt:
			init, body = y.Init, y.Body
		case *ast.TypeSwitchStmt:
			init, body = y.Init, y.Body
		case *ast.SelectStmt:
			body, always = y.Body, true
		}
		for _, c := range body.List {
			switch cc := c.(type) {
			case *ast.CaseClause:
				always = always || cc.List == nil
				bodies, nodes = append(bodies, cc.Body), append(nodes, cc)
			case *ast.CommClause:
				bodies, nodes = append(bodies, cc.Body), append(nodes, cc)
			}
		}
		return init, bodies, nodes, always
	}
	execL = func(s ast.Stmt, ps []path, label string) flow {
		switch x := s.(type) {
		case *ast.AssignStmt:
			ps = assign(x.Lhs, x.Rhs, ps)
		case *ast.DeclStmt:
			if gd, ok := x.Decl.(*ast.GenDecl); ok {
				for _, sp := range gd.Specs {
					if vs, ok := sp.(*ast.ValueSpec); ok {
						lhs := make([]ast.Expr, len(vs.Names))
						for i, n := range vs.Names {
							lhs[i] = n
						}
						ps = assign(lhs, vs.Values, ps)
					}
				}
			}
		case *ast.ReturnStmt:
			return flow{} // this path does not reach anything after it
		case *ast.ExprStmt:
			if c, ok := x.X.(*ast.CallExpr); ok {
				if id, ok := ast.Unparen(c.Fun).(*ast.Ident); ok {
					if b, ok := info.Uses[id].(*types.Builtin); ok && b.Name() == "panic" {
						return flow{}
					}
				}
			}
		case *ast.BranchStmt:
			target := ""
			if x.Label != nil {
				target = x.Label.Name
			}
			switch x.Tok {
			case token.BREAK:
				return flow{brk: map[string][]path{target: ps}}
			case token.CONTINUE:
				return flow{cont: map[string][]path{target: ps}}
			}
			// fallthrough is handled by the switch; goto never reaches here, because a
			// spread in a function containing one is reported as untraceable (kvWalk).
		case *ast.BlockStmt:
			return execListF(x.List, ps)
		case *ast.LabeledStmt:
			return execL(x.Stmt, ps, x.Label.Name)
		case *ast.IfStmt:
			if x.Init != nil {
				ps = exec(x.Init, ps)
			}
			thenF := execListF(x.Body.List, clonePaths(ps))
			elseF := flow{next: clonePaths(ps)}
			if x.Else != nil {
				elseF = execF(x.Else, elseF.next)
			}
			out := flow{next: union(thenF.next, elseF.next)}
			for _, f := range []flow{thenF, elseF} {
				mergeInto(&out.brk, f.brk)
				mergeInto(&out.cont, f.cont)
			}
			return out
		case *ast.ForStmt:
			if x.Init != nil {
				ps = exec(x.Init, ps)
			}
			brk, again, outer := iterate(x.Body, x.Post, ps, label)
			outer.next = union(ps, brk, again)
			return outer
		case *ast.RangeStmt:
			brk, again, outer := iterate(x.Body, nil, ps, label)
			outer.next = union(ps, brk, again)
			return outer
		case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			init, bodies, _, always := clauses(x)
			if init != nil {
				ps = exec(init, ps)
			}
			var out flow
			var ft []path // paths that fell through from the clause before
			for _, b := range bodies {
				f := execListF(b, union(clonePaths(ps), ft))
				ft = nil
				if endsInFallthrough(b) {
					ft = f.next // they enter the next clause, not the code after the switch
				} else {
					out.next = union(out.next, f.next)
				}
				out.next = union(out.next, take(f.brk, label)) // a break leaves the switch
				mergeInto(&out.brk, f.brk)
				mergeInto(&out.cont, f.cont)
			}
			if !always {
				out.next = union(out.next, ps)
			}
			return out
		}
		return flow{next: ps}
	}
	contains := func(n, target ast.Node) bool {
		return n != nil && !reflect.ValueOf(n).IsNil() && n.Pos() <= target.Pos() && target.End() <= n.End()
	}
	// pathsAt returns the paths on entry to target, descending only into the statements
	// on the way to it.
	var pathsAt func(list []ast.Stmt, ps []path, target ast.Node) []path
	pathsAt = func(list []ast.Stmt, ps []path, target ast.Node) []path {
		for _, s := range list {
			if !contains(s, target) {
				ps = exec(s, ps)
				continue
			}
			loopLabel := ""
			if ls, ok := s.(*ast.LabeledStmt); ok {
				loopLabel, s = ls.Label.Name, ls.Stmt
			}
			switch x := s.(type) {
			case *ast.BlockStmt:
				return pathsAt(x.List, ps, target)
			case *ast.IfStmt:
				if x.Init != nil && !contains(x.Init, target) {
					ps = exec(x.Init, ps)
				}
				if contains(x.Body, target) {
					return pathsAt(x.Body.List, ps, target)
				}
				if contains(x.Else, target) {
					return pathsAt([]ast.Stmt{x.Else}, ps, target)
				}
			case *ast.ForStmt:
				if x.Init != nil && !contains(x.Init, target) {
					ps = exec(x.Init, ps)
				}
				if contains(x.Body, target) {
					_, again, _ := iterate(x.Body, x.Post, ps, loopLabel)
					return pathsAt(x.Body.List, union(ps, again), target)
				}
			case *ast.RangeStmt:
				if contains(x.Body, target) {
					_, again, _ := iterate(x.Body, nil, ps, loopLabel)
					return pathsAt(x.Body.List, union(ps, again), target)
				}
			case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
				init, bodies, nodes, _ := clauses(x)
				if init != nil && !contains(init, target) {
					ps = exec(init, ps)
				}
				var ft []path
				for i, n := range nodes {
					entry := union(clonePaths(ps), ft)
					if contains(n, target) {
						return pathsAt(bodies[i], entry, target)
					}
					ft = nil
					if endsInFallthrough(bodies[i]) {
						ft = execListF(bodies[i], entry).next
					}
				}
			}
			// The target is in this statement itself, or in a closure inside it, which
			// sees the paths as they are here.
			var lit *ast.FuncLit
			ast.Inspect(s, func(n ast.Node) bool {
				if fl, ok := n.(*ast.FuncLit); ok && contains(fl, target) {
					lit = fl
				}
				return lit == nil
			})
			if lit != nil {
				return pathsAt(lit.Body.List, ps, target)
			}
			return ps
		}
		return ps
	}
	// enclosing returns the statement list of the top-level function containing n.
	enclosing := func(n ast.Node) []ast.Stmt {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && contains(fd.Body, n) {
				return fd.Body.List
			}
		}
		return nil
	}

	// calleeResults interprets a same-package function's body up to each of its
	// single-result return statements and collects the sequences it can return. Its
	// parameters are unknown (empty). nil means the function is not one the guard can
	// read (another package, a method value, a bare return); a result it cannot fully
	// trace (recursion, goto, element writes) yields an untraced sequence.
	calleeMemo := map[*types.Func]seqs{}
	calleeBusy := map[*types.Func]bool{}
	calleeResults = func(fn *types.Func) seqs {
		fd := decls[fn]
		if fd == nil || fd.Body == nil {
			return nil
		}
		if alts, ok := calleeMemo[fn]; ok {
			return alts
		}
		if calleeBusy[fn] || hasGoto(fd.Body.List) || writesAnyElements(fd.Body.List, info) {
			return seqs{{untraced}}
		}
		calleeBusy[fn] = true
		defer delete(calleeBusy, fn)
		var out seqs
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false // a closure's returns are its own
			}
			ret, ok := n.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			if len(ret.Results) != 1 {
				out = append(out, []ast.Expr{untraced}) // a bare or multi-value return
				return true
			}
			for _, p := range pathsAt(fd.Body.List, []path{{}}, ret) {
				out = append(out, evalSlice(ret.Results[0], p)...)
			}
			return true
		})
		if out == nil {
			out = seqs{{untraced}}
		}
		calleeMemo[fn] = out
		return out
	}

	// classify pairs one flattened sequence as slog does: a string is a key and takes two
	// slots; a slog.Attr, or any other value (logged under !BADKEY), takes one. An explicit
	// conversion to an interface (any("k")) is looked through, because slog classifies
	// the dynamic value, not the static type.
	classify := func(seq []ast.Expr) {
		if hasUntraced(seq) {
			overflow = true
			return
		}
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
		ps := []path{{}}
		if call.Ellipsis.IsValid() {
			body := enclosing(call)
			if hasGoto(body) || writesAnyElements(body, info) {
				overflow = true
				return
			}
			ps = pathsAt(body, ps, call)
		}
		for _, p := range ps {
			for _, seq := range evalArgs(args, call.Ellipsis.IsValid(), p) {
				classify(seq)
			}
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
			args := x.Args
			if isMethod && isMethodExpr(x, info) && len(args) > 0 {
				args = args[1:] // (*slog.Logger).Info(logger, ...): drop the explicit receiver
			}
			if isMethod && name == "WithGroup" && len(args) > 0 {
				addKey(args[0])
			} else if !isMethod && attrCtors[name] {
				if len(args) > 0 {
					addKey(args[0])
				}
				if name == "Group" && len(args) > 1 {
					kvWalk(x, args[1:])
				}
			} else if start, ok := kvStart[name]; ok && len(args) > start {
				kvWalk(x, args[start:])
			}
		case *ast.CompositeLit:
			if isSlogAttr(info.TypeOf(x)) {
				attrLitKey(x)
			}
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].pos < out[j].pos })
	return out, overflow
}

// hasGoto reports whether a function body contains a goto statement.
func hasGoto(body []ast.Stmt) bool {
	found := false
	for _, s := range body {
		ast.Inspect(s, func(n ast.Node) bool {
			if b, ok := n.(*ast.BranchStmt); ok && b.Tok == token.GOTO {
				found = true
			}
			return !found
		})
	}
	return found
}

// writesAnyElements reports whether a function body writes elements of a []any in place
// (fields[i] = v, copy(fields, ...), clear(fields)), which the interpreter does not model.
func writesAnyElements(body []ast.Stmt, info *types.Info) bool {
	found := false
	for _, s := range body {
		ast.Inspect(s, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					if ix, ok := ast.Unparen(l).(*ast.IndexExpr); ok && isAnySlice(info.TypeOf(ix.X)) {
						found = true
					}
				}
			case *ast.CallExpr:
				if fn, ok := ast.Unparen(x.Fun).(*ast.Ident); ok && len(x.Args) > 0 {
					if b, ok := info.Uses[fn].(*types.Builtin); ok && (b.Name() == "copy" || b.Name() == "clear") &&
						isAnySlice(info.TypeOf(x.Args[0])) {
						found = true
					}
				}
			}
			return !found
		})
	}
	return found
}

// funcDecls maps each function and method declared in files to its declaration, so the
// guard can read a same-package helper that builds a []any it returns.
func funcDecls(files []*ast.File, info *types.Info) map[*types.Func]*ast.FuncDecl {
	out := map[*types.Func]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				if fn, ok := info.Defs[fd.Name].(*types.Func); ok {
					out[fn] = fd
				}
			}
		}
	}
	return out
}

// calledFunc returns the declared function or method call invokes, or nil (a builtin,
// a conversion, a func-typed variable).
func calledFunc(call *ast.CallExpr, info *types.Info) *types.Func {
	var id *ast.Ident
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = fun
	case *ast.SelectorExpr:
		id = fun.Sel
	default:
		return nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	return fn
}

// isBuiltin reports whether call is the named builtin.
func isBuiltin(call *ast.CallExpr, info *types.Info, name string) bool {
	fn, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if !ok {
		return false
	}
	b, ok := info.Uses[fn].(*types.Builtin)
	return ok && b.Name() == name
}

// isMethodExpr reports whether call invokes a method expression such as
// (*slog.Logger).Info, whose receiver is passed as the first argument.
func isMethodExpr(call *ast.CallExpr, info *types.Info) bool {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	tv, ok := info.Types[sel.X]
	return ok && tv.IsType()
}

// untraced stands, inside a traced sequence, for slice contents the interpreter does not
// model (a call result, a non-constant reslice, ...). A sequence holding it makes the
// spread untraceable, so the guard fails rather than reading the contents as empty.
var untraced ast.Expr = &ast.BadExpr{}

func hasUntraced(seq []ast.Expr) bool {
	for _, e := range seq {
		if e == untraced {
			return true
		}
	}
	return false
}

// constInt returns e's value when it is an integer constant.
func constInt(e ast.Expr, info *types.Info) (int, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil {
		return 0, false
	}
	v, ok := constant.Int64Val(constant.ToInt(tv.Value))
	return int(v), ok
}

// endsInFallthrough reports whether a case clause body ends in a fallthrough statement.
func endsInFallthrough(body []ast.Stmt) bool {
	if len(body) == 0 {
		return false
	}
	b, ok := body[len(body)-1].(*ast.BranchStmt)
	return ok && b.Tok == token.FALLTHROUGH
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

// TestLogKeys_PathOverflowIsReported pins that the path bound never drops a branch
// silently: eleven independent conditional appends make 2048 paths, past the bound, and
// logKeys says so rather than checking an arbitrary subset.
func TestLogKeys_PathOverflowIsReported(t *testing.T) {
	var body strings.Builder
	body.WriteString("fields := []any{\"a\", 1}\n")
	for i := 0; i < 11; i++ {
		body.WriteString("if c {\nfields = append(fields, \"k\", 1)\n}\n")
	}
	body.WriteString("slog.Info(\"m\", fields...)\n")
	src := "package p\nimport \"log/slog\"\nvar c bool\nfunc f() {\n" + body.String() + "}\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info := newInfo()
	if _, err := (&types.Config{Importer: importer.Default()}).Check("p", fset, []*ast.File{f}, info); err != nil {
		t.Fatalf("type-check: %v", err)
	}
	if _, over := logKeys(f, info, funcDecls([]*ast.File{f}, info)); !over {
		t.Fatal("2048 paths did not report an overflow")
	}
}

// TestLogKeys_UnmodelledSlicesAreReportedUntraceable pins that a slice change the
// interpreter does not model (an in-place element write, copy, clear, a multi-value or
// out-of-package call result, a non-constant reslice) makes the spread untraceable
// instead of being checked as it was before, or read as empty.
func TestLogKeys_UnmodelledSlicesAreReportedUntraceable(t *testing.T) {
	for name, stmt := range map[string]string{
		"index assignment": `fields[0] = "projectID"`,
		"copy":             `copy(fields, []any{"projectID"})`,
		"clear":            `clear(fields)`,
		"multi-value call": `fields, _ = mk()`,
		"call result":      `fields = mk1()`,
		"variable reslice": `fields = fields[n:]`,
	} {
		t.Run(name, func(t *testing.T) {
			src := "package p\nimport \"log/slog\"\nvar (\nid any\nn int\nmk func() ([]any, error)\nmk1 func() []any\n)\nfunc f() {\nfields := []any{\"kind\", id}\n" +
				stmt + "\nslog.Info(\"m\", fields...)\n}\n"
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "p.go", src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			info := newInfo()
			if _, err := (&types.Config{Importer: importer.Default()}).Check("p", fset, []*ast.File{f}, info); err != nil {
				t.Fatalf("type-check: %v", err)
			}
			if _, over := logKeys(f, info, funcDecls([]*ast.File{f}, info)); !over {
				t.Fatal("an in-place element write was not reported as untraceable")
			}
		})
	}
}

// TestLogKeys_GotoIsReportedUntraceable pins that a goto, whose jump the interpreter does
// not model, makes a spread in its function untraceable instead of being taken to fall
// through (which would let an assignment the jump skips hide the key it jumped with).
func TestLogKeys_GotoIsReportedUntraceable(t *testing.T) {
	src := `package p
import "log/slog"
var (
	c  bool
	id any
)
func f() {
	fields := []any{"a", 1}
	if c {
		fields = []any{"projectID", id}
		goto L
	}
	fields = []any{"kind", 1}
L:
	slog.Info("m", fields...)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info := newInfo()
	if _, err := (&types.Config{Importer: importer.Default()}).Check("p", fset, []*ast.File{f}, info); err != nil {
		t.Fatalf("type-check: %v", err)
	}
	if _, over := logKeys(f, info, funcDecls([]*ast.File{f}, info)); !over {
		t.Fatal("a spread in a function with a goto was not reported as untraceable")
	}
}
