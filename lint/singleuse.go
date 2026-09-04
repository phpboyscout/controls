// Package lint holds singleuse, an advisory analyzer for the capture a restart
// cannot reuse.
//
// A restart re-invokes a service's StartFunc, so anything the closure captured
// is shared across runs. A server, a listener or a supervisor built once at
// wiring is the previous run's by the second call, and the two failures that
// follow look nothing alike: the restart cannot work, or the stopped thing
// carries on looking healthy. controls spec 0004 states the rule; this pass
// flags the shape.
package lint

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/types/typeutil"
)

// Analyzer is singleuse: it flags a StartFunc that reaches a server, listener
// or supervisor it did not build.
var Analyzer = &analysis.Analyzer{
	Name:     "singleuse",
	Doc:      "flags a StartFunc that captures a single-use value (server, listener, supervisor) built outside the run",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

const (
	controlsPath  = "gitlab.com/phpboyscout/go/controls"
	startFuncType = controlsPath + ".StartFunc"
	childType     = controlsPath + ".Child"
)

// singleUseTypes cannot be used again after they stop, keyed by the named
// type's package path and name. A pointer to one, or an alias of one, matches
// too. The list is the heuristic (spec 0005 D4): a type is added when a
// capture of it is found.
var singleUseTypes = map[string]struct{}{
	"google.golang.org/grpc.Server": {},
	"net/http.Server":               {},
	controlsPath + ".Supervisor":    {},
	"net.Listener":                  {},
	"net.TCPListener":               {},
	"net.UnixListener":              {},
}

func run(pass *analysis.Pass) (any, error) {
	insp, ok := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	if !ok {
		return nil, nil
	}

	c := &checker{pass: pass, reported: map[string]struct{}{}}

	insp.Preorder([]ast.Node{(*ast.CallExpr)(nil), (*ast.CompositeLit)(nil), (*ast.FuncDecl)(nil)}, func(n ast.Node) {
		switch n := n.(type) {
		case *ast.CallExpr:
			c.withStartCall(n)
		case *ast.CompositeLit:
			c.childLiteral(n)
		case *ast.FuncDecl:
			c.startMethod(n)
		}
	})

	return nil, nil
}

type checker struct {
	pass     *analysis.Pass
	reported map[string]struct{}
}

// report emits a diagnostic once per position and message, so a value
// referenced twice in one body is named once.
func (c *checker) report(pos token.Pos, format string, args ...any) {
	key := c.pass.Fset.Position(pos).String() + "|" + format
	if _, done := c.reported[key]; done {
		return
	}

	c.reported[key] = struct{}{}
	c.pass.Reportf(pos, format, args...)
}

func (c *checker) typeOf(e ast.Expr) types.Type { return c.pass.TypesInfo.TypeOf(e) }

// namedKey returns "path.Name" for a named type, through any alias and one
// pointer, with the pointer marker it stripped.
func namedKey(t types.Type) (key, ptr string) {
	if t == nil {
		return "", ""
	}

	t = types.Unalias(t)

	if p, isPtr := t.(*types.Pointer); isPtr {
		t = types.Unalias(p.Elem())
		ptr = "*"
	}

	n, isNamed := t.(*types.Named)
	if !isNamed || n.Obj().Pkg() == nil {
		return "", ""
	}

	return n.Obj().Pkg().Path() + "." + n.Obj().Name(), ptr
}

// singleUse reports whether t is on the list, and how to name it.
func singleUse(t types.Type) (string, bool) {
	key, ptr := namedKey(t)
	if _, ok := singleUseTypes[key]; !ok {
		return "", false
	}

	return ptr + key, true
}

func isNamed(t types.Type, name string) bool {
	key, ptr := namedKey(t)

	return ptr == "" && key == name
}

// withStartCall handles controls.WithStart(x).
func (c *checker) withStartCall(call *ast.CallExpr) {
	fn, ok := typeutil.Callee(c.pass.TypesInfo, call).(*types.Func)
	if !ok || fn.Name() != "WithStart" || fn.Pkg() == nil || fn.Pkg().Path() != controlsPath || len(call.Args) != 1 {
		return
	}

	c.startExpr(call.Args[0], "StartFunc")
}

// childLiteral handles controls.Child{Start: x}.
func (c *checker) childLiteral(lit *ast.CompositeLit) {
	if !isNamed(c.typeOf(lit), childType) {
		return
	}

	for _, e := range lit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			continue
		}

		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Start" {
			c.startExpr(kv.Value, "Child.Start")
		}
	}
}

