// bilc is a source-to-source preprocessor: it reads a .bil file (Go plus the
// `par`, `seq`, and `proc` constructs) and emits legal, compilable Go.
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"go/scanner"
	"go/token"
	"os"
)

type tok struct {
	pos token.Pos
	tok token.Token
	lit string
}

const importSync = "\nimport \"sync\"\n"

// parHelper is appended at the end of the file rather than injected after
// the package clause: declaration order doesn't matter for top-level Go
// funcs, and appending avoids landing a func before the source's own
// import block (which Go rejects — imports must precede all other decls).
const parHelper = `
func par(branches ...func()) {
	var wg sync.WaitGroup
	wg.Add(len(branches))
	for _, b := range branches {
		go func(b func()) {
			defer wg.Done()
			b()
		}(b)
	}
	wg.Wait()
}
`

// parForHelper backs replicated par (`par VAR := range EXPR { BODY }`): unlike par's
// branches, the branch count here is a runtime value, so it can't be
// unrolled into N literal func() branches at preprocess time the way par's
// own transform does — the fan-out has to happen in the runtime helper
// instead.
const parForHelper = `
func parFor(n int, body func(int)) {
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			body(i)
		}(i)
	}
	wg.Wait()
}
`

// makeChansHelper backs the common "array of N channels, one per replica"
// setup a replicated `par` (Example 4) needs — `make([]chan T, N)` alone
// only allocates N nil slots, each element is still its own separate
// channel and needs its own `make(chan T)` (see Example 4's design notes
// on this exact confusion) — this collapses that allocate-then-fill-loop
// pattern into one call, still plain, unbuffered `make(chan T)` underneath.
// Ordinary Go (a generic helper), not new `bilc` syntax: nothing about
// filling a channel slice needs special parsing the way `par`/`alt`/`->` do.
const makeChansHelper = `
func makeChans[T any](n int) []chan T {
	cs := make([]chan T, n)
	for i := range cs {
		cs[i] = make(chan T)
	}
	return cs
}
`

// splitNHelper backs safe N-way disjoint splitting of a slice across
// concurrent par branches (Example 10) — the answer settled on after
// trying, and reverting, a static checker that tried to *verify* hand-
// written slice arithmetic was disjoint (only ever provable for one narrow
// canonical shape, and still lets two independent expressions drift out of
// sync the way `mid`/`mip` did). This sidesteps that: disjointness becomes
// a property of one trusted implementation, verified once, the same way
// `makeChans` means nobody re-verifies "is this channel genuinely
// unbuffered" at every call site. Handles uneven division (remainder
// folded into the last chunk) for free — something no single fixed-stride
// arithmetic expression can express at all.
//
// The three-index slice (`s[lo:hi:hi]`, not `s[lo:hi]`) matters: a plain
// two-index slice's capacity reaches to the end of `s`'s backing array,
// not just to `hi`, so `append` on any chunk but the last could otherwise
// silently overwrite the next chunk's elements without ever reallocating
// — logically-disjoint index ranges, but not actually disjoint memory.
// Capping capacity at `hi` forces `append` on any chunk to reallocate
// instead, closing that off structurally rather than relying on nobody
// ever appending to a chunk.
const splitNHelper = `
func splitN[T any](s []T, n int) [][]T {
	chunk := len(s) / n
	out := make([][]T, n)
	for i := range n {
		lo := i * chunk
		hi := lo + chunk
		if i == n-1 {
			hi = len(s)
		}
		out[i] = s[lo:hi:hi]
	}
	return out
}
`

// splitN2DHelper backs safe two-way disjoint tiling of a matrix (`[][]T`)
// into an nr*nc grid of rectangular blocks, for a nested replicated par
// (`par i := range nr { par j := range nc { ... } }`) — the 2D
// counterpart of splitN. It's a separate function, not a variadic-
// dimension generalization of splitN: Go generics can't unify a 1D
// splitter and a 2D one behind one signature, because the *input* type
// itself changes shape with dimensionality (`[]T` vs `[][]T`, not just a
// type parameter). `splitN2D` is this session's two-dimensional member of
// what could become a small family (a 3D case, if one is ever needed,
// would be its own sibling, e.g. `splitN3D` — not a variadic extension of
// this one).
//
// out[i][j] is the (i,j)'th tile, itself a [][]T. Each axis independently
// folds its own remainder into its last row/column band, exactly like
// splitN's own last-chunk handling. Both the outer per-row-band
// allocation (out[i] := make([][][]T, nc), each entry itself a
// freshly-made [][]T) and the inner row slice (m[r][c0:c1:c1], three-
// index) are capacity-capped for the same reason splitN's own three-index
// slice is — see splitN's own doc comment for the capacity-gap reasoning,
// unchanged here, just applied per axis.
const splitN2DHelper = `
func splitN2D[T any](m [][]T, nr, nc int) [][][][]T {
	rchunk := len(m) / nr
	cchunk := len(m[0]) / nc
	out := make([][][][]T, nr)
	for i := range nr {
		r0 := i * rchunk
		r1 := r0 + rchunk
		if i == nr-1 {
			r1 = len(m)
		}
		out[i] = make([][][]T, nc)
		for j := range nc {
			c0 := j * cchunk
			c1 := c0 + cchunk
			if j == nc-1 {
				c1 = len(m[0])
			}
			tile := make([][]T, r1-r0)
			for r := r0; r < r1; r++ {
				tile[r-r0] = m[r][c0:c1:c1]
			}
			out[i][j] = tile
		}
	}
	return out
}
`

// altNHelper backs replicated alt (Example 13) — one
// process listening across a runtime-determined number of channels at
// once. Go's own `select` can't do this at all: its case list is fixed at
// compile time, so there's no idiom to fall back on the way `pri alt`
// could reuse nested `select`s. `reflect.Select` is the standard library's
// own answer to exactly this ("dynamic number of channels" is precisely
// what it exists for), so this wraps that rather than inventing anything —
// same spirit as `pri alt` using the standard nested-select idiom. Unlike
// `par`/`parFor`/`makeChans`/`splitN`, this helper (and its `reflect`
// import) is only injected when the source actually calls `altN` — those
// four are cheap enough to always include, but `reflect`-based dispatch
// carries real overhead relative to a native `select`, worth not forcing
// on every generated program given Bil's actual target (STRATEGY.md:
// "massively parallel processor arrays," a tight-memory environment) —
// see Example 13's design notes.
//
// guards is variadic and optional so every existing unguarded call site
// (`altN(chans)`) keeps working unchanged: `len(guards) == 0` means "no
// replica is gated," matching a replicated alt with no guard at all.
// When guards are supplied, a false guard nils out that replica's channel
// before the reflect.SelectCase is built — the same trick plain `alt`'s
// `(cond) && chan -> target` already uses for a single conditional guard
// (see emitAltClauses), generalized across a runtime-determined number of
// replicas: a nil channel is never ready, so reflect.Select (like Go's own
// `select`) just never picks it. This is a faithful per-replica boolean
// guard, not an approximation — the standard "disable a select case at
// runtime" idiom is exactly guard-per-replica.
const altNHelper = `
func altN[T any](chans []chan T, guards ...bool) (int, T) {
	cases := make([]reflect.SelectCase, len(chans))
	for i, ch := range chans {
		if len(guards) > 0 && !guards[i] {
			ch = nil
		}
		cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)}
	}
	chosen, recv, _ := reflect.Select(cases)
	return chosen, recv.Interface().(T)
}
`

type transformer struct {
	file         *token.File
	src          []byte
	toks         []tok
	usedStop     bool // set when a `stop` statement is rewritten
	usedAltN     bool // set when the source calls `altN`
	usedSplitN2D bool // set when the source calls `splitN2D`
	usedLink     bool // set when a `link[...]` send or receive is rewritten
}

