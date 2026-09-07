// Package main implements array/slice disjointness across par branches by
// proving it, not by trusting one hardcoded pattern. bilc's own token-level
// checker used to do the latter (trust splitN specifically, reject
// everything else outright) — since removed in favor of this package being
// the sole enforcer, once it was clear this package's own proof already
// covers every case that check did, and more (a replicated par body
// indexing a splitN result, never inspected by the old check at all).
//
// A Region is { Base, Start, End }: Base is the root array/slice object
// (found by walking a slicing chain back to its origin), Start/End are
// linear expressions normalized relative to that root. Two regions from
// the same Base are proven disjoint by showing end1<=start2 or
// end2<=start1 — see proveDisjoint.
//
// linExpr is a general linear combination (see linearize), not one
// hardcoded variable — a plain local variable is resolved through its own
// single defining assignment wherever possible (findSingleDef), so two
// independently-declared variables (a hypothetical mid/mip pair) are
// provably equal exactly when they trace back to the same normalized
// form, and stop being trusted the moment their definitions actually
// diverge. This is what makes it safe to go further than bilc's own
// checker without repeating the failure mode of the token-based checker
// that was tried and reverted before splitN existed (see splitNHelper's
// doc comment, tools/bilc/bilc.go:77) — that attempt had no type
// information and no way to trace a variable back to its definition at
// all; this one does.
//
// No function is special-cased by name. Instead, arraySummary recognizes
// the general shape "X := make([]T,N); for VAR := range N { ...;
// X[VAR] = BASE[lo:hi] }; return X" in any function's body and derives a
// region formula parameterized by VAR and the function's own parameters —
// splitN included, but not by name. The one piece of splitN's own shape
// that needs explicit handling is its last-iteration remainder override
// (findSingleDef tolerates a reassignment *only* when it's guarded by
// `VAR == <loop bound>-1`-shaped condition — see its doc comment for why
// that's sound for the comparisons this file ever needs, not just
// convenient).
//
// A variable can also be *reassigned* through one of the several ways Go
// lets a slice grow or change without a fresh `:=` — append, reslicing
// within capacity (`s = s[:4]`), or any function call that might return a
// view of its own prior value (`s = bytes.TrimSpace(s)`, same-file or
// not). isSelfReferentialReassign tolerates all of these uniformly (v
// appearing anywhere in its own RHS), not by naming each operation: the
// same one-line soundness argument covers every case (see its own doc
// comment) — a category error in the *other* direction is what actually
// mattered here, not needing to model every operation precisely.
//
// copy(dst, src) is handled directly (a write to dst, a read of src) — the
// one bulk-touching operation that isn't a reassignment or an indexed
// write at all, so neither of those paths would ever see it.
//
// A capacity-capped region's further slices never need their own bounds
// proven: Go's runtime bounds check already guarantees a further slice of
// a capped region can't exceed it (stays within, or panics — a different,
// acceptable failure mode). regionOf's SliceExpr case widens back to a
// capped, replicated base's own [Start,End) rather than trying to prove
// the narrower sub-window's bounds numerically (which, for a symbolic
// stride like splitN's chunk, generally can't be done at all — its
// concrete value isn't known). Always a sound over-approximation, and
// restricted to a base that's *itself* already replicated (RepIndex != nil)
// — a plain capped root array must not widen a slice of it back to the
// whole array, or two genuinely disjoint hand-written slices of it would
// look like they overlap.
package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------
// linExpr: a general linear combination over terms.
// ---------------------------------------------------------------------

// term identifies an atomic quantity in a linExpr: a *types.Var (a
// variable left free — a replicated/loop index, or a local that couldn't
// be resolved further) or an atomKey (a canonicalized string standing for
// an expression that can't be reduced to linear form, e.g. len(s)/n —
// keyed from its own resolved operands, so the same quantity computed
// twice from the same inputs is recognized as one atom, not two unrelated
// unknowns).
type term any

type atomKey string

// linExpr is Σ coeff·term + const.
type linExpr struct {
	terms map[term]int64
	konst int64
}

func constLin(k int64) linExpr { return linExpr{konst: k} }

func termLin(t term) linExpr { return linExpr{terms: map[term]int64{t: 1}} }

func (e linExpr) clone() linExpr {
	m := make(map[term]int64, len(e.terms))
	for t, c := range e.terms {
		m[t] = c
	}
	return linExpr{terms: m, konst: e.konst}
}

func addLin(a, b linExpr) linExpr {
	out := a.clone()
	if out.terms == nil {
		out.terms = map[term]int64{}
	}
	for t, c := range b.terms {
		out.terms[t] += c
		if out.terms[t] == 0 {
			delete(out.terms, t)
		}
	}
	out.konst += b.konst
	return out
}

func scaleLin(a linExpr, k int64) linExpr {
	out := linExpr{terms: make(map[term]int64, len(a.terms)), konst: a.konst * k}
	for t, c := range a.terms {
		if v := c * k; v != 0 {
			out.terms[t] = v
		}
	}
	return out
}

func subLin(a, b linExpr) linExpr { return addLin(a, scaleLin(b, -1)) }

func (e linExpr) isConst() bool { return len(e.terms) == 0 }

// key is a stable, order-independent identity for e — two linExprs
// resolving to the same normalized form produce the same key, which is
// how independently-declared variables (mid/mip) are proven equal: not by
// name, but by resolving to identical keys.
func (e linExpr) key() string {
	keys := make([]string, 0, len(e.terms))
	for t, c := range e.terms {
		keys = append(keys, fmt.Sprintf("%s*%d", termKey(t), c))
	}
	sort.Strings(keys)
	return strings.Join(keys, "+") + fmt.Sprintf("=%d", e.konst)
}

func termKey(t term) string {
	switch v := t.(type) {
	case *types.Var:
		return fmt.Sprintf("var#%p", v)
	case atomKey:
		return "atom:" + string(v)
	default:
		return fmt.Sprintf("?%v", t)
	}
}

// ---------------------------------------------------------------------
// linearize: expression -> linExpr.
// ---------------------------------------------------------------------

// linCtx carries what linearize/resolveVar need: type info, an env
// substituting a callee's parameter/loop-index objects with the caller's
// own (already-linearized) actual values — exactly like channels.go's
// env, just carrying a linExpr instead of a *types.Var — which var (if
// any) is the current replicated/loop index staying free rather than
// being resolved, the search root for tracing a local's own definition,
// and a cycle guard.
type linCtx struct {
	info *types.Info
	// file is the whole file — used only to find a region-producing
	// local's own definition (findRegionSource), since that local (e.g.
	// `chunks` in `chunks := splitN(data, n)`) is typically declared in
	// the enclosing function, outside the branch closure entirely, not
	// within searchRoot's narrower scope.
	file *ast.File
	env  map[*types.Var]linExpr // scalar parameter substitution (channels.go-style)
	renv map[*types.Var]Region  // array/slice parameter substitution — a Region travels as a unit
	// indexVars holds every replicated/loop index currently free rather
	// than resolved. A single (1D) replicated construct has exactly one;
	// a nested 2D one (parFor(nr, func(i){ parFor(nc, func(j){...}) })
	// — see grid.go) has two, both live simultaneously in the inner
	// branch body — a set, not one variable, for exactly that reason.
	indexVars  map[*types.Var]bool
	searchRoot ast.Node
	visiting   map[*types.Var]bool
	nonNeg     map[atomKey]bool // atoms proven non-negative when constructed (len(...), or len(...)-derived)
	// allowLastIterGuard: see resolveVar's doc comment. Only ever set
	// true inside an array-summary's own body while deriving its
	// lo/hi formulas.
	allowLastIterGuard bool
}

