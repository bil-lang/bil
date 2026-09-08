// Package main implements Bil's point-to-point channel rule:
//
//	The name of a channel may only be used in one component of a parallel
//	for input, and in one other component of the parallel for output.
//
// bilc's own checkParBranchChannelUsage (tools/bilc/bilc.go) already
// enforces this, but only for call-shaped par branches, and only by
// trusting the callee's declared parameter direction (<-chan T / chan<- T)
// rather than checking actual usage — every bilc check shares that same
// blind spot. This
// checker runs against bilc's transformed Go output, where every par
// branch — whatever shape it started as in .bil (seq{}, a bare {}, a proc
// call) — has already become a func(){...} literal argument to par(), and
// resolves identifiers through go/types instead of trusting annotations.
//
// A channel-*array* element (chans[idx]) is a distinct identity from the
// whole array, not covered by naive base-identifier resolution: an index
// is classified as a compile-time-constant literal (chans[0] and
// chans[1] are different identities — checked via go/types' own recorded
// constant value, the same technique regions.go's linearize uses, not
// just a literal-token match) or opaque (anything else — a loop variable
// included, conservatively rejected the same "can't verify, so reject"
// way every other unresolvable shape in this project is).
//
// A loop-indexed touch (chans[i] inside a parFor replication, or
// chans[j] inside a plain `for j := range n` loop) is deliberately *not*
// given its own "trust every index once" classification distinct from
// opaque, even though that was the original design here: tracing through
// conflictsAmong shows send and receive occurrences are already compared
// in entirely separate groups (one call for dirSend, one for dirRecv), so
// the real, load-bearing reason 12-scatter-gather.bil's own pattern (a
// parFor sends via chans[i], a sequential loop receives via chans[j])
// never conflicts is that separation, not anything about the index
// itself — a same-direction, same-index-shape comparison never actually
// arises in the real corpus, so treating a loop-indexed touch as opaque
// produces identical results to a dedicated "family" classification would
// have, for meaningfully less code. If a real future need for "two
// different loops, same direction, provably disjoint ranges" ever shows
// up, that's exactly the sort of arithmetic regions.go already does for
// arrays — not something to redo here on faith.
//
// Bil's timer exemption ("a timer may be used for input by any number of
// components of a parallel") is a genuine
// exemption from the point-to-point rule above, not an oversight — a
// timer channel isn't governed by "one input, one other output" at all.
// Recognized by element type, not by name or provenance: any chan whose
// element is time.Time (isTimerChan) is exactly what every timer/ticker-
// producing stdlib API returns (time.After, time.Tick, (*time.Timer).C,
// (*time.Ticker).C), regardless of how the channel was actually obtained
// or how many names it's bound to — a channel bound to a name is what
// actually needs this exemption at all (bilc's own transpile of Bil's
// `timer -> After(d)` idiom, an inline call with no name, was already
// invisible here for an unrelated reason — baseIdent has no case for
// *ast.CallExpr — but a named binding, `t := time.After(d)` used via `t`
// in more than one branch, is a real *types.Var identity this checker
// does track, and without this exemption would incorrectly flag).
package vet

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
)

// direction of a channel occurrence.
type direction int

const (
	dirSend direction = iota
	dirRecv
	dirClose
)

// touchKind classifies how a channel-array element was indexed — see the
// package doc comment. touchWhole is a plain scalar channel — not a bare
// (unindexed) reference to a channel array, which can't arise here at
// all: Go's send/receive operators require a scalar chan T operand
// syntactically, so a whole channel array can only ever appear as a call
// argument, and this package deliberately doesn't substitute an
// array-typed parameter (no real example needs a channel array passed
// whole into a callee and indexed there — see the package doc comment).
type touchKind int

const (
	touchWhole  touchKind = iota
	touchLit              // a compile-time-constant index
	touchOpaque           // anything else, including a loop variable
)