func tokenize(filename string, src []byte) *transformer {
	fset := token.NewFileSet()
	file := fset.AddFile(filename, fset.Base(), len(src))
	var s scanner.Scanner
	// Comments deliberately not requested (no scanner.ScanComments): every
	// structural parser below (isReplicatedAlt, isReplicatedPar,
	// parseAltGuard, the arrow matchers, ...) expects a rigid token sequence
	// right after an anchor like `{`, with no allowance for a COMMENT token
	// landing in between — a comment placed inside one of bilc's own special
	// constructs would otherwise break the match (confirmed: `alt j := range
	// n { // comment` panics with "unsupported alt shape"). None of that
	// logic needs comment text anyway — output is reassembled by copying
	// byte ranges straight from src (see flushTo in transform), so any
	// comment's bytes still ride along untouched between whichever real
	// tokens bracket it; only the token *stream* loses the COMMENT entries
	// that were tripping up the parsers.
	s.Init(file, src, nil, 0)
	var toks []tok
	for {
		pos, t, lit := s.Scan()
		toks = append(toks, tok{pos, t, lit})
		if t == token.EOF {
			break
		}
	}
	return &transformer{file: file, src: src, toks: toks}
}

func (t *transformer) off(p token.Pos) int { return t.file.Offset(p) }