func newLinCtx(info *types.Info, file *ast.File, root ast.Node) *linCtx {
	return &linCtx{
		info:       info,
		file:       file,
		env:        map[*types.Var]linExpr{},
		renv:       map[*types.Var]Region{},
		indexVars:  map[*types.Var]bool{},
		searchRoot: root,
		visiting:   map[*types.Var]bool{},
		nonNeg:     map[atomKey]bool{},
	}
}

// isIndexVar reports whether v is one of the currently-free replicated
// indices (see indexVars' own doc comment).
func (lc *linCtx) isIndexVar(v *types.Var) bool { return lc.indexVars[v] }

// soleIndexVarIn returns the one active index variable mentioned across
// es, or nil if none is mentioned, or if more than one distinct active
// index variable is mentioned (an ambiguous shape — conservatively not
// tagged as replicated, same "can't verify, so reject" stance as
// everywhere else in this file).
func (lc *linCtx) soleIndexVarIn(es ...linExpr) *types.Var {
	var found *types.Var
	for v := range lc.indexVars {
		for _, e := range es {
			if mentionsIndex(e, v) {
				if found != nil && found != v {
					return nil
				}
				found = v
			}
		}
	}
	return found
}

func (lc *linCtx) isNonNeg(t term) bool {
	switch v := t.(type) {
	case atomKey:
		return lc.nonNeg[v]
	case scaledTerm:
		return lc.isNonNeg(v.Stride)
	}
	return false
}

// linearize converts expr into a linExpr. Always succeeds: anything that
// can't be reduced to linear form (a non-linear product, a call, ...)
// becomes its own opaque atom — sound (comparisons over it just won't
// prove anything useful unless the identical atom appears on both sides),
// never a hard failure.
func linearize(expr ast.Expr, lc *linCtx) linExpr {
	expr = unparenExpr(expr)

	if tv, ok := lc.info.Types[expr]; ok && tv.Value != nil {
		if k, ok := constant.Int64Val(tv.Value); ok {
			return constLin(k)
		}
	}

	switch x := expr.(type) {
	case *ast.Ident:
		obj, ok := lc.info.ObjectOf(x).(*types.Var)
		if !ok {
			break
		}
		if v, ok := lc.env[obj]; ok {
			return v
		}
		if lc.isIndexVar(obj) {
			return termLin(obj)
		}
		return resolveVar(obj, lc, lc.allowLastIterGuard)
	case *ast.BinaryExpr:
		switch x.Op {
		case token.ADD:
			return addLin(linearize(x.X, lc), linearize(x.Y, lc))
		case token.SUB:
			return subLin(linearize(x.X, lc), linearize(x.Y, lc))
		case token.MUL:
			l := linearize(x.X, lc)
			r := linearize(x.Y, lc)
			if l.isConst() {
				return scaleLin(r, l.konst)
			}
			if r.isConst() {
				return scaleLin(l, r.konst)
			}
			// Neither side is a plain integer constant — the common real
			// case is indexVar * <opaque non-negative atom> (e.g.
			// i*chunk, chunk itself being len(s)/n, not a literal).
			// That's not representable as an int64 coefficient, but it's
			// still a recognizable shape: a scaledTerm, handled specially
			// by regionSelfConsistent. Recognized structurally — one side
			// reduces to a bare *types.Var, the other to some single term
			// — not by matching lc.indexVar specifically: inside a
			// summary's own body (applySummary), the variable that ends
			// up here after substitution is the *caller's* index
			// (env-substituted), not lc.indexVar (which, in that
			// context, is still the summary's own loop variable — a
			// different object). Anything else genuinely non-linear
			// (e.g. i*j, two free variables neither of which is a bare
			// var) falls through to the opaque-atom fallback below,
			// covering the whole product — sound, just not decomposed.
			if lt, ok := soleTerm(l); ok {
				if v, isVar := lt.(*types.Var); isVar {
					if rt, ok := soleTerm(r); ok {
						return termLin(scaledTerm{Index: v, Stride: rt})
					}
				}
			}
			if rt, ok := soleTerm(r); ok {
				if v, isVar := rt.(*types.Var); isVar {
					if lt, ok := soleTerm(l); ok {
						return termLin(scaledTerm{Index: v, Stride: lt})
					}
				}
			}
		case token.QUO:
			// Division only ever matters here as part of an opaque
			// atom's own identity (chunk := len(s)/n) — not linearized
			// itself (Go division isn't linear), but len(...)/<anything>
			// is soundly known non-negative, which regionSelfConsistent
			// needs to trust a symbolic stride without knowing its value.
			key := atomKey(canonicalKey(expr, lc))
			if isLenExpr(x.X) {
				lc.nonNeg[key] = true
			}
			return termLin(key)
		}
	}

	key := atomKey(canonicalKey(expr, lc))
	if isLenExpr(expr) {
		lc.nonNeg[key] = true
	}
	return termLin(key)
}

// scaledTerm represents an index variable scaled by a stride that isn't a
// plain integer constant (e.g. i*chunk where chunk is len(s)/n) — a
// literal-constant stride is just an ordinary int64 coefficient in
// linExpr.terms and never needs this. Comparable (both fields are), so
// it's usable as a term/map key directly.
type scaledTerm struct {
	Index  *types.Var
	Stride term
}

// mentionsIndex reports whether idx appears anywhere in e's terms — as a
// plain term, or nested inside a scaledTerm's Index.
func mentionsIndex(e linExpr, idx *types.Var) bool {
	if idx == nil {
		return false
	}
	for t := range e.terms {
		switch v := t.(type) {
		case *types.Var:
			if v == idx {
				return true
			}
		case scaledTerm:
			if v.Index == idx {
				return true
			}
		}
	}
	return false
}

// soleTerm reports whether e is exactly 1*T (one term, coefficient 1, no
// constant) and returns T.
func soleTerm(e linExpr) (term, bool) {
	if e.konst != 0 || len(e.terms) != 1 {
		return nil, false
	}
	for t, c := range e.terms {
		if c == 1 {
			return t, true
		}
	}
	return nil, false
}