// startExpr inspects what is handed over as a StartFunc: a closure, a named
// function, a method value, a call that builds one, or a variable holding one.
func (c *checker) startExpr(x ast.Expr, what string) {
	switch x := ast.Unparen(x).(type) {
	case *ast.FuncLit:
		c.scanBody(x.Body, x.Pos(), x.End(), what+" closure", "captures")
	case *ast.CallExpr:
		c.startCall(x, what)
	case *ast.Ident:
		c.startIdent(x, what)
	case *ast.SelectorExpr:
		c.methodValue(x, what)
	}
}

// startCall is either a conversion around the real expression or a call that
// builds the StartFunc from its arguments.
func (c *checker) startCall(call *ast.CallExpr, what string) {
	if tv, ok := c.pass.TypesInfo.Types[call.Fun]; ok && tv.IsType() && len(call.Args) == 1 {
		c.startExpr(call.Args[0], what)

		return
	}

	c.builderArgs(call, what, "")
}

func (c *checker) startIdent(id *ast.Ident, what string) {
	switch obj := c.pass.TypesInfo.Uses[id].(type) {
	case *types.Var:
		if isNamed(obj.Type(), startFuncType) {
			c.followStartFuncVar(obj, what)
		}
	case *types.Func:
		c.namedFunc(obj, what)
	}
}

// methodValue handles WithStart(sup.Start): the receiver is the capture.
func (c *checker) methodValue(sel *ast.SelectorExpr, what string) {
	s := c.pass.TypesInfo.Selections[sel]
	if s == nil || s.Kind() != types.MethodVal {
		return
	}

	name, ok := singleUse(c.typeOf(sel.X))
	if !ok {
		return
	}

	c.report(sel.Pos(), "%s is the %s method of %s (%s) built outside the run; a restart reuses it",
		what, sel.Sel.Name, types.ExprString(sel.X), name)
}

// namedFunc follows a function identifier to its declaration in this package.
func (c *checker) namedFunc(fn *types.Func, what string) {
	for _, f := range c.pass.Files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || c.pass.TypesInfo.Defs[fd.Name] != fn {
				continue
			}

			c.scanBody(fd.Body, fd.Pos(), fd.End(), what+" "+fn.Name(), "reaches")
		}
	}
}

// scanBody reports single-use values a body reaches from outside [lo, hi).
// verb is the word for a direct identifier: a closure "captures", a named
// function "reaches".
func (c *checker) scanBody(body *ast.BlockStmt, lo, hi token.Pos, label, verb string) {
	rebuilt := c.assignedIn(body)
	handled := map[*ast.Ident]struct{}{}

	outside := func(obj types.Object) bool {
		return obj != nil && (obj.Pos() < lo || obj.Pos() >= hi)
	}

	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			return c.bodySelector(n, label, outside, rebuilt, handled)
		case *ast.Ident:
			c.bodyIdent(n, label, verb, outside, rebuilt, handled)
		}

		return true
	})
}

func (c *checker) bodyIdent(id *ast.Ident, label, verb string, outside func(types.Object) bool,
	rebuilt map[string]struct{}, handled map[*ast.Ident]struct{},
) {
	if _, done := handled[id]; done {
		return
	}

	obj, ok := c.pass.TypesInfo.Uses[id].(*types.Var)
	if !ok || obj.IsField() || !outside(obj) {
		return
	}

	if _, isRebuilt := rebuilt[id.Name]; isRebuilt {
		return
	}

	if name, ok := singleUse(obj.Type()); ok {
		c.report(id.Pos(), "%s %s %s (%s) built outside the run; a restart reuses it", label, verb, id.Name, name)

		return
	}

	if isNamed(obj.Type(), startFuncType) {
		c.followStartFuncVar(obj, label)
	}
}