func matchBrace(toks []tok, open int) int {
	depth := 0
	for i := open; i < len(toks); i++ {
		switch toks[i].tok {
		case token.LBRACE:
			depth++
		case token.RBRACE:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	panic("unmatched {")
}

func matchBracket(toks []tok, open int) int {
	depth := 0
	for i := open; i < len(toks); i++ {
		switch toks[i].tok {
		case token.LBRACK:
			depth++
		case token.RBRACK:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	panic("unmatched [")
}

func matchParen(toks []tok, open int) int {
	depth := 0
	for i := open; i < len(toks); i++ {
		switch toks[i].tok {
		case token.LPAREN:
			depth++
		case token.RPAREN:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	panic("unmatched (")
}

// splitCallArgs splits the token range [lo,hi) (the inside of a call's
// parens) into comma-separated argument token ranges, respecting nested
// parens/brackets/braces so a comma inside a nested call or slice doesn't
// split an argument in two.
func splitCallArgs(toks []tok, lo, hi int) [][2]int {
	var args [][2]int
	if lo >= hi {
		return args
	}
	start := lo
	depth := 0
	for i := lo; i < hi; i++ {
		switch toks[i].tok {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth--
		case token.COMMA:
			if depth == 0 {
				args = append(args, [2]int{start, i})
				start = i + 1
			}
		}
	}
	if start < hi {
		args = append(args, [2]int{start, hi})
	}
	return args
}

// chanDir is a channel-typed proc parameter's declared direction, read off
// its Go type (`<-chan T` in, `chan<- T` out, bare `chan T` both/unknown).
// dirNone means the parameter isn't a channel at all.
type chanDir int

const (
	dirNone chanDir = iota
	dirIn
	dirOut
	dirBoth
)

// classifyChanType inspects the token range [lo,hi) — a single parameter's
// type, the tokens right after its name — and reports which direction it
// declares. `<-chan T` tokenizes as ARROW then CHAN (the arrow precedes the
// keyword); `chan<- T` tokenizes as CHAN then ARROW (the arrow follows it);
// a bare `chan T` is CHAN with no adjacent ARROW either side. Anything else
// (int, float64, []chan T, ...) isn't a channel parameter at all.
func classifyChanType(toks []tok, lo, hi int) chanDir {
	if lo >= hi {
		return dirNone
	}
	if toks[lo].tok == token.ARROW && lo+1 < hi && toks[lo+1].tok == token.CHAN {
		return dirIn
	}
	if toks[lo].tok == token.CHAN {
		if lo+1 < hi && toks[lo+1].tok == token.ARROW {
			return dirOut
		}
		return dirBoth
	}
	return dirNone
}

// paramNamesAndDirections parses a `proc`'s parameter list, [lo,hi) being
// the token range between its parens, into each parameter's own name and
// declared chanDir, in declaration order — dirNone for every non-channel
// parameter. Needed by checkProcChanBothDirections to know which identifier
// in the proc's body to watch for. Handles Go's short form for parameters
// that share a type (`xmin, xmax, ymin, ymax float64`): only the last name
// in such a run carries the type textually, so names are held back in
// `pending` until a typed segment is reached, then all of them (plus that
// segment's own name) get its direction — the same "which names does this
// type actually cover" problem `splitCallArgs`'s top-level-comma split
// doesn't resolve by itself.
func (t *transformer) paramNamesAndDirections(lo, hi int) (names []string, dirs []chanDir) {
	var pending []string
	for _, seg := range splitCallArgs(t.toks, lo, hi) {
		segLo, segHi := seg[0], seg[1]
		if segLo >= segHi {
			continue
		}
		if segHi-segLo == 1 && t.toks[segLo].tok == token.IDENT {
			pending = append(pending, t.toks[segLo].lit)
			continue
		}
		if t.toks[segLo].tok != token.IDENT {
			continue // shouldn't happen for a named parameter list
		}
		d := classifyChanType(t.toks, segLo+1, segHi)
		for _, n := range pending {
			names = append(names, n)
			dirs = append(dirs, d)
		}
		names = append(names, t.toks[segLo].lit)
		dirs = append(dirs, d)
		pending = nil
	}
	return names, dirs
}

// checkProcChanBothDirections enforces Bil's other channel usage rule: a
// channel parameter or free channel may not be used for both input and
// output within the same procedure — distinct from, and not covered by,
// the point-to-point rule (enforced downstream by
// tools/static_check/channels.go), which only ever compares usage *across*
// `par` branches. This one is purely local: for each `proc`, for each of
// its own *undirected* `chan T` parameters (a directional `<-chan
// T`/`chan<- T` already can't be misused this way — Go's own compiler
// rejects a send on a receive-only parameter and vice versa, so there's
// nothing for this check to add there), scan the proc's body for both a
// send (`name` immediately followed by ARROW — `name <- expr`) and a
// receive (ARROW immediately followed by `name` — bare `<-name`, `x :=
// <-name`, ...; or `name` immediately followed by the unspaced `->` sugar
// — `name -> x`, the same receive; this project's own arrow sugar reduces
// to `<-name` before the sugar itself is rewritten, so both forms need
// watching for). Finding both in the same proc is exactly the violation:
// refused outright, same "can't verify, so reject" stance as every other
// check in this file, not an attempt to prove the two uses never actually
// race.
func (t *transformer) checkProcChanBothDirections() error {
	toks := t.toks
	for i := 0; i+2 < len(toks); i++ {
		if !(toks[i].tok == token.IDENT && toks[i].lit == "proc" &&
			toks[i+1].tok == token.IDENT && toks[i+2].tok == token.LPAREN) {
			continue
		}
		procName := toks[i+1].lit
		parenClose := matchParen(toks, i+2)
		if parenClose+1 >= len(toks) || toks[parenClose+1].tok != token.LBRACE {
			continue // no body immediately following — not a shape we recognize
		}
		bodyOpen := parenClose + 1
		bodyClose := matchBrace(toks, bodyOpen)

		names, dirs := t.paramNamesAndDirections(i+3, parenClose)
		for idx, name := range names {
			if dirs[idx] != dirBoth {
				continue
			}
			var sawSend, sawRecv bool
			var sendPos token.Pos
			for k := bodyOpen + 1; k < bodyClose; k++ {
				if toks[k].tok != token.IDENT || toks[k].lit != name {
					continue
				}
				if k+1 < bodyClose && toks[k+1].tok == token.ARROW {
					sawSend = true
					sendPos = toks[k].pos
				}
				if k+1 < bodyClose && toks[k+1].tok == token.SUB &&
					k+2 < bodyClose && toks[k+2].tok == token.GTR &&
					t.off(toks[k+2].pos) == t.off(toks[k+1].pos)+1 {
					sawRecv = true
				}
			}
			for k := bodyOpen + 1; k < bodyClose; k++ {
				if toks[k].tok == token.ARROW && k+1 < bodyClose &&
					toks[k+1].tok == token.IDENT && toks[k+1].lit == name {
					sawRecv = true
				}
			}
			if sawSend && sawRecv {
				pos := t.file.Position(sendPos)
				return fmt.Errorf("%s: proc %q uses channel parameter %q for both send and receive — a channel parameter may not be used for both input and output in a procedure; declare it as <-chan or chan<-", pos, procName, name)
			}
		}
	}
	return nil
}

// splitBranches splits the token range [lo,hi) (the inside of a par{...})
// into top-level branches: each is a `seq{...}` block, a nested `par{...}`
// block, a replicated `par VAR := range EXPR { ... }` (see isReplicatedPar),
// a bare `{...}` block, or a `name(...)` call.
func (t *transformer) splitBranches(lo, hi int) [][2]int {
	var branches [][2]int
	i := lo
	for i < hi {
		if t.toks[i].tok == token.SEMICOLON {
			i++
			continue
		}
		start := i
		switch {
		case t.toks[i].tok == token.IDENT && (t.toks[i].lit == "seq" || t.toks[i].lit == "par") &&
			i+1 < hi && t.toks[i+1].tok == token.LBRACE:
			close := matchBrace(t.toks, i+1)
			branches = append(branches, [2]int{start, close + 1})
			i = close + 1
		case t.toks[i].tok == token.IDENT && t.toks[i].lit == "par" &&
			i+1 < hi && t.toks[i+1].tok == token.IDENT:
			_, _, _, _, _, closeIdx, ok := t.isReplicatedPar(i+1, hi)
			if !ok {
				panic(fmt.Sprintf("unsupported par-branch shape at %q (%v)", t.toks[i].lit, t.toks[i].tok))
			}
			branches = append(branches, [2]int{start, closeIdx + 1})
			i = closeIdx + 1
		case t.toks[i].tok == token.LBRACE:
			close := matchBrace(t.toks, i)
			branches = append(branches, [2]int{start, close + 1})
			i = close + 1
		case t.toks[i].tok == token.IDENT && i+1 < hi && t.toks[i+1].tok == token.LPAREN:
			close := matchParen(t.toks, i+1)
			branches = append(branches, [2]int{start, close + 1})
			i = close + 1
		default:
			panic(fmt.Sprintf("unsupported par-branch shape at %q (%v)", t.toks[i].lit, t.toks[i].tok))
		}
	}
	return branches
}

// isReplicatedPar reports whether tokens starting at lo (which must
// immediately follow the `par` keyword) form `VAR := range EXPR { BODY }`
// — replication attached directly to `par`, e.g. `par i :=
// range N { worker(...) }`. Deliberately not `par { for i := range N {
// ... } }` (an earlier design, since dropped): wrapping a bare `for` loop
// inside `par` reads exactly like an ordinary sequential loop to anyone
// who doesn't already know bilc special-cases that one shape — attaching
// `par` straight to the loop clause puts the replication at the keyword
// itself, impossible to mistake for a sequential loop. Returns the loop
// variable name, the
// range-expression token range, the body token range, and the index of
// the whole construct's closing `}`.
func (t *transformer) isReplicatedPar(lo, hi int) (varName string, exprLo, exprHi, bodyLo, bodyHi, closeIdx int, ok bool) {
	i := lo
	if i >= hi || t.toks[i].tok != token.IDENT {
		return
	}
	varName = t.toks[i].lit
	i++
	if i >= hi || t.toks[i].tok != token.DEFINE {
		return "", 0, 0, 0, 0, 0, false
	}
	i++
	if i >= hi || t.toks[i].tok != token.RANGE {
		return "", 0, 0, 0, 0, 0, false
	}
	i++
	exprLo = i
	for i < hi && t.toks[i].tok != token.LBRACE {
		i++
	}
	if i >= hi || exprLo == i {
		return "", 0, 0, 0, 0, 0, false
	}
	exprHi = i
	closeIdx = matchBrace(t.toks, i)
	bodyLo, bodyHi = i+1, closeIdx
	return varName, exprLo, exprHi, bodyLo, bodyHi, closeIdx, true
}

// isReplicatedAlt reports whether tokens starting at lo (which must
// immediately follow the `alt` keyword) form `VAR := range EXPR {
// [(COND) &&] BASE[VAR] -> BIND { BODY } }` — replication attached
// directly to `alt`, mirroring how isReplicatedPar attaches replicated
// iteration to `par` (Example 4). The single clause inside must index the
// channel array by exactly the replication variable (`chans[j]`, not some
// other expression) — that's the whole point of a replicated alt, one
// guard per replica, not a hand-picked subset. The optional leading `(COND)
// &&` is a per-replica guard — COND is evaluated with VAR bound to each
// replica in turn (see transform), so it
// can reference VAR freely (`guards[j]`, `len(q[j]) > 0`, ...), same
// freedom the non-replicated `(cond) && chan -> target` guard already has.
// Desugars (in transform) to a call to altN, the underlying primitive
// (Example 13) — this is sugar over that, not a separate mechanism, the
// same relationship `par i := range N` has to parFor. Returns the loop
// variable name, the range-expression token range, the guard condition's
// source (empty if unguarded), the channel-array's source, the bind
// variable name, the body token range, and the index of the whole
// construct's closing `}`.
func (t *transformer) isReplicatedAlt(lo, hi int) (varName string, exprLo, exprHi int, condSrc, baseSrc, bindVar string, bodyLo, bodyHi, closeIdx int, ok bool) {
	fail := func() (string, int, int, string, string, string, int, int, int, bool) {
		return "", 0, 0, "", "", "", 0, 0, 0, false
	}
	i := lo
	if i >= hi || t.toks[i].tok != token.IDENT {
		return fail()
	}
	varName = t.toks[i].lit
	i++
	if i >= hi || t.toks[i].tok != token.DEFINE {
		return fail()
	}
	i++
	if i >= hi || t.toks[i].tok != token.RANGE {
		return fail()
	}
	i++
	exprLo = i
	for i < hi && t.toks[i].tok != token.LBRACE {
		i++
	}
	if i >= hi || exprLo == i {
		return fail()
	}
	exprHi = i
	closeIdx = matchBrace(t.toks, i)
	inner, innerEnd := i+1, closeIdx
	for inner < innerEnd && t.toks[inner].tok == token.SEMICOLON {
		inner++
	}

	if inner < innerEnd && t.toks[inner].tok == token.LPAREN {
		// `(cond) && BASE[VAR] -> BIND { BODY }` — the guarded form; see the
		// doc comment above.
		condClose := matchParen(t.toks, inner)
		if condClose+1 >= innerEnd || t.toks[condClose+1].tok != token.LAND {
			return fail()
		}
		condSrc = string(t.src[t.off(t.toks[inner].pos) : t.off(t.toks[condClose].pos)+1])
		inner = condClose + 2
	}

	if inner >= innerEnd || t.toks[inner].tok != token.IDENT {
		return fail()
	}
	baseStart, baseEnd := inner, inner+1
	if baseEnd >= innerEnd || t.toks[baseEnd].tok != token.LBRACK {
		return fail()
	}
	bracketOpen := baseEnd
	bracketClose := matchBracket(t.toks, bracketOpen)
	if bracketClose != bracketOpen+2 || t.toks[bracketOpen+1].tok != token.IDENT || t.toks[bracketOpen+1].lit != varName {
		return fail() // must index by exactly the replication variable
	}
	baseSrc = string(t.src[t.off(t.toks[baseStart].pos):t.off(t.toks[baseEnd].pos)])

	j := bracketClose + 1
	if j+1 >= innerEnd || t.toks[j].tok != token.SUB || t.toks[j+1].tok != token.GTR ||
		t.off(t.toks[j+1].pos) != t.off(t.toks[j].pos)+1 {
		return fail()
	}
	j += 2
	if j >= innerEnd || t.toks[j].tok != token.IDENT {
		return fail()
	}
	bindVar = t.toks[j].lit
	j++
	if j >= innerEnd || t.toks[j].tok != token.LBRACE {
		return fail()
	}
	bodyOpen := j
	bodyClose := matchBrace(t.toks, bodyOpen)
	bodyLo, bodyHi = bodyOpen+1, bodyClose

	for k := bodyClose + 1; k < innerEnd; k++ {
		if t.toks[k].tok != token.SEMICOLON {
			return fail() // trailing tokens — replicated alt takes exactly one clause
		}
	}
	return varName, exprLo, exprHi, condSrc, baseSrc, bindVar, bodyLo, bodyHi, closeIdx, true
}

// altGuard is one parsed `alt` clause's guard. For every kind except
// conditional, header is the complete Go `select` clause header to emit
// (`case ...:`, no trailing body). Conditional guards (`(cond) & chan ->
// target`) don't get a header: they need a statement *before* the
// `select`, to conditionally nil out a channel, which no single case
// header can express; chanSrc/varSrc/cond are exposed for that. brace is
// the index of the clause's opening `{`.
type altGuard struct {
	header  string
	chanSrc string // conditional guards only: the channel expression's source
	varSrc  string // conditional guards only: the bind target (may be "_")
	cond    string // conditional guards only: the boolean pre-condition source, e.g. "(len(q) > 0)"
	isCond  bool
	brace   int
}

// parseAltGuard parses one `alt` clause's guard, starting at
// lo (bounded by hi, the alt-block's closing `}`): `default`, a bare
// receive (`<-expr`), a `chan -> target` arrow receive, or a *guarded*
// arrow receive `(cond) && chan -> target` (`&&` since it isn't standing
// in for Go's bitwise-and here either way — see the mechanics section) —
// each followed
// by `{`, a curly block, rather than Go's `case ...:`.
func (t *transformer) parseAltGuard(lo, hi int) (g altGuard, ok bool) {
	if lo >= hi {
		return altGuard{}, false
	}
	if t.toks[lo].tok == token.IDENT && t.toks[lo].lit == "link" {
		// `link[idx]` isn't a real Go channel (see matchLinkReceive) — the
		// unconditional-guard path below would otherwise match it structurally
		// (parsePrimaryExpr handles `base[idx]` generically) and silently emit
		// a broken `case v = <-link[idx]:`. Rejecting here surfaces the normal
		// "unsupported alt-guard shape" panic instead — alt/pri alt over links
		// isn't supported yet (see ../../emulator/README.md).
		return altGuard{}, false
	}
	if t.toks[lo].tok == token.LPAREN {
		// `(cond) && chan -> target { body }` — guarded input: the
		// guard is only offered to `select` at all when cond holds. Go's
		// select has no conditional-case syntax, so this desugars to the
		// standard "nil the channel out when the guard shouldn't fire"
		// trick — the same one the unconditional `chan -> target` guard
		// already generalizes over both a bind target and a call-shaped
		// one (parseArrowTarget), so this does too: for a call-shaped
		// target (`(cond) && time -> After(d) { ... }`), there's nothing
		// to bind (same as the unconditional case), so the synthetic
		// guard variable becomes the *whole call expression*, still
		// conditionally nil'd the same way — `case <-bilGuardN:` in the
		// select is then a plain discard-receive, no special-casing
		// needed in emitAltClauses at all.
		condClose := matchParen(t.toks, lo)
		if condClose+1 >= hi || t.toks[condClose+1].tok != token.LAND {
			return altGuard{}, false
		}
		condSrc := string(t.src[t.off(t.toks[lo].pos) : t.off(t.toks[condClose].pos)+1])
		gLo := condClose + 2
		if gLo < hi && t.toks[gLo].tok == token.IDENT && t.toks[gLo].lit == "link" {
			return altGuard{}, false // see the unconditional-guard check above
		}
		lhsEnd, ok1 := t.parsePrimaryExpr(gLo, hi)
		if !ok1 || lhsEnd+1 >= hi || t.toks[lhsEnd].tok != token.SUB || t.toks[lhsEnd+1].tok != token.GTR ||
			t.off(t.toks[lhsEnd+1].pos) != t.off(t.toks[lhsEnd].pos)+1 {
			return altGuard{}, false
		}
		rhsLo := lhsEnd + 2
		rhsEnd, isCall, ok2 := t.parseArrowTarget(rhsLo, hi)
		if !ok2 || rhsEnd >= hi || t.toks[rhsEnd].tok != token.LBRACE {
			return altGuard{}, false
		}
		var chanSrc, varSrc string
		if isCall {
			// Reconstruct as `chan.Method(args)`, not a raw slice of the
			// source (which would still contain the literal `->` token
			// text) — same substitution the unconditional call-shaped
			// case makes (see below, `chanSrc + "." + callSrc`).
			chanExprSrc := string(t.src[t.off(t.toks[gLo].pos):t.off(t.toks[lhsEnd].pos)])
			callSrc := string(t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[rhsEnd].pos)])
			chanSrc = chanExprSrc + "." + callSrc
			varSrc = "_"
		} else {
			chanSrc = string(t.src[t.off(t.toks[gLo].pos):t.off(t.toks[lhsEnd].pos)])
			varSrc = string(t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[rhsEnd].pos)])
		}
		return altGuard{cond: condSrc, chanSrc: chanSrc, varSrc: varSrc, isCond: true, brace: rhsEnd}, true
	}
	if t.toks[lo].tok == token.IDENT && t.toks[lo].lit == "skip" &&
		lo+1 < hi && t.toks[lo+1].tok == token.LBRACE {
		// `alt`'s SKIP guard: always ready, never blocks — Go's
		// `select`'s own `default:` is exactly that, so this is the only
		// clause shape that still bottoms out in a raw Go keyword. Spelled
		// `skip`, not `default`, to match every other clause's own
		// vocabulary, and to stay distinct from `placed par`'s unrelated
		// `default` (SKIP means "no guard was ready"; placed par's default
		// means "everywhere else in the grid").
		return altGuard{header: "default:", brace: lo + 1}, true
	}
	if t.toks[lo].tok == token.ARROW {
		i := lo + 1
		for i < hi && t.toks[i].tok != token.LBRACE {
			i++
		}
		if i >= hi || i == lo+1 {
			return altGuard{}, false
		}
		expr := t.src[t.off(t.toks[lo].pos):t.off(t.toks[i].pos)]
		return altGuard{header: "case " + string(expr) + ":", brace: i}, true
	}
	lhsEnd, ok1 := t.parsePrimaryExpr(lo, hi)
	if !ok1 || lhsEnd+1 >= hi || t.toks[lhsEnd].tok != token.SUB || t.toks[lhsEnd+1].tok != token.GTR ||
		t.off(t.toks[lhsEnd+1].pos) != t.off(t.toks[lhsEnd].pos)+1 {
		return altGuard{}, false
	}
	rhsLo := lhsEnd + 2
	chanSrc := string(t.src[t.off(t.toks[lo].pos):t.off(t.toks[lhsEnd].pos)])

	rhsEnd, isCall, ok2 := t.parseArrowTarget(rhsLo, hi)
	if !ok2 || rhsEnd >= hi || t.toks[rhsEnd].tok != token.LBRACE {
		return altGuard{}, false
	}
	if isCall {
		// `c -> Method(args)`: not a bind, a method call on `c` whose
		// result is received from — e.g. `time -> After(N)` -> `<-time.After(N)`.
		callSrc := string(t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[rhsEnd].pos)])
		return altGuard{header: "case <-" + chanSrc + "." + callSrc + ":", brace: rhsEnd}, true
	}
	// `chan -> var`: assignment, not declaration — `x` in `request -> x` is
	// always a pre-declared variable. Also matches the Example 1
	// statement-level `c -> x` sugar, which is likewise `x = <-c`, not
	// `x := <-c`. `var` is blank-identifier-safe
	// too: `case _ = <-c:` is legal Go (unlike `case _ := <-c:`, "no new
	// variables on left side of :="), so `c -> _` needs no special case.
	varSrc := t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[rhsEnd].pos)]
	return altGuard{header: fmt.Sprintf("case %s = <-%s:", string(varSrc), chanSrc), brace: rhsEnd}, true
}