// isLenExpr reports whether e is (syntactically, after unwrapping parens)
// a call to the builtin len — used to seed non-negativity, since Go's
// len() is always >= 0.
func isLenExpr(e ast.Expr) bool {
	call, ok := unparenExpr(e).(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := unparenExpr(call.Fun).(*ast.Ident)
	return ok && id.Name == "len"
}

// canonicalKey renders expr into a string that's stable across
// substitution: sub-expressions are linearized (through the same env) and
// keyed via their own .key(), not raw source text, so len(s)/n computed
// from a callee's own parameters and, after substitution, from the
// caller's actual arguments, canonicalize identically when the arguments
// resolve the same way.
func canonicalKey(expr ast.Expr, lc *linCtx) string {
	switch x := expr.(type) {
	case *ast.CallExpr:
		parts := make([]string, len(x.Args))
		for i, a := range x.Args {
			parts[i] = linearize(a, lc).key()
		}
		return exprHead(x.Fun) + "(" + strings.Join(parts, ",") + ")"
	case *ast.BinaryExpr:
		return linearize(x.X, lc).key() + " " + x.Op.String() + " " + linearize(x.Y, lc).key()
	default:
		return types.ExprString(expr)
	}
}

func exprHead(e ast.Expr) string {
	if id, ok := unparenExpr(e).(*ast.Ident); ok {
		return id.Name
	}
	return types.ExprString(e)
}

func unparenExpr(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// indexBase unwraps every consecutive *ast.IndexExpr layer, returning the
// innermost base expression — e.g. tile[0][0] -> tile, chunk[0] -> chunk
// (a single-level index is already its own fixed point, so this is a
// strict generalization of "just take idx.X" that doesn't change 1D
// behavior at all). Used by collectArrayRegions' write detection: an
// indexed write into a multi-axis region (a 2D tile's own element,
// tile[r][c] = v) should conservatively mark the *whole* region written —
// every axis — matching the existing "whole region touched" philosophy
// for the 1D case, not narrowed to one axis's index specifically (see the
// package doc comment on why precise element-level bounds aren't
// attempted at all).
func indexBase(e ast.Expr) ast.Expr {
	for {
		ie, ok := unparenExpr(e).(*ast.IndexExpr)
		if !ok {
			return e
		}
		e = ie.X
	}
}

// ---------------------------------------------------------------------
// Variable resolution: trace a local to its own defining expression.
// ---------------------------------------------------------------------

// resolveVar traces v to its value by finding its single defining `:=`
// within lc.searchRoot and recursively linearizing that expression —
// real value-tracing, not a hardcoded allow-list (see the package doc
// comment). A variable that's declared more than once, reassigned
// unconditionally, or has no traceable origin in searchRoot is left as
// its own atomic term instead — not rejected outright, just not resolved
// past that point.
//
// allowLastIterGuard, used only by the array-summarizer resolving a
// loop's own end-bound local (its "hi"), additionally tolerates exactly
// one reassignment when it's guarded by `lc.indexVar == <expr>` — the
// generic "last iteration gets the remainder" idiom. This is sound for
// every comparison this file ever performs (never just convenient): the
// only place an end-bound resolved this way is used is comparing two
// *distinct* instantiations of lc.indexVar against each other (WLOG
// i<j), and i<j<=N-1 already establishes i can never be the guarded
// instantiation — so the guard is always false for the side whose
// end-bound actually gets read, and the pre-override value is exactly
// correct there, not an approximation.
func resolveVar(v *types.Var, lc *linCtx, allowLastIterGuard bool) linExpr {
	if lc.visiting[v] {
		return termLin(v)
	}
	def, ok := findSingleDef(v, lc, allowLastIterGuard)
	if !ok {
		return termLin(v)
	}
	lc.visiting[v] = true
	defer delete(lc.visiting, v)
	return linearize(def, lc)
}

// isSelfReferentialReassign reports whether stmt is `v = EXPR` where v
// itself appears somewhere inside EXPR — append (`v = append(v, x)`),
// reslicing within capacity (`v = v[:4]`), or any function call that
// might return a view of v's own prior value (`v = bytes.TrimSpace(v)`,
// `v = slices.Insert(v, 2, x)`, ...), same-file or not. Tolerated
// unconditionally (not gated by allowLastIterGuard — this isn't specific
// to a replicated loop) as a non-disqualifying reassignment, for one
// general reason that covers all of these without needing to know what
// the specific operation does: continuing to treat v as its
// pre-reassignment identity afterward is always a *sound* over-
// approximation. Either the operation keeps the same backing memory
// (exactly right), or it returns a narrower view of it (treating it as
// the wider original still contains the truth, just less precisely), or
// it returns something entirely unrelated (unnecessarily conservative,
// but never unsound — a later over-flag, never a missed one). The one
// thing that's never sound is the *previous* behavior: falling back to
// treating v as a fresh, unrelated identity the moment any reassignment
// is seen, which can silently lose a real hazard if the reassignment
// actually did keep the same memory (the common case for all of these).
//
// Without this, `sub := chunk[1:3]; sub = append(sub, x)` — the ordinary,
// idiomatic way anyone actually writes a growing slice in Go — would make
// sub unresolvable (an ordinary disqualifying reassignment) and fall back
// to treating it as its own unrelated identity, invisible to both the
// disjointness proof and the append-safety check despite still very much
// denoting a view into the original shared array at the point of the
// call itself.
func isSelfReferentialReassign(x *ast.AssignStmt, v *types.Var, info *types.Info) bool {
	if x.Tok != token.ASSIGN || len(x.Lhs) != 1 || len(x.Rhs) != 1 {
		return false
	}
	lhs, ok := x.Lhs[0].(*ast.Ident)
	if !ok || info.ObjectOf(lhs) != v {
		return false
	}
	found := false
	ast.Inspect(x.Rhs[0], func(n ast.Node) bool {
		if found {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && info.ObjectOf(id) == v {
			found = true
		}
		return true
	})
	return found
}

// findSingleDef finds v's sole `x := expr` within the whole file,
// requiring exactly one such declaration. Any other reassignment
// (`x = ...`, `x++`) makes v unresolvable, unless allowLastIterGuard is
// set and every such reassignment is inside an `if lc.indexVar == <expr>
// { x = ... }` with no else (see resolveVar's doc comment for why that
// specific case is safe to tolerate), or it's a self-referential
// reassignment (isSelfReferentialReassign, always tolerated).
//
// Searches lc.file, not lc.searchRoot: a variable captured from an
// enclosing function (e.g. `mid` declared in main, used inside a par
// branch closure) is declared outside searchRoot's narrower scope, and
// findSingleDef matches by object identity, not name or position, so
// widening the search is always safe — it can only ever match ast.Idents
// that actually resolve to v, which can only appear within v's own real
// lexical scope regardless of how much of the tree gets walked.
func findSingleDef(v *types.Var, lc *linCtx, allowLastIterGuard bool) (ast.Expr, bool) {
	var def ast.Expr
	defCount := 0
	badWrite := false

	isLastIterGuard := func(stmt ast.Stmt) bool {
		ifs, ok := stmt.(*ast.IfStmt)
		if !ok || ifs.Init != nil || ifs.Else != nil || len(lc.indexVars) == 0 {
			return false
		}
		cond, ok := ifs.Cond.(*ast.BinaryExpr)
		if !ok || cond.Op != token.EQL {
			return false
		}
		id, ok := unparenExpr(cond.X).(*ast.Ident)
		if !ok {
			return false
		}
		condVar, ok := lc.info.ObjectOf(id).(*types.Var)
		if !ok || !lc.isIndexVar(condVar) {
			return false
		}
		if len(ifs.Body.List) != 1 {
			return false
		}
		assign, ok := ifs.Body.List[0].(*ast.AssignStmt)
		if !ok || assign.Tok != token.ASSIGN || len(assign.Lhs) != 1 {
			return false
		}
		id, ok = assign.Lhs[0].(*ast.Ident)
		return ok && lc.info.ObjectOf(id) == v
	}

	var checkWrite func(stmt ast.Stmt) bool
	checkWrite = func(stmt ast.Stmt) bool {
		if allowLastIterGuard && isLastIterGuard(stmt) {
			return true
		}
		return false
	}

	ast.Inspect(lc.file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || lc.info.ObjectOf(id) != v {
					continue
				}
				if x.Tok == token.DEFINE {
					defCount++
					if len(x.Rhs) == len(x.Lhs) {
						def = x.Rhs[i]
					} else {
						def = nil
					}
				} else if !isSelfReferentialReassign(x, v, lc.info) {
					badWrite = true
				}
			}
		case *ast.IfStmt:
			if checkWrite(x) {
				return false // already accounted for; don't also flag the nested assign as a bad write
			}
		case *ast.IncDecStmt:
			if id, ok := x.X.(*ast.Ident); ok && lc.info.ObjectOf(id) == v {
				badWrite = true
			}
		}
		return true
	})

	if defCount == 1 && !badWrite && def != nil {
		return def, true
	}
	return nil, false
}

