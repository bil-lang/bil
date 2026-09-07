// Package main implements Bil's general variable-usage rule:
//
//	Variables which are changed by input or assignment in one of the
//	processes of a parallel may not be used in expressions or for
//	assignment in any other process in the parallel. A variable may appear
//	in expressions in any number of components of a parallel so long as it
//	is not assigned in any parallel component.
//
// This is the least-covered rule in the project:
// checkNoPointerParams (tools/bilc/bilc.go) covers only a variable's
// address escaping via &x, call-shaped branches only — "the general case,
// a bare variable not behind a pointer or array, is not covered at all."
//
// Unlike channels.go's rule (which needs interprocedural parameter
// substitution, because a channel is a reference to shared runtime state),
// resolving a call-site *parameter* here needs none: Go copies a scalar or
// plain (reference-field-free) struct at every function call boundary
// (confirmed against LANG-DESIGN.md's own design note on proc params —
// "genuinely copied by Go's own call semantics, so always safe regardless
// of how many branches receive one"), so a proc mutating its own by-value
// parameter cannot affect the caller's original variable.
//
// This check does still recurse into same-file called functions, the same
// bounded way channels.go does — not to substitute parameters (unnecessary,
// per the above), but because a callee can reference a *package-level*
// variable directly, with no parameter involved at all, and that's exactly
// as shared/hazardous as a branch touching it directly would be. isFree
// (channels.go) already reduces to "declared at package level" once we're
// inside any function beyond the original branch (Go's own lexical scoping
// means a callee can't see anything else from its caller), so no
// additional logic is needed beyond recursing with the callee's own
// *ast.FuncDecl as the new scope.
//
// Deliberately out of scope (see the plan this was built from for the full
// reasoning): arrays/slices (regions.go / grid.go's domain),
// pointers (checkNoPointerParams's domain), channels (channels.go /
// confinement.go's domain), maps (a genuine gap — Go maps are reference
// types, so the "no substitution needed for parameters" argument doesn't
// hold for them — closing it would need channels.go-style parameter
// substitution too, separable follow-up work), interfaces (can hold any
// underlying type, excluded conservatively), and channel arrays
// (replicated par/parFor).
package main

import (
	"go/ast"
	"go/token"
	"go/types"
)

// VarConflict is a variable written in one par branch that is also used —
// read or written — in a different sibling branch of the same par(...)
// call.
type VarConflict struct {
	Obj         *types.Var
	WriteBranch int
	WritePos    token.Pos
	UseBranch   int
	UsePos      token.Pos
}

// CheckSharedVariables walks file for par(...) calls and reports
// VarConflicts among their branches.
func CheckSharedVariables(info *types.Info, file *ast.File) []VarConflict {
	declByFunc := funcDecls(info, file)

	var conflicts []VarConflict

	for _, branches := range parBranches(file) {
		written := make([]map[*types.Var]token.Pos, len(branches))
		used := make([]map[*types.Var]token.Pos, len(branches))
		for i, b := range branches {
			written[i], used[i] = collectVarUsage(info, b.Body, b, declByFunc, map[*types.Func]bool{})
		}

		for i := range branches {
		byWrittenVar:
			for obj, wpos := range written[i] {
				for j := range branches {
					if i == j {
						continue
					}
					if upos, ok := used[j][obj]; ok {
						conflicts = append(conflicts, VarConflict{
							Obj: obj, WriteBranch: i, WritePos: wpos,
							UseBranch: j, UsePos: upos,
						})
						continue byWrittenVar // one conflict is enough evidence for this (branch, var) pair
					}
				}
			}
		}
	}

	return conflicts
}

// collectVarUsage walks body — a par branch's own FuncLit body, or a
// same-file callee's body reached by recursing into it — recording:
//   - written: a free variable assigned to, incremented/decremented, or
//     address-taken, mapped to the position of that mutation.
//   - used: every free variable referenced at all (read or write), mapped
//     to the position of its first occurrence.
//
// scope is whichever function body is currently being walked (see isFree,
// channels.go, for what "free relative to scope" means at each recursion
// depth). A variable is only tracked at all if its type is in scope — see
// inScopeVarType.
//
// visiting guards a call cycle the same way channels.go's does — see its
// doc comment.
func collectVarUsage(
	info *types.Info,
	body ast.Node,
	scope ast.Node,
	declByFunc map[*types.Func]*ast.FuncDecl,
	visiting map[*types.Func]bool,
) (written, used map[*types.Var]token.Pos) {
	written = map[*types.Var]token.Pos{}
	used = map[*types.Var]token.Pos{}

	record := func(m map[*types.Var]token.Pos, ident *ast.Ident) {
		obj, ok := info.ObjectOf(ident).(*types.Var)
		if !ok || !isFree(obj, scope) || !inScopeVarType(obj.Type()) {
			return
		}
		if _, exists := m[obj]; !exists {
			m[obj] = ident.Pos()
		}
	}

	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if x.Tok != token.DEFINE {
				for _, lhs := range x.Lhs {
					if id := baseIdent(lhs); id != nil {
						record(written, id)
					}
				}
			}
		case *ast.IncDecStmt:
			if id := baseIdent(x.X); id != nil {
				record(written, id)
			}
		case *ast.UnaryExpr:
			// Taking a free variable's address lets a mutation reach it
			// through a pointer later, outside this walk's view — same
			// conservative call checkNoPointerParams already makes for &x
			// reaching a call-shaped branch, generalized here to any
			// branch shape.
			if x.Op == token.AND {
				if id := baseIdent(x.X); id != nil {
					record(written, id)
				}
			}
		case *ast.Ident:
			record(used, x)
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				// A method call on a free variable's receiver — whether it
				// actually mutates isn't analyzable here without inspecting
				// the method itself, so treat it conservatively as a
				// potential write (same "can't verify, so restrict" stance
				// as the rest of this project) rather than the plain read
				// the generic *ast.Ident case below would otherwise record
				// it as (Inspect still visits sel.X as an *ast.Ident too,
				// which is harmless — record's first-occurrence-wins dedup
				// makes the redundant "used" entry a no-op).
				if id := baseIdent(sel.X); id != nil {
					record(written, id)
				}
				break
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

			visiting[calleeObj] = true
			w, u := collectVarUsage(info, decl.Body, decl, declByFunc, visiting)
			mergeInto(written, w)
			mergeInto(used, u)
			delete(visiting, calleeObj)
		}
		return true
	})

	return written, used
}

// mergeInto adds src's entries into dst, keeping dst's existing position
// on a collision (first-seen wins, same rule record uses within one frame).
func mergeInto(dst, src map[*types.Var]token.Pos) {
	for obj, pos := range src {
		if _, exists := dst[obj]; !exists {
			dst[obj] = pos
		}
	}
}

// inScopeVarType reports whether t is a plain scalar/struct type this
// check tracks — false for anything array/slice/map/chan/pointer/
// interface-shaped, each deferred elsewhere (see the package doc comment).
// For a struct, every field is checked the same way, recursively (handling
// embedded and nested structs for free): a struct is only in scope if
// every field, transitively, is too — copying a struct by value only
// copies its header, so a slice/map/chan/pointer field is exactly as
// shared after the copy as a bare one would be.
func inScopeVarType(t types.Type) bool {
	switch u := t.Underlying().(type) {
	case *types.Array, *types.Slice, *types.Map, *types.Chan, *types.Pointer, *types.Interface:
		return false
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			if !inScopeVarType(u.Field(i).Type()) {
				return false
			}
		}
	}
	return true
}