// parseArrowTarget parses the right-hand side of a `->`
// arrow: either a plain primary expression (a bind target, e.g. `x`,
// `arr[i]`) or a call-shaped one (`Method(args)`). The call form
// generalizes the arrow into "receive from a method call on the channel"
// rather than "receive into this variable" — e.g. `time -> After(N)`
// desugars to plain Go `<-time.After(N)`, no new evaluation semantics
// invented. Returns the end
// index, whether it's call-shaped, and ok.
func (t *transformer) parseArrowTarget(lo, hi int) (end int, isCall, ok bool) {
	end, ok = t.parsePrimaryExpr(lo, hi)
	if !ok {
		return
	}
	if end < hi && t.toks[end].tok == token.LPAREN {
		end = matchParen(t.toks, end) + 1
		isCall = true
	}
	return
}

// parsePrimaryExpr parses a bare identifier optionally followed by `.field`
// selectors and/or `[index]` subscripts (`c`, `arr[i]`, `s.field`), starting
// at lo (which must be IDENT), bounded by hi. Returns the index just past
// the expression; ok is false if lo isn't an identifier.
func (t *transformer) parsePrimaryExpr(lo, hi int) (end int, ok bool) {
	if lo >= hi || t.toks[lo].tok != token.IDENT {
		return lo, false
	}
	i := lo + 1
	for i < hi {
		switch t.toks[i].tok {
		case token.LBRACK:
			i = matchBracket(t.toks, i) + 1
		case token.PERIOD:
			if i+1 >= hi || t.toks[i+1].tok != token.IDENT {
				return i, true
			}
			i += 2
		default:
			return i, true
		}
	}
	return i, true
}