// ---------------------------------------------------------------------
// Region: a normalized [Start,End) view of a root array/slice.
// ---------------------------------------------------------------------

// Interval is a normalized [Start,End) view along one axis, in the same
// term/linExpr vocabulary linearize produces.
type Interval struct {
	Start, End linExpr
	// RepIndex is non-nil when Start/End were derived from (and still
	// mention) a replicated/summarized construct's own index variable —
	// regionSelfConsistent uses it to prove any two distinct
	// instantiations disjoint along this axis without enumerating them.
	RepIndex *types.Var
}

// Region is Base sliced along one or more independent axes — one for an
// ordinary 1D array/slice (splitN's own domain), two for a rectangular
// tile of a [][]T matrix (splitN2D's domain — grid.go). Base is found by
// walking a slicing/indexing chain back to its origin (the "normalize to
// the root" step); Axes[0] is always the outermost dimension, matching
// how Go's own slicing syntax always narrows the outermost dimension
// first (regionOf's SliceExpr case).
//
// Two axis-aligned regions of the same Base are disjoint if *any single
// axis* is disjoint (proveDisjoint) — the standard separating-axis
// argument, which holds even for a genuinely nested [][]T (each row its
// own independent backing array): two tiles can only share memory within
// rows both include, and a disjoint column axis rules that out regardless
// of row overlap. A 1D Region is just the one-axis case of this, so
// nothing about the 1D proof changes.
type Region struct {
	Base term
	Axes []Interval
	// Capped is true when this region's outermost (axis-0) capacity is
	// known bounded to its own length (a three-index slice, or a
	// summarized function's own three-index slice) — append is only safe
	// on a capped region. Region-level, not per-axis: append only ever
	// grows the outermost dimension of whatever's passed to it, so only
	// axis 0's capacity is ever actually at stake.
	Capped bool
}

func sameBase(a, b term) bool {
	ak, bk := termKey(a), termKey(b)
	return ak == bk
}

// regionOf resolves expr to the Region it denotes, if it denotes one at
// all — a root array/slice identifier, a slicing chain over one, or an
// index into a summarized function's return value.
func regionOf(expr ast.Expr, lc *linCtx, summaries summaryTables) (Region, bool) {
	expr = unparenExpr(expr)

	switch x := expr.(type) {
	case *ast.SliceExpr:
		base, ok := regionOf(x.X, lc, summaries)
		if !ok || len(base.Axes) == 0 {
			return Region{}, false
		}
		// Go's slicing syntax always narrows the outermost dimension —
		// axis 0 — whether base is a plain 1D region or a further axis
		// (e.g. axis 1, columns) of a multi-axis tile (grid.go); any
		// remaining axes travel through unchanged.
		ax0 := base.Axes[0]

		lo := constLin(0)
		if x.Low != nil {
			lo = linearize(x.Low, lc)
		}
		hi := subLin(ax0.End, ax0.Start)
		if x.High != nil {
			hi = linearize(x.High, lc)
		}

		start, end := addLin(ax0.Start, lo), addLin(ax0.Start, hi)
		repIndex := ax0.RepIndex
		if base.Capped && ax0.RepIndex != nil {
			// base is itself already a replicated/index-parameterized
			// sub-region (e.g. chunks[i], not the plain root array) —
			// Go's own runtime bounds check guarantees a further slice
			// of a capacity-capped region can never exceed that region's
			// own [Start,End) — it either stays within or panics (a
			// different, acceptable failure mode, not a data race — see
			// the package doc comment's capacity-gap addendum). So for
			// disjointness purposes this region inherits its capped
			// parent's full extent rather than needing its own narrower
			// bounds proven numerically (which, for a symbolic stride
			// like splitN's chunk, generally can't be — we don't know
			// its concrete value). Always a sound over-approximation:
			// this region is provably a subset of what's now being
			// compared, never the reverse.
			//
			// Requiring ax0.RepIndex specifically (not just
			// base.Capped) matters: a plain capped *root* array (e.g.
			// data := make([]int, 12), which is capped by construction)
			// must NOT widen a slice of it back to the whole array —
			// that would defeat slicing entirely and make two genuinely
			// disjoint hand-written slices of data look like they
			// overlap.
			start, end = ax0.Start, ax0.End
		} else if repIndex == nil {
			// A hand-written slice (not going through a summarized
			// function at all, and not itself capped) whose own bounds
			// reference one of the currently-active replicated
			// construct's indices — e.g. data[i*k:(i+1)*k] directly
			// inside a parFor body. ax0.RepIndex alone wouldn't catch
			// this: base is just the root array here, never itself
			// replicated. soleIndexVarIn returns nil (conservatively,
			// not just unprovable) if lo/hi mention more than one active
			// index var at once — an ambiguous shape no fixture needs.
			repIndex = lc.soleIndexVarIn(lo, hi)
		}
		axes := append([]Interval{{Start: start, End: end, RepIndex: repIndex}}, base.Axes[1:]...)
		return Region{Base: base.Base, Axes: axes, Capped: x.Slice3}, true

	case *ast.IndexExpr:
		// A doubly-indexed access (tiles[i][j]) into a splitN2D-shaped
		// grid summary's result resolves both indices together, into a
		// 2-axis Region — see grid.go. Anything else (a single index, or
		// a double index whose inner base isn't a grid summary) falls
		// through to the ordinary single-index path unchanged.
		if r, ok := regionFromDoubleIndex(x, lc, summaries); ok {
			return r, true
		}
		return regionFromIndex(x.X, x.Index, lc, summaries)

	case *ast.Ident:
		obj, ok := lc.info.ObjectOf(x).(*types.Var)
		if !ok {
			return Region{}, false
		}
		if r, ok := lc.renv[obj]; ok {
			return r, true
		}
		switch obj.Type().Underlying().(type) {
		case *types.Slice, *types.Array:
		default:
			return Region{}, false
		}

		lenAtom := atomKey(canonicalKey(&ast.CallExpr{Fun: ast.NewIdent("len"), Args: []ast.Expr{x}}, lc))
		lc.nonNeg[lenAtom] = true

		if def, ok := findSingleDef(obj, lc, false); ok {
			// A local traced back to its own slicing/indexing definition
			// (chunk := data[0:5]) resolves through that — not as its
			// own fresh root — so the derived Region (and its own
			// Capped-ness) is correct relative to whatever it's really a
			// view of.
			if r, ok := regionOf(def, lc, summaries); ok {
				return r, true
			}
			// A genuinely fresh two-argument make([]T, n) is the one
			// shape known capacity-bounded (cap==len, by Go's own
			// semantics) — anything else traced-but-unresolved (a
			// three-argument make, a composite literal, ...) falls
			// through conservatively uncapped below.
			if call, ok := unparenExpr(def).(*ast.CallExpr); ok {
				if id, ok := unparenExpr(call.Fun).(*ast.Ident); ok && id.Name == "make" && len(call.Args) == 2 {
					return Region{Base: term(obj), Axes: []Interval{{Start: constLin(0), End: termLin(lenAtom)}}, Capped: true}, true
				}
			}
		}
		// A parameter, or anything else not traceable to one of the
		// above — conservatively not known capacity-bounded (matters
		// only for the append-safety check; disjointness proofs never
		// look at Capped).
		return Region{Base: term(obj), Axes: []Interval{{Start: constLin(0), End: termLin(lenAtom)}}, Capped: false}, true
	}

	return Region{}, false
}

