package main

import "go/ast"

// parBranches returns each par(...) call's FuncLit branch arguments found
// in file (a branch that isn't a literal, a bare func value passed by
// name, can't be inspected here — bilc's own par-transform always emits
// literals today, so this doesn't lose coverage in practice).
//
// Includes single-branch calls — "a single branch is legal, it's just a
// no-op" (LANG-DESIGN.md) — even though cross-branch checks
// (channels.go/variables.go/refshare.go) naturally find nothing to
// compare there (their own conflict-grouping already requires 2+
// branches touching the same identity). regions.go's self-consistency
// check is the one rule that needs a single-branch call too: a
// replicated construct's own overlapping-stride hazard exists whether or
// not it has sibling branches — see regions.go's package doc comment.
func parBranches(file *ast.File) [][]*ast.FuncLit {
	var calls [][]*ast.FuncLit

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "par" {
			return true
		}

		var branches []*ast.FuncLit
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.FuncLit); ok {
				branches = append(branches, lit)
			}
		}
		if len(branches) >= 1 {
			calls = append(calls, branches)
		}
		return true // keep descending: nested par(...) calls get their own entry
	})

	return calls
}
