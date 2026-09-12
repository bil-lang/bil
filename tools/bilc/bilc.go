// Package bilc is a source-to-source preprocessor: it reads a .bil file (Go
// plus the `par`, `seq`, and `proc` constructs) and emits legal, compilable
// Go.
package bilc

import (
	"bytes"
	"fmt"
	"go/format"
	"go/scanner"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
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
//
// The third return value is reflect.Select's own recvOK — true unless the
// winning channel was closed and drained — plumbed straight through
// rather than discarded, so a comma-ok replicated-alt guard (`chan[i] ->
// v, ok { ... }` / `chan[i] :-> v, ok { ... }`) can detect a closed
// channel exactly like Go's native two-value receive does. Every
// desugared call site always receives all three values (see transform's
// replicated-alt emission) and discards the ones the .bil source didn't
// ask for — altN itself stays a single primitive rather than forking into
// a comma-ok and a non-comma-ok variant.
const altNHelper = `
func altN[T any](chans []chan T, guards ...bool) (int, T, bool) {
	cases := make([]reflect.SelectCase, len(chans))
	for i, ch := range chans {
		if len(guards) > 0 && !guards[i] {
			ch = nil
		}
		cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)}
	}
	chosen, recv, recvOK := reflect.Select(cases)
	return chosen, recv.Interface().(T), recvOK
}
`

type transformer struct {
	file          *token.File
	src           []byte
	toks          []tok
	usedStop      bool // set when a `stop` statement is rewritten
	usedAltN      bool // set when the source calls `altN`
	usedSplitN2D  bool // set when the source calls `splitN2D`
	usedLink      bool // set when a `link[...]` send or receive is rewritten
	usedPlacement bool // set when a `placed par` block is rewritten

	// placeAliases holds every `place NAME at link[EXPR]` declaration's
	// scope, collected by collectPlaceAliases before transform() runs.
	// Scoped per enclosing proc/func body, not file-global -- placement
	// makes multiple distinct, differently-roled procs normal in one
	// file, and a file-global alias table would force artificial
	// cross-proc name uniqueness.
	placeAliases []placeAliasScope
}

// placeAliasScope is one proc/func body's `place`-alias table: NAME ->
// the link index expression's source text (e.g. "east", "3"), resolved
// by matchLinkReceive/matchLinkSend and the link-rejection guards via
// resolvePlaceAlias.
type placeAliasScope struct {
	procName       string // the enclosing proc/func's name -- used by PlacementManifest to group aliases
	bodyLo, bodyHi int
	aliases        map[string]string
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

// resync writes a `//line` directive into out mapping whatever comes next
// back to offset's true position in the original .bil source — see
// flushTo in transform, the only call site. This is what lets error
// positions from a later go/parser-based read of the transpiled Go (both
// tools/vet's own AST walk and any go run compile error or panic) point at
// the .bil file and line instead of the generated Go file, without bilc
// having to track cumulative line drift itself: since t.file already
// knows the true line for any offset regardless of how much synthetic
// text came before, re-anchoring unconditionally before every verbatim
// copy is trivially correct under arbitrary nesting for free, and needs
// no changes to any of the emit*/desugar functions that call flushTo
// (directly, or via a nested transform whose own flushTo does the same).
// go/scanner only recognizes a `//line` comment as a directive when its
// `//` is the very first byte of its physical line — no leading
// whitespace, ever — so out must already be at a fresh line start before
// the directive is written; go/printer has a matching carve-out that
// preserves exactly this shape through a go/format.Source round-trip.
func (t *transformer) resync(out *bytes.Buffer, offset int) {
	// The leading "\n" is unconditional, not just "if out doesn't already
	// end in one": transform recurses (par/seq/alt branches, ...), and a
	// nested call's returned []byte gets spliced straight into its
	// caller's out build up — often right after caller-written text with
	// no trailing newline of its own (e.g. seq's own out.WriteString("{")).
	// The nested call's *own* buffer has no way to know that at the time
	// its first flushTo/resync runs (it may still be empty), so it must
	// always open its own fresh line rather than trusting whatever
	// preceded it once spliced in — an extra blank line where one turns
	// out to be unnecessary is harmless; a directive left glued to
	// preceding text on the same line is not (go/scanner only honors
	// `//line` when it is the first byte of its physical line).
	pos := t.file.Position(t.file.Pos(offset))
	fmt.Fprintf(out, "\n//line %s:%d\n", pos.Filename, pos.Line)
}

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
// tools/vet/channels.go), which only ever compares usage *across*
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

// arrowKind distinguishes Bil's two receive-binding arrows. `->` assigns
// into an already-declared target — occam's own `?` semantics (`x =
// <-c`) — and is what a receive inside a loop needs when the same
// variable is meant to persist across iterations. `:->` declares the
// target fresh at the receive site, mirroring Go's own `:=` for a channel
// receive (`x := <-c`, `case v := <-ch:`) — a deliberate, Go-idiomatic
// extension with no occam equivalent (occam has no inline declare-on-use
// at all). Both arrows carry the same meaning at every receive site — a
// bare statement, a simple `alt` guard, or a replicated `alt` guard —
// rather than the binding rule being an accident of which construct
// happens to surround it.
type arrowKind int

const (
	arrowAssign arrowKind = iota
	arrowDeclare
)

// matchArrow reports whether the tokens starting at i (bounded by hi)
// spell one of Bil's two zero-space receive arrows: `->` (SUB immediately
// followed by GTR) or `:->` (COLON immediately followed by SUB
// immediately followed by GTR). end is the index of the first token past
// the arrow. Go has neither token, so this is the same unspaced-adjacency
// trick `->` alone has always used, extended by one more leading token
// for `:->`; `case` is a first-class Go keyword, so there's no risk of
// this colliding with `:=` (a single DEFINE token) or a slice/label colon
// (never followed by `->`).
func (t *transformer) matchArrow(i, hi int) (kind arrowKind, end int, ok bool) {
	if i+2 < hi &&
		t.toks[i].tok == token.COLON && t.toks[i+1].tok == token.SUB && t.toks[i+2].tok == token.GTR &&
		t.off(t.toks[i+1].pos) == t.off(t.toks[i].pos)+1 &&
		t.off(t.toks[i+2].pos) == t.off(t.toks[i+1].pos)+1 {
		return arrowDeclare, i + 3, true
	}
	if i+1 < hi &&
		t.toks[i].tok == token.SUB && t.toks[i+1].tok == token.GTR &&
		t.off(t.toks[i+1].pos) == t.off(t.toks[i].pos)+1 {
		return arrowAssign, i + 2, true
	}
	return 0, 0, false
}

// matchOkTarget checks for an optional Go-style comma-ok suffix — `, IDENT`
// — immediately after a receive's primary bind target, starting at i
// (bounded by hi). This is what lets `chan -> v, ok` / `chan :-> v, ok`
// desugar to Go's own two-value receive (`v, ok = <-chan` / `v, ok :=
// <-chan`), the idiomatic way to detect a closed, drained channel — `ok`
// is false exactly then. Absent (no comma at all, or a comma not followed
// by a plain identifier — e.g. the comma belongs to something else
// entirely) is not a failure: every single-value receive keeps working
// unchanged, since this suffix is purely additive. The identifier is
// captured via its token literal (`.lit`), not a source-position slice —
// deliberately, to sidestep the trailing-whitespace-in-a-slice pitfall
// that `matchArrow`'s callers already had to work around for the primary
// target (see the `strings.TrimSpace` call sites).
func (t *transformer) matchOkTarget(i, hi int) (okSrc string, end int, hasOk bool) {
	if i >= hi || t.toks[i].tok != token.COMMA {
		return "", i, false
	}
	j := i + 1
	if j >= hi || t.toks[j].tok != token.IDENT {
		return "", i, false
	}
	return t.toks[j].lit, j + 1, true
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
// BIND may be introduced with either arrow (see arrowKind): `-> BIND`
// assigns into an already-declared BIND, `:-> BIND` declares it fresh. An
// optional comma-ok suffix (see matchOkTarget) — `-> BIND, OK` / `:->
// BIND, OK` — extends this to also expose whether the winning receive came
// from a closed, drained channel. Desugars (in transform) to a call to
// altN, the underlying primitive (Example 13) — this is sugar over that,
// not a separate mechanism, the same relationship `par i := range N` has
// to parFor. Returns the loop variable name, the range-expression token
// range, the guard condition's source (empty if unguarded), the
// channel-array's source, the bind variable name and its arrow kind, the
// comma-ok target (if any) and whether one was given, the body token
// range, and the index of the whole construct's closing `}`.
func (t *transformer) isReplicatedAlt(lo, hi int) (varName string, exprLo, exprHi int, condSrc, baseSrc, bindVar string, bindKind arrowKind, okVar string, hasOk bool, bodyLo, bodyHi, closeIdx int, ok bool) {
	fail := func() (string, int, int, string, string, string, arrowKind, string, bool, int, int, int, bool) {
		return "", 0, 0, "", "", "", 0, "", false, 0, 0, 0, false
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
	ak, arrowEnd, akOk := t.matchArrow(j, innerEnd)
	if !akOk {
		return fail()
	}
	bindKind = ak
	j = arrowEnd
	if j >= innerEnd || t.toks[j].tok != token.IDENT {
		return fail()
	}
	bindVar = t.toks[j].lit
	j++
	if src, end, has := t.matchOkTarget(j, innerEnd); has {
		okVar, hasOk, j = src, true, end
	}
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
	return varName, exprLo, exprHi, condSrc, baseSrc, bindVar, bindKind, okVar, hasOk, bodyLo, bodyHi, closeIdx, true
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
	chanSrc string    // conditional guards only: the channel expression's source
	varSrc  string    // conditional guards only: the bind target (may be "_")
	okSrc   string    // conditional guards only: the comma-ok target, if hasOk (may be "_")
	cond    string    // conditional guards only: the boolean pre-condition source, e.g. "(len(q) > 0)"
	kind    arrowKind // conditional guards only: which arrow bound varSrc (non-conditional guards bake this into header already)
	hasOk   bool      // conditional guards only: whether a comma-ok target was given
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
	if t.toks[lo].tok == token.IDENT && (t.toks[lo].lit == "link" || t.isPlaceAlias(lo)) {
		// `link[idx]` isn't a real Go channel (see matchLinkReceive) — the
		// unconditional-guard path below would otherwise match it structurally
		// (parsePrimaryExpr handles `base[idx]` generically) and silently emit
		// a broken `case v = <-link[idx]:`. Rejecting here surfaces the normal
		// "unsupported alt-guard shape" panic instead — alt/pri alt over links
		// isn't supported yet (see ../../emulator/README.md), including over a
		// place-aliased link, same as a literal `link[...]`.
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
		if gLo < hi && t.toks[gLo].tok == token.IDENT && (t.toks[gLo].lit == "link" || t.isPlaceAlias(gLo)) {
			return altGuard{}, false // see the unconditional-guard check above
		}
		lhsEnd, ok1 := t.parsePrimaryExpr(gLo, hi)
		if !ok1 {
			return altGuard{}, false
		}
		kind, arrowEnd, akOk := t.matchArrow(lhsEnd, hi)
		if !akOk {
			return altGuard{}, false
		}
		rhsLo := arrowEnd
		valEnd, isCall, ok2 := t.parseArrowTarget(rhsLo, hi)
		if !ok2 {
			return altGuard{}, false
		}
		brace := valEnd
		var okSrc string
		var hasOk bool
		if !isCall {
			if src, end, has := t.matchOkTarget(valEnd, hi); has {
				okSrc, hasOk, brace = src, true, end
			}
		}
		if brace >= hi || t.toks[brace].tok != token.LBRACE {
			return altGuard{}, false
		}
		var chanSrc, varSrc string
		if isCall {
			// Reconstruct as `chan.Method(args)`, not a raw slice of the
			// source (which would still contain the literal `->` token
			// text) — same substitution the unconditional call-shaped
			// case makes (see below, `chanSrc + "." + callSrc`).
			chanExprSrc := string(t.src[t.off(t.toks[gLo].pos):t.off(t.toks[lhsEnd].pos)])
			callSrc := string(t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[valEnd].pos)])
			chanSrc = chanExprSrc + "." + callSrc
			varSrc = "_"
		} else {
			chanSrc = string(t.src[t.off(t.toks[gLo].pos):t.off(t.toks[lhsEnd].pos)])
			// TrimSpace: the slice runs to the *next* token's start (valEnd
			// is `{`/`,`), which can trail whitespace the way `_ ` before
			// `{` does here — harmless where varSrc only feeds an `=`
			// assignment, but a blank identifier must compare exactly
			// equal to "_" below to catch the illegal `_ := ...` case.
			// okSrc needs no such trim — matchOkTarget captures it via the
			// token's own literal.
			varSrc = strings.TrimSpace(string(t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[valEnd].pos)]))
		}
		return altGuard{cond: condSrc, chanSrc: chanSrc, varSrc: varSrc, okSrc: okSrc, hasOk: hasOk, kind: kind, isCond: true, brace: brace}, true
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
	if !ok1 {
		return altGuard{}, false
	}
	kind, arrowEnd, akOk := t.matchArrow(lhsEnd, hi)
	if !akOk {
		return altGuard{}, false
	}
	rhsLo := arrowEnd
	chanSrc := string(t.src[t.off(t.toks[lo].pos):t.off(t.toks[lhsEnd].pos)])

	valEnd, isCall, ok2 := t.parseArrowTarget(rhsLo, hi)
	if !ok2 {
		return altGuard{}, false
	}
	brace := valEnd
	var okSrc string
	var hasOk bool
	if !isCall {
		if src, end, has := t.matchOkTarget(valEnd, hi); has {
			okSrc, hasOk, brace = src, true, end
		}
	}
	if brace >= hi || t.toks[brace].tok != token.LBRACE {
		return altGuard{}, false
	}
	if isCall {
		// `c -> Method(args)`: not a bind, a method call on `c` whose
		// result is received from — e.g. `time -> After(N)` -> `<-time.After(N)`.
		callSrc := string(t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[valEnd].pos)])
		return altGuard{header: "case <-" + chanSrc + "." + callSrc + ":", brace: brace}, true
	}
	// `chan -> var` assigns into an already-declared var (`case v =
	// <-chan:`); `chan :-> var` declares it fresh, exactly like Go's own
	// `case v := <-chan:` (see arrowKind). Both match the Example 1
	// statement-level `c -> x` / `c :-> x` sugar the same way. An optional
	// comma-ok target (`chan -> v, ok` / `chan :-> v, ok`) extends this to
	// Go's own two-value receive case (`case v, ok = <-chan:` / `case v,
	// ok := <-chan:`), which `select` supports natively — for detecting a
	// closed, drained channel. `=` is blank-identifier-safe (`case _ =
	// <-c:` is legal Go), but `:=` is not (`case _ := <-c:` / `case _, _
	// := <-c:` are both "no new variables on left side of :=") —
	// declaring-and-discarding everything is meaningless anyway, so that
	// combination falls back to a plain discard receive.
	varSrc := strings.TrimSpace(string(t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[valEnd].pos)]))
	switch {
	case hasOk && kind == arrowDeclare && varSrc == "_" && okSrc == "_":
		return altGuard{header: "case <-" + chanSrc + ":", brace: brace}, true
	case hasOk && kind == arrowDeclare:
		return altGuard{header: fmt.Sprintf("case %s, %s := <-%s:", varSrc, okSrc, chanSrc), brace: brace}, true
	case hasOk:
		return altGuard{header: fmt.Sprintf("case %s, %s = <-%s:", varSrc, okSrc, chanSrc), brace: brace}, true
	case kind == arrowDeclare && varSrc == "_":
		return altGuard{header: "case <-" + chanSrc + ":", brace: brace}, true
	case kind == arrowDeclare:
		return altGuard{header: fmt.Sprintf("case %s := <-%s:", varSrc, chanSrc), brace: brace}, true
	default:
		return altGuard{header: fmt.Sprintf("case %s = <-%s:", varSrc, chanSrc), brace: brace}, true
	}
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

