package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
)

// deployManifest mirrors bilc.DeployManifest's JSON shape (see
// tools/bilc/bilc.go's jsonManifest/jsonLeaf/jsonMatch/jsonCondition) --
// duplicated here rather than imported, since those types are unexported
// and the JSON shape is small and stable. Only the fields inferGridSize
// actually needs are kept.
type deployManifest struct {
	Leaves []deployLeaf `json:"leaves"`
}

type deployLeaf struct {
	Match deployMatch `json:"match"`
}

type deployMatch struct {
	ID      string `json:"id,omitempty"`
	Row     string `json:"row,omitempty"`
	Col     string `json:"col,omitempty"`
	Default bool   `json:"default,omitempty"`
}

// maxGridSearch bounds inferGridSize's search -- generous for any
// realistic placed-par program (every example so far needs at most a
// handful of rows/cols), while keeping a runaway search bounded.
const maxGridSearch = 32

// rcLeaf is an explicit (non-default, non-id) leaf's row/col match,
// tracking which axes (if any) are wildcarded (`processor(0, *)`) --
// distinct from a fully-resolved position, since whether an axis is
// wildcarded matters for collision detection (see fits).
type rcLeaf struct {
	rowExpr, colExpr string
	rowWild, colWild bool
}

// concreteID is a flat processor(ID)-form leaf's id expression.
type concreteID struct{ idExpr string }

// inferGridSize computes the smallest rows x cols grid at which every
// explicit leaf in a DeployManifest is unambiguously reachable -- see the
// emu command's design notes for why this is computed rather than
// guessed or required as a flag.
//
// A `default` leaf never needs a distinct cell: by construction it
// covers whatever no more specific leaf claims. A leaf that wildcards
// one axis (`processor(0, *)`) is *meant* to overlap with a more
// specific leaf sharing its other axis (e.g. `processor(0,0)`) -- that's
// the same "catch the rest" layering as `default`, not a collision, so
// two leaves with *different* wildcard patterns are never compared
// against each other at all. The real bug this guards against is two
// leaves with the *same* wildcard pattern whose concrete axes evaluate
// to the same value -- e.g. `processor(*, 0)` and `processor(*,
// cols-1)` both wildcard row and differ only in col, so they collide
// exactly when cols=1 (both resolve to column 0) and the first-declared
// one silently wins for every row, the second never firing at all.
// Leaves are otherwise addressed by flat id (`processor(ID)`, checked
// against id values directly, never converted to row/col).
//
// Returns an error only if no size within maxGridSearch works, or a
// match/id expression fails to parse/evaluate -- both should be
// unreachable for any real bilc-generated manifest.
func inferGridSize(manifest []byte) (rows, cols int, err error) {
	var dm deployManifest
	if err := json.Unmarshal(manifest, &dm); err != nil {
		return 0, 0, fmt.Errorf("parsing deploy manifest: %w", err)
	}

	var byRC []rcLeaf
	var byID []concreteID
	for _, leaf := range dm.Leaves {
		m := leaf.Match
		switch {
		case m.Default:
			continue
		case m.ID != "":
			byID = append(byID, concreteID{m.ID})
		default:
			byRC = append(byRC, rcLeaf{
				rowExpr: m.Row, colExpr: m.Col,
				rowWild: m.Row == "*", colWild: m.Col == "*",
			})
		}
	}
	if len(byRC) == 0 && len(byID) == 0 {
		return 1, 1, nil // nothing explicit to distinguish -- e.g. a placed-par with only a default clause
	}

	// Prefer the emulator page's own long-standing default (6x7,
	// static/index.html) when it's collision-free -- the smallest
	// distinguishing grid is often correct but visually thin (e.g. a
	// 3-role row-0 demo's minimum is 1x2, which never actually shows
	// its relay/idle roles at all). Only search for something else when
	// a program's own placement genuinely needs more than 6x7.
	const defaultRows, defaultCols = 6, 7
	if fits(byRC, byID, defaultRows, defaultCols) {
		return defaultRows, defaultCols, nil
	}
	for cols := 1; cols <= maxGridSearch; cols++ {
		for rows := 1; rows <= maxGridSearch; rows++ {
			if fits(byRC, byID, rows, cols) {
				return rows, cols, nil
			}
		}
	}
	return 0, 0, fmt.Errorf("no grid up to %dx%d distinguishes every placed processor -- pass -rows/-cols explicitly", maxGridSearch, maxGridSearch)
}

