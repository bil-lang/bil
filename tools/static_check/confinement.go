// Channel confinement: Bil channels are meant to be declared once and
// threaded through by parameter-passing, never copied, stored, or handed
// back out as a value — Go channels are ordinary first-class values, so
// every way Go lets a value escape its original binding is a way a
// channel can end up somewhere channels.go's par-branch analysis never looks, quietly
// invalidating its reasoning (which assumes a channel only ever moves
// through direct parameter-passing).
//
// bilc's own checkNoChannelAliasing (tools/bilc/bilc.go) already rejects a
// bare `x := c` / `x = c` copy via a token-adjacency scan, but that leaves
// real gaps: a channel disappearing into a struct-literal field
// (`Box{ch: c}`), returned from a function (`return c`), or sent as the
// payload on another channel (channel-of-channel, which nothing rejects
// today the way `chan *T` already is).
//
// A channel-array element (`chans[i]`) gets the same treatment as a
// scalar channel, not a blanket exemption: `makeChans` and the
// worker-farm/scatter-gather/replicated-alt/barrier/token-ring/mandelbrot
// examples legitimately build and index `[]chan T` arrays, handing one
// element per replicated branch, but that pattern only stays visible to
// channels.go's own direction-conflict reasoning (which now understands
// `chans[i]` directly, per its own doc comment) while the element is used
// *directly* — a call argument, or the direct operand of a send/receive.
// The moment it's aliased into another variable, returned, or stored into
// a container literal built from pre-existing channels (rather than
// makeChans' own always-fresh `make(chan T)` per slot), channels.go loses
// all visibility into what that variable represents — exactly the same
// hazard aliasing a scalar channel already is. So this file no longer
// carves out an exemption for a container-typed value at all; every rule
// below applies uniformly to any expression that resolves to a scalar
// `chan T`, wherever it came from.
package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
)

// LeakKind identifies which escape mechanism a Leak was found through.
type LeakKind int

const (
	LeakAlias      LeakKind = iota // x := c / x = c
	LeakComposite                  // Box{ch: c} — struct literal only
	LeakReturn                     // return c
	LeakPayload                    // otherChan <- c
	LeakChanOfChan                 // chan (chan T) declared at all
)

// Leak is one channel value found outside the confined shape this checker
// trusts: created via make (or received as a parameter), used directly in
// a send/receive, or passed as a direct argument to another call.
type Leak struct {
	Kind   LeakKind
	Pos    token.Pos
	Detail string // source text of the offending expression/type
}

func (l Leak) Message() string {
	const rule = "channels may only be sent to, received from, or passed as an argument"
	switch l.Kind {
	case LeakAlias:
		return fmt.Sprintf("channel leak: %s is assigned to another variable — %s", l.Detail, rule)
	case LeakComposite:
		return fmt.Sprintf("channel leak: %s is stored in a struct literal — %s", l.Detail, rule)
	case LeakReturn:
		return fmt.Sprintf("channel leak: %s is returned from a function — %s", l.Detail, rule)
	case LeakPayload:
		return fmt.Sprintf("channel leak: %s is sent as a message payload on another channel — a channel value may not itself be communicated", l.Detail)
	case LeakChanOfChan:
		return fmt.Sprintf("channel leak: %s declares a channel of channels — a channel's element type may not itself be a channel", l.Detail)
	default:
		return fmt.Sprintf("channel leak: %s", l.Detail)
	}
}

// CheckNoChannelOfChannel rejects `chan (chan T)` (any direction) wherever
// the type is spelled, regardless of whether it's ever used — same
// "reject at the declaration, don't wait for a use" stance bilc's own
// checkNoPointerChannels takes for `chan *T`. Pure syntax, no type-checking
// needed.
func CheckNoChannelOfChannel(file *ast.File) []Leak {
	var out []Leak
	ast.Inspect(file, func(n ast.Node) bool {
		ct, ok := n.(*ast.ChanType)
		if !ok {
			return true
		}
		if _, ok := ct.Value.(*ast.ChanType); ok {
			out = append(out, Leak{Kind: LeakChanOfChan, Pos: ct.Pos(), Detail: types.ExprString(ct)})
		}
		return true
	})
	return out
}