// occurrence is one channel operation found in a par branch (possibly
// inside a proc it calls), attributed back to the channel's identity as
// seen from the branch itself.
type occurrence struct {
	base *types.Var // the channel variable, or the channel-array variable
	kind touchKind
	lit  int64 // valid when kind == touchLit
	dir  direction
	pos  token.Pos
}

// ChannelConflict is a channel used for the same direction (both send, both
// receive, or both close) in two different branches of the same par(...)
// call — the violation the point-to-point rule forbids. A close/close pair is exactly
// as much a same-direction conflict as a send/send or recv/recv pair: Go
// panics on a double close of the same channel just as it panics on nothing
// here, so it fits the existing symmetric check without needing its own
// pass (see the asymmetric close/send case below, which does).
type ChannelConflict struct {
	Identity   *types.Var
	Dir        direction
	FirstPos   token.Pos
	SecondPos  token.Pos
	FirstFrom  int // branch index
	SecondFrom int // branch index
}

// CloseSendConflict is a channel closed in one branch while sent to in a
// different branch — an asymmetric pair conflictsAmong's same-direction
// grouping can't express (Dir would need to mean two different things at
// once). Go panics on a send to a closed channel, so this is exactly as
// real a hazard as the symmetric cases above; close vs *receive* is
// deliberately not flagged — receiving from a closed channel is
// well-defined (zero value, ok == false) and is the standard "close
// signals completion" idiom.
type CloseSendConflict struct {
	Identity  *types.Var
	ClosePos  token.Pos
	SendPos   token.Pos
	CloseFrom int
	SendFrom  int
}

// CheckChannelUsage walks file for par(...) calls and reports
// ChannelConflicts (send/send, recv/recv, close/close) and
// CloseSendConflicts (close/send) among their branches.
func CheckChannelUsage(info *types.Info, file *ast.File) ([]ChannelConflict, []CloseSendConflict) {
	declByFunc := funcDecls(info, file)

	var conflicts []ChannelConflict
	var closeSend []CloseSendConflict

	for _, branches := range parBranches(file) {
		occsByBranch := make([][]occurrence, len(branches))
		for i, b := range branches {
			var occs []occurrence
			collectChannelUsage(info, b.Body, nil, b, declByFunc, map[*types.Func]bool{}, &occs)
			occsByBranch[i] = occs
		}

		conflicts = append(conflicts, conflictsAmong(occsByBranch, dirSend)...)
		conflicts = append(conflicts, conflictsAmong(occsByBranch, dirRecv)...)
		conflicts = append(conflicts, conflictsAmong(occsByBranch, dirClose)...)
		closeSend = append(closeSend, closeSendConflictsAmong(occsByBranch)...)
	}

	return conflicts, closeSend
}

// closeSendConflictsAmong finds, among occsByBranch, every close/send pair
// on the same base channel from two different branches — see
// CloseSendConflict's doc comment for why this needs its own pass rather
// than reusing conflictsAmong's single-direction grouping.
func closeSendConflictsAmong(occsByBranch [][]occurrence) []CloseSendConflict {
	type touch struct {
		branch int
		occ    occurrence
	}
	closes := map[*types.Var][]touch{}
	sends := map[*types.Var][]touch{}
	for branch, occs := range occsByBranch {
		for _, o := range occs {
			switch o.dir {
			case dirClose:
				closes[o.base] = append(closes[o.base], touch{branch, o})
			case dirSend:
				sends[o.base] = append(sends[o.base], touch{branch, o})
			}
		}
	}

	var out []CloseSendConflict
	for base, cls := range closes {
		reported := map[[2]int]bool{}
		for _, c := range cls {
			for _, s := range sends[base] {
				if c.branch == s.branch || !touchesConflict(c.occ, s.occ) {
					continue
				}
				key := [2]int{c.branch, s.branch}
				if key[0] > key[1] {
					key[0], key[1] = key[1], key[0]
				}
				if reported[key] {
					continue
				}
				reported[key] = true
				out = append(out, CloseSendConflict{
					Identity:  base,
					ClosePos:  c.occ.pos, CloseFrom: c.branch,
					SendPos: s.occ.pos, SendFrom: s.branch,
				})
			}
		}
	}
	return out
}

