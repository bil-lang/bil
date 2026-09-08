// Package main implements a general ban on sharing a pointer, map, or
// interface value across par branches — a plain, deliberately blunt rule
// rather than an attempt to verify safe sub-cases (see the plan this was
// built from for the full reasoning).
//
// Pointers and maps are the Go-side compromise this project exists to
// close (STRATEGY.md: "Go's channels/goroutines are compromised CSP").
// Unlike a channel (the intended way for processes to interact —
// channels.go / confinement.go's domain) or an array (where
// splitN/splitN2D give actually-proven-safe disjoint-sharing patterns to
// trust — regions.go / grid.go's domain), there's no established safe
// way to share a pointer or map across concurrent branches. Verifying one
// would need to correctly distinguish safe operations (reassigning a
// by-value pointer parameter) from unsafe ones (dereferencing it) via
// interprocedural substitution — real complexity with real room for a
// false negative, a race wrongly classified as safe. So instead: a
// pointer/map/interface value free to more than one branch is rejected
// outright, regardless of what either branch actually does with it.
// Confined to a single branch is untouched — no concurrent access is
// possible there, so nothing to check.
//
// variables.go's existing UnaryExpr/token.AND handling already covers a
// *plain* variable's address escaping (`&x` written in one branch, x used
// in another) — that stays as-is; this file's job is the case that leaves
// invisible: an *already-existing* pointer (or map) variable shared into
// two branches, with no `&x` moment for anything to catch. See the plan
// this was built from for the worked example.
package vet

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
)

// isRestrictedRefType reports whether t is a type this rule bans from
// being shared across par branches. Channels and arrays/slices are
// deliberately excluded — see the package doc comment.
func isRestrictedRefType(t types.Type) bool {
	switch t.Underlying().(type) {
	case *types.Pointer, *types.Map, *types.Interface:
		return true
	}
	return false
}

// refKindLabel names t's restricted kind for diagnostics.
func refKindLabel(t types.Type) string {
	switch t.Underlying().(type) {
	case *types.Pointer:
		return "pointer"
	case *types.Map:
		return "map"
	case *types.Interface:
		return "interface"
	}
	return "value"
}

// RefShareConflict is a pointer/map/interface identity referenced by more
// than one branch of the same par(...) call.
type RefShareConflict struct {
	Obj        *types.Var
	FirstFrom  int
	FirstPos   token.Pos
	SecondFrom int
	SecondPos  token.Pos
}

func (c RefShareConflict) Message(fset *token.FileSet) string {
	return fmt.Sprintf(
		"%s %s is shared between par branches (branch %d, also branch %d at %s) — a pointer, map, or interface value may not be shared across components of a parallel; confine it to one branch, or communicate over a channel instead",
		refKindLabel(c.Obj.Type()), c.Obj.Name(), c.FirstFrom+1, c.SecondFrom+1, fset.Position(c.SecondPos),
	)
}

// CheckRefSharing walks file for par(...) calls and reports
// RefShareConflicts among their branches.
func CheckRefSharing(info *types.Info, file *ast.File) []RefShareConflict {
	declByFunc := funcDecls(info, file)

	var conflicts []RefShareConflict

	for _, branches := range parBranches(file) {
		touched := make([]map[*types.Var]token.Pos, len(branches))
		for i, b := range branches {
			touched[i] = collectRefTouches(info, b.Body, b, declByFunc, map[*types.Func]bool{})
		}

		byIdentity := map[*types.Var]map[int]token.Pos{}
		for i, m := range touched {
			for obj, pos := range m {
				if byIdentity[obj] == nil {
					byIdentity[obj] = map[int]token.Pos{}
				}
				if _, exists := byIdentity[obj][i]; !exists {
					byIdentity[obj][i] = pos
				}
			}
		}

		conflicts = append(conflicts, refConflictsFrom(byIdentity)...)
	}

	return conflicts
}

// refConflictsFrom turns a per-identity branch-index->pos map into
// RefShareConflicts whenever an identity has 2+ distinct branches recorded
// (two smallest branch indices — enough to point at the problem without
// flooding on an identity touched from many branches).
func refConflictsFrom(byIdentity map[*types.Var]map[int]token.Pos) []RefShareConflict {
	var out []RefShareConflict
	for identity, byBranch := range byIdentity {
		if len(byBranch) < 2 {
			continue
		}
		first, second := -1, -1
		var firstPos, secondPos token.Pos
		for branch, pos := range byBranch {
			if first == -1 || branch < first {
				second, secondPos = first, firstPos
				first, firstPos = branch, pos
			} else if second == -1 || branch < second {
				second, secondPos = branch, pos
			}
		}
		out = append(out, RefShareConflict{
			Obj:       identity,
			FirstFrom: first, FirstPos: firstPos,
			SecondFrom: second, SecondPos: secondPos,
		})
	}
	return out
}

// collectRefTouches walks body — a par branch's own FuncLit body, or a
// same-file callee's body reached by recursing into it — recording every
// free, restricted-type variable referenced anywhere (any way at all: read,
// write, passed on, whatever — mere reference is the thing this rule cares
// about, see the package doc comment for why). scope is whichever function
// body is currently being walked (see isFree, channels.go). visiting
// guards a call cycle the same way channels.go's does.
func collectRefTouches(
	info *types.Info,
	body ast.Node,
	scope ast.Node,
	declByFunc map[*types.Func]*ast.FuncDecl,
	visiting map[*types.Func]bool,
) map[*types.Var]token.Pos {
	touched := map[*types.Var]token.Pos{}

	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			obj, ok := info.ObjectOf(x).(*types.Var)
			if ok && isRestrictedRefType(obj.Type()) && isFree(obj, scope) {
				if _, exists := touched[obj]; !exists {
					touched[obj] = x.Pos()
				}
			}
		case *ast.CallExpr:
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

			visiting[calleeObj] = true
			mergeInto(touched, collectRefTouches(info, decl.Body, decl, declByFunc, visiting))
			delete(visiting, calleeObj)
		}
		return true
	})

	return touched
}