// regionFromIndex resolves base[idx], where base is either a plain
// []T/[N]T (a scalar element access, not a region — returns false) or the
// return value of a summarized function (a region, parameterized by the
// summary's own loop variable, substituted with idx here).
func regionFromIndex(baseExpr, idxExpr ast.Expr, lc *linCtx, summaries summaryTables) (Region, bool) {
	src, ok := findCallSource(baseExpr, lc, summaries.arr)
	if !ok {
		return Region{}, false
	}
	return applySummary(src, idxExpr, lc, summaries)
}

// callSource pairs a computed summary (an *arraySummary, or grid.go's
// *gridSummary) with the actual argument expressions and calling context
// it was invoked with — resolution is deferred until an index is actually
// applied (regionFromIndex / grid.go's applyGridSummary), since that's
// when the summary's own loop variable(s) get substituted with the
// caller's index expression(s). Generic over the summary type so both
// summary kinds share this one tracing mechanism.
type callSource[S any] struct {
	summary  S
	callArgs []ast.Expr
	callCtx  *linCtx
}

// findCallSource traces baseExpr (e.g. `chunks` in `chunks[i]`) back to
// the call that produced it — a bare identifier assigned, exactly once,
// from a call to a function with a computed summary in summaries. A
// region-producing local like `chunks` is typically declared in the
// enclosing function, outside the branch closure that's actually using
// it; findSingleDef already searches the whole file for exactly this
// reason.
func findCallSource[S any](baseExpr ast.Expr, lc *linCtx, summaries map[*types.Func]S) (callSource[S], bool) {
	id, ok := unparenExpr(baseExpr).(*ast.Ident)
	if !ok {
		return callSource[S]{}, false
	}
	obj, ok := lc.info.ObjectOf(id).(*types.Var)
	if !ok {
		return callSource[S]{}, false
	}
	def, ok := findSingleDef(obj, lc, false)
	if !ok {
		return callSource[S]{}, false
	}
	call, ok := unparenExpr(def).(*ast.CallExpr)
	if !ok {
		return callSource[S]{}, false
	}
	fnID, ok := unparenExpr(call.Fun).(*ast.Ident)
	if !ok {
		return callSource[S]{}, false
	}
	fnObj, ok := lc.info.ObjectOf(fnID).(*types.Func)
	if !ok {
		return callSource[S]{}, false
	}
	summary, ok := summaries[fnObj]
	if !ok {
		return callSource[S]{}, false
	}
	return callSource[S]{summary: summary, callArgs: call.Args, callCtx: lc}, true
}

// applySummary resolves src[idxExpr] into a concrete Region: a fresh
// linCtx substitutes src.summary's own parameters with the actual
// arguments (resolved in src.callCtx) and its loop variable with
// idxExpr (resolved in lc, the *using* context — usually just a bare
// reference to lc's own replicated index, kept free).
func applySummary(src callSource[*arraySummary], idxExpr ast.Expr, lc *linCtx, summaries summaryTables) (Region, bool) {
	s := src.summary

	baseArgRegion, ok := regionOf(s.baseArgExpr(src.callArgs), src.callCtx, summaries)
	if !ok || len(baseArgRegion.Axes) == 0 {
		return Region{}, false
	}
	baseAx0 := baseArgRegion.Axes[0]

	// Only the loop variable itself needs substituting here — the
	// summary's other parameters (e.g. splitN's own n) are left as their
	// own unresolved terms, keyed by their parameter identity. That's
	// fine: lo/hi are both linearized in this same sub context, so any
	// repeated reference to them (chunk appearing in both lo and hi, say)
	// keys identically within this one application, which is all
	// regionSelfConsistent needs — it never has to relate this summary's
	// internal computation to an unrelated, independently-written one.
	sub := newLinCtx(lc.info, lc.file, s.Decl.Body)
	sub.nonNeg = lc.nonNeg
	sub.allowLastIterGuard = true
	sub.indexVars[s.LoopVar] = true
	sub.env[s.LoopVar] = linearize(idxExpr, lc)

	lo := linearize(s.LoExpr, sub)
	hi := linearize(s.HiExpr, sub)

	idxLin := linearize(idxExpr, lc)
	var repIndex *types.Var
	if t, ok := soleTerm(idxLin); ok {
		if v, ok := t.(*types.Var); ok && lc.isIndexVar(v) {
			repIndex = v
		}
	}

	return Region{
		Base: baseArgRegion.Base,
		Axes: []Interval{{
			Start:    addLin(baseAx0.Start, lo),
			End:      addLin(baseAx0.Start, hi),
			RepIndex: repIndex,
		}},
		Capped: s.Capped,
	}, true
}

// summaryTables bundles both recognized summary kinds — 1D (arraySummary)
// and 2D (grid.go's gridSummary) — into the one value threaded through
// regionOf and everything that calls it, so adding a second summary kind
// meant changing one parameter's type, not adding a second parameter to
// every signature in this file.
type summaryTables struct {
	arr  map[*types.Func]*arraySummary
	grid map[*types.Func]*gridSummary
}