// CheckChannelConfinement walks the whole file — not just par(...)
// branches, since a leak can happen in any function, matching
// checkNoChannelAliasing's own unscoped reach today — for a scalar chan T
// value appearing anywhere other than its confined shape.
func CheckChannelConfinement(info *types.Info, file *ast.File) []Leak {
	var out []Leak
	guarded := nilGuardedAliases(info, file)

	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range x.Rhs {
				if !isScalarChan(info.TypeOf(rhs)) || exemptFromAlias(rhs, info) {
					continue
				}
				if i < len(x.Lhs) && guarded[lhsObject(x.Lhs[i], info)] {
					continue // select-guard idiom (see nilGuardedAliases) — not a leak
				}
				out = append(out, Leak{Kind: LeakAlias, Pos: rhs.Pos(), Detail: types.ExprString(rhs)})
			}

		case *ast.CompositeLit:
			// No exemption for a container-typed literal (a channel array
			// built via []chan T{c1, c2} rather than makeChans' own
			// always-fresh make(chan T) pattern) — each element is checked
			// on its own merit below, same as a struct-literal field.
			for _, elt := range x.Elts {
				val := elt
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					val = kv.Value
				}
				if isScalarChan(info.TypeOf(val)) && !exemptFromAlias(val, info) {
					out = append(out, Leak{Kind: LeakComposite, Pos: val.Pos(), Detail: types.ExprString(val)})
				}
			}

		case *ast.ReturnStmt:
			for _, res := range x.Results {
				if isScalarChan(info.TypeOf(res)) && !exemptFromAlias(res, info) {
					out = append(out, Leak{Kind: LeakReturn, Pos: res.Pos(), Detail: types.ExprString(res)})
				}
			}

		case *ast.SendStmt:
			if isScalarChan(info.TypeOf(x.Value)) {
				out = append(out, Leak{Kind: LeakPayload, Pos: x.Value.Pos(), Detail: types.ExprString(x.Value)})
			}
		}
		return true
	})

	return out
}

// nilGuardedAliases finds every variable bound by the "guarded select"
// idiom `bilc`'s own alt-guard lowering emits for a conditional alt clause
// (`(cond) && chan -> var`) — a nil channel makes a select case permanently
// unready, so a guard becomes:
//
//	bilGuard0 := in
//	if !(cond) {
//	    bilGuard0 = nil
//	}
//	select { case v = <-bilGuard0: ... }
//
// That first line is a bare channel-to-variable copy — exactly the LeakAlias
// shape — but it's bilc's own generated code, not a user-introduced alias,
// and bilGuard0 only ever appears immediately re-nilled and then as a
// select operand. Recognized structurally (`x := c` immediately followed
// by `if ... { x = nil }`, same statement list) rather than by variable
// name, so it isn't tied to bilc's current `bilGuard` naming.
func nilGuardedAliases(info *types.Info, file *ast.File) map[types.Object]bool {
	exempt := map[types.Object]bool{}

	scan := func(list []ast.Stmt) {
		for i, stmt := range list {
			assign, ok := stmt.(*ast.AssignStmt)
			if !ok || i+1 >= len(list) {
				continue
			}
			if !isNilGuardFollowUp(assign, list[i+1]) {
				continue
			}
			if obj := lhsObject(assign.Lhs[0], info); obj != nil {
				exempt[obj] = true
			}
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BlockStmt:
			scan(x.List)
		case *ast.CommClause:
			scan(x.Body)
		case *ast.CaseClause:
			scan(x.Body)
		}
		return true
	})

	return exempt
}

// isNilGuardFollowUp reports whether next is exactly `if <cond> { x = nil }`
// for assign's single LHS identifier x, with no else and nothing else in
// the if's body.
func isNilGuardFollowUp(assign *ast.AssignStmt, next ast.Stmt) bool {
	if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false
	}
	lhs, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		return false
	}

	ifStmt, ok := next.(*ast.IfStmt)
	if !ok || ifStmt.Init != nil || ifStmt.Else != nil || len(ifStmt.Body.List) != 1 {
		return false
	}
	nilAssign, ok := ifStmt.Body.List[0].(*ast.AssignStmt)
	if !ok || nilAssign.Tok != token.ASSIGN || len(nilAssign.Lhs) != 1 || len(nilAssign.Rhs) != 1 {
		return false
	}
	target, ok := nilAssign.Lhs[0].(*ast.Ident)
	if !ok || target.Name != lhs.Name {
		return false
	}
	rhsIdent, ok := nilAssign.Rhs[0].(*ast.Ident)
	return ok && rhsIdent.Name == "nil"
}

// lhsObject resolves a simple assignment target to its types.Object, or
// nil if expr isn't a plain identifier.
func lhsObject(expr ast.Expr, info *types.Info) types.Object {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return nil
	}
	return info.ObjectOf(id)
}

// exemptFromAlias reports whether expr's value isn't an alias of something
// already bound — a fresh call result (make(chan T), or any other call —
// a legitimate creation/factory pattern). Anything else that resolves to
// a scalar channel — a bare identifier, a selector (box.ch), an indexed
// channel-array element (chans[i]), a paren-wrapped one of those — is the
// alias this check exists for: channels.go's own direction-conflict
// reasoning has no way to trace a variable back to its defining
// expression (unlike regions.go's findSingleDef for arrays), so once a
// channel value — scalar or array element alike — is aliased into another
// variable or returned, channels.go loses all visibility into what that
// variable actually represents. A channel-array element used *directly*
// (a call argument, or the direct operand of a send/receive) stays fully
// visible to channels.go, per its own doc comment; only escaping through
// an intermediate binding is the hazard this rejects.
func exemptFromAlias(expr ast.Expr, info *types.Info) bool {
	switch x := expr.(type) {
	case *ast.CallExpr:
		return true
	case *ast.ParenExpr:
		return exemptFromAlias(x.X, info)
	}
	return false
}

// isScalarChan is isChan (channels.go) with a nil guard — info.TypeOf can
// return nil for an expression go/types didn't record a type for.
func isScalarChan(t types.Type) bool {
	return t != nil && isChan(t)
}