func fits(byRC []rcLeaf, byID []concreteID, rows, cols int) bool {
	type resolved struct {
		row, col         int
		rowWild, colWild bool
	}
	resolvedLeaves := make([]resolved, 0, len(byRC))
	for _, l := range byRC {
		var r, c int
		if !l.rowWild {
			v, err := evalGridExpr(l.rowExpr, rows, cols)
			if err != nil || v < 0 || v >= rows {
				return false
			}
			r = v
		}
		if !l.colWild {
			v, err := evalGridExpr(l.colExpr, rows, cols)
			if err != nil || v < 0 || v >= cols {
				return false
			}
			c = v
		}
		resolvedLeaves = append(resolvedLeaves, resolved{r, c, l.rowWild, l.colWild})
	}
	for i := range resolvedLeaves {
		for j := i + 1; j < len(resolvedLeaves); j++ {
			a, b := resolvedLeaves[i], resolvedLeaves[j]
			if a.rowWild != b.rowWild || a.colWild != b.colWild {
				continue // different specificity -- intentional layering, not a collision
			}
			rowSame := a.rowWild || a.row == b.row
			colSame := a.colWild || a.col == b.col
			if rowSame && colSame {
				return false // same wildcard pattern, indistinguishable on every concrete axis
			}
		}
	}
	seenID := map[int]bool{}
	for _, l := range byID {
		id, err := evalGridExpr(l.idExpr, rows, cols)
		if err != nil {
			return false
		}
		if id < 0 || id >= rows*cols {
			return false
		}
		if seenID[id] {
			return false
		}
		seenID[id] = true
	}
	return true
}

// evalGridExpr evaluates a bilc match/id expression (arithmetic only --
// int literals, `rows`/`cols` identifiers, `+ - * /`, parens; never the
// comparison/boolean operators a leaf's separate if/else conditions use,
// since match expressions are themselves never boolean) against a
// candidate grid size, using go/parser rather than a hand-rolled
// tokenizer since the grammar is exactly a subset of real Go expression
// syntax.
func evalGridExpr(expr string, rows, cols int) (int, error) {
	node, err := parser.ParseExpr(expr)
	if err != nil {
		return 0, fmt.Errorf("parsing %q: %w", expr, err)
	}
	return evalGridNode(node, rows, cols)
}

func evalGridNode(n ast.Expr, rows, cols int) (int, error) {
	switch n := n.(type) {
	case *ast.BasicLit:
		if n.Kind != token.INT {
			return 0, fmt.Errorf("non-integer literal %q", n.Value)
		}
		var v int
		if _, err := fmt.Sscanf(n.Value, "%d", &v); err != nil {
			return 0, err
		}
		return v, nil
	case *ast.Ident:
		switch n.Name {
		case "rows":
			return rows, nil
		case "cols":
			return cols, nil
		}
		return 0, fmt.Errorf("unknown identifier %q in match expression", n.Name)
	case *ast.ParenExpr:
		return evalGridNode(n.X, rows, cols)
	case *ast.UnaryExpr:
		x, err := evalGridNode(n.X, rows, cols)
		if err != nil {
			return 0, err
		}
		switch n.Op {
		case token.SUB:
			return -x, nil
		case token.ADD:
			return x, nil
		}
		return 0, fmt.Errorf("unsupported unary operator %v in match expression", n.Op)
	case *ast.BinaryExpr:
		x, err := evalGridNode(n.X, rows, cols)
		if err != nil {
			return 0, err
		}
		y, err := evalGridNode(n.Y, rows, cols)
		if err != nil {
			return 0, err
		}
		switch n.Op {
		case token.ADD:
			return x + y, nil
		case token.SUB:
			return x - y, nil
		case token.MUL:
			return x * y, nil
		case token.QUO:
			if y == 0 {
				return 0, fmt.Errorf("division by zero in match expression")
			}
			return x / y, nil
		}
		return 0, fmt.Errorf("unsupported operator %v in match expression", n.Op)
	default:
		return 0, fmt.Errorf("unsupported expression shape %T in match expression", n)
	}
}