// ---------------------------------------------------------------------
// arraySummary: recognizing "X := make(...); for VAR := range N { ...;
// X[VAR] = BASE[lo:hi] }; return X" in any function — not one named
// splitN. See the package doc comment.
// ---------------------------------------------------------------------

type arraySummary struct {
	Decl           *ast.FuncDecl
	LoopVar        *types.Var // the loop's own induction variable
	BaseParamIdx   int        // which of Decl's own params is being sliced
	LoExpr, HiExpr ast.Expr   // raw bound expressions, in terms of LoopVar and Decl's params
	Capped         bool       // the loop's own assignment used a three-index slice
}

func (s *arraySummary) baseArgExpr(callArgs []ast.Expr) ast.Expr {
	if s.BaseParamIdx < len(callArgs) {
		return callArgs[s.BaseParamIdx]
	}
	return nil
}

// computeArraySummaries computes an arraySummary for every same-file
// function whose body matches the recognized shape, memoized once per
// file.
func computeArraySummaries(info *types.Info, file *ast.File) map[*types.Func]*arraySummary {
	out := map[*types.Func]*arraySummary{}
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Body == nil {
			continue
		}
		if s, ok := computeSummary(fd, info); ok {
			if obj, ok := info.Defs[fd.Name].(*types.Func); ok {
				out[obj] = s
			}
		}
	}
	return out
}

func computeSummary(decl *ast.FuncDecl, info *types.Info) (*arraySummary, bool) {
	if decl.Body == nil {
		return nil, false
	}

	var loopVar *types.Var
	var baseExpr, loExpr, hiExpr ast.Expr
	var capped, found bool

	ast.Inspect(decl.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		rs, ok := n.(*ast.RangeStmt)
		if !ok || rs.Key == nil || rs.Value != nil || rs.Body == nil {
			return true
		}
		keyIdent, ok := rs.Key.(*ast.Ident)
		if !ok {
			return true
		}
		lv, ok := info.Defs[keyIdent].(*types.Var)
		if !ok {
			return true
		}
		for _, stmt := range rs.Body.List {
			as, ok := stmt.(*ast.AssignStmt)
			if !ok || as.Tok != token.ASSIGN || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				continue
			}
			idx, ok := as.Lhs[0].(*ast.IndexExpr)
			if !ok {
				continue
			}
			idxIdent, ok := unparenExpr(idx.Index).(*ast.Ident)
			if !ok || info.ObjectOf(idxIdent) != lv {
				continue
			}
			se, ok := as.Rhs[0].(*ast.SliceExpr)
			if !ok || se.Low == nil || se.High == nil {
				continue
			}
			loopVar, baseExpr, loExpr, hiExpr, capped = lv, se.X, se.Low, se.High, se.Slice3
			found = true
		}
		return true
	})

	if !found {
		return nil, false
	}

	baseIdent, ok := unparenExpr(baseExpr).(*ast.Ident)
	if !ok {
		return nil, false
	}
	baseObj, ok := info.ObjectOf(baseIdent).(*types.Var)
	if !ok {
		return nil, false
	}
	idx := paramIndex(decl, baseObj, info)
	if idx < 0 {
		return nil, false
	}

	return &arraySummary{
		Decl: decl, LoopVar: loopVar, BaseParamIdx: idx,
		LoExpr: loExpr, HiExpr: hiExpr, Capped: capped,
	}, true
}

func paramIndex(decl *ast.FuncDecl, v *types.Var, info *types.Info) int {
	i := 0
	for _, field := range decl.Type.Params.List {
		names := field.Names
		if len(names) == 0 {
			i++
			continue
		}
		for _, name := range names {
			if info.Defs[name] == types.Object(v) {
				return i
			}
			i++
		}
	}
	return -1
}

// ---------------------------------------------------------------------
// Disjointness proof.
// ---------------------------------------------------------------------

func (r Region) key() string {
	parts := make([]string, len(r.Axes))
	for i, ax := range r.Axes {
		parts[i] = ax.Start.key() + ":" + ax.End.key()
	}
	return termKey(r.Base) + "[" + strings.Join(parts, ",") + "]"
}

// hasRepIndex reports whether any of r's axes carries a RepIndex.
func hasRepIndex(r Region) bool {
	for _, ax := range r.Axes {
		if ax.RepIndex != nil {
			return true
		}
	}
	return false
}

// intervalSelfConsistent reports whether ax's own formula guarantees any
// two distinct instantiations of its RepIndex are disjoint *along this one
// axis*, without needing to know RepIndex's concrete value — the fact
// that closes the replicated-construct gap (see the package doc comment).
// ax.Start must be exactly Stride*RepIndex+K (a plain int coefficient, or
// the scaledTerm form for a symbolic stride like len(s)/n), and
// ax.End-ax.Start must be exactly that same Stride, known non-negative.
func intervalSelfConsistent(ax Interval, lc *linCtx) bool {
	if ax.RepIndex == nil || len(ax.Start.terms) != 1 {
		return false
	}
	stride := subLin(ax.End, ax.Start)

	if c, ok := ax.Start.terms[term(ax.RepIndex)]; ok {
		return c > 0 && stride.isConst() && stride.konst == c
	}
	for t, c := range ax.Start.terms {
		st, ok := t.(scaledTerm)
		if !ok || st.Index != ax.RepIndex || c != 1 {
			continue
		}
		return !stride.isConst() && stride.key() == termLin(st.Stride).key() && lc.isNonNeg(st.Stride)
	}
	return false
}

// regionSelfConsistent reports whether r's own formula guarantees any two
// *distinct full instantiations* of r (differing in at least one axis'
// coordinate — a replicated/summarized construct's own family, e.g.
// tiles[i][j] for every (i,j) a nested parFor produces) are pairwise
// disjoint, without enumerating them.
//
// Requires *every* axis that carries a RepIndex to independently pass its
// own intervalSelfConsistent — not just one. Sound by case analysis: two
// distinct full instantiations must differ in at least one coordinate,
// but which one isn't known in advance (e.g. two tiles with the same row
// index and different column indices differ only on the column axis) — so
// every replicated axis has to be independently provably disjoint-when-
// varying for the whole region to be safe against every possible way two
// instantiations could differ. This is *not* the same question
// proveDisjoint(r, r, lc) would ask (that only requires *some* axis to be
// disjoint for two already-fixed regions being compared) — deliberately a
// separate function, not routed through proveDisjoint, for exactly this
// reason.
func regionSelfConsistent(r Region, lc *linCtx) bool {
	sawRepIndex := false
	for _, ax := range r.Axes {
		if ax.RepIndex == nil {
			continue
		}
		sawRepIndex = true
		if !intervalSelfConsistent(ax, lc) {
			return false
		}
	}
	return sawRepIndex
}