// conflictsAmong finds, among occsByBranch's occurrences in direction dir,
// every pair from two *different* branches whose touches conflict
// (touchesConflict) — grouped by base object first (so unrelated channels
// are never compared against each other), then one conflict reported per
// distinct pair of branches to avoid flooding on a channel used from many
// branches, the same restraint this check has always used.
func conflictsAmong(occsByBranch [][]occurrence, dir direction) []ChannelConflict {
	byBase := map[*types.Var][]struct {
		branch int
		occ    occurrence
	}{}
	for branch, occs := range occsByBranch {
		for _, o := range occs {
			if o.dir != dir {
				continue
			}
			byBase[o.base] = append(byBase[o.base], struct {
				branch int
				occ    occurrence
			}{branch, o})
		}
	}

	var out []ChannelConflict
	for base, touches := range byBase {
		reported := map[[2]int]bool{}
		for i := 0; i < len(touches); i++ {
			for j := i + 1; j < len(touches); j++ {
				a, b := touches[i], touches[j]
				if a.branch == b.branch || !touchesConflict(a.occ, b.occ) {
					continue
				}
				first, second := a, b
				if second.branch < first.branch {
					first, second = second, first
				}
				key := [2]int{first.branch, second.branch}
				if reported[key] {
					continue
				}
				reported[key] = true
				out = append(out, ChannelConflict{
					Identity: base, Dir: dir,
					FirstFrom: first.branch, FirstPos: first.occ.pos,
					SecondFrom: second.branch, SecondPos: second.occ.pos,
				})
			}
		}
	}
	return out
}

// touchesConflict reports whether a and b — two same-direction touches of
// the same base channel/channel-array, from different branches — can't be
// proven to reach different channels: a whole or opaque touch conflicts
// with anything (can't verify, so reject, same stance as everywhere else
// in this project); two literal touches conflict iff their values match.
func touchesConflict(a, b occurrence) bool {
	if a.kind == touchLit && b.kind == touchLit {
		return a.lit == b.lit
	}
	return true
}

// funcDecls maps every same-file, top-level plain function's *types.Func to
// its declaration, so a call can be resolved to a body to look inside.
func funcDecls(info *types.Info, file *ast.File) map[*types.Func]*ast.FuncDecl {
	m := map[*types.Func]*ast.FuncDecl{}
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Body == nil {
			continue
		}
		if obj, ok := info.Defs[fd.Name].(*types.Func); ok {
			m[obj] = fd
		}
	}
	return m
}

