// grid.go extends regions.go's disjointness proof to genuine 2D tiling —
// splitN2D's own domain (tools/bilc/bilc.go), consumed via a nested
// replicated par (`par i := range nr { par j := range nc { ... } }`).
//
// Go has no single-expression syntax for slicing both dimensions of a
// [][]T at once (unlike NumPy's m[r0:r1, c0:c1]) — a rectangular tile can
// only be built by an explicit per-row loop. So unlike regions.go's own
// 1D engine (which proves *arbitrary hand-written* slice arithmetic
// disjoint, not just splitN's own), 2D tiling can only ever come from a
// helper function performing that loop internally — there's no
// hand-written syntax to give parity to. gridSummary recognizes exactly
// the shape splitN2D's own body has: two nested range loops (the row and
// column axes) around an innermost `for r := r0; r < r1; r++` loop
// slicing each row's own column range — not one named splitN2D, matching
// arraySummary's own "recognize the shape, not the name" spirit, but
// necessarily narrower than arraySummary's single-loop generality, since
// this one specific three-level shape is the only one Go's own syntax
// makes 2D tiling possible with at all.
//
// A tile is consumed as tiles[i][j] — a doubly-indexed expression, two
// nested *ast.IndexExprs. regionOf's own IndexExpr case (regions.go) tries
// regionFromDoubleIndex first; anything that isn't this exact shape falls
// through to the ordinary single-index path unchanged.
package main

import (
	"go/ast"
	"go/token"
	"go/types"
)

// gridSummary describes a recognized 2D-tiling function: out[i][j] is the
// (RowLoopVar=i, ColLoopVar=j)'th tile of Decl's BaseParamIdx'th
// parameter, with axis bounds RowLoExpr/RowHiExpr (in terms of
// RowLoopVar) and ColLoExpr/ColHiExpr (in terms of ColLoopVar) — the 2D
// counterpart of arraySummary.
type gridSummary struct {
	Decl                 *ast.FuncDecl
	RowLoopVar           *types.Var
	ColLoopVar           *types.Var
	BaseParamIdx         int
	RowLoExpr, RowHiExpr ast.Expr
	ColLoExpr, ColHiExpr ast.Expr
	Capped               bool
}

func (s *gridSummary) baseArgExpr(callArgs []ast.Expr) ast.Expr {
	if s.BaseParamIdx < len(callArgs) {
		return callArgs[s.BaseParamIdx]
	}
	return nil
}

// computeGridSummaries computes a gridSummary for every same-file function
// whose body matches the recognized shape, memoized once per file — the
// 2D counterpart of computeArraySummaries.
func computeGridSummaries(info *types.Info, file *ast.File) map[*types.Func]*gridSummary {
	out := map[*types.Func]*gridSummary{}
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Body == nil {
			continue
		}
		if s, ok := computeGridSummary(fd, info); ok {
			if obj, ok := info.Defs[fd.Name].(*types.Func); ok {
				out[obj] = s
			}
		}
	}
	return out
}