// isStmtStart reports whether token index i begins a new statement: either
// the very first token, or the token right after a statement separator (the
// `;` go/scanner auto-inserts at line ends, or a block-opening `{`).
func (t *transformer) isStmtStart(i int) bool {
	return i == 0 || t.toks[i-1].tok == token.SEMICOLON || t.toks[i-1].tok == token.LBRACE
}

// caseClause is one `TypeExpr { body }` clause inside a `chan -> case v {
// ... }` block: [typeLo, typeHi) is the type name's token range, [bodyLo,
// bodyHi) is the clause body's.
type caseClause struct {
	typeLo, typeHi, bodyLo, bodyHi int
}

// matchArrowCaseDispatch recognizes `chan -> case v { Type1 { body1 } Type2
// { body2 } ... }` as a whole statement — a variant/tagged protocol
// receive (Example 9), fusing a receive with a type-based dispatch into
// one construct the same way `alt`'s `chan -> var { body }` fuses a receive
// with a channel-based one. Desugars to a single Go type switch: `switch v
// := (<-chan).(type) { case Type1: body1 case Type2: body2 }`. `v` is
// named once, right after `case`, not per clause — Go's type switch only
// ever has one bound variable shared by every case, a hard constraint of
// the underlying construct, not a Bil choice.
func (t *transformer) matchArrowCaseDispatch(lo, hi int) (chanSrc, varSrc string, cases []caseClause, closeIdx int, ok bool) {
	if !t.isStmtStart(lo) {
		return "", "", nil, 0, false
	}
	if lo < hi && t.toks[lo].tok == token.IDENT && t.toks[lo].lit == "link" {
		return "", "", nil, 0, false // see matchLinkReceive; not a real channel, no tagged-protocol dispatch
	}
	lhsEnd, ok1 := t.parsePrimaryExpr(lo, hi)
	if !ok1 || lhsEnd+1 >= hi || t.toks[lhsEnd].tok != token.SUB || t.toks[lhsEnd+1].tok != token.GTR ||
		t.off(t.toks[lhsEnd+1].pos) != t.off(t.toks[lhsEnd].pos)+1 {
		return "", "", nil, 0, false
	}
	i := lhsEnd + 2
	if i >= hi || t.toks[i].tok != token.CASE {
		return "", "", nil, 0, false
	}
	i++
	if i >= hi || t.toks[i].tok != token.IDENT {
		return "", "", nil, 0, false
	}
	varSrc = t.toks[i].lit
	i++
	if i >= hi || t.toks[i].tok != token.LBRACE {
		return "", "", nil, 0, false
	}
	blockClose := matchBrace(t.toks, i)
	j := i + 1
	for j < blockClose {
		if t.toks[j].tok == token.SEMICOLON {
			j++
			continue
		}
		typeEnd, okT := t.parsePrimaryExpr(j, blockClose)
		if !okT || typeEnd >= blockClose || t.toks[typeEnd].tok != token.LBRACE {
			return "", "", nil, 0, false
		}
		bodyClose := matchBrace(t.toks, typeEnd)
		cases = append(cases, caseClause{typeLo: j, typeHi: typeEnd, bodyLo: typeEnd + 1, bodyHi: bodyClose})
		j = bodyClose + 1
	}
	if len(cases) == 0 {
		return "", "", nil, 0, false
	}
	chanSrc = string(t.src[t.off(t.toks[lo].pos):t.off(t.toks[lhsEnd].pos)])
	return chanSrc, varSrc, cases, blockClose, true
}

// matchArrowReceive recognizes `c -> x` as a whole statement — sugar for
// `x = <-c`, read left-to-right rather than Go's receive-flows-right
// `x = <-c`. A call-shaped target generalizes this to
// "receive from a method call on c" instead of "receive into x" — `c ->
// Method(args)` -> `<-c.Method(args)` (see parseArrowTarget) — an expression
// statement that evaluates/blocks on the receive and discards the value,
// same as writing that Go directly. Go has no `->` token, so this is SUB
// immediately followed by GTR (no space — that adjacency is what
// disambiguates it from a subtraction-then-comparison, which can't appear
// here anyway: Go doesn't allow a bare comparison as a statement). Only
// attempted at statement starts (see isStmtStart), which also rules out
// `if`/`for` conditions and call arguments — those never follow `;`/`{`.
func (t *transformer) matchArrowReceive(lo, hi int) (rhsLo, rhsHi, lhsEnd int, isCall, ok bool) {
	if !t.isStmtStart(lo) {
		return 0, 0, 0, false, false
	}
	lhsEnd, ok = t.parsePrimaryExpr(lo, hi)
	if !ok || lhsEnd+1 >= hi || t.toks[lhsEnd].tok != token.SUB || t.toks[lhsEnd+1].tok != token.GTR ||
		t.off(t.toks[lhsEnd+1].pos) != t.off(t.toks[lhsEnd].pos)+1 {
		return 0, 0, 0, false, false
	}
	rhsLo = lhsEnd + 2
	rhsHi, isCall, ok = t.parseArrowTarget(rhsLo, hi)
	if !ok || rhsHi >= hi || (t.toks[rhsHi].tok != token.SEMICOLON && t.toks[rhsHi].tok != token.RBRACE) {
		return 0, 0, 0, false, false
	}
	return rhsLo, rhsHi, lhsEnd, isCall, true
}

// matchLinkReceive recognizes `link[idx] -> var` as a whole statement —
// `link` is Bil's reserved, index-addressed array of nearest-neighbour
// links (an emulator concept: see ../../emulator/README.md and its
// nodeprog/bilink package), not a real Go channel — the physical link
// crosses a WASM-instance/Worker boundary, which no in-process Go
// channel can express. `link[idx]` already parses as an ordinary
// primary expression (parsePrimaryExpr handles `base[idx]` generically),
// so matchArrowReceive already matches it structurally; this just
// narrows that match to exactly `link[idx]` (rejecting a call-shaped
// target or any chaining past the single index, like `link[idx].field`)
// and reports the index and bind-variable source separately, so the
// caller can emit `var = bilink.Recv(idx)` instead of a plain `<-`.
func (t *transformer) matchLinkReceive(lo, hi int) (idxSrc, varSrc string, end int, ok bool) {
	if lo >= hi || t.toks[lo].tok != token.IDENT || t.toks[lo].lit != "link" {
		return "", "", 0, false
	}
	rhsLo, rhsHi, lhsEnd, isCall, ok := t.matchArrowReceive(lo, hi)
	if !ok || isCall {
		return "", "", 0, false
	}
	if lo+1 >= hi || t.toks[lo+1].tok != token.LBRACK {
		return "", "", 0, false
	}
	open := lo + 1
	close := matchBracket(t.toks, open)
	if close+1 != lhsEnd {
		return "", "", 0, false // trailing chain past `link[idx]` — not supported
	}
	idxSrc = string(t.src[t.off(t.toks[open+1].pos):t.off(t.toks[close].pos)])
	varSrc = string(t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[rhsHi].pos)])
	return idxSrc, varSrc, rhsHi, true
}