// bodySelector handles a selector chain in a body. A type expression such as
// net.Listener falls through harmlessly: its root is a package name whose
// selected object is not a variable, and its identifiers are not variables
// either.
func (c *checker) bodySelector(sel *ast.SelectorExpr, label string, outside func(types.Object) bool,
	rebuilt map[string]struct{}, handled map[*ast.Ident]struct{},
) bool {
	root := rootIdent(sel)
	if root == nil {
		return true
	}

	rootObj := c.pass.TypesInfo.Uses[root]

	if _, isPkg := rootObj.(*types.PkgName); isPkg {
		c.packageLevel(sel, label, handled)

		return true
	}

	if !outside(rootObj) {
		return true
	}

	if _, isRebuilt := rebuilt[types.ExprString(sel)]; isRebuilt {
		return true
	}

	if _, isRebuilt := rebuilt[root.Name]; isRebuilt {
		return true
	}

	if name, ok := singleUse(c.typeOf(sel)); ok {
		c.report(sel.Pos(), "%s reaches %s (%s) through captured %s; a restart reuses it",
			label, types.ExprString(sel), name, root.Name)

		return true
	}

	c.promotedThrough(sel, label, root.Name)

	return true
}

// packageLevel reports pkg.Var where Var is single-use, once, as a capture.
func (c *checker) packageLevel(sel *ast.SelectorExpr, label string, handled map[*ast.Ident]struct{}) {
	v, ok := c.pass.TypesInfo.Uses[sel.Sel].(*types.Var)
	if !ok {
		return
	}

	handled[sel.Sel] = struct{}{}

	if name, ok := singleUse(v.Type()); ok {
		c.report(sel.Pos(), "%s captures %s (%s) built outside the run; a restart reuses it",
			label, types.ExprString(sel), name)
	}
}

// promotedThrough reports a method or field reached through an embedded
// single-use field, which the selector's own type does not show.
func (c *checker) promotedThrough(sel *ast.SelectorExpr, label, root string) {
	s := c.pass.TypesInfo.Selections[sel]
	if s == nil || len(s.Index()) < 2 {
		return
	}

	recv := s.Recv()
	path := types.ExprString(sel.X)

	for _, i := range s.Index()[:len(s.Index())-1] {
		st, ok := structOf(recv)
		if !ok {
			return
		}

		f := st.Field(i)
		path += "." + f.Name()
		recv = f.Type()

		if name, ok := singleUse(f.Type()); ok {
			c.report(sel.Pos(), "%s reaches %s (%s) through captured %s; a restart reuses it", label, path, name, root)

			return
		}
	}
}

func structOf(t types.Type) (*types.Struct, bool) {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}

	st, ok := t.Underlying().(*types.Struct)

	return st, ok
}

// assignedIn collects what a body assigns, by expression text, so a value
// rebuilt per run is not reported as reused.
func (c *checker) assignedIn(body *ast.BlockStmt) map[string]struct{} {
	assigned := map[string]struct{}{}

	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok == token.DEFINE {
			return true
		}

		for _, l := range as.Lhs {
			assigned[types.ExprString(l)] = struct{}{}
		}

		return true
	})

	return assigned
}

// followStartFuncVar finds the assignments that built a StartFunc variable,
// in the file that declares it, and inspects each call's arguments.
func (c *checker) followStartFuncVar(v *types.Var, what string) {
	for _, f := range c.pass.Files {
		if v.Pos() < f.Pos() || v.Pos() > f.End() {
			continue
		}

		ast.Inspect(f, func(n ast.Node) bool {
			if e, ok := c.singleAssignmentTo(n, v); ok {
				c.builderCall(e, what, v.Name())
			}

			return true
		})
	}
}

// singleAssignmentTo returns the expression a one-to-one assignment or
// declaration gives v, whether by :=, = or var.
func (c *checker) singleAssignmentTo(n ast.Node, v *types.Var) (ast.Expr, bool) {
	switch n := n.(type) {
	case *ast.AssignStmt:
		if len(n.Lhs) == 1 && len(n.Rhs) == 1 && c.refersTo(n.Lhs[0], v) {
			return n.Rhs[0], true
		}
	case *ast.ValueSpec:
		if len(n.Names) == 1 && len(n.Values) == 1 && c.pass.TypesInfo.Defs[n.Names[0]] == v {
			return n.Values[0], true
		}
	}

	return nil, false
}