// intervalDisjoint proves ax1 and ax2 — the same axis of two Regions
// sharing a Base — never overlap: either a plain constant-bounds proof
// (end1<=start2 or end2<=start1, both fully resolved), or, when both
// intervals share the same RepIndex and formula (two instantiations of
// one replicated/summarized construct's own axis), intervalSelfConsistent's
// general monotonicity argument.
func intervalDisjoint(ax1, ax2 Interval, lc *linCtx) bool {
	if ax1.RepIndex != nil && ax1.RepIndex == ax2.RepIndex &&
		ax1.Start.key() == ax2.Start.key() && ax1.End.key() == ax2.End.key() {
		return intervalSelfConsistent(ax1, lc)
	}
	return provableLE(ax1.End, ax2.Start) || provableLE(ax2.End, ax1.Start)
}

// proveDisjoint proves a and b never overlap. Different roots are
// trivially disjoint. Same root, same axis count: disjoint if *any single
// axis* is disjoint (intervalDisjoint) — the standard separating-axis
// argument for axis-aligned regions (see Region's own doc comment), which
// a plain 1D Region is just the one-axis case of. Anything else
// (mismatched axis counts, mismatched terms, an unresolved coefficient, a
// non-linear combination) is unprovable and rejected, matching this
// project's standing "can't verify, so reject" stance.
func proveDisjoint(a, b Region, lc *linCtx) bool {
	if !sameBase(a.Base, b.Base) {
		return true
	}
	if len(a.Axes) == 0 || len(a.Axes) != len(b.Axes) {
		return false
	}
	for i := range a.Axes {
		if intervalDisjoint(a.Axes[i], b.Axes[i], lc) {
			return true
		}
	}
	return false
}

// provableLE proves x <= y from constants alone.
func provableLE(x, y linExpr) bool {
	diff := subLin(y, x)
	return diff.isConst() && diff.konst >= 0
}

// ---------------------------------------------------------------------
// Per-branch collection: written vs. used regions, interprocedural.
// ---------------------------------------------------------------------

type regionOcc struct {
	region Region
	pos    token.Pos
}

func mergeRegionMap(dst, src map[string]regionOcc) {
	for k, v := range src {
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
}

// collectArrayRegions walks body (a par branch's own FuncLit body, a
// parFor body, or a same-file callee's body reached by recursing into it)
// collecting written and used regions, keyed by their normalized form —
// only regions whose Base is free relative to lc.searchRoot (isFree,
// channels.go) are tracked, matching every other rule in this package.
// appendViolations collects append calls on an uncapped tracked region
// (see the package doc comment's capacity-gap addendum).
func collectArrayRegions(
	info *types.Info,
	body ast.Node,
	lc *linCtx,
	declByFunc map[*types.Func]*ast.FuncDecl,
	summaries summaryTables,
	visiting map[*types.Func]bool,
) (written, used map[string]regionOcc, appendViolations []token.Pos) {
	written = map[string]regionOcc{}
	used = map[string]regionOcc{}

	recordInto := func(m map[string]regionOcc, r Region, pos token.Pos) {
		baseVar, ok := r.Base.(*types.Var)
		if !ok || !isFree(baseVar, lc.searchRoot) {
			return
		}
		k := r.key()
		if _, exists := m[k]; !exists {
			m[k] = regionOcc{region: r, pos: pos}
		}
	}

	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				idx, ok := unparenExpr(lhs).(*ast.IndexExpr)
				if !ok {
					continue
				}
				base, ok := regionOf(indexBase(idx), lc, summaries)
				if !ok {
					continue
				}
				// Conservatively, the whole region indexBase(idx) denotes
				// counts as written — not narrowed to any index
				// specifically, on any axis. Precisely bounding a single
				// element would need proving the index is within base's
				// own [Start,End), which for a symbolic stride
				// (chunk := len(s)/n) needs chunk>=1, not just chunk>=0 —
				// a strict-positivity fact this checker doesn't (and
				// shouldn't: n could exceed len(s)) claim to know. Any
				// touch of a shared array/slice argument needs
				// disjointness, not just a provable write to one index.
				// indexBase unwraps every level of
				// indexing (tile[r][c] -> tile), not just one, so a
				// write into one element of a multi-axis 2D tile
				// (grid.go) is recorded the same conservative way.
				recordInto(written, base, idx.Pos())
			}

		case *ast.CallExpr:
			fnID, isIdentCall := unparenExpr(x.Fun).(*ast.Ident)

			if isIdentCall && fnID.Name == "len" && len(x.Args) == 1 {
				// len(x) reads x's length, not its contents — not a use
				// of the whole region for disjointness purposes. Skip
				// descending into its argument so the generic tryUse
				// pass below never sees it as a bare "whole array" touch
				// (which it otherwise would, e.g. len(data) inside
				// mip := len(data)/2).
				return false
			}

			if isIdentCall && fnID.Name == "append" && len(x.Args) > 0 {
				if r, ok := regionOf(x.Args[0], lc, summaries); ok {
					if _, isVar := r.Base.(*types.Var); isVar && isFree(r.Base.(*types.Var), lc.searchRoot) && !r.Capped {
						appendViolations = append(appendViolations, x.Pos())
					}
				}
			}

			if isIdentCall && fnID.Name == "copy" && len(x.Args) == 2 {
				// copy(dst, src) writes into dst's existing elements (up
				// to min(len(dst),len(src))) and reads src — unlike
				// append, it never grows or reallocates, so it's exactly
				// the conservative "whole region touched" shape the rest
				// of this file already uses, not a capacity concern.
				if r, ok := regionOf(x.Args[0], lc, summaries); ok {
					recordInto(written, r, x.Pos())
				}
				if r, ok := regionOf(x.Args[1], lc, summaries); ok {
					recordInto(used, r, x.Pos())
				}
			}

			if isIdentCall {
				if calleeObj, ok := info.ObjectOf(fnID).(*types.Func); ok {
					if decl, ok := declByFunc[calleeObj]; ok && !visiting[calleeObj] {
						sub := newLinCtx(info, lc.file, decl.Body)
						sub.nonNeg = lc.nonNeg

						pi := 0
						for _, field := range decl.Type.Params.List {
							names := field.Names
							if len(names) == 0 {
								pi++
								continue
							}
							for _, name := range names {
								if pi < len(x.Args) {
									if pobj, ok := info.Defs[name].(*types.Var); ok {
										if r, ok := regionOf(x.Args[pi], lc, summaries); ok {
											sub.renv[pobj] = r
										} else {
											sub.env[pobj] = linearize(x.Args[pi], lc)
										}
									}
								}
								pi++
							}
						}

						visiting[calleeObj] = true
						w2, u2, av2 := collectArrayRegions(info, decl.Body, sub, declByFunc, summaries, visiting)
						mergeRegionMap(written, w2)
						mergeRegionMap(used, u2)
						appendViolations = append(appendViolations, av2...)
						delete(visiting, calleeObj)

						// Once we've recursed into a same-file callee,
						// each argument's own precise region (if any) is
						// already captured above via renv substitution —
						// don't also let the generic tryUse pass below
						// independently record a bare argument identifier
						// (e.g. writeLeft(data), data passed whole) as
						// its own separate "whole array" use; that's
						// strictly redundant with, and wider than, what
						// the recursion already found, and doubles up
						// conflicts rather than adding a real one.
						return false
					}
				}
			}
		}

		// Only these three forms ever denote a region on their own
		// (regionOf's own cases) — restricting to them, rather than
		// trying every ast.Expr, matters for the stop-descending step
		// just below: a *ast.CallExpr also satisfies ast.Expr, but unless
		// it was just handled by recursing into a same-file callee above
		// (which already returned false), it must keep descending into
		// its own arguments regardless — that's the only way a call to
		// an unrecognized function (append/copy/len excepted, handled
		// above; anything external) ever gets its arguments' own regions
		// seen at all.
		switch expr := n.(type) {
		case *ast.Ident, *ast.IndexExpr, *ast.SliceExpr:
			if r, ok := regionOf(expr.(ast.Expr), lc, summaries); ok {
				recordInto(used, r, expr.(ast.Expr).Pos())
				// Don't also separately record this region's own base
				// identifier as an independent "whole array" use — e.g.
				// data[mip:len(data)] resolving here means data itself,
				// visited again as x.X's child node, must not also count
				// as a bare, unrelated touch of the whole array.
				return false
			}
		}
		return true
	})

	return written, used, appendViolations
}