// matchArrowReceive recognizes `c -> x` or `c :-> x` as a whole statement —
// sugar for `x = <-c` (assign, `x` already declared) or `x := <-c`
// (declare, `x` fresh) respectively, read left-to-right rather than Go's
// receive-flows-right `x = <-c` (see arrowKind). An optional comma-ok
// suffix (see matchOkTarget) extends this to Go's own two-value receive —
// `c -> x, ok` / `c :-> x, ok` — for detecting a closed, drained channel.
// A call-shaped target generalizes the (no-comma-ok) form to "receive from
// a method call on c" instead of "receive into x" — `c -> Method(args)` ->
// `<-c.Method(args)` (see parseArrowTarget) — an expression statement that
// evaluates/blocks on the receive and discards the value, same as writing
// that Go directly (the arrow kind is irrelevant here since there's
// nothing to bind). rhsLo/rhsHi bound the primary target only, for text
// extraction; matchEnd is the index just past the whole construct
// (primary target, plus the comma-ok suffix if present), for the caller to
// resume scanning from. Only attempted at statement starts (see
// isStmtStart), which also rules out `if`/`for` conditions and call
// arguments — those never follow `;`/`{`.
func (t *transformer) matchArrowReceive(lo, hi int) (rhsLo, rhsHi, lhsEnd int, kind arrowKind, okSrc string, hasOk bool, matchEnd int, isCall, ok bool) {
	if !t.isStmtStart(lo) {
		return 0, 0, 0, 0, "", false, 0, false, false
	}
	lhsEnd, ok = t.parsePrimaryExpr(lo, hi)
	if !ok {
		return 0, 0, 0, 0, "", false, 0, false, false
	}
	var arrowEnd int
	kind, arrowEnd, ok = t.matchArrow(lhsEnd, hi)
	if !ok {
		return 0, 0, 0, 0, "", false, 0, false, false
	}
	rhsLo = arrowEnd
	rhsHi, isCall, ok = t.parseArrowTarget(rhsLo, hi)
	if !ok {
		return 0, 0, 0, 0, "", false, 0, false, false
	}
	matchEnd = rhsHi
	if !isCall {
		if src, end, has := t.matchOkTarget(rhsHi, hi); has {
			okSrc, hasOk, matchEnd = src, true, end
		}
	}
	if matchEnd >= hi || (t.toks[matchEnd].tok != token.SEMICOLON && t.toks[matchEnd].tok != token.RBRACE) {
		return 0, 0, 0, 0, "", false, 0, false, false
	}
	return rhsLo, rhsHi, lhsEnd, kind, okSrc, hasOk, matchEnd, isCall, true
}