func (c *checker) refersTo(e ast.Expr, v *types.Var) bool {
	id, ok := e.(*ast.Ident)
	if !ok {
		return false
	}

	return c.pass.TypesInfo.Defs[id] == v || c.pass.TypesInfo.Uses[id] == v
}

func (c *checker) builderCall(e ast.Expr, what, via string) {
	if call, ok := ast.Unparen(e).(*ast.CallExpr); ok {
		c.builderArgs(call, what, via)
	}
}

// builderArgs reports single-use arguments to a call that produces a
// StartFunc. via names the variable the StartFunc travelled through, if any.
func (c *checker) builderArgs(call *ast.CallExpr, what, via string) {
	for _, a := range call.Args {
		name, ok := singleUse(c.typeOf(a))
		if !ok {
			continue
		}

		if via == "" {
			c.report(a.Pos(), "%s is built by a call that captures %s (%s); a restart reuses it",
				what, types.ExprString(a), name)

			continue
		}

		c.report(a.Pos(), "%s uses %s, built by a call that captures %s (%s); a restart reuses it",
			what, via, types.ExprString(a), name)
	}
}

// startMethod handles a Start(ctx) error method that reads a single-use
// receiver field, at any depth, that it does not assign.
func (c *checker) startMethod(fd *ast.FuncDecl) {
	if fd.Recv == nil || fd.Name.Name != "Start" || fd.Body == nil || !c.isStartShaped(fd) {
		return
	}

	recv := c.receiver(fd)
	if recv == nil {
		return
	}

	assigned := c.assignedIn(fd.Body)

	ast.Inspect(fd.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		path := c.fieldPath(sel, recv)
		if path == "" || assignedPrefix(assigned, recv.Name(), path) {
			return true
		}

		if name, ok := singleUse(c.typeOf(sel)); ok {
			c.report(sel.Pos(), "Start reads receiver field %s (%s) it did not build; a restart reuses it", path, name)
		}

		return true
	})
}

// assignedPrefix reports whether the body assigned recv.path or any prefix of
// it, which rebuilds what the path reaches.
func assignedPrefix(assigned map[string]struct{}, recv, path string) bool {
	parts := strings.Split(path, ".")
	for i := range parts {
		if _, ok := assigned[recv+"."+strings.Join(parts[:i+1], ".")]; ok {
			return true
		}
	}

	return false
}

func (c *checker) isStartShaped(fd *ast.FuncDecl) bool {
	fn, ok := c.pass.TypesInfo.Defs[fd.Name].(*types.Func)
	if !ok {
		return false
	}

	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Params().Len() != 1 || sig.Results().Len() != 1 {
		return false
	}

	return types.TypeString(sig.Params().At(0).Type(), nil) == "context.Context" &&
		types.TypeString(sig.Results().At(0).Type(), nil) == "error"
}

func (c *checker) receiver(fd *ast.FuncDecl) types.Object {
	if len(fd.Recv.List) != 1 || len(fd.Recv.List[0].Names) != 1 {
		return nil
	}

	return c.pass.TypesInfo.Defs[fd.Recv.List[0].Names[0]]
}

// fieldPath returns "a.b.c" when sel is recv.a.b.c through field selections
// only, else "".
func (c *checker) fieldPath(sel *ast.SelectorExpr, recv types.Object) string {
	var parts []string

	var e ast.Expr = sel

	for {
		se, ok := e.(*ast.SelectorExpr)
		if !ok {
			break
		}

		s := c.pass.TypesInfo.Selections[se]
		if s == nil || s.Kind() != types.FieldVal {
			return ""
		}

		parts = append([]string{se.Sel.Name}, parts...)
		e = se.X
	}

	id, ok := e.(*ast.Ident)
	if !ok || c.pass.TypesInfo.Uses[id] != recv {
		return ""
	}

	return strings.Join(parts, ".")
}

// rootIdent walks a selector or call chain to the identifier it starts from.
func rootIdent(e ast.Expr) *ast.Ident {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x
		case *ast.SelectorExpr:
			e = x.X
		case *ast.CallExpr:
			e = x.Fun
		case *ast.ParenExpr:
			e = x.X
		default:
			return nil
		}
	}
}