// matchLinkSend recognizes `link[idx] <- value` as a whole statement —
// the send-side counterpart to matchLinkReceive; see its doc comment
// for why `link` needs rewriting to a bilink call rather than passing
// through as plain Go (which is otherwise never needed for a send:
// `chan <- value` is already legal Go, so no other channel send in
// this file gets special-cased the way receives are). value's end is
// found by scanning to the enclosing statement's terminator (the `;`
// go/scanner inserts at line end, or the block's closing `}`), tracking
// paren/bracket/brace depth so a nested call or composite literal in
// the value expression doesn't end the scan early.
func (t *transformer) matchLinkSend(lo, hi int) (idxSrc, valSrc string, end int, ok bool) {
	if !t.isStmtStart(lo) {
		return "", "", 0, false
	}
	if lo >= hi || t.toks[lo].tok != token.IDENT || t.toks[lo].lit != "link" {
		return "", "", 0, false
	}
	if lo+1 >= hi || t.toks[lo+1].tok != token.LBRACK {
		return "", "", 0, false
	}
	open := lo + 1
	close := matchBracket(t.toks, open)
	if close+1 >= hi || t.toks[close+1].tok != token.ARROW {
		return "", "", 0, false
	}
	idxSrc = string(t.src[t.off(t.toks[open+1].pos):t.off(t.toks[close].pos)])
	valLo := close + 2
	depth := 0
	j := valLo
	for j < hi {
		switch t.toks[j].tok {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK:
			depth--
		case token.RBRACE:
			if depth == 0 {
				goto foundEnd // enclosing block's closing brace -- valid terminator
			}
			depth--
		case token.SEMICOLON:
			if depth == 0 {
				goto foundEnd
			}
		}
		j++
	}
foundEnd:
	if j == valLo || j >= hi {
		return "", "", 0, false
	}
	valSrc = string(t.src[t.off(t.toks[valLo].pos):t.off(t.toks[j].pos)])
	return idxSrc, valSrc, j, true
}

// emitAltClauses parses every clause of an `alt { ... }` block ([lo,hi) is
// the block's interior; hi is the index of its closing `}`) and writes a Go
// `select` — preceded by any setup guarded clauses need — to out.
//
// Conditional guards (`(cond) & chan -> target`) get a synthetic channel
// variable each, set to the real channel or to `nil` depending on cond,
// emitted *before* the `select` — Go has no syntax for a case that's only
// sometimes present, so a guard that shouldn't fire this round uses a nil
// channel instead (nil channel ops block forever, so select never picks
// it). This is why the whole thing is wrapped in a `{ }` block by the
// caller: it's no longer just one `select` statement.
func (t *transformer) emitAltClauses(out *bytes.Buffer, lo, hi int) {
	type clause struct {
		g              altGuard
		bodyLo, bodyHi int
	}
	var clauses []clause
	j := lo
	for j < hi {
		if t.toks[j].tok == token.SEMICOLON {
			j++
			continue
		}
		g, ok := t.parseAltGuard(j, hi)
		if !ok {
			panic(fmt.Sprintf("unsupported alt-guard shape at %q (%v)", t.toks[j].lit, t.toks[j].tok))
		}
		bodyClose := matchBrace(t.toks, g.brace)
		clauses = append(clauses, clause{g, g.brace + 1, bodyClose})
		j = bodyClose + 1
	}

	for idx, cl := range clauses {
		if !cl.g.isCond {
			continue
		}
		fmt.Fprintf(out, "bilGuard%d := %s\n", idx, cl.g.chanSrc)
		fmt.Fprintf(out, "if !%s {\n\tbilGuard%d = nil\n}\n", cl.g.cond, idx)
	}

	out.WriteString("select {\n")
	emitted := make([]bool, len(clauses))
	for idx, cl := range clauses {
		if emitted[idx] {
			continue
		}
		emitted[idx] = true
		switch {
		case cl.g.isCond:
			ch := fmt.Sprintf("bilGuard%d", idx)
			if cl.g.varSrc == "_" {
				out.WriteString("case <-" + ch + ":\n")
			} else {
				out.WriteString("case " + cl.g.varSrc + " = <-" + ch + ":\n")
			}
			out.Write(t.transform(cl.bodyLo, cl.bodyHi))
			out.WriteString("\n")
		default:
			out.WriteString(cl.g.header + "\n")
			out.Write(t.transform(cl.bodyLo, cl.bodyHi))
			out.WriteString("\n")
		}
	}
	out.WriteString("}\n")
}

// emitPriAltClauses parses every clause of a `pri alt { ... }` block and
// writes Bil's priority alt — always take the first (highest-priority)
// ready guard, never Go's `select`'s pseudo-random pick among several
// ready cases — as a cascade of non-blocking `select`s, one per priority
// level, ending in a single blocking `select` covering every clause. This
// is the standard Go idiom for a priority select, not a Bil invention:
// level k offers only the top k clauses plus a `default:` falling through
// to level k+1; the final level (all clauses, no `default:`) is the only
// one that actually blocks, and only once a full top-to-bottom pass has
// confirmed nothing was already ready. This is a faithful priority
// guarantee, not merely a close approximation: it's a guarantee about the
// state at the moment `pri alt` is evaluated (if two guards are already
// ready when it starts, the higher-priority one always wins, exactly what
// level 1 gives here), not about resolving two guards that become ready
// during evaluation at what looks like the same instant — "the same
// instant" isn't a meaningful, resolvable notion in any real concurrent
// system, hardware included.
//
// Supports every guard shape plain `alt` does, including conditional
// guards (`(cond) && chan -> target`, Example 6): a conditional clause's
// channel-nilling setup happens once, up front, before any cascade level
// runs — the guard's eligibility is decided for the whole `pri alt`
// evaluation, not re-decided per level — and every level that offers that
// clause just receives from its already-resolved synthetic `bilGuardN`
// variable, same as a plain `alt` does with its own (single, unnested)
// `select`. This is also why `pri alt`, like plain `alt`, is wrapped in a
// `{ }` block by its caller rather than emitting bare nested `select`s
// directly: those setup statements need somewhere to live, and wrapping
// keeps a second `pri alt`/`alt` elsewhere in the same enclosing scope
// from re-declaring the same `bilGuardN` names.
func (t *transformer) emitPriAltClauses(out *bytes.Buffer, lo, hi int) {
	type clause struct {
		g              altGuard
		header         string
		bodyLo, bodyHi int
	}
	var clauses []clause
	j := lo
	for j < hi {
		if t.toks[j].tok == token.SEMICOLON {
			j++
			continue
		}
		g, ok := t.parseAltGuard(j, hi)
		if !ok {
			panic(fmt.Sprintf("unsupported pri-alt-guard shape at %q (%v)", t.toks[j].lit, t.toks[j].tok))
		}
		bodyClose := matchBrace(t.toks, g.brace)
		clauses = append(clauses, clause{g: g, bodyLo: g.brace + 1, bodyHi: bodyClose})
		j = bodyClose + 1
	}
	if len(clauses) == 0 {
		panic("pri alt with no clauses")
	}

	for idx := range clauses {
		cl := &clauses[idx]
		if !cl.g.isCond {
			cl.header = cl.g.header
			continue
		}
		ch := fmt.Sprintf("bilGuard%d", idx)
		fmt.Fprintf(out, "%s := %s\n", ch, cl.g.chanSrc)
		fmt.Fprintf(out, "if !%s {\n\t%s = nil\n}\n", cl.g.cond, ch)
		if cl.g.varSrc == "_" {
			cl.header = "case <-" + ch + ":"
		} else {
			cl.header = "case " + cl.g.varSrc + " = <-" + ch + ":"
		}
	}

	var writeLevel func(k int)
	writeLevel = func(k int) {
		out.WriteString("select {\n")
		for idx := 0; idx < k; idx++ {
			cl := clauses[idx]
			out.WriteString(cl.header + "\n")
			out.Write(t.transform(cl.bodyLo, cl.bodyHi))
			out.WriteString("\n")
		}
		if k < len(clauses) {
			out.WriteString("default:\n")
			writeLevel(k + 1)
		}
		out.WriteString("}\n")
	}
	writeLevel(1)
}