// funcBodyRange is one named top-level `proc`/`func` declaration's name
// and body token range, as found by collectFuncBodyRanges.
type funcBodyRange struct {
	name           string
	bodyLo, bodyHi int
}

// collectFuncBodyRanges finds every named top-level `proc`/`func`
// declaration's name and body token range. Go doesn't allow nested named
// declarations, so a flat scan has no overlap to worry about — unlike
// isReplicatedPar/isReplicatedAlt and friends, this doesn't need to be
// invoked from a specific position; it just walks the whole file once.
// Skips a declaration with a return type between its parameter list and
// body (the same "not a shape we recognize" stance
// checkProcChanBothDirections already takes for the same reason) — place
// aliases simply aren't resolvable inside one, matching that check's own
// limitation rather than inventing a new one.
func (t *transformer) collectFuncBodyRanges() (ranges []funcBodyRange) {
	toks := t.toks
	for i := 0; i+2 < len(toks); i++ {
		isProc := toks[i].tok == token.IDENT && toks[i].lit == "proc"
		if !isProc && toks[i].tok != token.FUNC {
			continue
		}
		if toks[i+1].tok != token.IDENT || toks[i+2].tok != token.LPAREN {
			continue
		}
		parenClose := matchParen(toks, i+2)
		if parenClose+1 >= len(toks) || toks[parenClose+1].tok != token.LBRACE {
			continue
		}
		bodyOpen := parenClose + 1
		bodyClose := matchBrace(toks, bodyOpen)
		ranges = append(ranges, funcBodyRange{name: toks[i+1].lit, bodyLo: bodyOpen + 1, bodyHi: bodyClose})
	}
	return ranges
}

// collectPlaceAliases scans every proc/func body range (collectFuncBodyRanges)
// for `place NAME at link[EXPR]` declarations, building each body's own
// alias table (t.placeAliases) — scoped per body, not file-global, since
// placement makes multiple distinct, differently-roled procs normal in
// one file and a file-global table would force artificial cross-proc name
// uniqueness. Run once, early in Transform(), before transform() itself
// (which calls matchPlaceAliasDecl to erase the declaration from the
// output, and matchLinkReceive/matchLinkSend to resolve alias uses via
// resolvePlaceAlias) — mirroring the existing usedAltN/usedSplitN2D
// prescan's own timing. Rejects an alias literally named `link` (it would
// silently and confusingly shadow the real thing) or declared twice in
// the same scope, right here rather than as a separate pass, since this
// scan already visits every declaration in order and duplicate detection
// falls out for free; checkPlaceAliasNames handles the one rule that
// needs whole-file information instead (collision with a proc/func name).
func (t *transformer) collectPlaceAliases() error {
	for _, br := range t.collectFuncBodyRanges() {
		lo, hi := br.bodyLo, br.bodyHi
		var aliases map[string]string
		for i := lo; i < hi; i++ {
			if !(t.toks[i].tok == token.IDENT && t.toks[i].lit == "place" &&
				i+1 < hi && t.toks[i+1].tok == token.IDENT &&
				i+2 < hi && t.toks[i+2].tok == token.IDENT && t.toks[i+2].lit == "at" &&
				i+3 < hi && t.toks[i+3].tok == token.IDENT && t.toks[i+3].lit == "link" &&
				i+4 < hi && t.toks[i+4].tok == token.LBRACK) {
				continue
			}
			name := t.toks[i+1].lit
			open := i + 4
			close := matchBracket(t.toks, open)
			idxExpr := strings.TrimSpace(string(t.src[t.off(t.toks[open+1].pos):t.off(t.toks[close].pos)]))
			if name == "link" {
				return fmt.Errorf("%s: place alias %q may not shadow the reserved `link` identifier", t.file.Position(t.toks[i].pos), name)
			}
			if aliases == nil {
				aliases = make(map[string]string)
			}
			if _, dup := aliases[name]; dup {
				return fmt.Errorf("%s: place alias %q declared more than once in the same scope", t.file.Position(t.toks[i].pos), name)
			}
			aliases[name] = idxExpr
		}
		if aliases != nil {
			t.placeAliases = append(t.placeAliases, placeAliasScope{procName: br.name, bodyLo: lo, bodyHi: hi, aliases: aliases})
		}
	}
	return nil
}