// computeGridSummary recognizes splitN2D's own shape in decl's body:
//
//	for I := range NR {
//	    ...
//	    for J := range NC {
//	        ...
//	        for R := R0; R < R1; R++ {
//	            ... = BASE[R][C0:C1] ...
//	        }
//	    }
//	}
//
// — an outer range loop (the row axis, I), an inner range loop nested in
// its body (the column axis, J), and a plain ForStmt nested in the inner
// loop's body whose own Init/Cond give the row axis's bounds (R0, R1) and
// whose body contains a SliceExpr of BASE[R] giving the column axis's
// bounds (C0, C1) — R itself, and the exact statements computing R0/R1/
// C0/C1, are never inspected structurally beyond this: they're captured
// as raw expressions and resolved later, the same deferred-resolution
// approach arraySummary's own LoExpr/HiExpr already uses (applyGridSummary
// linearizes them in a sub-context with RowLoopVar/ColLoopVar as the free
// index variables, letting the existing findSingleDef-based variable
// tracing follow through however many intermediate locals compute them).
func computeGridSummary(decl *ast.FuncDecl, info *types.Info) (*gridSummary, bool) {
	if decl.Body == nil {
		return nil, false
	}

	rangeVar := func(root ast.Node) (*ast.RangeStmt, *types.Var, bool) {
		var found *ast.RangeStmt
		var v *types.Var
		ast.Inspect(root, func(n ast.Node) bool {
			if found != nil {
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
			found, v = rs, lv
			return false
		})
		return found, v, found != nil
	}

	outerRS, rowVar, ok := rangeVar(decl.Body)
	if !ok {
		return nil, false
	}
	innerRS, colVar, ok := rangeVar(outerRS.Body)
	if !ok {
		return nil, false
	}

	// The innermost row-band ForStmt (`for r := r0; r < r1; r++`), nested
	// inside the column loop's body — its own Init/Cond give the row
	// axis's bounds.
	var rowFS *ast.ForStmt
	var rowIdxVar *types.Var
	var rowLoExpr, rowHiExpr ast.Expr
	ast.Inspect(innerRS.Body, func(n ast.Node) bool {
		if rowFS != nil {
			return false
		}
		fs, ok := n.(*ast.ForStmt)
		if !ok || fs.Init == nil || fs.Cond == nil || fs.Body == nil {
			return true
		}
		initAssign, ok := fs.Init.(*ast.AssignStmt)
		if !ok || initAssign.Tok != token.DEFINE || len(initAssign.Lhs) != 1 || len(initAssign.Rhs) != 1 {
			return true
		}
		rIdent, ok := initAssign.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		rv, ok := info.Defs[rIdent].(*types.Var)
		if !ok {
			return true
		}
		cond, ok := fs.Cond.(*ast.BinaryExpr)
		if !ok || cond.Op != token.LSS {
			return true
		}
		condIdent, ok := unparenExpr(cond.X).(*ast.Ident)
		if !ok || info.ObjectOf(condIdent) != rv {
			return true
		}
		rowFS, rowIdxVar = fs, rv
		rowLoExpr, rowHiExpr = initAssign.Rhs[0], cond.Y
		return false
	})
	if rowFS == nil {
		return nil, false
	}

	// The tile's own column slice, found anywhere inside the row ForStmt's
	// body: BASE[rowIdxVar][colLo:colHi].
	var baseExpr ast.Expr
	var colLoExpr, colHiExpr ast.Expr
	var capped, found bool
	ast.Inspect(rowFS.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		se, ok := n.(*ast.SliceExpr)
		if !ok || se.Low == nil || se.High == nil {
			return true
		}
		idx, ok := unparenExpr(se.X).(*ast.IndexExpr)
		if !ok {
			return true
		}
		rIdent, ok := unparenExpr(idx.Index).(*ast.Ident)
		if !ok || info.ObjectOf(rIdent) != rowIdxVar {
			return true
		}
		baseExpr = idx.X
		colLoExpr, colHiExpr, capped = se.Low, se.High, se.Slice3
		found = true
		return false
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
	pIdx := paramIndex(decl, baseObj, info)
	if pIdx < 0 {
		return nil, false
	}

	return &gridSummary{
		Decl: decl, RowLoopVar: rowVar, ColLoopVar: colVar, BaseParamIdx: pIdx,
		RowLoExpr: rowLoExpr, RowHiExpr: rowHiExpr,
		ColLoExpr: colLoExpr, ColHiExpr: colHiExpr,
		Capped: capped,
	}, true
}

// regionFromDoubleIndex resolves x (tiles[i][j]) into a 2-axis Region,
// when x.X is itself an *ast.IndexExpr (tiles[i]) whose base traces, via
// findCallSource, to a gridSummary'd call. Returns false for anything
// else (a single index, or a double index whose inner base isn't a grid
// summary), letting regionOf fall through to the ordinary single-index
// path unchanged.
func regionFromDoubleIndex(x *ast.IndexExpr, lc *linCtx, summaries summaryTables) (Region, bool) {
	inner, ok := x.X.(*ast.IndexExpr)
	if !ok {
		return Region{}, false
	}
	src, ok := findCallSource(inner.X, lc, summaries.grid)
	if !ok {
		return Region{}, false
	}
	return applyGridSummary(src, inner.Index, x.Index, lc, summaries)
}

// applyGridSummary resolves src[rowIdxExpr][colIdxExpr] into a concrete
// 2-axis Region — the 2D counterpart of applySummary. A fresh linCtx
// substitutes src.summary's own parameters with the actual arguments
// (resolved in src.callCtx) and both loop variables with rowIdxExpr/
// colIdxExpr (resolved in lc, the *using* context).
func applyGridSummary(src callSource[*gridSummary], rowIdxExpr, colIdxExpr ast.Expr, lc *linCtx, summaries summaryTables) (Region, bool) {
	s := src.summary

	baseArgRegion, ok := regionOf(s.baseArgExpr(src.callArgs), src.callCtx, summaries)
	if !ok || len(baseArgRegion.Axes) == 0 {
		return Region{}, false
	}
	baseAx0 := baseArgRegion.Axes[0]

	sub := newLinCtx(lc.info, lc.file, s.Decl.Body)
	sub.nonNeg = lc.nonNeg
	sub.allowLastIterGuard = true
	sub.indexVars[s.RowLoopVar] = true
	sub.indexVars[s.ColLoopVar] = true
	sub.env[s.RowLoopVar] = linearize(rowIdxExpr, lc)
	sub.env[s.ColLoopVar] = linearize(colIdxExpr, lc)

	rowLo := linearize(s.RowLoExpr, sub)
	rowHi := linearize(s.RowHiExpr, sub)
	colLo := linearize(s.ColLoExpr, sub)
	colHi := linearize(s.ColHiExpr, sub)

	repIndexFor := func(idxExpr ast.Expr) *types.Var {
		idxLin := linearize(idxExpr, lc)
		if t, ok := soleTerm(idxLin); ok {
			if v, ok := t.(*types.Var); ok && lc.isIndexVar(v) {
				return v
			}
		}
		return nil
	}

	return Region{
		Base: baseArgRegion.Base,
		Axes: []Interval{
			{Start: addLin(baseAx0.Start, rowLo), End: addLin(baseAx0.Start, rowHi), RepIndex: repIndexFor(rowIdxExpr)},
			{Start: colLo, End: colHi, RepIndex: repIndexFor(colIdxExpr)},
		},
		Capped: s.Capped,
	}, true
}