// transform rewrites token range [lo,hi) and returns the resulting source
// bytes, covering the byte range [off(toks[lo]), off(toks[hi])).
func (t *transformer) transform(lo, hi int) []byte {
	var out bytes.Buffer
	cursor := t.off(t.toks[lo].pos)
	flushTo := func(o int) {
		out.Write(t.src[cursor:o])
		cursor = o
	}
	i := lo
	for i < hi {
		tk := t.toks[i]
		switch {
		case tk.tok == token.IDENT && tk.lit == "proc":
			flushTo(t.off(tk.pos))
			out.WriteString("func")
			cursor = t.off(tk.pos) + len("proc")
			i++

		case tk.tok == token.IDENT && tk.lit == "pri" &&
			i+1 < hi && t.toks[i+1].tok == token.IDENT && t.toks[i+1].lit == "alt" &&
			i+2 < hi && t.toks[i+2].tok == token.LBRACE:
			// `pri alt { ... }` rewrites
			// to a cascade of `select`s — see emitPriAltClauses for why a single
			// `select` can't express "always take the first ready
			// guard" the way plain `alt` can just reuse `select` for "take any
			// ready guard". Wrapped in `{ }` for the same reason plain `alt`
			// is: a conditional clause needs setup statements ahead of the
			// cascade itself.
			flushTo(t.off(tk.pos))
			close := matchBrace(t.toks, i+2)
			out.WriteString("{\n")
			t.emitPriAltClauses(&out, i+3, close)
			out.WriteString("}")
			cursor = t.off(t.toks[close].pos) + 1
			i = close + 1

		case tk.tok == token.IDENT && tk.lit == "alt" &&
			i+1 < hi && t.toks[i+1].tok == token.LBRACE:
			// `alt` (kept as a keyword like par/seq/proc) rewrites
			// to `select`, but its clauses use Bil's own guard
			// shape (`chan -> var { body }`, `default { body }`, `(cond) &&
			// chan -> var { body }`) rather than Go's `case ...: body` — see
			// parseAltGuard. Wrapped in `{ }`, not just `select {...}`
			// directly: a guarded clause needs setup statements ahead of
			// the `select` itself (see emitAltClauses), so this is a block,
			// not always a single statement.
			flushTo(t.off(tk.pos))
			close := matchBrace(t.toks, i+1)
			out.WriteString("{\n")
			t.emitAltClauses(&out, i+2, close)
			out.WriteString("}")
			cursor = t.off(t.toks[close].pos) + 1
			i = close + 1

		case tk.tok == token.IDENT && tk.lit == "alt" &&
			i+1 < hi && t.toks[i+1].tok == token.IDENT:
			// `alt VAR := range EXPR { [(COND) &&] BASE[VAR] -> BIND { BODY }
			// }` — replicated alt, attached directly to `alt` — see
			// isReplicatedAlt. Unguarded, desugars to a bare call to altN,
			// the underlying primitive (Example 13): `j, v := altN(chans);
			// BODY`. Guarded, it first builds a per-replica []bool by
			// evaluating COND once per index — VAR is re-bound each
			// iteration, so COND can reference it (`guards[j]`, `len(q[j]) >
			// 0`, ...) — then passes that to altN, which nils out any
			// replica whose guard came back false (see altNHelper).
			varName, exprLo, exprHi, condSrc, baseSrc, bindVar, bodyLo, bodyHi, closeIdx, ok := t.isReplicatedAlt(i+1, hi)
			if !ok {
				panic(fmt.Sprintf("unsupported alt shape at %q (%v)", t.toks[i+1].lit, t.toks[i+1].tok))
			}
			flushTo(t.off(tk.pos))
			out.WriteString("{\n")
			if condSrc != "" {
				out.WriteString("bilGuards := make([]bool, len(" + baseSrc + "))\n")
				out.WriteString("for " + varName + " := range " + baseSrc + " {\n")
				out.WriteString("bilGuards[" + varName + "] = " + condSrc + "\n")
				out.WriteString("}\n")
				out.WriteString(varName + ", " + bindVar + " := altN(" + baseSrc + ", bilGuards...)\n")
			} else {
				out.WriteString(varName + ", " + bindVar + " := altN(" + baseSrc + ")\n")
			}
			if varName != "_" {
				// `chans[j]` in the source is what tells isReplicatedAlt
				// which array to alt over — but that indexing expression
				// itself never survives into the output (only the bare
				// array does, as altN's argument), so from the emitted
				// Go's point of view `j` is a fresh binding the body may or
				// may not go on to reference. Silence "declared and not
				// used" unconditionally rather than only when the body
				// happens not to reference VAR: writing `chans[j]` already
				// reads as "using" j, and it would be surprising if
				// whether that compiles depended on what the body does
				// with it (confirmed by hitting exactly this compile error
				// on `alt j := range n { chans[j] -> v { println(v) } }`).
				// A redundant `_ = j` is harmless even when the body does
				// reference VAR too.
				out.WriteString("_ = " + varName + "\n")
			}
			out.Write(t.transform(bodyLo, bodyHi))
			out.WriteString("\n}")
			cursor = t.off(t.toks[closeIdx].pos) + 1
			i = closeIdx + 1
			t.usedAltN = true
			_ = exprLo
			_ = exprHi

		case tk.tok == token.IDENT && tk.lit == "stop" &&
			i+1 < hi && (t.toks[i+1].tok == token.SEMICOLON || t.toks[i+1].tok == token.RBRACE):
			// `stop` used as a bare statement (not a call, not a value) ->
			// time.Sleep(<max duration>): a non-busy, effectively-permanent
			// block. NOT select{} — that's provably-permanent to the Go
			// runtime, which crashes with a deadlock panic the moment
			// nothing else is alive, contradicting STOP's actual meaning
			// ("never terminate"). A pending timer isn't provably
			// permanent, so the runtime never flags it, even alone.
			flushTo(t.off(tk.pos))
			out.WriteString("time.Sleep(1<<63 - 1)")
			cursor = t.off(tk.pos) + len("stop")
			i++
			t.usedStop = true

		case tk.tok == token.IDENT && tk.lit == "skip" &&
			i+1 < hi && (t.toks[i+1].tok == token.SEMICOLON || t.toks[i+1].tok == token.RBRACE):
			// `skip` used as a bare statement (not a call, not a value) ->
			// Bil's SKIP process: do nothing, terminate immediately.
			// Emitted as nothing at all rather than some placeholder no-op
			// call — Go doesn't need an explicit "do nothing" statement the
			// way some languages require every alternative to have an
			// explicit body, so dropping the token leaves either a lone
			// `;` (a legal empty statement) or an empty `{ }` block, both
			// already what Bil emits today when SKIP is elided by hand.
			flushTo(t.off(tk.pos))
			cursor = t.off(tk.pos) + len("skip")
			i++

		case tk.tok == token.IDENT && tk.lit == "seq" &&
			i+1 < hi && t.toks[i+1].tok == token.LBRACE:
			flushTo(t.off(tk.pos))
			close := matchBrace(t.toks, i+1)
			out.WriteString("{")
			out.Write(t.transform(i+2, close))
			out.WriteString("}")
			cursor = t.off(t.toks[close].pos) + 1
			i = close + 1

		case tk.tok == token.IDENT && tk.lit == "par" &&
			i+1 < hi && t.toks[i+1].tok == token.LBRACE:
			flushTo(t.off(tk.pos))
			close := matchBrace(t.toks, i+1)
			out.WriteString("par(\n")
			for _, br := range t.splitBranches(i+2, close) {
				out.WriteString("func() {\n")
				out.Write(t.transform(br[0], br[1]))
				out.WriteString("\n},\n")
			}
			out.WriteString(")")
			cursor = t.off(t.toks[close].pos) + 1
			i = close + 1

		case tk.tok == token.IDENT && tk.lit == "par" &&
			i+1 < hi && t.toks[i+1].tok == token.IDENT:
			// `par VAR := range EXPR { BODY }` — replicated par, attached
			// directly to `par` — see isReplicatedPar.
			varName, exprLo, exprHi, bodyLo, bodyHi, closeIdx, ok := t.isReplicatedPar(i+1, hi)
			if !ok {
				panic(fmt.Sprintf("unsupported par shape at %q (%v)", t.toks[i+1].lit, t.toks[i+1].tok))
			}
			flushTo(t.off(tk.pos))
			out.WriteString("parFor(")
			out.Write(t.src[t.off(t.toks[exprLo].pos):t.off(t.toks[exprHi].pos)])
			out.WriteString(", func(" + varName + " int) {\n")
			out.Write(t.transform(bodyLo, bodyHi))
			out.WriteString("\n})")
			cursor = t.off(t.toks[closeIdx].pos) + 1
			i = closeIdx + 1

		case tk.tok == token.IDENT:
			if idxSrc, varSrc, end, ok := t.matchLinkReceive(i, hi); ok {
				flushTo(t.off(tk.pos))
				out.WriteString(varSrc + " = bilink.Recv(" + idxSrc + ")")
				cursor = t.off(t.toks[end].pos)
				i = end
				t.usedLink = true
			} else if idxSrc, valSrc, end, ok := t.matchLinkSend(i, hi); ok {
				flushTo(t.off(tk.pos))
				out.WriteString("bilink.Send(" + idxSrc + ", " + valSrc + ")")
				cursor = t.off(t.toks[end].pos)
				i = end
				t.usedLink = true
			} else if chanSrc, varSrc, cases, closeIdx, ok := t.matchArrowCaseDispatch(i, hi); ok {
				flushTo(t.off(tk.pos))
				out.WriteString("switch " + varSrc + " := (<-" + chanSrc + ").(type) {\n")
				for _, c := range cases {
					out.WriteString("case " + string(t.src[t.off(t.toks[c.typeLo].pos):t.off(t.toks[c.typeHi].pos)]) + ":\n")
					out.Write(t.transform(c.bodyLo, c.bodyHi))
					out.WriteString("\n")
				}
				out.WriteString("}")
				cursor = t.off(t.toks[closeIdx].pos) + 1
				i = closeIdx + 1
			} else if rhsLo, rhsHi, lhsEnd, isCall, ok := t.matchArrowReceive(i, hi); ok {
				flushTo(t.off(tk.pos))
				if isCall {
					out.WriteString("<-")
					out.Write(t.src[t.off(tk.pos):t.off(t.toks[lhsEnd].pos)])
					out.WriteString(".")
					out.Write(t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[rhsHi].pos)])
				} else {
					out.Write(t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[rhsHi].pos)])
					out.WriteString(" = <-")
					out.Write(t.src[t.off(tk.pos):t.off(t.toks[lhsEnd].pos)])
				}
				cursor = t.off(t.toks[rhsHi].pos)
				i = rhsHi
			} else {
				i++
			}

		default:
			i++
		}
	}
	flushTo(t.off(t.toks[hi].pos))
	return out.Bytes()
}