// collectChannelUsage walks body (a par branch's own FuncLit body, or a
// callee proc's body reached through it) recording channel occurrences.
// ast.Inspect's own default top-down descent already reaches into a
// parFor replication's FuncLit argument, or a sequential for/range loop's
// body, with no special-casing needed — a loop is just an ordinary
// subtree to this walk (see the package doc comment for why a loop index
// doesn't need its own tracked identity).
//
// env substitutes a callee's channel-typed parameter objects for the
// identity that was actually passed at its call site — nil at the
// outermost (branch) frame, where there's nothing to substitute yet.
//
// scope is the function we're currently inside — a branch's *ast.FuncLit
// at the top frame, a callee's *ast.FuncDecl once recursed. An identifier
// not resolved through env (i.e. not one of the current function's own
// substituted parameters) still resolves if it's free relative to scope
// (isFree) — at the top frame that's a real captured channel; at any
// recursed frame it can only be a package-level channel, since Go's own
// scoping rules mean a callee can't see anything else from its caller (see
// isFree's doc comment). A callee's own unrelated locals are never free
// relative to itself, so they're never tracked — this stays one bounded
// layer of interprocedural resolution, not general points-to analysis.
//
// visiting guards against a call cycle (direct or mutual recursion, e.g.
// examples/11-recursive-proc.bil) by refusing to recurse into a function
// already on the current path.
func collectChannelUsage(
	info *types.Info,
	body ast.Node,
	env map[*types.Var]*types.Var,
	scope ast.Node,
	declByFunc map[*types.Func]*ast.FuncDecl,
	visiting map[*types.Func]bool,
	out *[]occurrence,
) {
	classify := func(expr ast.Expr) (occurrence, bool) {
		if idx, ok := unparenExpr(expr).(*ast.IndexExpr); ok {
			if o, ok := classifyIndexed(info, idx, scope); ok {
				return o, true
			}
		}
		id := baseIdent(expr)
		if id == nil {
			return occurrence{}, false
		}
		obj, ok := info.ObjectOf(id).(*types.Var)
		if !ok || !isChan(obj.Type()) || isTimerChan(obj.Type()) {
			return occurrence{}, false
		}
		if mapped, found := env[obj]; found {
			return occurrence{base: mapped, kind: touchWhole}, true
		}
		if isFree(obj, scope) {
			return occurrence{base: obj, kind: touchWhole}, true
		}
		return occurrence{}, false
	}

	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SendStmt:
			if o, ok := classify(x.Chan); ok {
				o.dir, o.pos = dirSend, x.Pos()
				*out = append(*out, o)
			}
		case *ast.UnaryExpr:
			if x.Op == token.ARROW {
				if o, ok := classify(x.X); ok {
					o.dir, o.pos = dirRecv, x.Pos()
					*out = append(*out, o)
				}
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "close" && len(x.Args) == 1 {
				if _, isBuiltin := info.ObjectOf(id).(*types.Builtin); isBuiltin {
					if o, ok := classify(x.Args[0]); ok {
						o.dir, o.pos = dirClose, x.Pos()
						*out = append(*out, o)
					}
					break
				}
			}
			calleeID, ok := x.Fun.(*ast.Ident)
			if !ok {
				break
			}
			calleeObj, ok := info.ObjectOf(calleeID).(*types.Func)
			if !ok {
				break
			}
			decl, ok := declByFunc[calleeObj]
			if !ok || visiting[calleeObj] {
				break
			}

			resolve := func(expr ast.Expr) *types.Var {
				o, ok := classify(expr)
				if !ok || o.kind != touchWhole {
					return nil
				}
				return o.base
			}
			newEnv := substituteParams(info, decl, x.Args, resolve)
			visiting[calleeObj] = true
			collectChannelUsage(info, decl.Body, newEnv, decl, declByFunc, visiting, out)
			delete(visiting, calleeObj)
		}
		return true
	})
}

// classifyIndexed classifies idx.X[idx.Index] as a channel-array element
// touch — see the package doc comment for the lit/opaque classification.
// Returns false for anything that isn't an indexed reference to a free,
// channel-*array*-typed base (a plain array/slice of some other element
// type, an already-scalar channel, or a base that isn't free relative to
// scope).
func classifyIndexed(info *types.Info, idx *ast.IndexExpr, scope ast.Node) (occurrence, bool) {
	baseID := baseIdent(idx.X)
	if baseID == nil {
		return occurrence{}, false
	}
	baseObj, ok := info.ObjectOf(baseID).(*types.Var)
	if !ok {
		return occurrence{}, false
	}
	sl, ok := baseObj.Type().Underlying().(*types.Slice)
	if !ok || !isChan(sl.Elem()) {
		return occurrence{}, false
	}
	// A channel-array-typed variable is never substituted through env — no
	// real example passes a whole channel array as a proc parameter and
	// indexes it inside the callee (see the package doc comment) — so the
	// only thing that matters here is whether baseObj is free relative to
	// the current scope, the same "declared outside, captured" test every
	// other rule in this package uses.
	if !isFree(baseObj, scope) {
		return occurrence{}, false
	}

	if k, ok := constIndexValue(info, idx.Index); ok {
		return occurrence{base: baseObj, kind: touchLit, lit: k}, true
	}
	return occurrence{base: baseObj, kind: touchOpaque}, true
}