// checkPlaceAliasNames rejects a place alias that collides with any
// top-level proc/func name in the file — the one place-alias-name rule
// that needs whole-file information rather than falling out of
// collectPlaceAliases' own single scoped pass (see its doc comment for
// the other two). A stray reference to an alias name outside its scope
// needs no check here — it passes through untouched and Go's own
// compiler rejects it (`undefined: NAME`), the same fallback this file
// relies on elsewhere (e.g. a wrong `link` index isn't validated either).
func (t *transformer) checkPlaceAliasNames() error {
	procNames := map[string]bool{}
	for i := 0; i+1 < len(t.toks); i++ {
		isProc := t.toks[i].tok == token.IDENT && t.toks[i].lit == "proc"
		if (isProc || t.toks[i].tok == token.FUNC) && t.toks[i+1].tok == token.IDENT {
			procNames[t.toks[i+1].lit] = true
		}
	}
	for _, scope := range t.placeAliases {
		for name := range scope.aliases {
			if procNames[name] {
				return fmt.Errorf("place alias %q collides with a proc/func name", name)
			}
		}
	}
	return nil
}

// isPlaceAlias reports whether the identifier token at i is a resolvable
// place-alias name, without needing the index it resolves to — used by
// the reject-if-`link` guards in parseAltGuard and matchSwitchTypeGuard,
// which stay unsupported for an aliased link exactly like a literal
// `link[...]` already is.
func (t *transformer) isPlaceAlias(i int) bool {
	_, ok := t.resolvePlaceAlias(i)
	return ok
}

// resolvePlaceAlias reports whether the identifier token at i is a
// place-alias name usable at this position — i.e. it falls within some
// proc/func body that declared it via `place NAME at link[EXPR]` (see
// collectPlaceAliases) — returning the link index expression's source
// text it stands for.
func (t *transformer) resolvePlaceAlias(i int) (idxExpr string, ok bool) {
	if t.toks[i].tok != token.IDENT {
		return "", false
	}
	name := t.toks[i].lit
	for _, scope := range t.placeAliases {
		if i < scope.bodyLo || i >= scope.bodyHi {
			continue
		}
		if idx, found := scope.aliases[name]; found {
			return idx, true
		}
	}
	return "", false
}

// matchPlaceAliasDecl recognizes `place NAME at link[EXPR]` as a whole
// statement — a compile-time-only declaration; collectPlaceAliases (run
// earlier, before transform()) does the actual NAME -> EXPR binding, so
// this only needs to recognize the shape and report where it ends, so
// transform can erase it from the output (matching how a bare `skip`
// statement erases itself: just the recognized tokens, leaving whatever
// naturally follows — a `;` or `}` — for the outer scan to copy through
// unchanged, a harmless empty statement). `place`/`at` are contextual
// soft keywords, only meaningful in this exact five-token shape at a
// statement start — neither is a Go keyword, so ordinary identifier use
// elsewhere is unaffected.
func (t *transformer) matchPlaceAliasDecl(lo, hi int) (end int, ok bool) {
	if !t.isStmtStart(lo) {
		return 0, false
	}
	if !(t.toks[lo].tok == token.IDENT && t.toks[lo].lit == "place" &&
		lo+1 < hi && t.toks[lo+1].tok == token.IDENT &&
		lo+2 < hi && t.toks[lo+2].tok == token.IDENT && t.toks[lo+2].lit == "at" &&
		lo+3 < hi && t.toks[lo+3].tok == token.IDENT && t.toks[lo+3].lit == "link" &&
		lo+4 < hi && t.toks[lo+4].tok == token.LBRACK) {
		return 0, false
	}
	close := matchBracket(t.toks, lo+4)
	return close + 1, true
}

// matchLinkReceive recognizes `link[idx] -> var` as a whole statement, or
// `ALIAS -> var` where ALIAS was bound to a link index by a `place ALIAS
// at link[EXPR]` declaration in this same proc/func body (see
// resolvePlaceAlias) — `link` is Bil's reserved, index-addressed array of
// nearest-neighbour links (an emulator concept: see
// ../../emulator/README.md and its nodeprog/bilink package), not a real
// Go channel — the physical link crosses a WASM-instance/Worker boundary,
// which no in-process Go channel can express. `link[idx]` already parses
// as an ordinary primary expression (parsePrimaryExpr handles `base[idx]`
// generically), so matchArrowReceive already matches it structurally;
// this just narrows that match to exactly `link[idx]` (rejecting a
// call-shaped target or any chaining past the single index, like
// `link[idx].field`) — or, for the alias form, to a bare identifier with
// no chaining at all, since an alias always stands for a whole `link[idx]`
// expression, never something further indexable — and reports the index
// and bind-variable source separately, so the caller can emit `var =
// bilink.Recv(idx)` instead of a plain `<-`. Only the assign arrow (`->`)
// with no comma-ok suffix is supported for now — `link[idx] :-> var`
// (declare) and `link[idx] -> var, ok` (comma-ok) aren't part of this
// construct yet, for either spelling.
func (t *transformer) matchLinkReceive(lo, hi int) (idxSrc, varSrc string, end int, ok bool) {
	if lo >= hi || t.toks[lo].tok != token.IDENT {
		return "", "", 0, false
	}
	isLiteralLink := t.toks[lo].lit == "link"
	var aliasIdx string
	if !isLiteralLink {
		aliasIdx, ok = t.resolvePlaceAlias(lo)
		if !ok {
			return "", "", 0, false
		}
	}
	rhsLo, rhsHi, lhsEnd, kind, _, hasOk, matchEnd, isCall, ok := t.matchArrowReceive(lo, hi)
	if !ok || isCall || kind != arrowAssign || hasOk {
		return "", "", 0, false
	}
	if isLiteralLink {
		if lo+1 >= hi || t.toks[lo+1].tok != token.LBRACK {
			return "", "", 0, false
		}
		open := lo + 1
		close := matchBracket(t.toks, open)
		if close+1 != lhsEnd {
			return "", "", 0, false // trailing chain past `link[idx]` — not supported
		}
		idxSrc = string(t.src[t.off(t.toks[open+1].pos):t.off(t.toks[close].pos)])
	} else {
		if lhsEnd != lo+1 {
			return "", "", 0, false // trailing chain past a bare alias — not supported
		}
		idxSrc = aliasIdx
	}
	varSrc = string(t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[rhsHi].pos)])
	return idxSrc, varSrc, matchEnd, true
}

