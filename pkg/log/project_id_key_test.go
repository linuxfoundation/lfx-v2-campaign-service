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
		{"a multi-value assignment makes the slice unknown", `fields := []any{"projectID", id}
			fields, e = mk()
			slog.Info("m", fields...)`, nil},
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

	// Spread slices are resolved by interpreting the statements of the enclosing function
	// up to the log call: every []any variable maps to the sequences it may hold there.
	// Statements run in order; an if, switch or select runs each branch from the state on
	// entry and joins the branch exits (plus the entry state when no branch need run); a
	// loop body runs zero or one times. Assignments through other variables (aliases) are
	// followed because every []any variable is tracked.
	const maxAlternatives = 1024
	type seqs = [][]ast.Expr
	type state = map[types.Object]seqs
	cartesian := func(as, bs seqs) seqs {
		var out seqs
		for _, x := range as {
			for _, y := range bs {
				if len(out) == maxAlternatives {
					return out
				}
				out = append(out, append(append([]ast.Expr{}, x...), y...))
			}
		}
		return out
	}
	clone := func(st state) state {
		out := make(state, len(st))
		for k, v := range st {
			out[k] = v
		}
		return out
	}
	join := func(sts ...state) state {
		out := state{}
		seen := map[types.Object]map[string]bool{}
		for _, st := range sts {
			for o, alts := range st {
				if seen[o] == nil {
					seen[o] = map[string]bool{}
				}
				for _, alt := range alts {
					var key strings.Builder
					for _, e := range alt {
						key.WriteString(strconv.Itoa(int(e.Pos())) + ",")
					}
					if !seen[o][key.String()] && len(out[o]) < maxAlternatives {
						seen[o][key.String()] = true
						out[o] = append(out[o], alt)
					}
				}
			}
		}
		// A variable assigned on only some paths may still hold its earlier value (or
		// nothing) on the others.
		for o := range out {
			for _, st := range sts {
				if _, ok := st[o]; !ok {
					out[o] = append(out[o], nil)
					break
				}
			}
		}
		return out
	}

	var evalSlice func(e ast.Expr, st state) seqs
	evalArgs := func(args []ast.Expr, spreadLast bool, st state) seqs {
		if !spreadLast || len(args) == 0 {
			return seqs{args}
		}
		return cartesian(seqs{args[:len(args)-1]}, evalSlice(args[len(args)-1], st))
	}
	evalSlice = func(e ast.Expr, st state) seqs {
		switch x := ast.Unparen(e).(type) {
		case *ast.Ident:
			if alts, ok := st[objOf(x)]; ok {
				return alts
			}
		case *ast.CompositeLit:
			if isAnySlice(info.TypeOf(x)) {
				return seqs{x.Elts}
			}
		case *ast.CallExpr:
			if isAppend(x, info) && len(x.Args) > 0 {
				return cartesian(evalSlice(x.Args[0], st), evalArgs(x.Args[1:], x.Ellipsis.IsValid(), st))
			}
		}
		return seqs{nil} // a parameter, a call result, or anything else not traced
	}

	assign := func(lhs []ast.Expr, rhs []ast.Expr, st state) {
		vals := make([]seqs, len(lhs))
		for i := range lhs {
			vals[i] = seqs{nil} // a multi-value call's results are not traced
			if len(lhs) == len(rhs) {
				vals[i] = evalSlice(rhs[i], st)
			}
		}
		for i, l := range lhs {
			if id, ok := l.(*ast.Ident); ok {
				if o := objOf(id); o != nil && isAnySlice(o.Type()) {
					st[o] = vals[i]
				}
			}
		}
	}
	var exec func(s ast.Stmt, st state) state
	execList := func(list []ast.Stmt, st state) state {
		for _, s := range list {
			st = exec(s, st)
		}
		return st
	}
	// loopOnce runs one iteration of a for loop's body and post statement.
	loopOnce := func(x *ast.ForStmt, st state) state {
		out := execList(x.Body.List, clone(st))
		if x.Post != nil {
			out = exec(x.Post, out)
		}
		return out
	}
	exec = func(s ast.Stmt, st state) state {
		switch x := s.(type) {
		case *ast.AssignStmt:
			assign(x.Lhs, x.Rhs, st)
		case *ast.DeclStmt:
			if gd, ok := x.Decl.(*ast.GenDecl); ok {
				for _, sp := range gd.Specs {
					if vs, ok := sp.(*ast.ValueSpec); ok {
						lhs := make([]ast.Expr, len(vs.Names))
						for i, n := range vs.Names {
							lhs[i] = n
						}
						assign(lhs, vs.Values, st)
					}
				}
			}
		case *ast.BlockStmt:
			return execList(x.List, st)
		case *ast.LabeledStmt:
			return exec(x.Stmt, st)
		case *ast.IfStmt:
			if x.Init != nil {
				st = exec(x.Init, st)
			}
			thenSt := execList(x.Body.List, clone(st))
			elseSt := clone(st)
			if x.Else != nil {
				elseSt = exec(x.Else, elseSt)
			}
			return join(thenSt, elseSt)
		case *ast.ForStmt:
			if x.Init != nil {
				st = exec(x.Init, st)
			}
			return join(st, loopOnce(x, st))
		case *ast.RangeStmt:
			return join(st, execList(x.Body.List, clone(st)))
		case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			var body *ast.BlockStmt
			switch y := x.(type) {
			case *ast.SwitchStmt:
				if y.Init != nil {
					st = exec(y.Init, st)
				}
				body = y.Body
			case *ast.TypeSwitchStmt:
				if y.Init != nil {
					st = exec(y.Init, st)
				}
				body = y.Body
			case *ast.SelectStmt:
				body = y.Body
			}
			exits := []state{}
			hasDefault := false
			for _, c := range body.List {
				switch cc := c.(type) {
				case *ast.CaseClause:
					hasDefault = hasDefault || cc.List == nil
					exits = append(exits, execList(cc.Body, clone(st)))
				case *ast.CommClause:
					hasDefault = true // a select always runs one clause
					exits = append(exits, execList(cc.Body, clone(st)))
				}
			}
			if !hasDefault {
				exits = append(exits, st)
			}
			return join(exits...)
		}
		return st
	}
	contains := func(n, target ast.Node) bool {
		return n != nil && n.Pos() <= target.Pos() && target.End() <= n.End()
	}
	// stateAt returns the state on entry to target, descending only into the statements
	// on the path to it.
	var stateAt func(list []ast.Stmt, st state, target ast.Node) state
	stateAt = func(list []ast.Stmt, st state, target ast.Node) state {
		for _, s := range list {
			if !contains(s, target) {
				st = exec(s, st)
				continue
			}
			switch x := s.(type) {
			case *ast.BlockStmt:
				return stateAt(x.List, st, target)
			case *ast.LabeledStmt:
				return stateAt([]ast.Stmt{x.Stmt}, st, target)
			case *ast.IfStmt:
				if x.Init != nil && !contains(x.Init, target) {
					st = exec(x.Init, st)
				}
				if contains(x.Body, target) {
					return stateAt(x.Body.List, st, target)
				}
				if contains(x.Else, target) {
					return stateAt([]ast.Stmt{x.Else}, st, target)
				}
			case *ast.ForStmt:
				if x.Init != nil && !contains(x.Init, target) {
					st = exec(x.Init, st)
				}
				if contains(x.Body, target) {
					return stateAt(x.Body.List, join(st, loopOnce(x, st)), target)
				}
			case *ast.RangeStmt:
				if contains(x.Body, target) {
					return stateAt(x.Body.List, join(st, execList(x.Body.List, clone(st))), target)
				}
			case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
				var init ast.Stmt
				var body *ast.BlockStmt
				switch y := x.(type) {
				case *ast.SwitchStmt:
					init, body = y.Init, y.Body
				case *ast.TypeSwitchStmt:
					init, body = y.Init, y.Body
				case *ast.SelectStmt:
					body = y.Body
				}
				if init != nil && !contains(init, target) {
					st = exec(init, st)
				}
				for _, c := range body.List {
					switch cc := c.(type) {
					case *ast.CaseClause:
						if contains(cc, target) {
							return stateAt(cc.Body, st, target)
						}
					case *ast.CommClause:
						if contains(cc, target) {
							return stateAt(cc.Body, st, target)
						}
					}
				}
			}
			// The target is in this statement itself, or in a closure inside it, which
			// sees the state as it is here.
			var lit *ast.FuncLit
			ast.Inspect(s, func(n ast.Node) bool {
				if fl, ok := n.(*ast.FuncLit); ok && contains(fl, target) {
					lit = fl
				}
				return lit == nil
			})
			if lit != nil {
				return stateAt(lit.Body.List, st, target)
			}
			return st
		}
		return st
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
		st := state{}
		if call.Ellipsis.IsValid() {
			st = stateAt(enclosing(call), st, call)
		}
		for _, seq := range evalArgs(args, call.Ellipsis.IsValid(), st) {
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
			if isMethod && name == "WithGroup" && len(x.Args) > 0 {
				addKey(x.Args[0])
			} else if !isMethod && attrCtors[name] {
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