// constIndexValue reports whether e is a compile-time-constant integer
// expression (a literal, or a named const) — the same go/types-recorded-
// constant technique regions.go's linearize uses, not just a literal-token
// match.
func constIndexValue(info *types.Info, e ast.Expr) (int64, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil {
		return 0, false
	}
	return constant.Int64Val(tv.Value)
}

// substituteParams maps each of decl's channel-typed parameters to the
// identity its actual argument resolves to in the calling frame (via
// resolve, the caller's own env/isTop-aware resolver). A parameter whose
// argument doesn't resolve to a tracked identity is simply absent from the
// result — occurrences on it inside the callee won't be recorded, the same
// "skip, don't flag" stance the rest of this checker takes for anything it
// can't confidently resolve.
func substituteParams(info *types.Info, decl *ast.FuncDecl, args []ast.Expr, resolve func(ast.Expr) *types.Var) map[*types.Var]*types.Var {
	env := map[*types.Var]*types.Var{}
	argIdx := 0
	for _, field := range decl.Type.Params.List {
		names := field.Names
		if len(names) == 0 {
			names = []*ast.Ident{nil} // unnamed param still consumes one arg slot
		}
		for _, name := range names {
			if argIdx >= len(args) {
				return env
			}
			arg := args[argIdx]
			argIdx++
			if name == nil {
				continue
			}
			paramObj, ok := info.Defs[name].(*types.Var)
			if !ok || !isChan(paramObj.Type()) {
				continue
			}
			if identity := resolve(arg); identity != nil {
				env[paramObj] = identity
			}
		}
	}
	return env
}

// baseIdent unwraps index/slice/selector/star/paren expressions down to the
// root identifier — e.g. chans[i] and s.field both roll up to the base
// variable they're a component of. Used as the fallback beneath
// classifyIndexed: anything not recognized as a channel-array element
// touch (a plain scalar channel, or an index/selector/etc. this package
// doesn't specially classify) still resolves to its base, whole.
func baseIdent(e ast.Expr) *ast.Ident {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.SelectorExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.SliceExpr:
			e = x.X
		default:
			return nil
		}
	}
}

// isFree reports whether obj is declared outside scope — i.e. scope's body
// captures/references it rather than owning it. scope is whichever
// function we're currently resolving identifiers against: a par branch's
// *ast.FuncLit at the top frame, or a callee's *ast.FuncDecl once
// interprocedural recursion has descended into it — both satisfy ast.Node.
// Predeclared/builtin objects (no valid position) are never free.
//
// Beyond the top frame this collapses to exactly "declared at package
// level": Go's own lexical scoping means a top-level func can only ever
// see its own parameters/locals (declared inside its own Pos()/End() span,
// so not free) and package-level names (declared outside every function in
// the file, so free) — nothing from whatever branch happened to call it.
func isFree(obj types.Object, scope ast.Node) bool {
	p := obj.Pos()
	if !p.IsValid() {
		return false
	}
	return p < scope.Pos() || p >= scope.End()
}

func isChan(t types.Type) bool {
	_, ok := t.Underlying().(*types.Chan)
	return ok
}

// isTimerChan reports whether t is a channel of time.Time — see the
// package doc comment for why this exempts it from the point-to-point
// rule entirely, and why element type (not name or provenance) is the
// right thing to check.
func isTimerChan(t types.Type) bool {
	ch, ok := t.Underlying().(*types.Chan)
	if !ok {
		return false
	}
	named, ok := ch.Elem().(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "time" && obj.Name() == "Time"
}