// checkNoGoStatement rejects bare `go` statements — concurrency in Bil goes
// through `par`, which joins its branches; a raw `go` is fire-and-forget
// and has no place in source (the preprocessor's own injected runtime is
// the only place `go` is allowed, and that's added after this check runs).
func (t *transformer) checkNoGoStatement() error {
	for _, tk := range t.toks {
		if tk.tok == token.GO {
			pos := t.file.Position(tk.pos)
			return fmt.Errorf("%s: bare `go` statement not allowed; use `par` instead", pos)
		}
	}
	return nil
}

// checkNoPointerChannels rejects channel types whose element type is a
// pointer (`chan *T`, `chan<- *T`, `<-chan *T`), wherever they appear
// (make(), proc/func params, var decls) — a pointer sent over a channel
// lets two processes share mutable memory through what looks like a
// message, defeating the point of communicating by value.
func (t *transformer) checkNoPointerChannels() error {
	toks := t.toks
	for i, tk := range toks {
		if tk.tok != token.CHAN {
			continue
		}
		elem := i + 1
		if elem < len(toks) && toks[elem].tok == token.ARROW {
			elem++
		}
		if elem < len(toks) && toks[elem].tok == token.MUL {
			pos := t.file.Position(tk.pos)
			return fmt.Errorf("%s: channel of pointer type not allowed; send values, not pointers, over channels", pos)
		}
	}
	return nil
}

// checkNoBufferedChannels rejects `make(chan T, N)` — a capacity argument
// creates a buffered channel, letting a send complete with no receiver
// actually ready. Channels must stay a synchronous rendezvous.
func (t *transformer) checkNoBufferedChannels() error {
	toks := t.toks
	for i := 0; i+2 < len(toks); i++ {
		if !(toks[i].tok == token.IDENT && toks[i].lit == "make" && toks[i+1].tok == token.LPAREN) {
			continue
		}
		open := i + 1
		close := matchParen(toks, open)
		elem := open + 1
		if elem < close && toks[elem].tok == token.ARROW {
			elem++
		}
		if elem >= close || toks[elem].tok != token.CHAN {
			continue // not a chan make() call
		}
		depth := 0
		for j := open + 1; j < close; j++ {
			switch toks[j].tok {
			case token.LPAREN, token.LBRACK, token.LBRACE:
				depth++
			case token.RPAREN, token.RBRACK, token.RBRACE:
				depth--
			case token.COMMA:
				if depth == 0 {
					pos := t.file.Position(toks[i].pos)
					return fmt.Errorf("%s: buffered channels not allowed; channels must be synchronous (make(chan T), no capacity argument)", pos)
				}
			}
		}
	}
	return nil
}

func Transform(src []byte) ([]byte, error) {
	t := tokenize("source.bil", src)
	for _, tk := range t.toks {
		if tk.tok == token.IDENT && tk.lit == "altN" {
			t.usedAltN = true
		}
		if tk.tok == token.IDENT && tk.lit == "splitN2D" {
			t.usedSplitN2D = true
		}
	}
	if err := t.checkNoGoStatement(); err != nil {
		return nil, err
	}
	if err := t.checkNoPointerChannels(); err != nil {
		return nil, err
	}
	if err := t.checkProcChanBothDirections(); err != nil {
		return nil, err
	}
	if err := t.checkNoBufferedChannels(); err != nil {
		return nil, err
	}
	body := t.transform(0, len(t.toks)-1) // exclude EOF sentinel

	// Inject `import "sync"` right after the package clause, and append the
	// par helper at the end of the file (see parHelper's comment for why).
	pkgEnd := bytes.IndexByte(body, '\n')
	if pkgEnd < 0 {
		pkgEnd = len(body)
	}
	var full bytes.Buffer
	if t.usedLink {
		// bilink only exists under this build constraint (it's backed by
		// syscall/js) -- a program using `link[...]` can only ever run
		// there, so this is auto-injected the same way the import is,
		// rather than left for whoever places the generated file to
		// remember by hand.
		full.WriteString("//go:build js && wasm\n\n")
	}
	full.Write(body[:pkgEnd])
	full.WriteString(importSync)
	if t.usedStop && !t.hasImport("time") {
		full.WriteString("\nimport \"time\"\n")
	}
	if t.usedAltN && !t.hasImport("reflect") {
		full.WriteString("\nimport \"reflect\"\n")
	}
	if t.usedLink && !t.hasImport("emulator/nodeprog/bilink") {
		full.WriteString("\nimport \"emulator/nodeprog/bilink\"\n")
	}
	full.Write(body[pkgEnd:])
	full.WriteString(parHelper)
	full.WriteString(parForHelper)
	full.WriteString(makeChansHelper)
	full.WriteString(splitNHelper)
	if t.usedSplitN2D {
		full.WriteString(splitN2DHelper)
	}
	if t.usedAltN {
		full.WriteString(altNHelper)
	}

	return format.Source(full.Bytes())
}

// hasImport reports whether the source appears to already import path
// (e.g. "time"). A plain string-literal scan, not real import parsing —
// good enough for the small examples this tool targets.
func (t *transformer) hasImport(path string) bool {
	quoted := `"` + path + `"`
	for _, tk := range t.toks {
		if tk.tok == token.STRING && tk.lit == quoted {
			return true
		}
	}
	return false
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: bilc source.bil source.go")
		os.Exit(2)
	}
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := Transform(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "transform error:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[2], out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