// ---------------------------------------------------------------------
// Top-level check.
// ---------------------------------------------------------------------

// ArraySharingConflict is a region written in one par branch that
// provably (or, since disjointness couldn't be proven, possibly)
// overlaps a region touched — written or read — in a different sibling
// branch.
type ArraySharingConflict struct {
	WriteBranch int
	WritePos    token.Pos
	UseBranch   int
	UsePos      token.Pos
}

// ArrayAppendViolation is an append call on a tracked region whose
// capacity isn't known bounded to its own length — see the package doc
// comment's capacity-gap addendum.
type ArrayAppendViolation struct {
	Pos token.Pos
}

// parForBranch detects `parFor(N, func(i int) {...})` as a single
// branch's own body, returning the FuncLit's body and its parameter (the
// replication index) — mirrors parBranches' recognition of par(...)
// itself, needed here both to mark the branch boundary and to expose the
// index for linCtx.indexVars.
func parForIndex(body ast.Node, info *types.Info) (ast.Node, *types.Var, bool) {
	var result ast.Node
	var idx *types.Var
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := unparenExpr(call.Fun).(*ast.Ident)
		if !ok || id.Name != "parFor" || len(call.Args) != 2 {
			return true
		}
		lit, ok := call.Args[1].(*ast.FuncLit)
		if !ok || len(lit.Type.Params.List) != 1 || len(lit.Type.Params.List[0].Names) != 1 {
			return true
		}
		v, ok := info.Defs[lit.Type.Params.List[0].Names[0]].(*types.Var)
		if !ok {
			return true
		}
		result, idx, found = lit.Body, v, true
		return false
	})
	return result, idx, found
}

// parForChain detects a (possibly nested) chain of `parFor(N, func(i
// int){...})` calls as a single branch's own body, descending into each
// one's own body in turn via parForIndex. A single, non-nested parFor is
// a chain of length 1 — today's exact behavior, unchanged. A 2D grid
// consumption pattern (parFor(nr, func(i){ parFor(nc, func(j){...}) }) —
// grid.go) is a chain of length 2, both index variables live
// simultaneously in the innermost body. Returns the chain in
// outermost-to-innermost order, plus the innermost FuncLit's body as the
// walk root.
func parForChain(body ast.Node, info *types.Info) (ast.Node, []*types.Var, bool) {
	var chain []*types.Var
	cur := body
	for {
		next, v, ok := parForIndex(cur, info)
		if !ok {
			break
		}
		chain = append(chain, v)
		cur = next
	}
	if len(chain) == 0 {
		return nil, nil, false
	}
	return cur, chain, true
}

// CheckArraySharing walks file for par(...) calls and reports
// ArraySharingConflicts and ArrayAppendViolations among their branches.
func CheckArraySharing(info *types.Info, file *ast.File) ([]ArraySharingConflict, []ArrayAppendViolation) {
	declByFunc := funcDecls(info, file)
	summaries := summaryTables{
		arr:  computeArraySummaries(info, file),
		grid: computeGridSummaries(info, file),
	}

	var conflicts []ArraySharingConflict
	var appendViolations []ArrayAppendViolation

	for _, branches := range parBranches(file) {
		written := make([]map[string]regionOcc, len(branches))
		used := make([]map[string]regionOcc, len(branches))

		// Shared across every branch of this par(...) call, and reused
		// for the disjointness proof below — regionSelfConsistent needs
		// the same non-negativity facts (e.g. "len(s)/n >= 0") that were
		// established while collecting, not a fresh, empty set.
		sharedNonNeg := map[atomKey]bool{}

		for i, b := range branches {
			body, idxVars, isParFor := parForChain(b.Body, info)
			walkBody, scope := ast.Node(b.Body), ast.Node(b)
			if isParFor {
				walkBody = body
			}

			lc := newLinCtx(info, file, scope)
			lc.nonNeg = sharedNonNeg
			for _, v := range idxVars {
				lc.indexVars[v] = true
			}

			w, u, av := collectArrayRegions(info, walkBody, lc, declByFunc, summaries, map[*types.Func]bool{})
			written[i], used[i] = w, u
			for _, pos := range av {
				appendViolations = append(appendViolations, ArrayAppendViolation{Pos: pos})
			}
		}

		proofCtx := newLinCtx(info, file, file)
		proofCtx.nonNeg = sharedNonNeg

		for i := range branches {
			// A written region parameterized by a replicated construct's
			// own index (e.g. chunks[i] inside a parFor body, or
			// tiles[i][j] inside a nested one — grid.go) represents every
			// concurrent instantiation at once, not one occurrence —
			// regionSelfConsistent checks it against itself directly (not
			// via proveDisjoint — see its own doc comment for why that
			// distinction matters once there's more than one axis),
			// closing the gap no existing check inspects at all (a
			// replicated par body).
			for _, w := range written[i] {
				if hasRepIndex(w.region) && !regionSelfConsistent(w.region, proofCtx) {
					conflicts = append(conflicts, ArraySharingConflict{
						WriteBranch: i, WritePos: w.pos,
						UseBranch: i, UsePos: w.pos,
					})
				}
			}

			for _, w := range written[i] {
				for j := range branches {
					if i == j {
						continue
					}
					for _, u := range used[j] {
						if !sameBase(w.region.Base, u.region.Base) {
							continue
						}
						if !proveDisjoint(w.region, u.region, proofCtx) {
							conflicts = append(conflicts, ArraySharingConflict{
								WriteBranch: i, WritePos: w.pos,
								UseBranch: j, UsePos: u.pos,
							})
						}
					}
				}
			}
		}
	}

	return conflicts, appendViolations
}