// matchLinkSend recognizes `link[idx] <- value` as a whole statement, or
// `ALIAS <- value` for a resolvable place-alias — the send-side
// counterpart to matchLinkReceive; see its doc comment for why `link`
// needs rewriting to a bilink call rather than passing through as plain
// Go (which is otherwise never needed for a send: `chan <- value` is
// already legal Go, so no other channel send in this file gets
// special-cased the way receives are). value's end is found by scanning
// to the enclosing statement's terminator (the `;` go/scanner inserts at
// line end, or the block's closing `}`), tracking paren/bracket/brace
// depth so a nested call or composite literal in the value expression
// doesn't end the scan early.
func (t *transformer) matchLinkSend(lo, hi int) (idxSrc, valSrc string, end int, ok bool) {
	if !t.isStmtStart(lo) {
		return "", "", 0, false
	}
	if lo >= hi || t.toks[lo].tok != token.IDENT {
		return "", "", 0, false
	}
	var closeAfter int // index of the last token of the index/alias expression
	if t.toks[lo].lit == "link" {
		if lo+1 >= hi || t.toks[lo+1].tok != token.LBRACK {
			return "", "", 0, false
		}
		open := lo + 1
		close := matchBracket(t.toks, open)
		idxSrc = string(t.src[t.off(t.toks[open+1].pos):t.off(t.toks[close].pos)])
		closeAfter = close
	} else {
		aliasIdx, aliasOK := t.resolvePlaceAlias(lo)
		if !aliasOK {
			return "", "", 0, false
		}
		idxSrc = aliasIdx
		closeAfter = lo
	}
	if closeAfter+1 >= hi || t.toks[closeAfter+1].tok != token.ARROW {
		return "", "", 0, false
	}
	valLo := closeAfter + 2
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

// matchSwitchTypeGuard recognizes `CHAN :-> V.(type)` immediately after a
// `switch` keyword — sugar for Go's own type-switch guard, `V := (<-CHAN).
// (type)`, read left-to-right like every other Bil receive (see
// arrowKind) rather than Go's receive-flows-right form. Only the `:->`
// (declare) form applies here, never plain `->`: Go's type-switch guard
// grammar has exactly two shapes, `v := x.(type)` or a bare `x.(type)`
// with no bound variable at all, and no assignment form (`v = x.(type)`)
// exists to fall back to — so an occam-style "assign into an
// already-declared variable" choice simply isn't available for this
// construct, unlike every other Bil receive. V is blank-safe: `V == "_"`
// signals the caller (see transform's `token.SWITCH` case) to emit Go's
// own bare `x.(type)` form instead of the needless-ceremony `_ :=
// x.(type)`, the same "collapse to the simpler underlying form" move
// already used for a blank target elsewhere in this file. Everything
// after the guard — the `case Type:` clauses, the closing `}` — is
// ordinary Go, untouched by this transform; only the header expression up
// to (and including) the opening `{` is rewritten, so no recursion into
// the switch body is needed the way the old (now-removed) `-> case`
// construct required: the body is just more tokens in transform's own
// flat walk, picked up automatically like any other `{ }` block's
// contents.
func (t *transformer) matchSwitchTypeGuard(lo, hi int) (chanSrc, varSrc string, brace int, ok bool) {
	if lo < hi && t.toks[lo].tok == token.IDENT && (t.toks[lo].lit == "link" || t.isPlaceAlias(lo)) {
		return "", "", 0, false // see matchLinkReceive; not a real channel, no type-switch dispatch (aliased or not)
	}
	lhsEnd, ok1 := t.parsePrimaryExpr(lo, hi)
	if !ok1 {
		return "", "", 0, false
	}
	kind, arrowEnd, akOk := t.matchArrow(lhsEnd, hi)
	if !akOk || kind != arrowDeclare {
		return "", "", 0, false
	}
	i := arrowEnd
	if i >= hi || t.toks[i].tok != token.IDENT {
		return "", "", 0, false
	}
	varSrc = t.toks[i].lit
	i++
	if i >= hi || t.toks[i].tok != token.PERIOD {
		return "", "", 0, false
	}
	i++
	if i >= hi || t.toks[i].tok != token.LPAREN {
		return "", "", 0, false
	}
	i++
	if i >= hi || t.toks[i].tok != token.TYPE {
		return "", "", 0, false
	}
	i++
	if i >= hi || t.toks[i].tok != token.RPAREN {
		return "", "", 0, false
	}
	i++
	if i >= hi || t.toks[i].tok != token.LBRACE {
		return "", "", 0, false
	}
	chanSrc = string(t.src[t.off(t.toks[lo].pos):t.off(t.toks[lhsEnd].pos)])
	return chanSrc, varSrc, i, true
}

// splitCommaExprs splits the token range [lo,hi) into comma-separated
// segments, respecting paren/bracket/brace nesting so a call or index
// expression's own internal comma doesn't split early -- needed because
// `processor(...)`'s arguments are arbitrary Go expressions (`cols-1`,
// say), not bare identifiers parsePrimaryExpr could handle. Only the
// segments' source text is needed (Go's own compiler validates each
// expression once transpiled, the same fallback this file relies on
// elsewhere), so this doesn't attempt to parse expression grammar at all
// -- just find top-level commas. ok is false only for an empty range
// (`processor()`, no arguments at all).
func (t *transformer) splitCommaExprs(lo, hi int) (exprs []string, ok bool) {
	if lo >= hi {
		return nil, false
	}
	depth := 0
	segStart := lo
	for i := lo; i < hi; i++ {
		switch t.toks[i].tok {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth--
		case token.COMMA:
			if depth == 0 {
				exprs = append(exprs, strings.TrimSpace(string(t.src[t.off(t.toks[segStart].pos):t.off(t.toks[i].pos)])))
				segStart = i + 1
			}
		}
	}
	exprs = append(exprs, strings.TrimSpace(string(t.src[t.off(t.toks[segStart].pos):t.off(t.toks[hi].pos)])))
	return exprs, true
}

// placedClause is one parsed clause inside a `placed par { ... }` block:
// either `processor(ID) { BODY }` (idExpr set, the flat, topology-agnostic
// form backed by bilink.ID()), `processor(R, C) { BODY }` (rowExpr/colExpr
// set, the grid-convenient form backed by bilink.Row()/Col()), or
// `default { BODY }` (isDefault set). Both processor arities are always
// available side by side in the same block -- neither desugars through the
// other, they're two independent, valid ways to write the same underlying
// switch case (see the "Design decisions" discussion this construct came
// out of: occam's own PROCESSOR is a topology-agnostic flat int, but a 2D
// grid is Bil's dominant case and deserves not to need row*cols+col
// arithmetic by hand).
type placedClause struct {
	isDefault      bool
	idExpr         string
	rowExpr        string
	colExpr        string
	bodyLo, bodyHi int
}

// parsePlacedParClauses parses every clause of a `placed par { ... }` block
// ([lo,hi) is the block's interior; hi is the index of its closing `}`).
// An optional `default { BODY }` must come last if present -- placement's
// whole point is the identity gate, so every other clause must be a
// `processor(...)`, never a bare `proc` call/seq/nested par/bare block the
// way an ordinary `par`'s branches can be.
func (t *transformer) parsePlacedParClauses(lo, hi int) (clauses []placedClause, ok bool) {
	j := lo
	sawDefault := false
	for j < hi {
		if t.toks[j].tok == token.SEMICOLON {
			j++
			continue
		}
		if sawDefault {
			return nil, false // default must be the last clause
		}
		if t.toks[j].tok == token.DEFAULT {
			j++
			if j >= hi || t.toks[j].tok != token.LBRACE {
				return nil, false
			}
			bodyClose := matchBrace(t.toks, j)
			clauses = append(clauses, placedClause{isDefault: true, bodyLo: j + 1, bodyHi: bodyClose})
			j = bodyClose + 1
			sawDefault = true
			continue
		}
		if t.toks[j].tok != token.IDENT || t.toks[j].lit != "processor" {
			return nil, false
		}
		j++
		if j >= hi || t.toks[j].tok != token.LPAREN {
			return nil, false
		}
		parenClose := matchParen(t.toks, j)
		exprs, ok := t.splitCommaExprs(j+1, parenClose)
		if !ok || len(exprs) < 1 || len(exprs) > 2 {
			return nil, false
		}
		j = parenClose + 1
		if j >= hi || t.toks[j].tok != token.LBRACE {
			return nil, false
		}
		bodyClose := matchBrace(t.toks, j)
		cl := placedClause{bodyLo: j + 1, bodyHi: bodyClose}
		if len(exprs) == 1 {
			cl.idExpr = exprs[0]
		} else {
			cl.rowExpr, cl.colExpr = exprs[0], exprs[1]
		}
		clauses = append(clauses, cl)
		j = bodyClose + 1
	}
	if len(clauses) == 0 {
		return nil, false
	}
	return clauses, true
}

// emitPlacedParClauses parses and emits a `placed par { ... }` block as a
// plain Go `switch` -- never `par(...)`/goroutines, since exactly one
// branch may ever execute per running node; reusing `par` (which spawns
// every branch as a goroutine) would run every role's code in this node's
// own process at once. Each clause's `case`/`default` header and body are
// written here directly rather than left for the outer scan to pick up
// its body on its own -- writing the header text through the opening `{`
// ourselves and recursing into the body via t.transform() avoids the
// ASI/`//line`-resync pitfall documented on matchSwitchTypeGuard's
// transform() case: a forced newline right after synthetic text ending in
// a token Go's automatic semicolon insertion fires after (like `)`)
// silently breaks the emitted Go, whereas `{`/`:` are never such tokens.
func (t *transformer) emitPlacedParClauses(out *bytes.Buffer, lo, hi int) {
	clauses, ok := t.parsePlacedParClauses(lo, hi)
	if !ok {
		panic(fmt.Sprintf("unsupported placed-par shape at %q (%v)", t.toks[lo].lit, t.toks[lo].tok))
	}
	out.WriteString("switch {\n")
	for _, cl := range clauses {
		switch {
		case cl.isDefault:
			out.WriteString("default:\n")
		case cl.idExpr != "":
			out.WriteString("case bilink.ID() == " + cl.idExpr + ":\n")
		default:
			out.WriteString("case bilink.Row() == " + cl.rowExpr + " && bilink.Col() == " + cl.colExpr + ":\n")
		}
		out.Write(t.transform(cl.bodyLo, cl.bodyHi))
		out.WriteString("\n")
	}
	out.WriteString("}")
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
			switch {
			case cl.g.hasOk && cl.g.varSrc == "_" && cl.g.okSrc == "_":
				out.WriteString("case <-" + ch + ":\n")
			case cl.g.hasOk && cl.g.kind == arrowDeclare:
				out.WriteString("case " + cl.g.varSrc + ", " + cl.g.okSrc + " := <-" + ch + ":\n")
			case cl.g.hasOk:
				out.WriteString("case " + cl.g.varSrc + ", " + cl.g.okSrc + " = <-" + ch + ":\n")
			case cl.g.varSrc == "_":
				out.WriteString("case <-" + ch + ":\n")
			case cl.g.kind == arrowDeclare:
				out.WriteString("case " + cl.g.varSrc + " := <-" + ch + ":\n")
			default:
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
		switch {
		case cl.g.hasOk && cl.g.varSrc == "_" && cl.g.okSrc == "_":
			cl.header = "case <-" + ch + ":"
		case cl.g.hasOk && cl.g.kind == arrowDeclare:
			cl.header = "case " + cl.g.varSrc + ", " + cl.g.okSrc + " := <-" + ch + ":"
		case cl.g.hasOk:
			cl.header = "case " + cl.g.varSrc + ", " + cl.g.okSrc + " = <-" + ch + ":"
		case cl.g.varSrc == "_":
			cl.header = "case <-" + ch + ":"
		case cl.g.kind == arrowDeclare:
			cl.header = "case " + cl.g.varSrc + " := <-" + ch + ":"
		default:
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
		// Resync before every individual *line* in [cursor, o), not once
		// for the whole span: go/format.Source's own gofmt-style
		// normalization collapses a run of 2+ consecutive blank source
		// lines down to 1 in its formatted output, silently invalidating
		// any line-count arithmetic that spans such a run (confirmed:
		// without this, code following a blank-line gap the transformer
		// itself didn't introduce — ordinary blank lines already present
		// in the .bil source — resynced one line short after formatting).
		// A directive immediately ahead of every line, blank or not,
		// sidesteps that: gofmt is free to delete however many blank
		// lines it likes, since no surviving non-blank line's mapping
		// ever depends on counting through them — each carries its own.
		for cursor < o {
			end := o
			if nl := bytes.IndexByte(t.src[cursor:o], '\n'); nl >= 0 {
				end = cursor + nl + 1
			}
			t.resync(&out, cursor)
			out.Write(t.src[cursor:end])
			cursor = end
		}
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

		case tk.tok == token.IDENT && tk.lit == "placed" &&
			i+1 < hi && t.toks[i+1].tok == token.IDENT && t.toks[i+1].lit == "par" &&
			i+2 < hi && t.toks[i+2].tok == token.LBRACE:
			// `placed par { processor(...) { BODY } ... default { BODY } }`
			// — see emitPlacedParClauses. "placed" and "par" are distinct
			// token literals (never equal), so this needs no ordering
			// relative to the plain `par` cases below the way `pri alt`
			// needs checking before plain `alt` — there's no shape either
			// case here could mistake for the other's.
			flushTo(t.off(tk.pos))
			close := matchBrace(t.toks, i+2)
			t.emitPlacedParClauses(&out, i+3, close)
			cursor = t.off(t.toks[close].pos) + 1
			i = close + 1
			t.usedPlacement = true

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
			// `alt VAR := range EXPR { [(COND) &&] BASE[VAR] -> BIND[, OK]
			// { BODY } }` — replicated alt, attached directly to `alt` —
			// see isReplicatedAlt. Guarded, it first builds a per-replica
			// []bool by evaluating COND once per index — VAR is re-bound
			// each iteration, so COND can reference it (`guards[j]`,
			// `len(q[j]) > 0`, ...) — then passes that to altN, which nils
			// out any replica whose guard came back false (see
			// altNHelper). altN always returns three values now (index,
			// payload, recvOK — see altNHelper); every desugar below
			// captures all three into fixed throwaway names first
			// (bilIdx/bilVal/bilOk), then binds each into its real target
			// or discards it with `_ = bilX` when the .bil source didn't
			// ask for it (OK only matters when a comma-ok target was
			// given). VAR always declares fresh (`:=`) regardless of
			// BIND's arrow — it's the construct's own per-dispatch replica
			// index, nothing outside this block could have predeclared it,
			// and that's independent of whether BIND/OK themselves are
			// being declared or assigned. BIND and OK, by contrast, always
			// share BIND's own op (`:=` for `:->`, `=` for `->` — one op
			// for both, since a single Go statement can't mix `:=` and
			// `=`). Routing every case through the same three throwaway
			// names — rather than a direct `j, v := altN(...)` for the
			// simple declare case — also sidesteps Go's "no new variables
			// on left side of :=" the moment BIND and OK are *both* `_`
			// under `:->`: an all-blank direct declare there would
			// otherwise be illegal, and this way it isn't a special case.
			varName, exprLo, exprHi, condSrc, baseSrc, bindVar, bindKind, okVar, hasOk, bodyLo, bodyHi, closeIdx, ok := t.isReplicatedAlt(i+1, hi)
			if !ok {
				panic(fmt.Sprintf("unsupported alt shape at %q (%v)", t.toks[i+1].lit, t.toks[i+1].tok))
			}
			flushTo(t.off(tk.pos))
			out.WriteString("{\n")
			altCall := "altN(" + baseSrc + ")"
			if condSrc != "" {
				out.WriteString("bilGuards := make([]bool, len(" + baseSrc + "))\n")
				out.WriteString("for " + varName + " := range " + baseSrc + " {\n")
				out.WriteString("bilGuards[" + varName + "] = " + condSrc + "\n")
				out.WriteString("}\n")
				altCall = "altN(" + baseSrc + ", bilGuards...)"
			}
			out.WriteString("bilIdx, bilVal, bilOk := " + altCall + "\n")
			// VAR is always freshly declared here, regardless of BIND's
			// arrow kind: it's the construct's own per-dispatch replica
			// index, with no occam equivalent and nothing outside this
			// block that could have predeclared it. `chans[j]` in the
			// source is what tells isReplicatedAlt which array to alt
			// over, but that indexing expression itself never survives
			// into the output (only the bare array does, as altN's
			// argument), so from the emitted Go's point of view `j` is a
			// fresh binding the body may or may not go on to reference —
			// the extra `_ = j` unconditionally silences "declared and not
			// used" rather than only when the body happens not to
			// reference VAR (confirmed by hitting exactly that compile
			// error on `alt j := range n { chans[j] -> v { println(v) } }`
			// before this safety net existed).
			if varName != "_" {
				out.WriteString(varName + " := bilIdx\n")
				out.WriteString("_ = " + varName + "\n")
			} else {
				out.WriteString("_ = bilIdx\n")
			}
			// BIND and OK, by contrast, follow BIND's own arrow: `:->`
			// declares both fresh, `->` assigns into both (already
			// declared outside) — same op for both, since Go can't mix
			// `:=` and `=` in one statement.
			op := "="
			if bindKind == arrowDeclare {
				op = ":="
			}
			bindTemp := func(target, temp string) {
				if target == "_" {
					out.WriteString("_ = " + temp + "\n")
				} else {
					out.WriteString(target + " " + op + " " + temp + "\n")
				}
			}
			bindTemp(bindVar, "bilVal")
			if hasOk {
				bindTemp(okVar, "bilOk")
			} else {
				out.WriteString("_ = bilOk\n")
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

		case tk.tok == token.SWITCH:
			if chanSrc, varSrc, brace, ok := t.matchSwitchTypeGuard(i+1, hi); ok {
				// The `{` is written here, as part of this synthetic
				// header, rather than left for the outer scan to copy
				// verbatim the way most other single-line rewrites do
				// (e.g. matchArrowReceive's) — because unlike those, the
				// next real token after our inserted text isn't a natural
				// statement terminator (`;`/`}`) but `{` itself, and
				// flushTo's unconditional per-line //line-directive resync
				// (see resync's doc comment) would otherwise land a
				// forced newline directly after our `.(type)`'s `)` —
				// exactly the token Go's automatic semicolon insertion
				// fires after, silently turning the guard into `... .
				// (type);` and breaking the type-switch grammar entirely
				// (confirmed: produces "use of .(type) outside type
				// switch"). Writing `{` (never an ASI trigger) ourselves
				// and recursing into the body — the same pattern
				// `alt`/`par`/replicated-alt already use for their own
				// bodies — sidesteps it: resync's next forced newline
				// lands right after a safe token instead.
				flushTo(t.off(tk.pos))
				if varSrc == "_" {
					out.WriteString("switch (<-" + chanSrc + ").(type) {\n")
				} else {
					out.WriteString("switch " + varSrc + " := (<-" + chanSrc + ").(type) {\n")
				}
				closeIdx := matchBrace(t.toks, brace)
				out.Write(t.transform(brace+1, closeIdx))
				out.WriteString("\n}")
				cursor = t.off(t.toks[closeIdx].pos) + 1
				i = closeIdx + 1
			} else {
				i++
			}

		case tk.tok == token.IDENT:
			if end, ok := t.matchPlaceAliasDecl(i, hi); ok {
				flushTo(t.off(tk.pos))
				cursor = t.off(t.toks[end].pos)
				i = end
			} else if idxSrc, varSrc, end, ok := t.matchLinkReceive(i, hi); ok {
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
			} else if rhsLo, rhsHi, lhsEnd, kind, okSrc, hasOk, matchEnd, isCall, ok := t.matchArrowReceive(i, hi); ok {
				flushTo(t.off(tk.pos))
				// TrimSpace: the slice runs to the *next* token's start
				// (rhsHi is `;`/`}`/`,`), which can trail whitespace —
				// harmless where varSrc only feeds an `=` assignment, but a
				// blank identifier must compare exactly equal to "_" below
				// to catch the illegal `_ := ...` / `_, _ := ...` cases.
				// okSrc needs no such trim — matchOkTarget captures it via
				// the token's own literal, not a position slice.
				varSrc := strings.TrimSpace(string(t.src[t.off(t.toks[rhsLo].pos):t.off(t.toks[rhsHi].pos)]))
				chanSrc := t.src[t.off(tk.pos):t.off(t.toks[lhsEnd].pos)]
				switch {
				case isCall:
					out.WriteString("<-")
					out.Write(chanSrc)
					out.WriteString(".")
					out.WriteString(varSrc)
				case hasOk && kind == arrowDeclare && varSrc == "_" && okSrc == "_":
					// `_, _ := <-c` isn't legal Go (no new variables on the
					// left of :=) — declaring and discarding both is
					// meaningless anyway, so fall back to a plain discard.
					out.WriteString("<-")
					out.Write(chanSrc)
				case hasOk && kind == arrowDeclare:
					out.WriteString(varSrc)
					out.WriteString(", ")
					out.WriteString(okSrc)
					out.WriteString(" := <-")
					out.Write(chanSrc)
				case hasOk:
					out.WriteString(varSrc)
					out.WriteString(", ")
					out.WriteString(okSrc)
					out.WriteString(" = <-")
					out.Write(chanSrc)
				case kind == arrowDeclare && varSrc == "_":
					// `_ := <-c` isn't legal Go ("no new variables on left
					// side of :="; _ never counts as new) — declaring and
					// discarding is meaningless anyway, so fall back to a
					// plain discard receive.
					out.WriteString("<-")
					out.Write(chanSrc)
				case kind == arrowDeclare:
					out.WriteString(varSrc)
					out.WriteString(" := <-")
					out.Write(chanSrc)
				default:
					out.WriteString(varSrc)
					out.WriteString(" = <-")
					out.Write(chanSrc)
				}
				cursor = t.off(t.toks[matchEnd].pos)
				i = matchEnd
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

// singleCallText reports whether the body [lo,hi) reduces to exactly one
// bare call statement (`name(...)`, no other statements before or after,
// modulo semicolons) — and if so, returns the called function's name.
// Used only by PlacementManifest, to decide whether a placement clause's
// body can be summarized as "this proc runs here"; a clause whose body
// does anything else (an if/else, several statements, ...) is recorded in
// the manifest as structurally present but left unresolved — see
// PlacementManifest's own doc comment for why that's the honest answer
// rather than a bug to fix.
func (t *transformer) singleCallText(lo, hi int) (name string, ok bool) {
	i := lo
	for i < hi && t.toks[i].tok == token.SEMICOLON {
		i++
	}
	if i >= hi || t.toks[i].tok != token.IDENT {
		return "", false
	}
	nameTok := i
	if i+1 >= hi || t.toks[i+1].tok != token.LPAREN {
		return "", false
	}
	close := matchParen(t.toks, i+1)
	j := close + 1
	for j < hi && t.toks[j].tok == token.SEMICOLON {
		j++
	}
	if j != hi {
		return "", false // more than the one statement
	}
	return t.toks[nameTok].lit, true
}

// yamlScalar renders expr — an arbitrary Go expression's source text, not
// a YAML-native value — as a single YAML flow scalar: unquoted only when
// it's a plain integer literal (so `id: 0` reads cleanly), double-quoted
// otherwise (`col: "cols-1"`), since almost everything placement deals in
// (`cols-1`, `north`, a role name) is source text YAML would otherwise
// try, and fail, to parse as one of its own types.
func yamlScalar(expr string) string {
	isInt := len(expr) > 0
	for i, r := range expr {
		if r >= '0' && r <= '9' {
			continue
		}
		if i == 0 && r == '-' {
			continue
		}
		isInt = false
		break
	}
	if isInt {
		return expr
	}
	return `"` + strings.ReplaceAll(expr, `"`, `\"`) + `"`
}

// PlacementManifest builds the topology manifest for a `placed par`-using
// .bil source file — a small, deliberately incomplete YAML description of
// a program's declared placement structure (which processor(s) run which
// proc, and each proc's link aliases), pulled forward from the much
// bigger, unstarted "layout tooling" subproject (processor -> host,
// channel -> link) because it falls out of bilc's own parsing almost for
// free: matchPlacedPar/parsePlacedParClauses and collectPlaceAliases
// already fully parse this structure to emit the runtime `switch`, so
// serializing it costs little more. Returns (nil, nil) for a file with no
// `placed par` block at all — most .bil files — so callers can treat a
// nil result as "nothing to write" without a separate has-any check of
// their own. Deliberately not a build-time image splitter, host mapper,
// or bootstrap protocol — those need real design work this file doesn't
// attempt.
//
// The manifest can only ever describe *structure*, never fully-resolved
// concrete positions: `processor(0, cols-1)` depends on `cols`, a value
// chosen when a program is launched, which bilc never knows at transform
// time. It also can't always name a single proc for a clause — the
// canonical worked example's `default` branch has an if/else inside it,
// so that clause is recorded with no `proc:` line rather than a guess
// (see singleCallText). This is intentional, honest incompleteness, not
// a bug: resolving it further would mean fixing grid dimensions at
// bilc-compile time, which is exactly the heterogeneous-multi-image
// build model this design deliberately isn't taking on.
//
// Only the first `placed par` block in a file is described — v1 only
// ever has one in practice, since the construct is a standalone
// statement (not nestable), so a file with more than one is unusual
// enough not to need the manifest format to anticipate it yet.
func PlacementManifest(filename string, src []byte) ([]byte, error) {
	if abs, err := filepath.Abs(filename); err == nil {
		filename = abs
	}
	t := tokenize(filename, src)
	if err := t.collectPlaceAliases(); err != nil {
		return nil, err
	}
	var clauses []placedClause
	for i := 0; i+2 < len(t.toks); i++ {
		if !(t.toks[i].tok == token.IDENT && t.toks[i].lit == "placed" &&
			t.toks[i+1].tok == token.IDENT && t.toks[i+1].lit == "par" &&
			t.toks[i+2].tok == token.LBRACE) {
			continue
		}
		close := matchBrace(t.toks, i+2)
		var ok bool
		clauses, ok = t.parsePlacedParClauses(i+3, close)
		if !ok {
			clauses = nil
		}
		break
	}
	if len(clauses) == 0 {
		return nil, nil
	}

	var out bytes.Buffer
	out.WriteString("placements:\n")
	var defaultClause *placedClause
	for i := range clauses {
		cl := &clauses[i]
		if cl.isDefault {
			defaultClause = cl
			continue
		}
		out.WriteString("  - match: {")
		if cl.idExpr != "" {
			fmt.Fprintf(&out, "id: %s", yamlScalar(cl.idExpr))
		} else {
			fmt.Fprintf(&out, "row: %s, col: %s", yamlScalar(cl.rowExpr), yamlScalar(cl.colExpr))
		}
		out.WriteString("}\n")
		if proc, ok := t.singleCallText(cl.bodyLo, cl.bodyHi); ok {
			fmt.Fprintf(&out, "    proc: %s\n", proc)
		}
	}
	if defaultClause != nil {
		out.WriteString("default:\n")
		if proc, ok := t.singleCallText(defaultClause.bodyLo, defaultClause.bodyHi); ok {
			fmt.Fprintf(&out, "  proc: %s\n", proc)
		}
	}

	if len(t.placeAliases) > 0 {
		scopes := append([]placeAliasScope(nil), t.placeAliases...)
		sort.Slice(scopes, func(i, j int) bool { return scopes[i].procName < scopes[j].procName })
		out.WriteString("links:\n  aliases:\n")
		for _, scope := range scopes {
			names := make([]string, 0, len(scope.aliases))
			for name := range scope.aliases {
				names = append(names, name)
			}
			sort.Strings(names)
			fmt.Fprintf(&out, "    %s: {", scope.procName)
			for i, name := range names {
				if i > 0 {
					out.WriteString(", ")
				}
				fmt.Fprintf(&out, "%s: %s", name, yamlScalar(scope.aliases[name]))
			}
			out.WriteString("}\n")
		}
	}
	return out.Bytes(), nil
}

func Transform(filename string, src []byte) ([]byte, error) {
	// Absolute, not whatever filename came in as: the `//line` directives
	// resync writes (see resync) end up in a Go file that go/parser reads
	// from a different directory entirely (bil run's os.CreateTemp lands
	// in os.TempDir(), not alongside the .bil source) — go/scanner
	// resolves a *relative* //line filename against the directory of the
	// file containing the directive, not the caller's original working
	// directory, so a relative .bil path here would silently resolve to a
	// nonexistent path under the temp directory instead of the real .bil
	// file. filepath.Abs only errors if os.Getwd fails, which nothing
	// downstream could recover from either; fall back to the given
	// filename as-is rather than failing the whole transform over it.
	if abs, err := filepath.Abs(filename); err == nil {
		filename = abs
	}
	t := tokenize(filename, src)
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
	if err := t.collectPlaceAliases(); err != nil {
		return nil, err
	}
	if err := t.checkPlaceAliasNames(); err != nil {
		return nil, err
	}
	body := t.transform(0, len(t.toks)-1) // exclude EOF sentinel

	// Inject `import "sync"` right after the package clause, and append the
	// par helper at the end of the file (see parHelper's comment for why).
	// pkgStart skips past whatever transform's first flushTo call resync'd
	// ahead of the package clause itself (see resync: a blank line, then a
	// `//line ...` directive) to find where "package main" actually
	// starts, so pkgEnd lands on the newline that ends *that* line rather
	// than one of the lines preceding it — landing early would sever the
	// leading directive from "package main" (illegal: nothing may precede
	// `package`) when the import/build-tag injection below splices in
	// between them.
	pkgStart := 0
	for {
		lineEnd := bytes.IndexByte(body[pkgStart:], '\n')
		if lineEnd < 0 {
			break
		}
		line := body[pkgStart : pkgStart+lineEnd]
		if len(line) != 0 && !bytes.HasPrefix(line, []byte("//line ")) {
			break
		}
		pkgStart += lineEnd + 1
	}
	pkgEnd := bytes.IndexByte(body[pkgStart:], '\n')
	if pkgEnd < 0 {
		pkgEnd = len(body) - pkgStart
	}
	pkgEnd += pkgStart
	var full bytes.Buffer
	if t.usedLink || t.usedPlacement {
		// bilink only exists under this build constraint (it's backed by
		// syscall/js) -- a program using `link[...]` or `placed par`
		// (which also calls into bilink, for Row()/Col()/ID()) can only
		// ever run there, so this is auto-injected the same way the
		// import is, rather than left for whoever places the generated
		// file to remember by hand.
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
	if (t.usedLink || t.usedPlacement) && !t.hasImport("emulator/nodeprog/bilink") {
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
